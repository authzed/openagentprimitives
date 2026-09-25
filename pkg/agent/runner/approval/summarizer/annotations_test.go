package summarizer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFallbackAnnotationSummary_CountsAnnotationLines pins the deterministic
// fallback's contract: it counts "Annotation " lines in the trusted text —
// the mirror's availability must never couple to the LLM's.
func TestFallbackAnnotationSummary_CountsAnnotationLines(t *testing.T) {
	got := FallbackAnnotationSummary(AnnotationRequest{TrustedText: "…preamble…\nAnnotation 1 [intent: change]:\n  Comment: reword\nAnnotation 2:\n  Comment: fix\n"})
	assert.Contains(t, got, "2 annotation")
}

// TestFallbackAnnotationSummary_ZeroLinesStillSafe verifies the fallback
// never returns an empty string or panics on text with no numbered lines.
func TestFallbackAnnotationSummary_ZeroLinesStillSafe(t *testing.T) {
	got := FallbackAnnotationSummary(AnnotationRequest{TrustedText: "no numbered lines here"})
	assert.Contains(t, got, "annotation") // never empty / never panics
}

// TestSummarizeAnnotations_FakeProviderReturnsSummary verifies the fake
// Provider test double returns its canned string from SummarizeAnnotations,
// same as it does for Summarize.
func TestSummarizeAnnotations_FakeProviderReturnsSummary(t *testing.T) {
	p := NewFake("the user requested 2 changes")
	out, err := p.SummarizeAnnotations(context.Background(), AnnotationRequest{
		TrustedText: "Annotation 1:\n  Comment: reword\n",
	})
	require.NoError(t, err)
	assert.Equal(t, "the user requested 2 changes", out)
}
