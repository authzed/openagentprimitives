package agentcmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func TestAgentPush_NonexistentFile(t *testing.T) {
	cmd := newAgentPushCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1", filepath.Join(t.TempDir(), "does-not-exist.oap")})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.oap")
}

func TestAgentPush_MalformedRef(t *testing.T) {
	path := aptest.WriteFile(t, t.TempDir(), "pm.oap", "not a real .oap, just needs to exist")

	cmd := newAgentPushCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"not-a-ref", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-ref")
}

func TestAgentPush_ReadsStdinForDashPath(t *testing.T) {
	cmd := newAgentPushCmd(&apcmd.Globals{})
	var stdin bytes.Buffer
	stdin.WriteString("fake .oap bytes")
	cmd.SetIn(&stdin)
	// Malformed ref (no "/") so Push fails synchronously without dialing any
	// registry — this test only asserts stdin was consumed and the failure
	// happened at the push step, not at some earlier "can't read stdin" step.
	cmd.SetArgs([]string{"not-a-ref", "-"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "push")
	assert.Contains(t, err.Error(), "not-a-ref")
}

func TestAgentPush_WrongArgCount(t *testing.T) {
	cmd := newAgentPushCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"only-a-ref"})
	assert.Error(t, cmd.Execute())
}

func TestAgentPull_MalformedRef(t *testing.T) {
	cmd := newAgentPullCmd(&apcmd.Globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"not-a-ref"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-ref")
}

func TestAgentPull_OutputFlagWiring(t *testing.T) {
	cmd := newAgentPullCmd(&apcmd.Globals{})
	out := cmd.Flags().Lookup("output")
	require.NotNil(t, out, "pull must register a -o/--output flag")
	assert.Equal(t, "o", out.Shorthand)
	assert.Equal(t, "", out.DefValue, "default is computed from the ref at run time, not a static flag default")
}

func TestAgentPull_PlainHTTPFlagWiring(t *testing.T) {
	cmd := newAgentPullCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("plain-http")
	require.NotNil(t, flag, "pull must register a --plain-http flag")
	assert.Equal(t, "false", flag.DefValue)
}

func TestAgentPush_PlainHTTPFlagWiring(t *testing.T) {
	cmd := newAgentPushCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("plain-http")
	require.NotNil(t, flag, "push must register a --plain-http flag")
	assert.Equal(t, "false", flag.DefValue)
}

func TestAgentPull_VerifyRequiresKey(t *testing.T) {
	// --verify without --key must be rejected before any network access —
	// there's nothing to verify against — and must not depend on a live
	// registry to fail (the ref below is never dialed).
	cmd := newAgentPullCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1", "--verify"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--key")
}

func TestAgentPull_VerifyGarbageKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := aptest.WriteFile(t, dir, "garbage.pub", "not a real PEM key")

	cmd := newAgentPullCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1", "--verify", "--key", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PEM file")
}

func TestAgentPull_VerifyFailsClosedBeforeWritingOutput(t *testing.T) {
	// A malformed ref makes oci.Verify fail synchronously (ref parsing, no
	// network dial needed) — this asserts the failure surfaces as the verify
	// step (not a later pull/write step) and that no output file is created,
	// i.e. Verify runs and must succeed strictly before any bytes would be
	// written.
	dir := t.TempDir()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	keyPath := writeECDSAPublicKeyPEM(t, dir, "key.pub", &priv.PublicKey)
	outPath := filepath.Join(dir, "out.oap")

	cmd := newAgentPullCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"not-a-ref", "--verify", "--key", keyPath, "-o", outPath})
	err = cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-ref")

	_, statErr := os.Stat(outPath)
	assert.True(t, os.IsNotExist(statErr), "verify failure must not write the output file")
}

func TestAgentPull_VerifyFlagWiring(t *testing.T) {
	cmd := newAgentPullCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("verify")
	require.NotNil(t, flag, "pull must register a --verify flag")
	assert.Equal(t, "false", flag.DefValue)

	keyFlag := cmd.Flags().Lookup("key")
	require.NotNil(t, keyFlag, "pull must register a --key flag")
	assert.Equal(t, "", keyFlag.DefValue)
}

func TestAgentPull_WithoutVerifyUnaffected(t *testing.T) {
	// Without --verify, pull's malformed-ref error path is unchanged from
	// before this task (guards against the --verify wiring accidentally
	// changing default behavior).
	cmd := newAgentPullCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"not-a-ref"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-ref")
	assert.NotContains(t, err.Error(), "verify")
}

func TestWriteFileAtomic_WritesReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bundle.oap")

	require.NoError(t, writeFileAtomic(target, []byte("first"), 0o644))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "first", string(got))

	// A second write clobbers the prior content in place (same semantics as
	// os.WriteFile on success).
	require.NoError(t, writeFileAtomic(target, []byte("second"), 0o644))
	got, err = os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "second", string(got))

	// The rename leaves no ".bundle.oap.tmp-*" turds behind in the directory.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "atomic write must leave only the final file, no temp remnants")
	assert.Equal(t, "bundle.oap", entries[0].Name())

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "perm must match the requested mode")
}

func TestWriteFileAtomic_UnwritableDirErrors(t *testing.T) {
	// A non-existent parent dir makes os.CreateTemp fail — the helper must
	// surface that error rather than silently succeeding, and must not create
	// the target.
	target := filepath.Join(t.TempDir(), "no-such-subdir", "bundle.oap")
	err := writeFileAtomic(target, []byte("x"), 0o644)
	require.Error(t, err)
	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr))
}

func TestAgentPush_MalformedRefBeforeReadingFile(t *testing.T) {
	// A malformed ref surfaces as a "push" error, not a file-not-found error,
	// even when the path doesn't exist either — the error message should not
	// be confusable between the two failure modes.
	cmd := newAgentPushCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"not-a-ref", filepath.Join(t.TempDir(), "also-missing.oap")})
	err := cmd.Execute()
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "not-a-ref") || strings.Contains(err.Error(), "also-missing.oap"))
}
