package identityadvisor

import "context"

// Fake is a test double for Provider. Callers set Rec/Err to control
// what Recommend returns; no network calls are made.
type Fake struct {
	Rec Recommendation
	Err error
}

// Recommend implements Provider.
func (f Fake) Recommend(context.Context, Request) (Recommendation, error) {
	return f.Rec, f.Err
}

// Name implements Provider.
func (Fake) Name() string { return "fake" }
