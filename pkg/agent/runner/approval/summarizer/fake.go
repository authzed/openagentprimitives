package summarizer

import "context"

// NewFake returns a Provider that returns `canned` from BOTH Summarize and
// SummarizeAnnotations — a test double for callers that don't want a live LLM.
// Not a _test.go file: callers in other packages (e.g. the runner's own
// tests) construct this fake too, so it must be part of the importable
// package surface.
func NewFake(canned string) Provider { return fakeProvider{canned} }

type fakeProvider struct{ canned string }

func (f fakeProvider) Name() string { return "fake" }

func (f fakeProvider) Summarize(context.Context, Request) (string, error) {
	return f.canned, nil
}

func (f fakeProvider) SummarizeAnnotations(context.Context, AnnotationRequest) (string, error) {
	return f.canned, nil
}
