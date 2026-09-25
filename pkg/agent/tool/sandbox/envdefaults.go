package sandbox

import "github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"

// toolkitEnvDefaults returns the env a dispatched ToolCall should carry in its
// spec.env, given whatever the caller has already put there.
//
// A toolkit's EnvDefaults is how a CLI's own hardening is declared next to the
// description of that CLI — the Claude Code toolkits cap CLAUDE_CODE_MAX_RETRIES
// this way, because the CLI retries an HTTP 401 ten times with backoff and a
// dead credential otherwise costs minutes per call. Stamping it here, rather
// than in the operator, means the ToolCall RECORDS what the toolkit
// contributed: it shows up in `kubectl get toolcall -o yaml`, and the toolspec
// validator sees the same env the tool will run with.
//
// Merge rules:
//   - Defaults are a FLOOR. A key already present in existing is left alone;
//     whoever set it meant it.
//   - Nothing to add means nothing changes — a nil existing stays nil rather
//     than becoming an empty map, so an unaffected toolkit's ToolCall keeps
//     omitting spec.env entirely.
//   - The returned map is always freshly allocated when it differs from
//     existing. The toolkit object is built once per bundle and shared by every
//     call in the session, so handing its own map to a call (or writing through
//     it) would leak one call's env into the next.
//
// This is only the ToolCall layer. Precedence against the operator-set layers
// below it — SpiceboxSession.spec.defaultEnv and SpiceboxClass.spec.envDefaults
// — is resolved where the exec environment is actually composed, in
// pkg/controllers/toolcall.execEnv.
func toolkitEnvDefaults(tk *toolkit.Toolkit, existing map[string]string) map[string]string {
	if tk == nil || len(tk.EnvDefaults) == 0 {
		return existing
	}
	out := make(map[string]string, len(existing)+len(tk.EnvDefaults))
	for k, v := range tk.EnvDefaults {
		out[k] = v
	}
	for k, v := range existing {
		out[k] = v
	}
	return out
}
