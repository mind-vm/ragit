package extract

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Sentinel env vars naming an isolation child. Environment rather than argv
// because a host application parses its own argv and must not have to know
// about, or tolerate, an extra subcommand appearing in it.
const (
	childEnvMarker    = "RAGIT_ISOLATED_EXTRACT"
	childEnvMemoryCap = "RAGIT_ISOLATED_MEMORY_BYTES"
)

// Exit codes the child uses to tell the parent *why* it died. The parent
// cannot otherwise distinguish "this document is bad" from "this process ran
// out of room", and per design.md §6 those must be handled oppositely.
const (
	exitDocumentRejected = 3
	exitMemoryExceeded   = 4
)

// DefaultChildMemoryLimit caps an isolation child. Matches the 512 MB used by
// the reference implementation in design.md §6.
const DefaultChildMemoryLimit = 512 << 20

// DefaultChildTimeout bounds one isolated extraction.
const DefaultChildTimeout = 60 * time.Second

// IsolatedExtractor runs local parsing in a short-lived child process with a
// memory ceiling and a timeout.
//
// This is the containment layer from design.md §6. Its value is structural
// rather than parser-specific: a blow-up in *any* parser — including one not
// yet audited, or a future dependency upgrade — kills a child process that
// owns nothing, and the parent marks that one document failed and carries on.
// Without it, the kernel's OOM killer picks a victim host-wide, and the
// application is merely the most likely one, not the only eligible one.
//
// # Wiring requirement
//
// A library cannot re-invoke itself the way an application can: ragit does not
// own main(). The host application must call [RunIsolatedChildIfInvoked] as
// the first statement in main(), which is what makes its binary able to serve
// as its own extraction child. Without that call, IsolatedExtractor's children
// start the host's normal startup path instead of parsing, so Extract fails
// with ErrUnavailable and a Chain falls through to the next layer — degraded,
// but not broken.
type IsolatedExtractor struct {
	// MemoryLimit caps the child. Zero means DefaultChildMemoryLimit.
	MemoryLimit int64
	// Timeout bounds one extraction. Zero means DefaultChildTimeout.
	Timeout time.Duration
	// binary is the executable to re-invoke. Zero value means this process's
	// own; overridden in tests.
	binary string
}

var _ Extractor = (*IsolatedExtractor)(nil)

// NewIsolatedExtractor builds an IsolatedExtractor with default limits.
func NewIsolatedExtractor() *IsolatedExtractor { return &IsolatedExtractor{} }

func (e *IsolatedExtractor) memoryLimit() int64 {
	if e.MemoryLimit > 0 {
		return e.MemoryLimit
	}
	return DefaultChildMemoryLimit
}

func (e *IsolatedExtractor) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return DefaultChildTimeout
}

// childRequest and childResponse are the parent/child wire format: one JSON
// object each way over stdin/stdout. Document bytes are base64-encoded so a
// binary PDF cannot terminate the JSON stream early.
type childRequest struct {
	Filename string `json:"filename"`
	Data     string `json:"data"`
}

type childResponse struct {
	Text      string          `json:"text"`
	PageCount int             `json:"page_count"`
	Metadata  json.RawMessage `json:"metadata"`
	Error     string          `json:"error,omitempty"`
}

// Extract parses data in a capped child process.
//
// Failures are classified per design.md §6's fallback rule:
//   - failing to spawn, or a child that died without a verdict (OOM kill,
//     timeout, missing RunIsolatedChildIfInvoked wiring) → ErrUnavailable,
//     so a Chain moves on;
//   - a child that parsed and rejected the document → a plain error, so a
//     Chain stops. A bad document does not buy its way into a less-contained
//     parser.
func (e *IsolatedExtractor) Extract(ctx context.Context, data []byte, filename string) (*Result, error) {
	binary := e.binary
	if binary == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("%w: locate own executable: %v", ErrUnavailable, err)
		}
		binary = self
	}

	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	payload, err := json.Marshal(childRequest{
		Filename: filename,
		Data:     base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return nil, fmt.Errorf("extract: encode child request: %w", err)
	}

	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(),
		childEnvMarker+"=1",
		childEnvMemoryCap+"="+strconv.FormatInt(e.memoryLimit(), 10),
	)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: extraction child exceeded %s", ErrUnavailable, e.timeout())
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			switch exitErr.ExitCode() {
			case exitDocumentRejected:
				// The child parsed and reached a verdict. Terminal.
				return nil, childVerdict(stdout.Bytes(), stderr.String())
			case exitMemoryExceeded:
				return nil, fmt.Errorf("%w: extraction child exceeded its %d-byte memory cap",
					ErrUnavailable, e.memoryLimit())
			}
		}
		// Killed by a signal, failed to exec, or exited without a verdict —
		// including a host binary that never called
		// RunIsolatedChildIfInvoked and simply ran its own startup instead.
		return nil, fmt.Errorf("%w: extraction child failed: %v: %s",
			ErrUnavailable, runErr, truncate(strings.TrimSpace(stderr.String()), 200))
	}

	var resp childResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		// Exit 0 but unintelligible output means this binary is not wired as
		// an extraction child. Treat as unavailable so the chain degrades.
		return nil, fmt.Errorf("%w: extraction child produced no usable response (is RunIsolatedChildIfInvoked wired into main?): %v",
			ErrUnavailable, err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("extract: %s", resp.Error)
	}

	return &Result{Text: resp.Text, PageCount: resp.PageCount, Metadata: resp.Metadata}, nil
}

func childVerdict(stdout []byte, stderr string) error {
	var resp childResponse
	if json.Unmarshal(stdout, &resp) == nil && resp.Error != "" {
		return fmt.Errorf("extract: %s", resp.Error)
	}
	if stderr != "" {
		return fmt.Errorf("extract: child rejected document: %s", truncate(strings.TrimSpace(stderr), 200))
	}
	return errors.New("extract: child rejected document")
}
