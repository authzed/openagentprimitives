package toolcall

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// execEnv composes the environment one tool exec receives.
//
// Precedence, high → low:
//
//  1. agentEnv — broker-resolved credentials.
//  2. ToolCall.spec.env — the caller's explicit per-call environment.
//  3. SpiceboxSession.spec.defaultEnv — the session's tool-hardening env.
//  4. the accepted toolkit's envDefaults — the CLI's own declared defaults.
//  5. the sandbox container's env, which includes
//     SpiceboxClass.spec.envDefaults. Nothing here exports it; it is simply
//     what the process already has, so any key this function returns shadows it.
//
// Levels 1-3 are the merge this controller has always done. Level 4 needs the
// extra work below because of WHERE a toolkit default is carried: the runner
// stamps it onto spec.env at synthesis, so the ToolCall records what the
// toolkit contributed — but that placement would put it at level 2, above every
// operator-set value. So a spec.env entry still carrying the toolkit's own
// declared value is demoted: an operator's session defaultEnv replaces it, and
// a class envDefaults key is dropped from the returned map entirely so the
// container's value is what the process sees.
//
// The value comparison is what separates "the runner stamped the toolkit's
// default" from "someone deliberately set this key on this call". A spec.env
// value that differs from the toolkit's default was chosen by the caller and
// keeps winning at level 2. A hand-written ToolCall that restates the toolkit's
// own default verbatim is indistinguishable from a stamped one — and yields the
// same value anyway, unless an operator has overridden it, which is the case
// this demotion exists to honor.
//
// A credential-injected key is never demoted: Reconcile already fails a
// ToolCall whose spec.env collides with one (ToolCallEnvShadowsAgent), so this
// is defense in depth rather than a reachable path.
func execEnv(
	agentEnv map[string]string,
	tc *spiceboxv1alpha1.ToolCall,
	sess *spiceboxv1alpha1.SpiceboxSession,
	tk *toolkit.Toolkit,
) map[string]string {
	var sessionEnv map[string]string
	var classEnv map[string]string
	if sess != nil {
		sessionEnv = sess.Spec.DefaultEnv
		if sess.Status.ResolvedClass != nil {
			classEnv = sess.Status.ResolvedClass.EnvDefaults
		}
	}

	// mergeEnv(a, b) lets a win, so nest: session defaultEnv (lowest) ←
	// tc.Spec.Env ← agentEnv. It returns nil when every layer is empty, which
	// the demotion loop below would assign into; allocate rather than rely on
	// the loop's writes being unreachable in exactly that case. The empty map
	// is normalized back to nil at the end.
	out := mergeEnv(agentEnv, mergeEnv(tc.Spec.Env, sessionEnv))
	if out == nil {
		out = map[string]string{}
	}

	if tk != nil {
		for k, declared := range tk.EnvDefaults {
			if tc.Spec.Env[k] != declared {
				continue // not the toolkit's default; the caller meant this value
			}
			if _, injected := agentEnv[k]; injected {
				continue
			}
			if v, ok := sessionEnv[k]; ok {
				out[k] = v
				continue
			}
			if _, ok := classEnv[k]; ok {
				delete(out, k)
			}
		}
	}

	// Match mergeEnv's contract: no variables means no map, so a caller's
	// len()==0 check and an equality check agree.
	if len(out) == 0 {
		return nil
	}
	return out
}
