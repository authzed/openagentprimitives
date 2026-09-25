package runner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// secretOutputStatusWriter is the subset of StatusPatcher that applySecretOutput
// needs. Defined here so tests can provide a minimal stub without importing the
// full StatusPatcher.
type secretOutputStatusWriter interface {
	WriteSatisfiedSecretOutput(ctx context.Context, name, handle, secretName string) error
}

// applySecretOutput diverts a producer's secret value out-of-band. When the
// result carries a SecretOutput:
//
//  1. The value (Content) is written to the per-session store under a fresh
//     handle.
//  2. If publisher is non-nil, the value is POSTed to the operator so it lands
//     in the per-session Secret. On publish failure the function returns an
//     error Result without echoing the value — the secret did NOT land.
//  3. If publish succeeds and sw is non-nil, the handle→secretName mapping is
//     recorded on AgentSession.status.
//  4. Content is replaced with the handle line + the (value-free) Description.
//
// The LLM and session memory only ever see the handle. The value NEVER appears
// in Content on ANY path — including store failure and publish failure.
func applySecretOutput(
	ctx context.Context,
	store secretout.Store,
	publisher secretout.Publisher,
	sw secretOutputStatusWriter,
	sessName string,
	r tool.Result,
	toolUseID string,
) tool.Result {
	if r.SecretOutput == nil {
		return r
	}
	spec := r.SecretOutput
	value := r.Content
	r.SecretOutput = nil
	if store == nil {
		r.IsError = true
		r.Content = fmt.Sprintf("secret-output %q: no store configured", spec.Name)
		return r
	}
	handle, err := store.Put(ctx, spec.Name, []byte(value))
	if err != nil {
		r.IsError = true
		r.Content = fmt.Sprintf("secret-output %q: capture failed: %v", spec.Name, err)
		return r
	}
	// Publish to the operator so the value lands in the per-session Secret.
	// On failure, surface a clear error (no value leak) and return — the
	// in-memory Put already completed but the operator copy is absent.
	if publisher != nil {
		if perr := publisher.Publish(ctx, spec.Name, []byte(value), handle); perr != nil {
			slog.Default().Info("secret-output: publish to operator failed",
				"name", spec.Name, "handle", handle, "err", perr.Error())
			r.IsError = true
			r.Content = fmt.Sprintf("secret-output %q: publish failed; value not written to secret: %v", spec.Name, perr)
			return r
		}
		// Record handle→secretName on status so operators can correlate the
		// handle to the Secret key. Best-effort: a status-write failure must
		// not abort the capture (the Secret was already written).
		if sw != nil {
			secretName := sessName + "-secret-outputs"
			if werr := sw.WriteSatisfiedSecretOutput(ctx, spec.Name, handle, secretName); werr != nil {
				slog.Default().Info("secret-output: WriteSatisfiedSecretOutput failed",
					"handle", handle, "secretName", secretName, "err", werr.Error())
			}
		}
	}
	r.Content = fmt.Sprintf("%s\n<secret-output name=%q ref=%q bytes=%d>",
		spec.Description, spec.Name, handle, len(value))
	return r
}
