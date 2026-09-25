package setup_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"

	// Wire all three authkinds for ResolveTarget + ParseBindingMatch.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	// Registers the static/oauth/federated credkind Kinds: isAlreadySetUp and
	// maskCredentialValue in engine.go call credresolve.ResolveSecretValue,
	// which now dispatches through registry.Get(cred.Type) instead of
	// switching on cred.Type.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// presentPlainly stands in for the presentation `cmd/oap` owns in production:
// it drives a flow's screens over the run's own streams, commits what they
// collected, and writes the summary.
//
// Deliberately minimal — no theme detection, no fail-closed driver, no error
// reframing. Those are the command's decisions, tested where they are made;
// what the engine needs from a presenter is only that Commit is called between
// the screens and the summary, which is what these tests exercise.
func presentPlainly(in io.Reader, out io.Writer) setup.FlowPresenter {
	theme := tui.NewTheme(tui.Caps{})
	return func(ctx context.Context, run setup.FlowRun) error {
		st, err := tui.Run(ctx, run.Screens, tui.Options{
			Theme: theme,
			Title: "oap · identity setup · " + run.ProviderID,
			In:    in,
			Out:   out,
		})
		if err != nil {
			return err
		}
		if err := run.Commit(ctx, st); err != nil {
			return err
		}
		return tui.RenderSummary(out, theme, st.Notes())
	}
}

// newRunRequest builds the RunRequest a test drives.
//
// It takes the reader rather than a string so that a caller wanting a different
// one — a reader that fails the test on any read, say — swaps it HERE, where
// both the request and the presenter closing over it are wired. Overwriting
// req.Stdin afterwards would leave the presenter prompting on the old stream
// while the engine read the new one: the split-brain run RunRequest.Present
// warns about, which reports no error at all and which today would be hidden by
// fake flows whose screens ask nothing.
func newRunRequest(identityName string, targets []string, in io.Reader, out io.Writer) setup.RunRequest {
	return setup.RunRequest{
		Namespace:    "default",
		IdentityName: identityName,
		Targets:      targets,
		Stdin:        in,
		Stdout:       out,
		Stderr:       io.Discard,
		Present:      presentPlainly(in, out),
	}
}

type recordingFlow struct {
	called       int
	wantErr      error
	storeVal     builtins.StoreValue
	verifyResult *builtins.VerifyResult // nil → Valid with empty detail
}

func (recordingFlow) Name() string { return "github-pat" }

// Screens describes one step that asks nothing. The engine drives every flow
// through the sequencer, so a fake needs a screen for the run to reach Result
// — but this fake's subject is the engine's dispatch, dedup, and verify-gate
// behaviour, none of which depends on a question being asked.
func (recordingFlow) Screens(context.Context, builtins.Request) ([]tui.Screen, error) {
	return []tui.Screen{noopScreen{}}, nil
}

// Result is what `called` counts: it is the point a real flow persists a
// credential, so it is the point at which the engine can be said to have run
// this flow. The retry loop re-runs Screens AND Result on each attempt, which
// is why the decline tests can assert an exact attempt count here.
func (f *recordingFlow) Result(ctx context.Context, req builtins.Request, _ *tui.State) error {
	f.called++
	if f.wantErr != nil {
		return f.wantErr
	}
	v := f.storeVal
	if v.Bearer == "" && v.OAuth == nil && v.KubeconfigYAML == "" {
		v = builtins.StoreValue{Bearer: "ghp_xxxx"}
	}
	return req.Store(ctx, v)
}

// noopScreen occupies a place in a fake flow's sequence without asking
// anything: Prepare returns no group, so no driver ever prompts for it.
type noopScreen struct{}

func (noopScreen) ID() string    { return "noop" }
func (noopScreen) Label() string { return "Noop" }
func (noopScreen) Prepare(context.Context, *tui.State) (*huh.Group, error) {
	return nil, nil
}

// Apply records a summary line so that runs driven by this fake exercise the
// engine's summary rendering. The value is deliberately not credential-shaped:
// one of the tests below asserts the raw token never reaches stdout.
func (noopScreen) Apply(_ context.Context, st *tui.State) error {
	st.Note("Step", "done")
	return nil
}

func (f *recordingFlow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	if f.verifyResult != nil {
		return *f.verifyResult, nil
	}
	return builtins.VerifyResult{Status: builtins.VerifyValid}, nil
}

