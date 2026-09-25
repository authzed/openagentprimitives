package installcmd

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// getConfigMap fetches a ConfigMap by name in the agentprimitives-system
// namespace (namespacedName, defined in install_secret_test.go).
func getConfigMap(t *testing.T, c client.Client, name string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), namespacedName(name), &cm))
	return &cm
}

func TestExternalSpiceDB_Enabled(t *testing.T) {
	cases := []struct {
		name string
		e    ExternalSpiceDB
		want bool
	}{
		{name: "zero value: disabled", e: ExternalSpiceDB{}, want: false},
		{name: "endpoint set: enabled", e: ExternalSpiceDB{Endpoint: "spicedb.example:443"}, want: true},
		{name: "endpoint set, token+insecure also set: enabled", e: ExternalSpiceDB{Endpoint: "x:443", Token: "tok", Insecure: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.e.enabled())
		})
	}
}

// TestApplyExternalSpiceDBConfig_CreatesFresh asserts that, against an empty
// cluster, applyExternalSpiceDBConfig creates both the endpoint ConfigMap and
// the token Secret with the external instance's values.
func TestApplyExternalSpiceDBConfig_CreatesFresh(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	e := ExternalSpiceDB{Endpoint: "spicedb.example.com:443", Token: "extern-tok", Insecure: false}

	require.NoError(t, applyExternalSpiceDBConfig(context.Background(), c, e))

	cm := getConfigMap(t, c, "spicebox-spicedb-config")
	assert.Equal(t, "spicedb.example.com:443", cm.Data["endpoint"])

	sec := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, "extern-tok", string(sec.Data["token"]))
}

// TestApplyExternalSpiceDBConfig_OverwritesExisting asserts that a re-run (or
// a switch from a prior external endpoint) overwrites the previously-written
// endpoint + token rather than leaving stale values, while not touching an
// unrelated Secret key.
func TestApplyExternalSpiceDBConfig_OverwritesExisting(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-config"},
			Data:       map[string]string{"endpoint": "spicebox-spicedb.agentprimitives-system.svc:50051"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agentprimitives-system", Name: "spicebox-spicedb-token"},
			Data:       map[string][]byte{"token": []byte("old-in-cluster-tok"), "preshared_key": []byte("old-in-cluster-tok")},
		},
	).Build()
	e := ExternalSpiceDB{Endpoint: "new-external.example:443", Token: "new-tok"}

	require.NoError(t, applyExternalSpiceDBConfig(context.Background(), c, e))

	cm := getConfigMap(t, c, "spicebox-spicedb-config")
	assert.Equal(t, "new-external.example:443", cm.Data["endpoint"])

	sec := getSecret(t, c, "spicebox-spicedb-token")
	assert.Equal(t, "new-tok", string(sec.Data["token"]))
	assert.Equal(t, "old-in-cluster-tok", string(sec.Data["preshared_key"]), "unrelated existing key must survive the upsert")
}

// stepReader hands back one input line per Read call, regardless of the
// caller's requested buffer size. It stands in for a real terminal in tests:
// a canonical-mode tty's read() returns as soon as one Enter-terminated line
// is available, not once the caller's buffer is full — which is exactly what
// lets apcmd.Confirm() (a fresh bufio.NewReader per call) and a follow-up
// bufio.NewScanner both read the same io.Reader in sequence without one
// swallowing the other's input. A bulk reader (strings.Reader/bytes.Buffer)
// does NOT have that property: bufio.NewReader's fill would read every
// remaining byte in one call, discarding whatever apcmd.Confirm() didn't return —
// exactly the failure resolveExternalSpiceDB must not have against a real
// tty, and exactly what stepReader avoids in these tests.
type stepReader struct {
	lines []string
}

func (r *stepReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	line := r.lines[0]
	r.lines = r.lines[1:]
	n := copy(p, line)
	return n, nil
}

// erroringReader fails the test if Read is ever called on it — used to prove
// a code path never touches stdin.
type erroringReader struct{ t *testing.T }

