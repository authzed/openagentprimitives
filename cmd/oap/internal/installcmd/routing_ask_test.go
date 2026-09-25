package installcmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/initpipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// declaredFlags flattens what webdRoutingInputs declared into the order the
// screens would be presented in, which is the only property the caller (and
// the operator answering them) can observe.
func declaredFlags(t *testing.T, comps []initpipeline.Component) []string {
	t.Helper()
	var out []string
	for _, c := range comps {
		for _, i := range c.Inputs {
			out = append(out, i.Flag)
		}
	}
	return out
}

// ask runs the ASK phase over a scripted stream. The line-oriented driver is
// what a non-file reader resolves to, so no pseudo-terminal is needed — but
// interactive is passed explicitly, because it is the caller's measurement of
// its own stdin and not something this phase re-derives.
func ask(t *testing.T, p cloud.InstallProfile, r WebdRoutingOpts, script string, interactive bool) (WebdRoutingOpts, error) {
	t.Helper()
	var out bytes.Buffer
	return askWebdRoutingInputs(context.Background(), &out, strings.NewReader(script), true /*noColor*/, interactive, p, r)
}

// The declaration rule, stated as a table: an input is declared exactly when
// its absence is already a hard error, and only then.
func TestWebdRoutingInputs_DeclaresExactlyTheValuesTheInstallWouldRefuseWithout(t *testing.T) {
	cases := []struct {
		name    string
		profile cloud.InstallProfile
		opts    WebdRoutingOpts
		want    []string
	}{
		{
			name:    "managed cloud, nothing supplied: all three are asked",
			profile: cloud.ManagedProfile,
			want:    []string{flagTrustedHostname, flagSandboxHostname, flagACMEEmail},
		},
		{
			name:    "managed cloud, --manual-webd-routing: the operator wires routing, so nothing is asked",
			profile: cloud.ManagedProfile,
			opts:    WebdRoutingOpts{manualWebdRouting: true},
			want:    nil,
		},
		{
			name:    "local kind, nothing supplied: no external hostname is a legitimate outcome, so nothing is asked",
			profile: cloud.DevProfile,
			want:    nil,
		},
		{
			name:    "unmanaged default kind, nothing supplied: same, nothing is asked",
			profile: cloud.ProductionProfile,
			want:    nil,
		},
		{
			name:    "both hostnames supplied, no email: only the certificate email is asked",
			profile: cloud.ManagedProfile,
			opts:    WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
			want:    []string{flagACMEEmail},
		},
		{
			name:    "everything supplied: nothing is asked",
			profile: cloud.ManagedProfile,
			opts: WebdRoutingOpts{
				trustedHostname: "webd.example.com",
				sandboxHostname: "sandbox.example.com",
				acmeEmail:       "ops@example.com",
			},
			want: nil,
		},
		{
			name:    "trusted supplied without a sandbox: the value the gate would refuse over is asked",
			profile: cloud.ManagedProfile,
			opts:    WebdRoutingOpts{trustedHostname: "webd.example.com", acmeEmail: "ops@example.com"},
			want:    []string{flagSandboxHostname},
		},
		{
			name:    "--disable-artifact-viewer: the sandbox origin is not asked for",
			profile: cloud.ManagedProfile,
			opts:    WebdRoutingOpts{disableViewer: true},
			want:    []string{flagTrustedHostname, flagACMEEmail},
		},
		{
			name:    "--tls-issuer: the email is still declared, and its own condition calls it off",
			profile: cloud.ManagedProfile,
			opts:    WebdRoutingOpts{tlsIssuer: "existing-issuer"},
			want:    []string{flagTrustedHostname, flagSandboxHostname, flagACMEEmail},
		},
		{
			name:    "local kind with an explicit trusted hostname: the gate does refuse there, so the sandbox is asked",
			profile: cloud.DevProfile,
			opts:    WebdRoutingOpts{trustedHostname: "webd.example.com", acmeEmail: "ops@example.com"},
			want:    []string{flagSandboxHostname},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, declaredFlags(t, webdRoutingInputs(tc.profile, tc.opts)))
		})
	}
}