func TestEngineDispatchesToBuiltin(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{}
	builtins.Register(flow)

	// We need a Toolkit named "gh" for cli:gh to resolve. The embedded
	// toolkits include gh, so resolution works.
	c := newClient(t)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	assert.Equal(t, 1, flow.called)
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	assert.NotEmpty(t, ai.Spec.Credentials, "no credentials created; output:\n%s", out.String())
}

func TestEngineDedupsCredentialAcrossTargets(t *testing.T) {
	// A credential required by more than one target — as two toolspecs on the
	// same toolkit produce (both need that toolkit's github-token) — is
	// provisioned, and printed, exactly once. Modeled here as the same target
	// listed twice; the dedup keys on the resolved credential, not the target.
	builtins.Reset()
	flow := &recordingFlow{}
	builtins.Register(flow)
	c := newClient(t)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:gh", "cli:gh"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	assert.Equal(t, 1, flow.called, "flow called %d times; want 1 (credential deduped across targets)", flow.called)
}

func TestEngineIdempotencySkip(t *testing.T) {
	// First run scaffolds. Second run should skip.
	builtins.Reset()
	flow := &recordingFlow{}
	builtins.Register(flow)
	c := newClient(t)
	for i := 0; i < 2; i++ {
		out := &strings.Builder{}
		_ = setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(""), out))
	}
	assert.Equal(t, 1, flow.called, "flow called %d times across 2 runs; want 1 (idempotency)", flow.called)
}

// TestEngineSkipsWhenCredentialPrePopulated covers the case where a credential
// with the declared name is already present and resolvable on the AgentIdentity.
// The idempotency check must detect it by name and skip the flow.
func TestEngineSkipsWhenCredentialPrePopulated(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{}
	builtins.Register(flow)

	// Pre-populated credential named "github-token" — the credential name the
	// gh toolkit declares for its GITHUB_TOKEN env var (credential: github-token).
	preID := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-tools", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "github-token",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "github-token", Key: "token"},
				},
			}},
		},
	}
	preSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("ghp_already_provisioned")},
	}
	c := newClient(t, preID, preSec)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("pm-tools", []string{"cli:gh"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	assert.Equal(t, 0, flow.called,
		"flow called %d times; want 0 (credential already present and valid)\noutput:\n%s",
		flow.called, out.String())
	assert.Contains(t, out.String(), "already set up")
}

func TestEngineExpiredOAuthSuggestRefresh(t *testing.T) {
	// Build an MCPServer + pre-existing AgentIdentity with an oauth
	// credential whose Secret has a past expires_at. The engine should
	// print the "oap identity refresh" hint and NOT invoke any builtin.
	builtins.Reset()
	neverCalled := &recordingFlow{}
	builtins.Register(neverCalled)

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	mcpSrv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.linear.app/sse"},
			// Per the no-inference contract, credential names must be
			// declared explicitly — there is no metadata.name fallback.
			Auth: spiceboxv1alpha1.MCPServerAuth{Provider: "oauth-mcp", Credential: "linear"},
		},
	}
	// Credential is named "linear" — matches the MCPServer's declared
	// spec.auth.credential.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot-linear", Namespace: "default"},
		Data: map[string][]byte{
			"access_token": []byte("old-token"),
			"expires_at":   []byte(past),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "linear", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: "my-bot-linear"},
				},
			}},
		},
	}
	c := newClient(t, mcpSrv, sec, ai)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"mcp:linear"}, strings.NewReader(""), out))
	require.NoError(t, err)
	assert.Equal(t, 0, neverCalled.called, "builtin should not be called for expired credential")
	assert.Contains(t, out.String(), "oap identity refresh")
}

func TestEngineMaskedTokenInOutput(t *testing.T) {
	// Run engine against cli:gh with a stub builtin that stores a
	// sufficiently long bearer token. Assert stdout contains the masked form.
	builtins.Reset()
	flow := &recordingFlow{storeVal: builtins.StoreValue{Bearer: "ghp_xxxx_yyyy_zzzz"}}
	builtins.Register(flow)
	c := newClient(t)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	// "ghp_xxxx_yyyy_zzzz" is 18 chars. Vendor-prefix detection: underscores at
	// indices 3, 8, 13. Last delimiter at index ≤ 11 is index 8 → candidate
	// prefix "ghp_xxxx_" (9 chars, all lowercase). Remainder 18-9=9 ≥ 8 → valid.
	// Masked form: "ghp_xxxx_****zzzz".
	assert.Contains(t, out.String(), "ghp_xxxx_****zzzz", "stdout does not contain masked token")
	assert.NotContains(t, out.String(), "ghp_xxxx_yyyy_zzzz", "stdout leaked the raw token")
}