func (r erroringReader) Read([]byte) (int, error) {
	r.t.Fatal("unexpected stdin read")
	return 0, io.EOF
}

func TestResolveExternalSpiceDB(t *testing.T) {
	t.Run("flag endpoint set: returns it directly, never touches stdin", func(t *testing.T) {
		got := resolveExternalSpiceDB(erroringReader{t}, &bytes.Buffer{}, "flag.example:443", "flag-tok", true, true /*isTTY*/)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "flag.example:443", Token: "flag-tok", Insecure: true}, got)
	})

	t.Run("no flag, non-interactive: zero value, never touches stdin", func(t *testing.T) {
		got := resolveExternalSpiceDB(erroringReader{t}, &bytes.Buffer{}, "", "", false, false /*isTTY*/)
		assert.Equal(t, ExternalSpiceDB{}, got)
	})

	t.Run("no flag, interactive, user declines: zero value", func(t *testing.T) {
		in := &stepReader{lines: []string{"n\n"}}
		got := resolveExternalSpiceDB(in, &bytes.Buffer{}, "", "", false, true /*isTTY*/)
		assert.Equal(t, ExternalSpiceDB{}, got)
	})

	t.Run("no flag, interactive, user accepts + fills endpoint/token/insecure=yes: builds struct from prompts", func(t *testing.T) {
		in := &stepReader{lines: []string{"y\n", "prompted.example:443\n", "prompted-tok\n", "y\n"}}
		got := resolveExternalSpiceDB(in, &bytes.Buffer{}, "", "", false /*flagInsecure ignored on the interactive path*/, true /*isTTY*/)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "prompted.example:443", Token: "prompted-tok", Insecure: true}, got)
	})

	t.Run("no flag, interactive, user accepts + fills endpoint/token/insecure=no (default): builds struct from prompts", func(t *testing.T) {
		in := &stepReader{lines: []string{"y\n", "prompted.example:443\n", "prompted-tok\n", "\n"}}
		got := resolveExternalSpiceDB(in, &bytes.Buffer{}, "", "", true /*flagInsecure ignored on the interactive path*/, true /*isTTY*/)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "prompted.example:443", Token: "prompted-tok", Insecure: false}, got)
	})

	t.Run("no flag, interactive, user accepts but gives empty endpoint: zero value", func(t *testing.T) {
		in := &stepReader{lines: []string{"y\n", "\n"}}
		got := resolveExternalSpiceDB(in, &bytes.Buffer{}, "", "", false, true /*isTTY*/)
		assert.Equal(t, ExternalSpiceDB{}, got)
	})
}

