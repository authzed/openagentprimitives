// How a channel kind's surface renders structured UI components — the optional
// seam that lets an agent author native layout (Slack Block Kit today) through
// respond_to_user, parallel to TextFormatter for the plain-text dialect.
package channelkinds

import "encoding/json"

// ComponentFormatter is the OPTIONAL interface a Kind implements to declare that
// its surface can render structured UI components and to supply the model-facing
// authoring instructions for them.
//
// The returned string is MODEL-FACING PROMPT TEXT, appended to the `blocks`
// property description of respond_to_user's input schema exactly as
// TextFormatter's instructions are appended to `text`.
//
// Unlike TextFormatter — which every registered kind must implement, because
// every surface renders text — this is genuinely optional per kind: components
// are an opt-in capability, advertised by the "components" token in
// Capabilities(). There is therefore no default and no registry-wide sweep.
type ComponentFormatter interface {
	ComponentFormattingInstructions() string
}

// ComponentValidator is the OPTIONAL interface a Kind implements to validate
// agent-authored components before they are published, and to project them to
// plain text so egress gates can measure their content.
//
// respond_to_user calls ValidateComponents SYNCHRONOUSLY in the runner, so a
// rejection returns to the model as an error it can correct and retry, rather
// than failing later at render time. The returned error is model-facing: name
// the offending block and say how to fix it. A byte-identical input must always
// validate the same way — no wall-clock, no randomness.
//
// ComponentsPlainText exists so the info-leakage egress gate measures the TEXT
// carried in components, not only the reply's `text` field: without it, an
// agent could route blockable content through `blocks` and bypass the gate — a
// fail-open path. It is best-effort: on input that will not parse it returns ""
// (ValidateComponents is the authority on rejection; this is only for
// measurement).
type ComponentValidator interface {
	ValidateComponents(raw json.RawMessage) error
	ComponentsPlainText(raw json.RawMessage) string
}

// ComponentFormattingInstructionsFor returns k's component authoring
// instructions, or "" when k is nil (an unregistered kind name) or does not
// implement ComponentFormatter.
//
// There is deliberately no non-empty default: a kind that advertises the
// "components" capability is expected to implement ComponentFormatter, and an
// empty string from a misconfigured one is honest, where a fabricated default
// would describe a dialect the surface does not speak.
func ComponentFormattingInstructionsFor(k Kind) string {
	cf, ok := k.(ComponentFormatter)
	if !ok || cf == nil {
		return ""
	}
	return cf.ComponentFormattingInstructions()
}

// ComponentValidatorFor returns k's ComponentValidator, or nil when k is nil or
// does not implement it. The caller treats a nil validator on a
// components-advertising channel as fail-closed (reject), not as "skip
// validation".
func ComponentValidatorFor(k Kind) ComponentValidator {
	cv, ok := k.(ComponentValidator)
	if !ok || cv == nil {
		return nil
	}
	return cv
}