// TestEngineRefusesABuiltinFlowWithNoWayToPresentIt pins the fail-closed edge
// of the presenter seam.
//
// A flow describes questions and nothing else, so with nowhere to ask them the
// only alternative to refusing is calling Result against an unanswered State —
// which every flow's own emptiness guard would then have to catch, and which
// would store an empty credential the first time one of them did not. The
// refusal is what makes that unreachable rather than merely unlikely.
func TestEngineRefusesABuiltinFlowWithNoWayToPresentIt(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{}
	builtins.Register(flow)
	c := newClient(t)

	out := &strings.Builder{}
	req := newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(""), out)
	req.Present = nil

	err := setup.Run(context.Background(), c, req)
	require.Error(t, err, "a builtin flow with no presenter must not run")
	assert.Contains(t, err.Error(), "no way to ask")
	assert.Zero(t, flow.called, "Result must never be reached without answers")

	var ai spiceboxv1alpha1.AgentIdentity
	if gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); gerr == nil {
		assert.Empty(t, ai.Spec.Credentials, "nothing may be stored for a flow that was never presented")
	}
}

// TestEnginePassesTheFlowToThePresenterWhole covers what a presenter is handed:
// the credential and provider it needs to title and word itself, the screens to
// drive, and a Commit that is the flow's own Result.
//
// Asserted because those four are the entire contract between this package and
// the command that owns the terminal — a presenter cannot name the credential a
// refusal is about, nor store what it collected, if any of them go missing.
func TestEnginePassesTheFlowToThePresenterWhole(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{}
	builtins.Register(flow)
	c := newClient(t)

	out := &strings.Builder{}
	req := newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(""), out)
	inner := req.Present
	var got setup.FlowRun
	calls := 0
	req.Present = func(ctx context.Context, run setup.FlowRun) error {
		calls++
		got = run
		return inner(ctx, run)
	}

	require.NoError(t, setup.Run(context.Background(), c, req), out.String())
	assert.Equal(t, 1, calls, "one attempt, one presentation")
	assert.Equal(t, "github-token", got.CredentialName, "the credential the run is setting up, in the user's words")
	assert.Equal(t, "github-pat", got.ProviderID, "the provider whose flow this is")
	assert.NotEmpty(t, got.Screens, "a flow that returned a nil error owes at least one screen")
	assert.NotNil(t, got.Commit, "without Commit the presenter has no way to store what it collected")
}

// scriptedFakeProvider creates a fake LLM provider that emits a store_credential
// call followed by agent_work_complete. Used to drive the LLM-fallback path
// in engine integration tests without a real Anthropic API key.
func scriptedFakeProvider(bearerValue string) func(context.Context) (llm.Provider, error) {
	mustMarshal := func(v interface{}) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{
				Type: "tool_use",
				ToolUse: &llm.ToolUseBlock{
					ID:    "tu_store",
					Name:  "store_credential",
					Input: mustMarshal(map[string]interface{}{"shape": "bearer", "value": bearerValue}),
				},
			}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
		}},
		{Resp: llm.Response{
			Content: []llm.ContentBlock{{
				Type: "tool_use",
				ToolUse: &llm.ToolUseBlock{
					ID:    "tu_done",
					Name:  "agent_work_complete",
					Input: mustMarshal(map[string]string{"summary": "credential stored"}),
				},
			}},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 80, OutputTokens: 30},
		}},
	}
	prov := llmfake.New(script)
	return func(ctx context.Context) (llm.Provider, error) { return prov, nil }
}

