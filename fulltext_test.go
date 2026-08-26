package ragit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
)

// handbookFixture is written so that a natural-language question about it
// shares only its content words with the text — every other word in the
// question ("how", "do", "i", "my") appears nowhere.
const handbookFixture = `# Accounts

## Resetting a password

A customer who cannot sign in resets their own password from the sign-in page.

## Refunds

Orders can be refunded in full within thirty days of delivery.
`

func newTextHarness(t *testing.T) (*harness, uuid.UUID) {
	t.Helper()
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte(handbookFixture),
	})
	return h, tenantID
}

// TestFullTextSearch_AnswersAPlainQuestion is the README's own retrieval
// example, which used to return nothing at all.
//
// The 'simple' configuration has no stopword dictionary and
// websearch_to_tsquery ANDs every term, so this asked for a chunk containing
// "how" AND "do" AND "i" AND "my" — a query no corpus answers — and got back
// an empty slice, which reads exactly like an empty corpus.
func TestFullTextSearch_AnswersAPlainQuestion(t *testing.T) {
	h, tenantID := newTextHarness(t)

	results, err := h.processor.FullTextSearch(context.Background(),
		ragit.Tenant(tenantID), "how do I reset my password?", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.NotEmpty(t, results, "a question a user would type must not return an empty corpus")
	require.Contains(t, results[0].Content, "password",
		"the chunk matching the most query terms must rank first")
}

// TestFullTextSearch_RequireAllTermsStaysStrict keeps the old behaviour
// reachable for a caller that composed the query itself, where nothing found
// is the meaningful answer rather than a dead end.
func TestFullTextSearch_RequireAllTermsStaysStrict(t *testing.T) {
	h, tenantID := newTextHarness(t)

	results, err := h.processor.FullTextSearch(context.Background(),
		ragit.Tenant(tenantID), "how do I reset my password?",
		ragit.SearchOptions{TopK: 5, RequireAllTerms: true})
	require.NoError(t, err)
	require.Empty(t, results)
}

// TestFullTextSearch_DoesNotRelaxWhenEveryTermMatches is the precision half:
// widening happens only where there was nothing to lose.
func TestFullTextSearch_DoesNotRelaxWhenEveryTermMatches(t *testing.T) {
	h, tenantID := newTextHarness(t)
	ctx := context.Background()

	// Both terms appear, but in different chunks, so the strict query matches
	// neither and the relaxed one matches both.
	split, err := h.processor.FullTextSearch(ctx, ragit.Tenant(tenantID),
		"password refunded", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, split, 2, "with no chunk carrying both terms, either is worth returning")

	// Both terms in one chunk: the strict query answers, and the chunk that
	// carries only one of them is not dragged in behind it.
	together, err := h.processor.FullTextSearch(ctx, ragit.Tenant(tenantID),
		"password sign-in", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, together, 1, "a query that matched strictly must not be widened")
	require.Contains(t, together[0].Content, "password")
}

// TestFullTextSearch_LeavesSearchSyntaxAlone is the guard on the rewrite. A
// negation is the sharp case: "reset -refunded" relaxed term-by-term would
// become "reset OR NOT refunded", which matches nearly everything — the
// opposite of what the caller asked for.
func TestFullTextSearch_LeavesSearchSyntaxAlone(t *testing.T) {
	h, tenantID := newTextHarness(t)
	ctx := context.Background()

	negated, err := h.processor.FullTextSearch(ctx, ragit.Tenant(tenantID),
		"sign-in -password", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, negated, "the excluded term must stay excluded, not become an alternative")

	phrase, err := h.processor.FullTextSearch(ctx, ragit.Tenant(tenantID),
		`"thirty days of delivery"`, ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, phrase, 1)

	missingPhrase, err := h.processor.FullTextSearch(ctx, ragit.Tenant(tenantID),
		`"delivery of thirty days"`, ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, missingPhrase, "a phrase that is not in the corpus is not a bag of words")

	// An explicit or is the caller already asking for the relaxed form.
	explicit, err := h.processor.FullTextSearch(ctx, ragit.Tenant(tenantID),
		"password or refunded", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, explicit, 2)
}
