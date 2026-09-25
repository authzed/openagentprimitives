package llmagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

func TestGuardStore(t *testing.T) {
	// A provider with a format gate but no builtin and no verify probe:
	// format is enforced fail-closed; liveness is indeterminate (warn).
	prov := &provider.Provider{
		ID: "github-pat", Title: "GitHub", Shape: "bearer",
		TokenShape: &provider.TokenShape{Pattern: `^(gh[a-z]_|github_pat_)`, Description: "a GitHub token"},
	}

	t.Run("wrong format: fail-closed error mentions the expected format", func(t *testing.T) {
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), prov, out, nil, builtins.StoreValue{Bearer: "sk-ant-oops"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GitHub token")
		assert.Empty(t, subjectID, "a fail-closed gate must not report a subject id")
	})

	t.Run("valid format, no probe: proceeds with an unsupported note", func(t *testing.T) {
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), prov, out, nil, builtins.StoreValue{Bearer: "ghp_ok"})
		require.NoError(t, err)
		assert.Contains(t, out.String(), "no live verification available")
		assert.Empty(t, subjectID, "unsupported verification extracts no id")
	})

	t.Run("rejected by registered flow: fail-closed tool error carries detail", func(t *testing.T) {
		builtins.Reset()
		t.Cleanup(builtins.Reset)
		builtins.Register(&rejectingFlow{})
		p := &provider.Provider{ID: "rej", Builtin: "rej-flow"}
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), p, out, nil, builtins.StoreValue{Bearer: "tok"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Bad credentials")
		assert.Contains(t, err.Error(), "ask the user", "the tool error must instruct the agent to re-prompt the user")
		assert.Empty(t, subjectID, "a rejection must not report a subject id")
	})

	t.Run("nil provider: proceeds (permissive), warns unverified", func(t *testing.T) {
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), nil, out, nil, builtins.StoreValue{Bearer: "tok"})
		require.NoError(t, err)
		assert.Contains(t, out.String(), "no live verification available")
		assert.Empty(t, subjectID)
	})

	t.Run("indeterminate: warns 'could not verify' and proceeds", func(t *testing.T) {
		builtins.Reset()
		t.Cleanup(builtins.Reset)
		builtins.Register(resultFlow{res: builtins.VerifyResult{
			Status: builtins.VerifyIndeterminate, Detail: "could not reach api.github.com",
		}})
		p := &provider.Provider{ID: "ind", Builtin: "result-flow"}
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), p, out, nil, builtins.StoreValue{Bearer: "tok"})
		require.NoError(t, err)
		assert.Contains(t, out.String(), "could not verify")
		assert.Contains(t, out.String(), "could not reach api.github.com")
		assert.Empty(t, subjectID, "an indeterminate check must not report a subject id")
	})

	t.Run("forbidden: warns and stores — re-pasting cannot fix an SSO or scope refusal", func(t *testing.T) {
		// The one consumer that must NOT fail closed here. This surface has no
		// human confirm loop: it fails closed by handing the agent a tool error
		// that says "ask the user for a corrected token". For a 403 there is no
		// corrected token to ask for, so that arm would loop forever on a
		// credential that authenticates perfectly well.
		builtins.Reset()
		t.Cleanup(builtins.Reset)
		builtins.Register(resultFlow{res: builtins.VerifyResult{
			Status: builtins.VerifyForbidden,
			Detail: "GitHub accepted the credential but refused this check — Resource protected by organization SAML enforcement.",
		}})
		p := &provider.Provider{ID: "fbd", Builtin: "result-flow"}
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), p, out, nil, builtins.StoreValue{Bearer: "tok"})
		require.NoError(t, err, "a refused check must not become a tool error that re-prompts forever")
		assert.Contains(t, out.String(), "authenticated but was refused for this check")
		assert.Contains(t, out.String(), "SAML enforcement", "the provider's own reason must reach the operator")
		assert.Empty(t, subjectID, "a forbidden check confirmed nothing, so it must not report a subject id")
	})

	t.Run("unrecognised verdict: refuses, and tells the agent to report rather than re-paste", func(t *testing.T) {
		// A verdict this build has no branch for. Reaching guardStore's default
		// arm must not be silent: this surface has no human to ask, so it
		// refuses — but it must not send the agent into a paste-again loop for
		// something a new token cannot fix, so the tool error says "report".
		builtins.Reset()
		t.Cleanup(builtins.Reset)
		builtins.Register(resultFlow{res: builtins.VerifyResult{
			Status: "verdict-this-build-does-not-know", Detail: "a verdict from a later build",
		}})
		p := &provider.Provider{ID: "unk", Builtin: "result-flow"}
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), p, out, nil, builtins.StoreValue{Bearer: "tok"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "do NOT store this value",
			"the refusal must be the unrecognised-verdict one, not some other gate")
		assert.Contains(t, err.Error(), "report",
			"an unrecognised verdict is not something a re-paste fixes; the agent must be told to report it")
		assert.NotContains(t, err.Error(), "ask the user for a corrected token",
			"that instruction belongs to the rejected arm and would loop here")
		assert.Empty(t, subjectID, "an unrecognised verdict must not report a subject id")
	})

	// This case is the one that matters for attestation: guardStore's own
	// VerifyResult carries the subject id back to the caller, so wrappedStore
	// (agent.go) can hand it to the store call without a second live probe.
	t.Run("valid with detail: prints the verified line, proceeds, and reports the subject id", func(t *testing.T) {
		builtins.Reset()
		t.Cleanup(builtins.Reset)
		builtins.Register(resultFlow{res: builtins.VerifyResult{
			Status: builtins.VerifyValid, Detail: "authenticated as octocat", SubjectID: "583231",
		}})
		p := &provider.Provider{ID: "val", Builtin: "result-flow"}
		out := &strings.Builder{}
		subjectID, err := guardStore(context.Background(), p, out, nil, builtins.StoreValue{Bearer: "tok"})
		require.NoError(t, err)
		assert.Contains(t, out.String(), "verified")
		assert.Contains(t, out.String(), "authenticated as octocat")
		assert.Equal(t, "583231", subjectID)
	})
}

