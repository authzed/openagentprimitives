package nats

import (
	"os"
	"path/filepath"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0600))
	return p
}

func testCAPEM(t *testing.T) string {
	t.Helper()
	m, err := GenerateTLS([]string{"x"})
	require.NoError(t, err)
	return string(m.CACertPEM)
}

func TestBuildOptionsNoAuthIsEmpty(t *testing.T) {
	opts, err := buildOptions(Options{URL: "nats://127.0.0.1:4222"})
	require.NoError(t, err)
	assert.Empty(t, opts, "no creds/CA configured -> plain connect (test/dev path)")
}

// mintedCredsPath writes a real creds file for a user named name.
func mintedCredsPath(t *testing.T, dir, name string) string {
	t.Helper()
	id, err := GenerateIdentity()
	require.NoError(t, err, "GenerateIdentity")
	creds, err := MintUser(id, UserGrant{Name: name, PubAllow: []string{"ap.>"}})
	require.NoError(t, err, "MintUser %q", name)
	return writeTemp(t, dir, "x.creds", creds)
}

// applied resolves an option slice into the nats.Options it produces, so a
// test can assert on the resulting configuration rather than on opaque funcs.
func applied(t *testing.T, opts []natsgo.Option) natsgo.Options {
	t.Helper()
	var o natsgo.Options
	for _, opt := range opts {
		require.NoError(t, opt(&o), "apply option")
	}
	return o
}

func TestBuildOptionsWithCredsAndCA(t *testing.T) {
	dir := t.TempDir()
	credsPath := mintedCredsPath(t, dir, "runner-ns1-sess1")
	caPath := writeTemp(t, dir, "ca.crt", testCAPEM(t))

	opts, err := buildOptions(Options{
		URL:        "nats://127.0.0.1:4222",
		CredsPath:  credsPath,
		CAPath:     caPath,
		ServerName: "spicebox-nats.agentprimitives-system.svc",
		// Deliberately unlike the JWT's name: the connection name is a
		// per-process label, and deriving the inbox from it would put webd
		// ("webd-live-view") on a prefix its grant ("webd") does not allow.
		Name: "runner-some-process-label",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, opts, "creds + CA configured -> non-empty option set")

	got := applied(t, opts)
	assert.Equal(t, InboxPrefixFor("runner-ns1-sess1"), got.InboxPrefix,
		"inbox prefix must come from the creds file's user name, not Options.Name")
	assert.Equal(t, "spicebox-nats.agentprimitives-system.svc", got.TLSConfig.ServerName)
}

// buildOptions must fail loudly rather than fall back to the shared default
// inbox: a client that quietly subscribes where its grant does not allow loses
// every request/reply, and nothing in that failure points back to the creds.
func TestBuildOptionsCredsFailuresAreFatal(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name      string
		credsPath func() string
		wantErr   string
	}{
		{
			name:      "missing creds file: error names the path",
			credsPath: func() string { return filepath.Join(dir, "absent.creds") },
			wantErr:   "read NATS creds",
		},
		{
			name:      "creds file that is not a decorated JWT: error names the path",
			credsPath: func() string { return writeTemp(t, dir, "junk.creds", "-----BEGIN NATS USER JWT-----\n") },
			wantErr:   "NATS creds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildOptions(Options{URL: "nats://127.0.0.1:4222", CredsPath: tc.credsPath()})
			require.Error(t, err, "must not silently default the inbox prefix")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A connection with no creds is unauthenticated (the e2e harness against its
// embedded server); there is no grant to disagree with, so it keeps nats.go's
// default inbox rather than being given a prefix derived from nothing.
func TestBuildOptionsWithoutCredsKeepsDefaultInbox(t *testing.T) {
	opts, err := buildOptions(Options{URL: "nats://127.0.0.1:4222", Name: "harness"})
	require.NoError(t, err)
	assert.Empty(t, applied(t, opts).InboxPrefix)
}
