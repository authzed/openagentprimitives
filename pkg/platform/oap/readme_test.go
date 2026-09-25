package oap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeReadmeFolder lays out a minimal valid folder source with a README.md.
func writeReadmeFolder(t *testing.T, readme string) string {
	t.Helper()
	dir := t.TempDir()
	// agent.name is omitted: it is inherited from the bundled AgentClass
	// (metadata.name: readme-agent) below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "a.yaml"), []byte(
		"apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: readme-agent\n"), 0o644))
	if readme != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644))
	}
	return dir
}

func TestReadme_RoundTripThroughPack(t *testing.T) {
	const doc = "# Readme Agent\n\nDoes a thing.\n"
	dir := writeReadmeFolder(t, doc)

	b, err := FromFolder(dir)
	require.NoError(t, err)
	assert.Equal(t, doc, string(b.Readme), "FromFolder must read README.md from the folder root")

	packed, err := Pack(b)
	require.NoError(t, err)

	got, err := Unpack(packed)
	require.NoError(t, err)
	assert.Equal(t, doc, string(got.Readme), "README bytes must survive Pack/Unpack")
	require.NotNil(t, got.Manifest.Content.Readme, "Content.Readme mirror must be populated on unpack")
	assert.NotEmpty(t, got.Manifest.Content.Readme.Digest)
}

func TestReadme_AbsentIsNilAndEmitsNoLayer(t *testing.T) {
	dir := writeReadmeFolder(t, "") // no README.md

	b, err := FromFolder(dir)
	require.NoError(t, err)
	assert.Nil(t, b.Readme, "no README.md → nil Readme")

	packed, err := Pack(b)
	require.NoError(t, err)
	got, err := Unpack(packed)
	require.NoError(t, err)
	assert.Nil(t, got.Readme, "README-less bundle must round-trip to nil Readme")
	assert.Nil(t, got.Manifest.Content.Readme, "no README layer → no Content.Readme mirror")
}

func TestReadme_PackIsDeterministic(t *testing.T) {
	dir := writeReadmeFolder(t, "# Same\n\nInput.\n")
	b, err := FromFolder(dir)
	require.NoError(t, err)

	p1, err := Pack(b)
	require.NoError(t, err)
	p2, err := Pack(b)
	require.NoError(t, err)
	assert.Equal(t, p1, p2, "two packs of the same README-bearing bundle must be byte-identical")
}