// TestContextWithSubjectID_RoundTrips verifies the context threading guardStore's
// result rides on: a stored id reads back unchanged, and an empty id is a no-op
// rather than an explicit empty claim (so ContextWithSubjectID(ctx, "") never
// masks a value a caller further down the chain already set).
func TestContextWithSubjectID_RoundTrips(t *testing.T) {
	ctx := ContextWithSubjectID(context.Background(), "583231")
	assert.Equal(t, "583231", SubjectIDFromContext(ctx))

	assert.Empty(t, SubjectIDFromContext(context.Background()), "no value set: reads back empty")

	noop := ContextWithSubjectID(ctx, "")
	assert.Equal(t, "583231", SubjectIDFromContext(noop), "an empty id must not clear a value already on the context")
}

// resultFlow is a registry fake whose Verify returns a fixed result, used
// to drive guardStore's Indeterminate / Valid branches.
type resultFlow struct{ res builtins.VerifyResult }

func (resultFlow) Name() string { return "result-flow" }
func (resultFlow) Screens(context.Context, builtins.Request) ([]tui.Screen, error) {
	return nil, errors.New("resultFlow asks nothing; it exists for its Verify")
}
func (resultFlow) Result(context.Context, builtins.Request, *tui.State) error {
	return errors.New("resultFlow stores nothing; it exists for its Verify")
}
func (f resultFlow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return f.res, nil
}

type rejectingFlow struct{}

func (rejectingFlow) Name() string { return "rej-flow" }
func (rejectingFlow) Screens(context.Context, builtins.Request) ([]tui.Screen, error) {
	return nil, errors.New("rejectingFlow asks nothing; it exists for its Verify")
}
func (rejectingFlow) Result(context.Context, builtins.Request, *tui.State) error {
	return errors.New("rejectingFlow stores nothing; it exists for its Verify")
}
func (rejectingFlow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{Status: builtins.VerifyRejected, Detail: "provider rejected the token: 401 — Bad credentials"}, nil
}