// The end-to-end route this task exists to add: a terminal-attached install on
// a managed cloud answers all three from the stream instead of being refused.
// Every asserted value differs from what a never-asked question produces (the
// empty default), so none of them can pass by accident — huh's accessible
// renderer reports an exhausted script as a completed form with a nil error.
func TestAskWebdRoutingInputs_AnswersFoldBackIntoTheOptions(t *testing.T) {
	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{},
		"webd.example.com\nsandbox.example.com\nops@example.com\n", true)

	require.NoError(t, err)
	assert.Equal(t, "webd.example.com", r.trustedHostname)
	assert.Equal(t, "sandbox.example.com", r.sandboxHostname)
	assert.Equal(t, "ops@example.com", r.acmeEmail)
	assert.NoError(t, validateWebdRouting(cloud.ManagedProfile, r.trustedHostname, r.sandboxHostname, r.disableViewer, r.manualWebdRouting),
		"the answers must leave the install in a state the gate accepts")
}

// A flag supplied on the command line is not asked for again. The script holds
// ONE line: if the trusted hostname were re-asked it would swallow that line,
// and the sandbox question — the one actually missing a value — would read
// end-of-input and resolve to its empty default with no error at all.
func TestAskWebdRoutingInputs_ASuppliedFlagIsNotReAsked(t *testing.T) {
	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{
		trustedHostname: "supplied.example.com",
		acmeEmail:       "ops@example.com",
	}, "sandbox.example.com\n", true)

	require.NoError(t, err)
	assert.Equal(t, "supplied.example.com", r.trustedHostname)
	assert.Equal(t, "sandbox.example.com", r.sandboxHostname, "the one unanswered input is what consumed the line")
	assert.Equal(t, "ops@example.com", r.acmeEmail)
}

// --hostname-suffix is the convenience form of both hostnames, so an operator
// who passed it is asked for neither. Expanded through withHostnameSuffix
// first, exactly as RunInstall does before it asks.
func TestAskWebdRoutingInputs_HostnameSuffixAnswersBothHostnames(t *testing.T) {
	expanded, err := WebdRoutingOpts{hostnameSuffix: "example.com"}.withHostnameSuffix()
	require.NoError(t, err)
	assert.Equal(t, []string{flagACMEEmail}, declaredFlags(t, webdRoutingInputs(cloud.ManagedProfile, expanded)),
		"the suffix answered both hostnames; only the certificate email is left")

	r, err := ask(t, cloud.ManagedProfile, expanded, "ops@example.com\n", true)

	require.NoError(t, err)
	assert.Equal(t, "webd.example.com", r.trustedHostname)
	assert.Equal(t, "sandbox.example.com", r.sandboxHostname)
	assert.Equal(t, "ops@example.com", r.acmeEmail, "the single scripted line went to the only question left")
}

// --tls-issuer names an existing issuer, which is what makes the ACME email
// unnecessary. The script offers a line; the assertion is that nothing
// consumed it.
func TestAskWebdRoutingInputs_TLSIssuerCallsOffTheEmailQuestion(t *testing.T) {
	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{
		trustedHostname: "webd.example.com",
		sandboxHostname: "sandbox.example.com",
		tlsIssuer:       "existing-issuer",
	}, "ops@example.com\n", true)

	require.NoError(t, err)
	assert.Empty(t, r.acmeEmail, "an existing issuer needs no ACME account email")
}

