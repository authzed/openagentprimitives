// Package-private file. validateToolspec is called from Reconcile after the
// session is resolved and Validated=True is set. It runs the toolspec
// validator against every candidate spec for the requested tool. Returns the
// accepting spec name on allow, or per-spec failures on deny.
//
// Special case: if the session has toolspecs configured for this tool but ALL
// of them are currently in an Invalid state (e.g. they are still being
// reconciled by the toolspec controller), validateToolspec returns a non-nil
// retryableError so the reconciler can requeue rather than permanently fail
// the ToolCall.
package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/validator"
)

// errToolspecNotReady is returned when all candidate toolspecs are currently
// Invalid (not yet reconciled). The caller should requeue.
var errToolspecNotReady = errors.New("toolspec candidates not ready yet, retrying")

// configFromSession decodes the session's stamped class config (JSON per value)
// into the plain map[string]any the toolspec constraint CEL binds as the
// `config` root. Absent config yields a non-nil empty map so a
// `config.X`-referencing constraint fails closed (missing key → CEL error →
// deny) rather than seeing a null root. An undecodable value is a hard error:
// a session that cannot present its own config must not run with config-driven
// constraints silently disabled.
func configFromSession(sess *spiceboxv1alpha1.SpiceboxSession) (map[string]any, error) {
	out := make(map[string]any, len(sess.Spec.ToolConfig))
	for k, raw := range sess.Spec.ToolConfig {
		var v any
		if err := json.Unmarshal(raw.Raw, &v); err != nil {
			return nil, fmt.Errorf("decode config key %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

func (r *Reconciler) validateToolspec(
	ctx context.Context,
	tc *spiceboxv1alpha1.ToolCall,
	sess *spiceboxv1alpha1.SpiceboxSession,
	resolved *resolvedCall,
) (acceptedBy string, failures []spiceboxv1alpha1.ToolspecFailure, err error) {
	logger := log.FromContext(ctx)

	candidates, gather, err := r.gatherCandidates(ctx, tc, sess)
	if err != nil {
		return "", nil, err
	}
	if len(candidates) == 0 {
		// Three terminal-vs-transient cases:
		//   1. No toolspec for this tool at all → permanent fail.
		//   2. Toolspec(s) exist but are NOT YET reconciled (no Valid
		//      condition yet) → transient, requeue with retryable error.
		//   3. Toolspec(s) exist and are Valid=False (e.g. ToolkitMissing
		//      from a stale revision pin) → permanent fail. Requeueing
		//      forever wastes operator cycles and leaves the agent
		//      hanging with no signal.
		if len(gather.pending) > 0 && len(gather.invalid) == 0 {
			logger.Info("toolspec candidates not yet reconciled, requeueing",
				"tool", tc.Spec.Tool, "pending", gather.pending)
			return "", nil, errToolspecNotReady
		}
		if len(gather.invalid) > 0 {
			parts := make([]string, 0, len(gather.invalid))
			for _, iv := range gather.invalid {
				parts = append(parts, fmt.Sprintf("%s: %s — %s", iv.name, iv.reason, iv.message))
			}
			msg := fmt.Sprintf("no spec authorizes tool %q: every candidate toolspec is Valid=False (%s)",
				tc.Spec.Tool, joinTwoLine(parts))
			return "", []spiceboxv1alpha1.ToolspecFailure{{SpecName: "", Reason: msg}}, nil
		}
		msg := fmt.Sprintf("no spec authorizes tool %q in this session", tc.Spec.Tool)
		return "", []spiceboxv1alpha1.ToolspecFailure{{
			SpecName: "", Reason: msg,
		}}, nil
	}

	cmd := path.Base(candidates[0].toolkit.Target.Binary)
	argv := append([]string(nil), resolved.tool.DefaultArgs...)
	argv = append(argv, tc.Spec.Args...)
	// The session's stamped class config, exposed to constraint CEL as `config`.
	// A session carrying undecodable config cannot be admitted: fail closed and
	// surface, rather than proceeding with empty config (which would silently
	// disable every config-driven constraint).
	cfg, err := configFromSession(sess)
	if err != nil {
		return "", nil, fmt.Errorf("toolcall %s/%s: %w", tc.Namespace, tc.Name, err)
	}
	inv := validator.Invocation{
		Command: cmd,
		Argv:    argv,
		Env:     tc.Spec.Env,
		Cwd:     "/work",
		Config:  cfg,
	}

	for _, c := range candidates {
		dec, derr := validator.Check(c.toolkit, c.spec, inv)
		if derr != nil {
			logger.Error(derr, "validator.Check internal error", "spec", c.specName)
			continue
		}
		if dec.Allow {
			logger.Info("toolspec allowed", "spec", c.specName, "tool", tc.Spec.Tool)
			// Hand the accepting toolkit to the exec path: execEnv needs its
			// EnvDefaults to know which spec.env entries are toolkit defaults
			// (and so demotable) rather than deliberate per-call values.
			resolved.toolkit = c.toolkit
			return c.specName, nil, nil
		}
		f := spiceboxv1alpha1.ToolspecFailure{
			SpecName: c.specName,
			Reason:   dec.Reason,
		}
		if dec.FailedOn != nil {
			f.FailedOn = &spiceboxv1alpha1.ToolspecFailedOnRef{
				Path: dec.FailedOn.Path, Message: dec.FailedOn.Message,
			}
		}
		failures = append(failures, f)
		logger.Info("toolspec denied",
			"spec", c.specName, "tool", tc.Spec.Tool,
			"reason", dec.Reason, "trace", dec.Trace)
	}
	return "", failures, nil
}

type candidate struct {
	specName string
	spec     *spec.Spec
	toolkit  *toolkit.Toolkit
}

// invalidToolspec captures one Valid=False candidate so the caller can
// surface a useful failure reason rather than silently retrying forever.
type invalidToolspec struct {
	name    string
	reason  string
	message string
}

// gatherResult separates skipped candidates into transient ("pending":
// no Valid condition yet — should be requeued) and terminal ("invalid":
// Valid=False or unrecoverable load error — fail-fast).
type gatherResult struct {
	pending []string          // toolspecs targeting tc.Spec.Tool with no Valid condition yet
	invalid []invalidToolspec // toolspecs targeting tc.Spec.Tool with Valid=False
}

// gatherCandidates fetches the effective toolspec set, filters by tool name,
// classifies each non-allowing candidate as pending vs invalid, and
// resolves toolkits via the registry for accepting candidates.
func (r *Reconciler) gatherCandidates(
	ctx context.Context,
	tc *spiceboxv1alpha1.ToolCall,
	sess *spiceboxv1alpha1.SpiceboxSession,
) ([]candidate, gatherResult, error) {
	// Use the non-cached reader for toolspec status lookups to avoid lag
	// between the spiceboxtoolspec controller writing Valid=True and this
	// reconciler seeing it via the shared cache.
	logger := log.FromContext(ctx)
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var cands []candidate
	var res gatherResult
	for _, name := range sess.Status.EffectiveToolspecs {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		if gerr := reader.Get(ctx, client.ObjectKey{Name: name}, &ts); gerr != nil {
			if apierrors.IsNotFound(gerr) {
				// The candidate genuinely doesn't exist (deleted, or a stale
				// name in EffectiveToolspecs). Skip it — the remaining
				// candidates (and the empty-set classification in the caller)
				// decide the outcome.
				logger.Info("toolspec candidate not found, skipping",
					"toolspec", name, "tool", tc.Spec.Tool)
				continue
			}
			// Any other error is transient (API/cache hiccup, RBAC blip). We
			// must NOT drop the candidate silently: doing so could leave the
			// candidate set empty and fail the ToolCall permanently with a
			// misleading "no spec authorizes" deny that has no recoverable
			// signal. Return the error so the reconciler requeues with
			// backoff and re-evaluates once the read succeeds.
			logger.Error(gerr, "transient error reading toolspec candidate; requeueing",
				"toolspec", name, "tool", tc.Spec.Tool)
			return nil, gatherResult{}, fmt.Errorf("reading toolspec %q: %w", name, gerr)
		}
		if ts.Spec.Toolkit.Name != tc.Spec.Tool {
			continue
		}
		vc := meta.FindStatusCondition(ts.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
		if vc == nil {
			res.pending = append(res.pending, name)
			continue
		}
		if vc.Status != "True" {
			res.invalid = append(res.invalid, invalidToolspec{name: name, reason: vc.Reason, message: vc.Message})
			continue
		}
		sp, serr := ts.Spec.ToSpec()
		if serr != nil {
			res.invalid = append(res.invalid, invalidToolspec{name: name, reason: "ToSpecFailed", message: serr.Error()})
			continue
		}
		tk, terr := r.ToolkitRegistry.Resolve(ctx, ts.Spec.Toolkit.Name, ts.Spec.Toolkit.Revision)
		if terr != nil {
			res.invalid = append(res.invalid, invalidToolspec{name: name, reason: "ToolkitResolveFailed", message: terr.Error()})
			continue
		}
		cands = append(cands, candidate{specName: name, spec: sp, toolkit: tk})
	}
	return cands, res, nil
}

// joinTwoLine joins parts with "; " — keeps the failure message readable
// in kubectl describe / agent show without needing real linebreaks.
func joinTwoLine(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "; " + p
	}
	return out
}
