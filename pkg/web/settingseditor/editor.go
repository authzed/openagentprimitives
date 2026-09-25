// Package settingseditor is the server-agnostic core of the settings editor
// UI: validation aggregation over the admission-webhook checks plus the pure
// settings resolver, YAML round-trip for the raw editor, and SSA drift
// reporting. It is consumed today by the desktop settings server
// (cmd/oap/internal/desktop/settingsui) and is deliberately free of any HTTP
// or desktop dependency so admind can host the same editor later.
package settingseditor

import v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

// Violation is the wire form of a resolver violation (the resolver's own
// Violation struct carries no json tags).
type Violation struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// Result is one validation pass over a SettingsSpec: hard admission errors
// (the webhook would reject), advisory resolver violations, and — when the
// spec passes — the effective settings the cluster tier alone would produce.
type Result struct {
	Errors     []string                    `json:"errors"`
	Violations []Violation                 `json:"violations"`
	Effective  *v1alpha1.EffectiveSettings `json:"effective,omitempty"`
}