// The sandbox and email questions are conditional on a trusted origin
// EXISTING. An operator who answers the first question with a blank line has
// declined external access, and must not then be asked to configure it.
//
// The script carries two more lines than the blank one so the assertions
// discriminate: a sandbox question that was asked anyway would consume the
// second line and show up in the result, where an unasked one and an
// exhausted stream both produce the same empty value.
func TestAskWebdRoutingInputs_ABlankHostnameCallsOffTheQuestionsBelowIt(t *testing.T) {
	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{}, "\nsandbox.example.com\nops@example.com\n", true)

	require.NoError(t, err)
	assert.Empty(t, r.trustedHostname)
	assert.Empty(t, r.sandboxHostname, "no trusted origin means no second origin to ask about")
	assert.Empty(t, r.acmeEmail, "and nothing to certify")
}

// The gate, not this phase, is what refuses — and it refuses in the words it
// always used. The expected message is derived from validateWebdRouting itself
// rather than transcribed, so it cannot drift into agreeing with a changed
// implementation.
func TestAskWebdRoutingInputs_NonInteractiveFailsWithTheUnchangedMessage(t *testing.T) {
	before := validateWebdRouting(cloud.ManagedProfile, "", "", false, false)
	require.Error(t, before, "a managed cloud with no hostname is refused today")

	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{}, "webd.example.com\nsandbox.example.com\nops@example.com\n", false)
	require.NoError(t, err, "asking nothing is not itself an error")
	assert.Empty(t, r.trustedHostname, "a run with nobody at the terminal must not take an answer off the stream")

	after := validateWebdRouting(cloud.ManagedProfile, r.trustedHostname, r.sandboxHostname, r.disableViewer, r.manualWebdRouting)
	require.Error(t, after)
	assert.Equal(t, before.Error(), after.Error(), "the ASK phase adds a route; it must not change the refusal")
}

// --assume-yes means "do not prompt me", and it has no answer to "what
// hostname?" — it can accept an offer oap makes, not supply a value oap cannot
// invent. So a -y install asks nothing and falls through to the same gate,
// with the same message: turning a scripted install's fail-fast into a
// question nobody is there to answer is strictly worse than the error it
// replaces, because a hang leaves the operator nothing to act on.
//
// The stream carries every answer and a terminal is attached, so this fails
// loudly if the phase asks: those are exactly the conditions under which a
// real -y run would block.
func TestAskWebdRoutingInputs_AssumeYesAsksNothingAndFallsThroughToTheGate(t *testing.T) {
	before := validateWebdRouting(cloud.ManagedProfile, "", "", false, false)
	require.Error(t, before, "a managed cloud with no hostname is refused today")

	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{assumeYes: true},
		"webd.example.com\nsandbox.example.com\nops@example.com\n", true)
	require.NoError(t, err)
	assert.Empty(t, r.trustedHostname, "--assume-yes must not turn a clean exit into a prompt")
	assert.Empty(t, r.sandboxHostname)
	assert.Empty(t, r.acmeEmail)

	after := validateWebdRouting(cloud.ManagedProfile, r.trustedHostname, r.sandboxHostname, r.disableViewer, r.manualWebdRouting)
	require.Error(t, after, "the install must still be refused, not proceed on answers nobody gave")
	assert.Equal(t, before.Error(), after.Error(), "--assume-yes changes nothing about the refusal")
}

// The same for the certificate email, whose refusal lives much further down —
// in the cert-manager TLS strategy, after the data plane is up. A
// non-interactive run must reach it with the same empty value it had before.
func TestAskWebdRoutingInputs_NonInteractiveLeavesTheEmailUnanswered(t *testing.T) {
	r, err := ask(t, cloud.ManagedProfile, WebdRoutingOpts{
		trustedHostname: "webd.example.com",
		sandboxHostname: "sandbox.example.com",
	}, "ops@example.com\n", false)

	require.NoError(t, err)
	assert.Empty(t, r.acmeEmail)
}

