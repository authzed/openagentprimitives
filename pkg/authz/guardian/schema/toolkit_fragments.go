package schema

import (
	"encoding/json"
	"log/slog"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// ToolkitFragments converts the SpiceDB schema fragments declared by toolkits
// into the fragment type the compose pass takes, so a toolkit ships the resource
// definitions its own permission checks name.
//
// Toolkits that declare nothing — most of them — contribute nothing.
//
// The conversion is a JSON round-trip rather than a field copy, and
// deliberately: pkg/apis/v1alpha1 imports pkg/toolspec/toolkit (SpiceboxToolkit
// mirrors Toolkit), so the fragment types cannot simply be shared, and the two
// declarations are kept byte-compatible on the wire for exactly this reason.
// SpiceboxToolkitSpec.ToToolkit already converts the other direction the same
// way. A field copy would compile fine and silently drop whichever field is
// added to one side and not the other — and both candidates for that are
// silent-and-widening: a dropped Wildcard narrows a permission nobody narrowed,
// a dropped RawZed loses the subject-relation forms the structured shape cannot
// express.
func ToolkitFragments(tks []toolkit.Toolkit) []*spiceboxv1alpha1.SpiceDBSchemaFragment {
	var out []*spiceboxv1alpha1.SpiceDBSchemaFragment
	for _, tk := range tks {
		frag := tk.SpiceDBSchema
		if frag == nil || (len(frag.Resources) == 0 && frag.RawZed == "") {
			continue
		}
		raw, err := json.Marshal(frag)
		if err != nil {
			// Unreachable for these types (no channels, no funcs), but a
			// silently-skipped fragment is a resource type that later goes
			// missing at Check time with no trace of why.
			slog.Default().Info("guardian.schema: toolkit fragment did not marshal; skipping",
				"toolkit", tk.Name, "err", err.Error())
			continue
		}
		var conv spiceboxv1alpha1.SpiceDBSchemaFragment
		if err := json.Unmarshal(raw, &conv); err != nil {
			slog.Default().Info("guardian.schema: toolkit fragment did not convert; skipping",
				"toolkit", tk.Name, "err", err.Error())
			continue
		}
		out = append(out, &conv)
	}
	return out
}
