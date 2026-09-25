// Package fake is a scripted llm.Provider for tests. Each call to Send
// pops the next Step from the script.
package fake

import (
	"context"
	"errors"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

var ErrInjected = errors.New("fake: injected error")

type Step struct {
	// Resp is returned when Err is nil.
	Resp llm.Response
	// Err is returned by Send when non-nil; Resp is ignored.
	Err error
}

type Provider struct {
	mu           sync.Mutex
	script       []Step
	idx          int
	seenRequests []llm.Request
	pricing      map[string]llm.ModelPricing

	// Caps is returned verbatim by Capabilities for every model id. Nil
	// (the zero value) reports an empty set, not a panic.
	//
	// Read under mu, like nativeMIMEs below. It stays exported for the many
	// tests that set it inline at construction; the lock covers the case
	// those tests do not have in view — a test that sets it while another
	// goroutine is inside Capabilities. There is no reason for two sibling
	// fields on the same struct to differ in how they are guarded.
	Caps llm.CapabilitySet

	// nativeMIMEs is returned verbatim by NativeInputMIMEs for every model id.
	// Nil (the zero value) reports an empty set, not a panic.
	nativeMIMEs llm.MIMESet
}

func New(script []Step) *Provider {
	return &Provider{script: script}
}

// Name implements llm.Provider.
func (*Provider) Name() string { return "fake" }

// SupportedFromEnv implements llm.Provider. The fake is always
// "supported" — tests inject it directly and don't care about env.
func (*Provider) SupportedFromEnv() bool { return true }

func (p *Provider) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seenRequests = append(p.seenRequests, req)
	if p.idx >= len(p.script) {
		return llm.Response{}, errors.New("fake: script exhausted")
	}
	step := p.script[p.idx]
	p.idx++
	if step.Err != nil {
		return llm.Response{}, step.Err
	}
	return step.Resp, nil
}

// Requests returns a copy of the requests that have been observed.
// Safe to call concurrently with Send.
func (p *Provider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]llm.Request, len(p.seenRequests))
	copy(out, p.seenRequests)
	return out
}

// SetPricing configures the prices the fake reports from Pricing. Tests use it
// to drive cost-estimation paths with no network.
func (p *Provider) SetPricing(m map[string]llm.ModelPricing) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pricing = m
}

// Pricing implements llm.Provider.
func (p *Provider) Pricing(model string) (llm.ModelPricing, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.pricing[model]
	return pr, ok
}

// Capabilities implements llm.Provider. Returns f.Caps for every model id
// (tests that care about per-model variation should check the model
// themselves before configuring Caps); a nil Caps reports an empty set.
func (f *Provider) Capabilities(_ string) llm.CapabilitySet {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Caps == nil {
		return llm.NewCapabilitySet()
	}
	return f.Caps
}

// NativeInputMIMEs implements llm.Provider. Returns f.nativeMIMEs for every
// model id (tests that care about per-model variation should check the model
// themselves before configuring it via SetNativeInputMIMEs).
//
// Locked like every other mutable field here: a harness configures the fake
// from the test goroutine while the runner's hydration pass reads it from the
// loop's, so an unguarded field is a data race the -race unit suite reports as
// a failure in whatever test happens to be running.
func (f *Provider) NativeInputMIMEs(_ string) llm.MIMESet {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nativeMIMEs
}

// SetNativeInputMIMEs sets what NativeInputMIMEs reports, for tests that need
// to exercise the native path without a real provider.
func (f *Provider) SetNativeInputMIMEs(s llm.MIMESet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nativeMIMEs = s
}

// Compile-time interface check.
var _ llm.Provider = (*Provider)(nil)
