package agentcmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func writeFixtureOap(t *testing.T) string {
	t.Helper()
	b, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(b)
	require.NoError(t, err)
	dir := t.TempDir()
	p := filepath.Join(dir, "pm.oap")
	require.NoError(t, os.WriteFile(p, packed, 0o644))
	return p
}

func TestAgentInspect_PrintsSummaryAndDigest(t *testing.T) {
	p := writeFixtureOap(t)
	cmd := newAgentInspectCmd(&apcmd.Globals{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{p})
	require.NoError(t, cmd.Execute())

	s := out.String()
	// agent.name is inherited from the bundled AgentClass, so the fixture's
	// agent is named after its class, "demo-class".
	assert.Contains(t, s, "demo-class")
	assert.Contains(t, s, "1.2.0")
	assert.Contains(t, s, "sha256:")
}

func TestAgentInspect_PrintsEmbeddedDependencyAndDigest(t *testing.T) {
	dir := oaptest.WriteDependencyBundle(t)
	_, packed, err := loadBundle(dir)
	require.NoError(t, err)
	packedBundle, err := oap.Unpack(packed)
	require.NoError(t, err)
	require.Len(t, packedBundle.Manifest.Requires.Agents, 1)
	wantDigest := packedBundle.Manifest.Requires.Agents[0].Digest

	cmd := newAgentInspectCmd(&apcmd.Globals{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{dir})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, out.String(), "Embedded agents:")
	assert.Contains(t, out.String(), oaptest.DependencyChildName)
	assert.Contains(t, out.String(), wantDigest)
}

func TestWriteEmbeddedAgentsReturnsWriterError(t *testing.T) {
	dir := oaptest.WriteDependencyBundle(t)
	_, packed, err := loadBundle(dir)
	require.NoError(t, err)
	packedBundle, err := oap.Unpack(packed)
	require.NoError(t, err)

	err = writeEmbeddedAgents(failingWriter{}, packedBundle, nil)
	require.EqualError(t, err, "write failed")
}

// writeReadmeFixtureOap packs a folder that includes a README.md and returns
// the packed file path plus the exact README text.
func writeReadmeFixtureOap(t *testing.T) (string, string) {
	t.Helper()
	const doc = "# Fixture Agent\n\nLong form docs.\n"
	dir := t.TempDir()
	// agent.name is omitted: it is inherited from the bundled AgentClass below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "a.yaml"), []byte(
		"apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: doc-agent\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte(doc), 0o644))

	b, err := oap.FromFolder(dir)
	require.NoError(t, err)
	packed, err := oap.Pack(b)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "doc.oap")
	require.NoError(t, os.WriteFile(p, packed, 0o644))
	return p, doc
}

func TestAgentInspect_ShowsDocsLineWhenReadmePresent(t *testing.T) {
	p, _ := writeReadmeFixtureOap(t)
	cmd := newAgentInspectCmd(&apcmd.Globals{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{p})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Docs:")
	assert.Contains(t, out.String(), "README.md")
}

func TestAgentInspect_NoDocsLineWhenReadmeAbsent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "a.yaml"), []byte(
		"apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: bare\n"), 0o644))

	cmd := newAgentInspectCmd(&apcmd.Globals{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{dir})
	require.NoError(t, cmd.Execute())
	assert.NotContains(t, out.String(), "Docs:")
}

func TestAgentInspect_ReadmeFlagDumpsRawMarkdown(t *testing.T) {
	p, doc := writeReadmeFixtureOap(t)
	cmd := newAgentInspectCmd(&apcmd.Globals{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--readme", p})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, doc, out.String(), "--readme dumps the raw markdown and nothing else")
}

func TestAgentLint_OKAndFailure(t *testing.T) {
	p := writeFixtureOap(t)
	ok := newAgentLintCmd(&apcmd.Globals{})
	var out bytes.Buffer
	ok.SetOut(&out)
	ok.SetArgs([]string{p})
	require.NoError(t, ok.Execute())
	assert.Contains(t, out.String(), "OK")
	// A clean run must say so for every advisory check, the same way
	// LintAgentUIs already does with "no agent-UI findings" — silence about a
	// check that ran and found nothing is indistinguishable from a check that
	// never ran at all.
	assert.Contains(t, out.String(), "no skills-shape findings")
	assert.Contains(t, out.String(), "no agent-UI findings")
	assert.Contains(t, out.String(), "no skill-pinning findings")
	assert.Contains(t, out.String(), "no artifacts-capability findings")

	// A folder whose binding target CR is absent must lint-fail.
	bad := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(bad, "manifests"), 0o755))
	// agent.name is omitted (inherited from the AgentClass "x" below); the
	// dangling Channel/missing binding target is what must fail the lint.
	require.NoError(t, os.WriteFile(filepath.Join(bad, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1\"\nquestions:\n  - name: c\n    type: string\n    prompt: c?\n    binding:\n      - target: \"Channel/missing#spec.slack.channelID\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "manifests", "a.yaml"), []byte(
		"apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: x\n"), 0o644))
	lintBad := newAgentLintCmd(&apcmd.Globals{})
	lintBad.SetArgs([]string{bad})
	lintBad.SilenceErrors = true
	lintBad.SilenceUsage = true
	err := lintBad.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Channel/missing")
}
