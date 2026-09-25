// Package fake provides a deterministic Explainer for tests + CI.
//
// Tests script Response (or ResponseErr) before driving the Explain
// call, and assert on Calls afterwards to verify the prompt-injection
// boundary (e.g. that no agent output crept in).
package fake

import (
	"context"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/explainer"
)

// Explainer is a scriptable test double. Safe for concurrent use.
type Explainer struct {
	mu sync.Mutex

	// Response is the canned Output returned from Explain when
	// ResponseErr is nil. Tests set this before invoking Explain.
	Response explainer.Output

	// ResponseErr, when non-nil, is returned instead of Response.
	ResponseErr error

	// calls records every Input passed to Explain.
	calls []explainer.Input
}

// Explain returns the scripted response and records the Input for
// later assertion.
func (e *Explainer) Explain(_ context.Context, in explainer.Input) (explainer.Output, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, in)
	if e.ResponseErr != nil {
		return explainer.Output{}, e.ResponseErr
	}
	return e.Response, nil
}

// Calls returns a defensive copy of every Input seen by Explain. Safe
// to call concurrently with Explain.
func (e *Explainer) Calls() []explainer.Input {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]explainer.Input, len(e.calls))
	copy(out, e.calls)
	return out
}

// Compile-time interface check.
var _ explainer.Explainer = (*Explainer)(nil)