// The unified init wizard gathers routing through its own rail screens and sets
// WebdRoutingOpts.preconfirmed before calling RunInstall; RunInstall's internal
// ASK must not prompt the same TTY a second time. This is the case fold-back
// alone does NOT cover: a trusted origin is set but the sandbox is left blank, so
// without preconfirmed webdRoutingInputs still declares the sandbox question
// (the guard asserts exactly that) and a scripted line would be consumed.
// preconfirmed short-circuits the whole phase, so the line is not read and the
// pre-gathered values return unchanged.
func TestAskWebdRoutingInputs_PreconfirmedSuppressesTheReAsk(t *testing.T) {
	// Guard: the same opts WITHOUT preconfirmed do declare the still-blank
	// questions, so the suppression below is doing real work rather than passing
	// vacuously — a trusted origin with no sandbox and no email re-asks for both.
	require.Equal(t, []string{flagSandboxHostname, flagACMEEmail},
		declaredFlags(t, webdRoutingInputs(cloud.ManagedProfile, WebdRoutingOpts{trustedHostname: "webd.example.com"})),
		"without preconfirmed, a trusted origin with no sandbox/email re-asks for both")

	r, err := ask(t, cloud.ManagedProfile,
		WebdRoutingOpts{trustedHostname: "webd.example.com", preconfirmed: true},
		"sandbox.example.com\n", true /*interactive*/)

	require.NoError(t, err)
	assert.Empty(t, r.sandboxHostname, "preconfirmed short-circuits the ASK; the scripted line is not consumed")
	assert.Equal(t, "webd.example.com", r.trustedHostname, "the pre-gathered value returns unchanged")
}

// testClusterIssuerGVR mirrors the unexported GVR pkg/platform/cloud reads
// off the webd Let's Encrypt ClusterIssuer (cloud.DetectLetsEncryptEmail);
// this package cannot see that var, so the fake dynamic client here is wired
// against the same literal GVR instead.
var testClusterIssuerGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers",
}

// newInstallcmdDetectDyn mirrors cloud's own newDetectDyn (gateway_detect_test.go):
// a fake dynamic client seeded with the webd ClusterIssuer when email is
// non-empty, or with none at all — the "fresh cluster" case — when it is "".
func newInstallcmdDetectDyn(t *testing.T, email string) dynamic.Interface {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{testClusterIssuerGVR: "ClusterIssuerList"}
	if email == "" {
		return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": cloud.WebdLetsEncryptIssuerName},
		"spec":       map[string]any{"acme": map[string]any{"email": email}},
	}}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, obj)
}

// newInstallcmdDetectDynErroring simulates a cluster read that fails for a
// reason OTHER than "the issuer doesn't exist yet" (e.g. a stale/expired
// kubeconfig token) — the case seededACMEEmail must not swallow silently.
func newInstallcmdDetectDynErroring(t *testing.T) dynamic.Interface {
	t.Helper()
	dyn := newInstallcmdDetectDyn(t, "").(*dynfake.FakeDynamicClient)
	dyn.PrependReactor("get", "clusterissuers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp: connection refused")
	})
	return dyn
}