// roundTripFunc adapts a plain function to http.RoundTripper, so tests can
// stub a client without a real listener.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// installVerifyHTTPStub points builtins.VerifyCredential's live-probe client
// at a canned 200 response for every request, so tests that exercise the
// LLM-fallback store path (which now runs guardStore's live-verification
// gate — see pkg/platform/identity/setup/llmagent/verifystore.go) don't make a real
// network call to the provider's declared verify: endpoint (e.g.
// https://api.github.com/user for github-pat) with a fixture token that was
// never a genuine credential.
//
// The body carries "id" alongside "login": github-pat declares
// subjectIDField: id, so this is what lets TestEngineLLMFallbackHappyPath prove
// the id guardStore's one live check resolves reaches the credential Secret.
func installVerifyHTTPStub(t *testing.T) {
	t.Helper()
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"login":"octocat","id":583231}`)),
			}, nil
		})}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// TestEngineLLMFallbackHappyPath verifies the provider-has-no-builtin path
// ends with the credential persisted via the LLM-fallback agent, and — this is
// the LLM-agent side of the two Store-closure threading lines in engine.go
// (builtinsRequestFor's Store closure reads llmagent.SubjectIDFromContext) —
// that the subject id guardStore's single live check resolved reaches the
// credential Secret's attestation annotations. Nothing else in this suite
// drives that hop: store_test.go calls setup.Store directly with a hand-built
// StoreRequest.SubjectID, which cannot catch a broken or dropped context carry.
func TestEngineLLMFallbackHappyPath(t *testing.T) {
	builtins.Reset()
	// No flow registered → engine falls through to LLM-fallback path. The
	// github-pat provider still declares a verify: probe (independent of the
	// Go builtin registry), so guardStore's live-verification gate would
	// otherwise fire a real HTTPS call to api.github.com with this fixture's
	// non-genuine token; stub the probe client instead.
	installVerifyHTTPStub(t)

	llmagent.LLMProviderFactory = scriptedFakeProvider("ghp_test_token_from_llm_fallback")
	t.Cleanup(func() { llmagent.LLMProviderFactory = llmagent.DefaultProviderFactory })

	c := newClient(t)
	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	// Assert AgentIdentity has the credential written by the LLM-fallback agent.
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.NotEmpty(t, ai.Spec.Credentials, "no credential created via LLM fallback; output:\n%s", out.String())
	require.NotNil(t, ai.Spec.Credentials[0].Static, "expected a static credential")

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].Static.SecretRef.Name}, &sec))
	assert.Equal(t, "github-pat", sec.Annotations[useridentity.AttestedProviderAnnotation])
	assert.Equal(t, "583231", sec.Annotations[useridentity.AttestedSubjectAnnotation])
}

// TestEngineLLMFallbackUserAbortContinues verifies that when the LLM agent calls
// agent_work_complete without store_credential (i.e. ErrUserAborted), the engine
// soft-skips that requirement and continues to the next one.
func TestEngineLLMFallbackUserAbortContinues(t *testing.T) {
	builtins.Reset()
	// No flow registered → engine uses LLM-fallback for both requirements.
	// The first LLM call goes straight to agent_work_complete without storing.
	mustMarshal := func(v interface{}) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	// We need two separate toolkits to create two distinct requirements.
	// Use "cli:gh" (which has a provider with no registered builtin after Reset)
	// and "cli:prompt-only-tool" (inline-prompt only).
	// For simplicity: script the first provider invocation to abort, second to succeed.
	callCount := 0
	llmagent.LLMProviderFactory = func(ctx context.Context) (llm.Provider, error) {
		callCount++
		if callCount == 1 {
			// First requirement: abort without storing.
			script := []llmfake.Step{
				{Resp: llm.Response{
					Content: []llm.ContentBlock{{
						Type: "tool_use",
						ToolUse: &llm.ToolUseBlock{
							ID:    "tu_abort",
							Name:  "agent_work_complete",
							Input: mustMarshal(map[string]string{"summary": "user declined"}),
						},
					}},
					StopReason: "tool_use",
					Usage:      llm.Usage{InputTokens: 50, OutputTokens: 20},
				}},
			}
			return llmfake.New(script), nil
		}
		// Second requirement: store then complete.
		script := []llmfake.Step{
			{Resp: llm.Response{
				Content: []llm.ContentBlock{{
					Type: "tool_use",
					ToolUse: &llm.ToolUseBlock{
						ID:    "tu_store",
						Name:  "store_credential",
						Input: mustMarshal(map[string]interface{}{"shape": "bearer", "value": "tok_second"}),
					},
				}},
				StopReason: "tool_use",
				Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
			}},
			{Resp: llm.Response{
				Content: []llm.ContentBlock{{
					Type: "tool_use",
					ToolUse: &llm.ToolUseBlock{
						ID:    "tu_done",
						Name:  "agent_work_complete",
						Input: mustMarshal(map[string]string{"summary": "stored"}),
					},
				}},
				StopReason: "tool_use",
				Usage:      llm.Usage{InputTokens: 80, OutputTokens: 30},
			}},
		}
		return llmfake.New(script), nil
	}
	t.Cleanup(func() { llmagent.LLMProviderFactory = llmagent.DefaultProviderFactory })

	// Build a toolkit with two sensitive env vars so we get two requirements.
	sbtk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "two-cred-tool"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "two-cred-tool",
			Version:         "1.0.0",
			ToolkitRevision: "1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "twotool"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env: spiceboxv1alpha1.ToolkitEnv{
				Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
					{Name: "FIRST_SECRET", Sensitive: true, Prompt: "Get your first secret.", Credential: "first-secret"},
					{Name: "SECOND_SECRET", Sensitive: true, Prompt: "Get your second secret.", Credential: "second-secret"},
				},
			},
		},
	}
	c := newClient(t, sbtk)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:two-cred-tool"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	assert.Equal(t, 2, callCount, "LLMProviderFactory call count")
	assert.Contains(t, out.String(), "skipped")
}

// TestEngineLLMFallbackBudgetSoftSkips verifies that when the LLM agent's
// budget is exhausted before it calls store_credential, the engine soft-skips
// the requirement and continues rather than returning a hard error.
func TestEngineLLMFallbackBudgetSoftSkips(t *testing.T) {
	builtins.Reset()

	mustMarshal := func(v interface{}) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	// Script a provider that emits a harmless tool call (open_browser) but
	// never calls store_credential or agent_work_complete. With MaxTurns=1
	// the budget check fires after the first turn.
	//
	// We need to override MaxTurns for this test. We do that by swapping the
	// factory to return a provider whose single script step is an open_browser
	// call, and we patch llmagent.MaxTurns temporarily.
	oldMaxTurns := llmagent.MaxTurns
	llmagent.MaxTurns = 1
	t.Cleanup(func() { llmagent.MaxTurns = oldMaxTurns })

	// Make open_browser report success. browser.Open already refuses to reach
	// the OS from a test binary; the recorder is what turns that refusal into
	// the successful open this scripted turn is written against.
	browsertest.Record(t)

	llmagent.LLMProviderFactory = func(ctx context.Context) (llm.Provider, error) {
		script := []llmfake.Step{
			// Turn 1: open_browser — does not terminate. After this turn,
			// MaxTurns=1 check fires before any further LLM call.
			{Resp: llm.Response{
				Content: []llm.ContentBlock{{
					Type: "tool_use",
					ToolUse: &llm.ToolUseBlock{
						ID:    "tu_browser",
						Name:  "open_browser",
						Input: mustMarshal(map[string]string{"url": "https://example.com"}),
					},
				}},
				StopReason: "tool_use",
				Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
			}},
		}
		return llmfake.New(script), nil
	}
	t.Cleanup(func() { llmagent.LLMProviderFactory = llmagent.DefaultProviderFactory })

	sbtk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "budget-test-tool"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "budget-test-tool",
			Version:         "1.0.0",
			ToolkitRevision: "1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "budgettool"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env: spiceboxv1alpha1.ToolkitEnv{
				Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
					{Name: "BUDGET_SECRET", Sensitive: true, Prompt: "Get your secret.", Credential: "budget-secret"},
				},
			},
		},
	}
	c := newClient(t, sbtk)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:budget-test-tool"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	assert.Contains(t, out.String(), "budget exhausted")
}

// TestEngineInlinePromptHappyPath verifies that a toolkit env entry with
// prompt: but no provider: routes through the LLM-fallback agent and ends
// with the credential persisted.
func TestEngineInlinePromptHappyPath(t *testing.T) {
	builtins.Reset()
	neverCalled := &recordingFlow{}
	builtins.Register(neverCalled)

	llmagent.LLMProviderFactory = scriptedFakeProvider("my_secret_token_from_inline_prompt")
	t.Cleanup(func() { llmagent.LLMProviderFactory = llmagent.DefaultProviderFactory })

	// Build a SpiceboxToolkit CR that declares one sensitive env with
	// prompt: only (no provider:). SpiceboxToolkit is cluster-scoped so
	// the fake client key has no namespace.
	sbtk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "prompt-only-tool"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "prompt-only-tool",
			Version:         "1.0.0",
			ToolkitRevision: "1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "mytool"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env: spiceboxv1alpha1.ToolkitEnv{
				Allowed: []spiceboxv1alpha1.ToolkitEnvVar{{
					Name:       "MY_SECRET",
					Sensitive:  true,
					Prompt:     "Ask the user to paste their MY_SECRET from settings.",
					Credential: "my-secret",
					// Provider intentionally empty → inline-prompt path.
				}},
			},
		},
	}
	c := newClient(t, sbtk)

	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:prompt-only-tool"}, strings.NewReader(""), out))
	require.NoError(t, err, "Run\n%s", out.String())
	assert.Equal(t, 0, neverCalled.called, "inline-prompt path must not invoke a builtin")
	// The credential should have been written by the LLM-fallback agent.
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	assert.NotEmpty(t, ai.Spec.Credentials, "no credential created via inline-prompt LLM fallback; output:\n%s", out.String())
}

// runEngineOnce drives one setup.Run over cli:gh with the given flow and
// stdin script, returning the accumulated stdout and the error.
func runEngineOnce(t *testing.T, c client.Client, stdin string) (*strings.Builder, error) {
	t.Helper()
	out := &strings.Builder{}
	err := setup.Run(context.Background(), c, newRunRequest("my-bot", []string{"cli:gh"}, strings.NewReader(stdin), out))
	return out, err
}

func TestEngineVerifyRejectedConfirmYesStores(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyRejected, Detail: "GitHub rejected the token: 401 Unauthorized",
	}}
	builtins.Register(flow)
	c := newClient(t)

	out, err := runEngineOnce(t, c, "y\n")
	require.NoError(t, err, out.String())
	assert.Equal(t, 1, flow.called)
	assert.Contains(t, out.String(), "401 Unauthorized")
	assert.Contains(t, out.String(), "Store it anyway?")
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	assert.NotEmpty(t, ai.Spec.Credentials, "confirmed token must be stored")
}

func TestEngineVerifyRejectedDeclineRepromptsThenFails(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyRejected, Detail: "GitHub rejected the token: 401 Unauthorized",
	}}
	builtins.Register(flow)
	c := newClient(t)

	// Decline all three attempts.
	out, err := runEngineOnce(t, c, "n\nn\nn\n")
	require.Error(t, err, out.String())
	assert.Equal(t, 3, flow.called, "flow must be re-run on decline, capped at 3 attempts")
	var ai spiceboxv1alpha1.AgentIdentity
	gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai)
	if gerr == nil {
		assert.Empty(t, ai.Spec.Credentials, "declined token must NOT be stored")
	}
}

func TestEngineVerifyRejectedDeclineThenConfirmStores(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyRejected, Detail: "GitHub rejected the token: 401 Unauthorized",
	}}
	builtins.Register(flow)
	c := newClient(t)

	// Decline attempt 1, confirm attempt 2.
	out, err := runEngineOnce(t, c, "n\ny\n")
	require.NoError(t, err, out.String())
	assert.Equal(t, 2, flow.called, "flow re-run once after decline, then confirmed")
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	assert.NotEmpty(t, ai.Spec.Credentials, "token confirmed on attempt 2 must be stored")
}

func TestEngineVerifyIndeterminateWarnsAndStores(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyIndeterminate, Detail: "could not reach api.github.com",
	}}
	builtins.Register(flow)
	c := newClient(t)

	out, err := runEngineOnce(t, c, "")
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "could not reach api.github.com")
	assert.NotContains(t, out.String(), "Store it anyway?", "indeterminate must not prompt")
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	assert.NotEmpty(t, ai.Spec.Credentials)
}

// Also covers the builtin-flow side of engine.go's two Store-closure
// threading lines: runRequirement's wrap closure passes res.SubjectID —
// already computed for the warn/confirm UX above — straight into
// storeCredential, with no second VerifyCredential call. Nothing else in this
// suite drives that hop with a registered flow's real VerifyResult.
func TestEngineVerifyValidShowsSubject(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyValid, Detail: "authenticated as octocat", Subject: "octocat", SubjectID: "583231",
	}}
	builtins.Register(flow)
	c := newClient(t)

	out, err := runEngineOnce(t, c, "")
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "authenticated as octocat")

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.NotEmpty(t, ai.Spec.Credentials)
	require.NotNil(t, ai.Spec.Credentials[0].Static)

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].Static.SecretRef.Name}, &sec))
	assert.Equal(t, "github-pat", sec.Annotations[useridentity.AttestedProviderAnnotation])
	assert.Equal(t, "583231", sec.Annotations[useridentity.AttestedSubjectAnnotation])
}

// TestEngineVerifyUnsupportedStoresQuietly covers the engine Store-wrapper's
// Unsupported branch: no verifier available → a dim note, NO warning, and the
// credential is still stored.
func TestEngineVerifyUnsupportedStoresQuietly(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyUnsupported, Detail: "no verifier available",
	}}
	builtins.Register(flow)
	c := newClient(t)

	out, err := runEngineOnce(t, c, "")
	require.NoError(t, err, out.String())
	assert.Contains(t, out.String(), "no live verification available")
	assert.NotContains(t, out.String(), "Store it anyway?", "unsupported must not prompt")
	assert.NotContains(t, out.String(), "could not verify the token", "unsupported must not warn like indeterminate")
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	assert.NotEmpty(t, ai.Spec.Credentials, "unsupported still stores")
}

// TestEngineVerifyForbiddenAsksBeforeStoring: a 403 means the provider took
// the credential and refused this one check. The wizard has a human at the
// keyboard, so it tells them — accurately, that the credential authenticated —
// and asks, rather than storing on a verdict that confirmed nothing.
//
// The two subtests are each other's control: without the Forbidden arm,
// "declined" stores anyway (and fails) while "confirmed" still passes.
func TestEngineVerifyForbiddenAsksBeforeStoring(t *testing.T) {
	forbidden := &builtins.VerifyResult{
		Status: builtins.VerifyForbidden,
		Detail: "GitHub accepted the credential but refused this check — Resource protected by organization SAML enforcement.",
	}

	t.Run("declined: nothing is stored, and the wording never calls it a rejection", func(t *testing.T) {
		builtins.Reset()
		builtins.Register(&recordingFlow{verifyResult: forbidden})
		c := newClient(t)

		out, err := runEngineOnce(t, c, "n\nn\nn\n")
		require.Error(t, err, out.String())
		assert.Contains(t, out.String(), "authenticated but was refused for this check")
		assert.Contains(t, out.String(), "Store it anyway?", "a human at the keyboard must be asked")
		assert.NotContains(t, out.String(), "could not verify the token",
			"a 403 is not a could-not-reach; the two are told apart on purpose")
		var ai spiceboxv1alpha1.AgentIdentity
		if gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); gerr == nil {
			assert.Empty(t, ai.Spec.Credentials, "a declined store must NOT be written")
		}
	})

	t.Run("confirmed: stores, proving the prompt is a real decision point", func(t *testing.T) {
		builtins.Reset()
		builtins.Register(&recordingFlow{verifyResult: forbidden})
		c := newClient(t)

		out, err := runEngineOnce(t, c, "y\n")
		require.NoError(t, err, out.String())
		var ai spiceboxv1alpha1.AgentIdentity
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
		assert.NotEmpty(t, ai.Spec.Credentials, "a confirmed store must still go through")
	})
}

// TestEngineVerifyUnrecognizedVerdictAsksBeforeStoring covers the Store
// wrapper's default arm. A verdict this build has no branch for must not be
// read as permission to store: with a human at the keyboard the engine warns
// and asks, exactly as it does for the verdicts it does understand.
//
// The two subtests are each other's control. Delete the default arm and
// "declined" fails (the credential is stored despite the refusal) while
// "confirmed" still passes — so the pair, not either alone, pins the arm.
func TestEngineVerifyUnrecognizedVerdictAsksBeforeStoring(t *testing.T) {
	// A value that is a valid VerifyStatus as far as the type system is
	// concerned and that no arm of the engine's switch names.
	const futureVerdict builtins.VerifyStatus = "verdict-this-build-does-not-know"

	t.Run("declined: nothing is stored", func(t *testing.T) {
		builtins.Reset()
		flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
			Status: futureVerdict, Detail: "a verdict from a later build",
		}}
		builtins.Register(flow)
		c := newClient(t)

		out, err := runEngineOnce(t, c, "n\nn\nn\n")
		require.Error(t, err, out.String())
		assert.Contains(t, out.String(), "Store it anyway?",
			"an unrecognised verdict must reach the human, not be stored on its own authority")
		var ai spiceboxv1alpha1.AgentIdentity
		if gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); gerr == nil {
			assert.Empty(t, ai.Spec.Credentials, "a declined unrecognised verdict must NOT be stored")
		}
	})

	t.Run("confirmed: stores, proving the prompt is a real decision point", func(t *testing.T) {
		builtins.Reset()
		flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
			Status: futureVerdict, Detail: "a verdict from a later build",
		}}
		builtins.Register(flow)
		c := newClient(t)

		out, err := runEngineOnce(t, c, "y\n")
		require.NoError(t, err, out.String())
		var ai spiceboxv1alpha1.AgentIdentity
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
		assert.NotEmpty(t, ai.Spec.Credentials, "a confirmed store must still go through")
	})
}

// TestEngineNonInteractiveDeclinesRatherThanAskingWhetherToStoreAnyway covers
// the one question this engine asks that the screen sequencer never sees.
//
// The store-anyway confirmation is asked from inside the Store callback, after
// the run that collected the credential has ended, so a fail-closed driver
// cannot refuse it for us. Reading stdin there in a run that was told not to
// prompt would block on a terminal nobody is watching, which is the outcome the
// flag exists to prevent — so an unblessed verdict is declined outright.
//
// Driven with a reader that fails the test on any read: a decline that happened
// only because stdin was empty would not be a decline at all. `cmd/oap` cannot
// reach this today (its flows all ask something, and nothing seeds an answer),
// which is exactly why it is pinned here — a flow that later derives its answer
// without asking must not turn this into a hang.
func TestEngineNonInteractiveDeclinesRatherThanAskingWhetherToStoreAnyway(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyRejected, Detail: "the provider rejected the token: 401 Unauthorized",
	}}
	builtins.Register(flow)
	c := newClient(t)

	out := &strings.Builder{}
	// Handed to the constructor, not assigned afterwards: the presenter closes
	// over the same reader, so a run asserted to ask NOTHING must refuse reads
	// on both sides of the seam.
	req := newRunRequest("my-bot", []string{"cli:gh"}, refusingReader{t}, out)
	req.NonInteractive = true

	err := setup.Run(context.Background(), c, req)
	require.Error(t, err, out.String())
	assert.Contains(t, err.Error(), "could not be put to anyone")
	assert.NotContains(t, out.String(), "Store it anyway?",
		"a run told not to prompt must not put a question on the terminal")

	var ai spiceboxv1alpha1.AgentIdentity
	if gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); gerr == nil {
		assert.Empty(t, ai.Spec.Credentials, "nothing may be stored on a verdict nobody could confirm")
	}
}

// refusingReader fails the test if anything reads from it, so a run asserted to
// ask nothing cannot pass merely because its input happened to be empty.
type refusingReader struct{ t *testing.T }

func (r refusingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("this run must ask nothing, but something read from stdin")
	return 0, io.EOF
}

// TestEngineVerifyRejectedNoInputFailsClosedAfterRetries covers the
// Store-wrapper's confirmation-unavailable branch: a Rejected token with no
// readable stdin can't be confirmed, so the engine re-prompts up to the cap
// and then fails closed without storing.
func TestEngineVerifyRejectedNoInputFailsClosedAfterRetries(t *testing.T) {
	builtins.Reset()
	flow := &recordingFlow{verifyResult: &builtins.VerifyResult{
		Status: builtins.VerifyRejected, Detail: "GitHub rejected the token: 401 Unauthorized",
	}}
	builtins.Register(flow)
	c := newClient(t)

	// Empty stdin → ConfirmYN reads EOF → confirmation unavailable.
	out, err := runEngineOnce(t, c, "")
	require.Error(t, err, out.String())
	assert.Contains(t, err.Error(), "confirmation unavailable")
	assert.Equal(t, 3, flow.called, "rejected + no input retries to the cap then fails closed")
	var ai spiceboxv1alpha1.AgentIdentity
	if gerr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); gerr == nil {
		assert.Empty(t, ai.Spec.Credentials, "must not store when confirmation is unavailable")
	}
}
