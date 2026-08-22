package extract

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Chain tries each extractor in order, falling back to the next one only
// when the current one was *unavailable*.
//
// The narrowness of that rule is the entire point, and it is the most
// expensive lesson in docs/design.md §6. A fallback fires on a transport or
// deployment failure — connection refused, DNS, timeout, a 5xx, a child
// process that could not be spawned. It does NOT fire when an extractor
// rejected the document itself: a corrupt PDF, an unsupported type, a clean
// parser error. Those are verdicts, and a verdict is final.
//
// Retrying a rejected document down the chain means feeding bytes that
// already broke one parser into progressively less-contained code paths —
// which is precisely how the OOM incident in §6 happened. A bad document must
// never buy its way back in.
type Chain struct {
	extractors []Extractor
}

var _ Extractor = (*Chain)(nil)

// NewChain builds a Chain from extractors in order of preference. Nil
// extractors are skipped, so a caller can write
//
//	NewChain(xbergOrNil, isolated, local)
//
// without branching on whether the sidecar is configured.
func NewChain(extractors ...Extractor) *Chain {
	kept := make([]Extractor, 0, len(extractors))
	for _, e := range extractors {
		if e != nil && !isNilInterface(e) {
			kept = append(kept, e)
		}
	}
	return &Chain{extractors: kept}
}

// Len reports how many extractors the chain will actually try. Useful for a
// startup log line: a chain of one is a very different deployment from a
// chain of three, and the difference is otherwise invisible until something
// fails.
func (c *Chain) Len() int { return len(c.extractors) }

// Extract runs the chain.
func (c *Chain) Extract(ctx context.Context, data []byte, filename string) (*Result, error) {
	if len(c.extractors) == 0 {
		return nil, fmt.Errorf("extract: chain has no extractors configured")
	}

	var unavailable []error
	for _, e := range c.extractors {
		result, err := e.Extract(ctx, data, filename)
		if err == nil {
			return result, nil
		}

		// A cancelled or timed-out *caller* context is not this extractor
		// being unavailable; it means the whole operation is over. Trying the
		// next extractor would only produce the same error more slowly.
		if ctx.Err() != nil {
			return nil, err
		}

		if errors.Is(err, ErrUnavailable) {
			unavailable = append(unavailable, err)
			continue
		}

		// A verdict on the document. Stop here.
		return nil, err
	}

	return nil, fmt.Errorf("%w: every extractor in the chain was unavailable: %s",
		ErrUnavailable, joinErrors(unavailable))
}

func joinErrors(errs []error) string {
	parts := make([]string, len(errs))
	for i, err := range errs {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}
