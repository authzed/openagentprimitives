// Package fake provides a fixture-driven Provider for tests.
//
// Select and Tests lookups are by Intent string.
// Generate and Refine are consumed in order (FIFO) across a single test run,
// so a test expecting "invalid, then valid on retry" populates Generate with
// two entries.
package fake

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/llm"
)

// Provider is a fixture-driven Provider. Zero value works — every field may be
// nil/empty and the corresponding method will return a clear error on call.
type Provider struct {
	Select   map[string]*llm.SelectResponse
	Generate []*llm.GenerateResponse
	Tests    map[string]*llm.TestResponse
	Refine   []*llm.GenerateResponse

	// Track consumption of ordered slices.
	generateIdx int
	refineIdx   int
}

func (p *Provider) SelectToolkit(_ context.Context, req llm.SelectRequest) (*llm.SelectResponse, error) {
	r, ok := p.Select[req.Intent]
	if !ok {
		return nil, fmt.Errorf("fake: no Select response for intent %q", req.Intent)
	}
	return r, nil
}

func (p *Provider) GenerateSpec(_ context.Context, _ llm.GenerateRequest) (*llm.GenerateResponse, error) {
	if p.generateIdx >= len(p.Generate) {
		return nil, fmt.Errorf("fake: GenerateSpec called %d times but only %d responses configured", p.generateIdx+1, len(p.Generate))
	}
	r := p.Generate[p.generateIdx]
	p.generateIdx++
	return r, nil
}

func (p *Provider) GenerateTestCases(_ context.Context, req llm.TestRequest) (*llm.TestResponse, error) {
	r, ok := p.Tests[req.Intent]
	if !ok {
		return nil, fmt.Errorf("fake: no TestCases response for intent %q", req.Intent)
	}
	return r, nil
}

func (p *Provider) RefineSpec(_ context.Context, _ llm.RefineRequest) (*llm.GenerateResponse, error) {
	if p.refineIdx >= len(p.Refine) {
		return nil, fmt.Errorf("fake: RefineSpec called %d times but only %d responses configured", p.refineIdx+1, len(p.Refine))
	}
	r := p.Refine[p.refineIdx]
	p.refineIdx++
	return r, nil
}

// GenerateCallCount returns the number of times GenerateSpec has been
// invoked. Used by tests that assert "this path didn't (or did N
// times) call generate".
func (p *Provider) GenerateCallCount() int { return p.generateIdx }

// RefineCallCount returns the number of times RefineSpec has been
// invoked. Used by tests that assert refine-loop behavior (early stop,
// max-iter cap, single-attempt convergence).
func (p *Provider) RefineCallCount() int { return p.refineIdx }