// TestCollectExternalSpiceDBEndpoint covers the endpoint/token collection half
// split out of resolveExternalSpiceDB (F3/M1). Its defining property: it must
// NEVER re-ask the "use an existing external SpiceDB?" confirm — that decision
// belongs to the caller (the linear Y/N prompt, or the wizard's SpiceDB screen).
// Re-asking it, with a default of No, is how a bare Enter used to silently
// reverse the wizard's Yes.
func TestCollectExternalSpiceDBEndpoint(t *testing.T) {
	t.Run("collects endpoint/token/insecure WITHOUT re-asking the use-external confirm", func(t *testing.T) {
		in := &stepReader{lines: []string{"prompted.example:443\n", "prompted-tok\n", "y\n"}}
		var out bytes.Buffer
		got := collectExternalSpiceDBEndpoint(in, &out, true /*isTTY*/, "" /*defaultEndpoint*/)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "prompted.example:443", Token: "prompted-tok", Insecure: true}, got)
		assert.NotContains(t, out.String(), "Use an existing external SpiceDB",
			"the collection half must never re-ask the use-external confirm — the caller already decided")
	})

	t.Run("insecure default (bare Enter): TLS", func(t *testing.T) {
		in := &stepReader{lines: []string{"prompted.example:443\n", "prompted-tok\n", "\n"}}
		got := collectExternalSpiceDBEndpoint(in, &bytes.Buffer{}, true, "")
		assert.Equal(t, ExternalSpiceDB{Endpoint: "prompted.example:443", Token: "prompted-tok", Insecure: false}, got)
	})

	t.Run("empty endpoint, no default: zero value (bundled)", func(t *testing.T) {
		in := &stepReader{lines: []string{"\n"}}
		got := collectExternalSpiceDBEndpoint(in, &bytes.Buffer{}, true, "")
		assert.Equal(t, ExternalSpiceDB{}, got)
	})

	t.Run("EOF: zero value", func(t *testing.T) {
		got := collectExternalSpiceDBEndpoint(&stepReader{}, &bytes.Buffer{}, true, "")
		assert.Equal(t, ExternalSpiceDB{}, got)
	})

	// R1: a caller keeping an already-external cluster passes the detected
	// endpoint as defaultEndpoint; a bare Enter must keep it (and still go on
	// to prompt for the token/insecure setting) rather than falling back to
	// "no endpoint given" / the bundled zero value.
	t.Run("empty endpoint WITH a default: keeps the default, still prompts for token/insecure", func(t *testing.T) {
		in := &stepReader{lines: []string{"\n", "prompted-tok\n", "y\n"}}
		var out bytes.Buffer
		got := collectExternalSpiceDBEndpoint(in, &out, true, "spicedb.example.com:443")
		assert.Equal(t, ExternalSpiceDB{Endpoint: "spicedb.example.com:443", Token: "prompted-tok", Insecure: true}, got)
		assert.Contains(t, out.String(), "spicedb.example.com:443", "the prompt must show the default endpoint")
	})

	t.Run("a typed endpoint overrides a supplied default", func(t *testing.T) {
		in := &stepReader{lines: []string{"typed.example:443\n", "prompted-tok\n", "\n"}}
		got := collectExternalSpiceDBEndpoint(in, &bytes.Buffer{}, true, "spicedb.example.com:443")
		assert.Equal(t, ExternalSpiceDB{Endpoint: "typed.example:443", Token: "prompted-tok"}, got)
	})

	t.Run("EOF with a default: zero value (EOF is not a bare-Enter keep)", func(t *testing.T) {
		got := collectExternalSpiceDBEndpoint(&stepReader{}, &bytes.Buffer{}, true, "spicedb.example.com:443")
		assert.Equal(t, ExternalSpiceDB{}, got, "an EOF (no terminal input at all) is distinct from a deliberate bare Enter and must not fabricate the detected endpoint")
	})
}

// TestResolveExternalSpiceDB_TokenPromptSuppressesEcho covers the credential
// half of the prompt: the value asked for here is the SpiceDB pre-shared key,
// the unscoped root credential for the whole authorization system, so it must
// not be echoed to the terminal as it is typed or left sitting in scrollback.
//
// Echo suppression itself is a property of the terminal and cannot be observed
// from a test that has none. What IS observable — and what fails before the
// fix, because the token used to be read by the same plain scanner as the
// endpoint — is that the token goes through the secret-reading path at all:
// that path reports whether it managed to suppress echo, and the caller says
// so out loud when it could not, rather than silently putting a root
// credential on screen.
func TestResolveExternalSpiceDB_TokenPromptSuppressesEcho(t *testing.T) {
	// stepReader is not an *os.File, so there is no terminal to suppress echo
	// on — the same situation as a `oap init` whose stdin is a character device
	// that is not this process's terminal. The value must still be read (the
	// prompt has to keep working), and the user must be told it was visible.
	in := &stepReader{lines: []string{"y\n", "prompted.example:443\n", "prompted-tok\n", "\n"}}
	var out bytes.Buffer
	got := resolveExternalSpiceDB(in, &out, "", "", false, true /*isTTY*/)

	assert.Equal(t, "prompted-tok", got.Token, "the token must still be read when echo cannot be suppressed")
	assert.Contains(t, out.String(), "warning:",
		"a token read with echo still on must be reported, not silently echoed")
	assert.NotContains(t, out.String(), "prompted-tok",
		"oap must never write the token to its own output")
}
