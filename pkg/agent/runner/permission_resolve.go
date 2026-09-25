package runner

import (
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// ResolvePermissionForArgs returns the permission that actually governs one
// call: the first PermissionVariant whose When matches args, else the tool's
// static fallback. It is the ONE answer to "which permission applies to this
// call with these arguments" — extracted because there were already two
// copies (the toolCallAuthzDeps closure and dispatchToolUses) and the
// agent-UI gate needed a third, which is the point at which a duplicated
// rule becomes a rule that drifts.
//
// A CEL compile or evaluation failure is returned, never swallowed: every
// caller denies on it. A variant expression that cannot be evaluated is an
// unknown authorization posture, and the only safe reading of unknown is no.
func ResolvePermissionForArgs(t tool.Tool, args map[string]any) (authz.Permission, error) {
	if vs := t.PermissionVariants(); len(vs) > 0 {
		got, matched, err := authz.ResolveVariant(vs, args)
		if err != nil {
			return authz.Permission{}, err
		}
		if matched {
			return got, nil
		}
	}
	return t.Permission(), nil
}
