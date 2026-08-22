package extract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"
)

// IsInIsolatedChild reports whether this process was spawned as an extraction
// child. Useful for a host application that wants to skip expensive startup
// work (opening database pools, joining a cluster) in a process that is only
// going to parse one file and exit.
func IsInIsolatedChild() bool { return os.Getenv(childEnvMarker) == "1" }

// RunIsolatedChildIfInvoked must be the first statement in a host
// application's main() if it wants [IsolatedExtractor] to work:
//
//	func main() {
//	    extract.RunIsolatedChildIfInvoked()
//	    // ... normal startup
//	}
//
// In the normal case it returns immediately and does nothing. When the
// process was spawned as an extraction child it reads one document from
// stdin, parses it with a [LocalExtractor], writes the result to stdout, and
// **exits without returning** — so nothing after the call runs in a child.
//
// A library cannot arrange this for itself: ragit does not own main(), so it
// cannot intercept startup the way an application re-invoking its own hidden
// subcommand can. This one-line requirement is the cost of that.
func RunIsolatedChildIfInvoked() {
	if !IsInIsolatedChild() {
		return
	}
	os.Exit(runChild(os.Stdin, os.Stdout))
}

// runChild is the child's whole life: read, cap memory, parse, reply.
func runChild(stdin io.Reader, stdout io.Writer) int {
	limit := parseMemoryLimit(os.Getenv(childEnvMemoryCap))
	stopWatchdog := enforceMemoryLimit(limit)
	defer stopWatchdog()

	payload, err := io.ReadAll(stdin)
	if err != nil {
		return writeChildError(stdout, fmt.Sprintf("read request: %v", err))
	}

	var req childRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return writeChildError(stdout, fmt.Sprintf("decode request: %v", err))
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		return writeChildError(stdout, fmt.Sprintf("decode document: %v", err))
	}

	result, err := NewLocalExtractor().Extract(context.Background(), data, req.Filename)
	if err != nil {
		return writeChildError(stdout, err.Error())
	}

	_ = json.NewEncoder(stdout).Encode(childResponse{
		Text:      result.Text,
		PageCount: result.PageCount,
		Metadata:  result.Metadata,
	})
	return 0
}

// writeChildError reports a document-level verdict: the child parsed and
// concluded the document is bad. Exit code exitDocumentRejected tells the
// parent this is terminal, not an availability problem — the distinction
// design.md §6 turns on.
func writeChildError(stdout io.Writer, msg string) int {
	_ = json.NewEncoder(stdout).Encode(childResponse{Error: msg})
	return exitDocumentRejected
}

func parseMemoryLimit(raw string) int64 {
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 {
		return DefaultChildMemoryLimit
	}
	return limit
}

// watchdogInterval is how often the child checks its own footprint. Short
// enough to catch a runaway allocation loop before it reaches the host's
// cgroup limit, long enough to cost nothing.
const watchdogInterval = 50 * time.Millisecond

// enforceMemoryLimit applies a soft GC target and a hard self-kill watchdog.
//
// Why not setrlimit: RLIMIT_AS is the obvious answer and the wrong one for a
// Go program. The runtime reserves large regions of virtual address space up
// front, so an RLIMIT_AS low enough to be a useful cap is frequently low
// enough to prevent the process from starting at all, and the failure mode
// differs by platform and Go version. RLIMIT_DATA only covers mmap on Linux
// ≥4.7 and does nothing useful on macOS.
//
// So: debug.SetMemoryLimit makes the GC work hard to stay under the cap
// (which is enough for merely wasteful parsing), and the watchdog handles the
// case it cannot — a single enormous allocation, or a live heap that genuinely
// will not shrink. Exiting via exitMemoryExceeded lets the parent report a
// containment event rather than a mystery signal.
//
// This is best-effort, and deliberately so. The strongest containment for a
// deployment is still a cgroup memory limit on the container; this layer's job
// is to make a blow-up kill a disposable child instead of leaving the choice
// of victim to the kernel.
func enforceMemoryLimit(limit int64) (stop func()) {
	debug.SetMemoryLimit(limit)

	// The hard trip point sits above the GC target: below it, exceeding the
	// soft limit is normal and simply means the collector is doing its job.
	hardLimit := limit + limit/4

	check := func() {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		if int64(stats.Sys) > hardLimit {
			fmt.Fprintf(os.Stderr, "ragit: extraction child exceeded %d bytes (using %d), aborting\n", limit, stats.Sys)
			os.Exit(exitMemoryExceeded)
		}
	}

	// Check once up front as well as on the interval. Parsing a small
	// document can finish inside a single tick, so an interval-only watchdog
	// would let a child run to completion under a cap it never actually
	// respected — and a cap smaller than the Go runtime's own footprint is a
	// misconfiguration worth reporting rather than silently ignoring.
	check()

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(watchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				check()
			}
		}
	}()

	var once bool
	return func() {
		if !once {
			once = true
			close(done)
		}
	}
}