// TestSeededACMEEmail covers seededACMEEmail, the helper askWebdRoutingInputs'
// caller uses to pre-fill the ACME email prompt from the cluster's existing
// Let's Encrypt ClusterIssuer.
//
// The value lands in acmeEmailDetected — the prompt Default — NOT acmeEmail:
// writing the flag field would suppress the question (webdRoutingInputs
// declares it only when acmeEmail is empty) and flow an unconfirmed email to
// the apply. Every case asserts acmeEmail is left exactly as the caller passed
// it, so a regression that writes the flag field again is caught here.
//
// Per AGENTS.md "never silently drop errors", a detect failure must be
// surfaced (warned), not swallowed — the last case is what proves that path is
// wired, not just the happy ones.
func TestSeededACMEEmail(t *testing.T) {
	cases := []struct {
		name         string
		r            WebdRoutingOpts
		issuer       string                               // "" => no issuer object; ignored when dynFn is set
		dynFn        func(t *testing.T) dynamic.Interface // overrides the issuer-based fake when set
		wantDetected string                               // expected acmeEmailDetected after the call
		wantWarn     string                               // substring expected in the warning output; "" => none expected
	}{
		{name: "empty email + issuer present: seeds the detected default", r: WebdRoutingOpts{}, issuer: "demo@example.test", wantDetected: "demo@example.test"},
		{name: "flag email set: keeps flag, does not detect", r: WebdRoutingOpts{acmeEmail: "flag@example.test"}, issuer: "demo@example.test", wantDetected: ""},
		{name: "manual routing: never seeds", r: WebdRoutingOpts{manualWebdRouting: true}, issuer: "demo@example.test", wantDetected: ""},
		{name: "tls-issuer set: never seeds", r: WebdRoutingOpts{tlsIssuer: "my-issuer"}, issuer: "demo@example.test", wantDetected: ""},
		{name: "no issuer: stays empty", r: WebdRoutingOpts{}, issuer: "", wantDetected: ""},
		{
			name:         "detect errors: warns and leaves r unchanged",
			r:            WebdRoutingOpts{},
			dynFn:        newInstallcmdDetectDynErroring,
			wantDetected: "",
			wantWarn:     "could not detect existing ACME email for the prompt default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dyn dynamic.Interface
			if tc.dynFn != nil {
				dyn = tc.dynFn(t)
			} else {
				dyn = newInstallcmdDetectDyn(t, tc.issuer)
			}
			var out bytes.Buffer
			got := seededACMEEmail(context.Background(), dyn, tc.r, &out)
			assert.Equal(t, tc.wantDetected, got.acmeEmailDetected, "the detected email seeds the prompt Default")
			assert.Equal(t, tc.r.acmeEmail, got.acmeEmail, "seededACMEEmail must never write the flag field acmeEmail")
			if tc.wantWarn != "" {
				assert.Contains(t, out.String(), tc.wantWarn)
			} else {
				assert.Empty(t, out.String(), "no detection was attempted (or it succeeded silently), so nothing should be printed")
			}
		})
	}
}

// TestWebdRoutingInputs_DetectedEmailBecomesTheCertPromptDefault is the test
// the isolated seededACMEEmail cases cannot be: it proves the detected value
// pre-fills the question rather than skipping it. With acmeEmail empty (no
// flag) and acmeEmailDetected set, the certificate Component is STILL declared
// — so the operator is still asked — and its Input's Default carries the
// detected email, which the ASK folds back on a bare-Enter.
//
// This is the exact contract the flag-set path breaks on purpose: a
// non-empty acmeEmail suppresses the Component (a flag is a decision), covered
// by TestWebdRoutingInputs_DeclaresExactlyTheValuesTheInstallWouldRefuseWithout.
func TestWebdRoutingInputs_DetectedEmailBecomesTheCertPromptDefault(t *testing.T) {
	comps := webdRoutingInputs(cloud.ManagedProfile, WebdRoutingOpts{
		trustedHostname:   "webd.example.com",
		sandboxHostname:   "sandbox.example.com",
		acmeEmailDetected: "detected@example.test",
	})

	require.Equal(t, []string{flagACMEEmail}, declaredFlags(t, comps),
		"the two hostnames are supplied; only the certificate email is still asked")

	in := certEmailInput(t, comps)
	assert.Equal(t, "detected@example.test", in.Default,
		"the detected email is the prompt's editable Default, not a skipped answer")
	assert.False(t, in.Required, "a certificate email is offered, never demanded, by this phase")
}

// certEmailInput returns the single flagACMEEmail Input declared across comps,
// failing the test if it is absent — the caller has already asserted it is the
// only declared flag, so exactly one match is expected.
func certEmailInput(t *testing.T, comps []initpipeline.Component) initpipeline.Input {
	t.Helper()
	for _, c := range comps {
		for _, i := range c.Inputs {
			if i.Flag == flagACMEEmail {
				return i
			}
		}
	}
	t.Fatalf("no %s Input declared", flagACMEEmail)
	return initpipeline.Input{}
}
