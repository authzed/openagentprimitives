// Package markup is the isolated secondary-LLM that generates sample HTML
// markup for the css artifact kind's browser preview. Like the approval
// summarizer, it is a SECOND, ISOLATED LLM call: the input CSS is treated as
// USER DATA, the output is structural HTML body markup only (no scripts), and
// the result is re-sanitized through the html renderer before it is shown. A
// compromised primary agent cannot reach this call.
package markup

import "context"

// Provider generates sample HTML body markup for a preview instruction. The
// returned markup is sanitized by the caller; treat its instruction input as
// untrusted. One round-trip; no streaming, no tools.
type Provider interface {
	// Generate returns HTML body markup for the instruction. Matches
	// channelassets.MarkupGenerator so it can be used as that callback.
	Generate(ctx context.Context, instruction string) (string, error)
	// Name returns a short identifier for logs ("anthropic", "fake", etc.).
	Name() string
}

// Fake is a deterministic Provider for tests and for wiring when no API key is
// available. It ignores the instruction and returns Markup verbatim, or a
// built-in sample document when Markup is empty.
type Fake struct{ Markup string }

func (f Fake) Generate(_ context.Context, _ string) (string, error) {
	if f.Markup == "" {
		return `<div class="preview-sample"><h1>Sample</h1><p>Preview markup.</p></div>`, nil
	}
	return f.Markup, nil
}

func (Fake) Name() string { return "fake" }
