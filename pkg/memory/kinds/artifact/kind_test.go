package artifact_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
)

func TestArtifactKind_RegistryAndShape(t *testing.T) {
	k, ok := memory.LookupKind("artifact")
	require.True(t, ok, "artifact Kind must be registered via init()")
	assert.Equal(t, "artifact-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(artifact.Artifact{}), k.ContentSchema())

	a := artifact.Artifact{Name: "report", RendererKind: "html", RevisionCount: 3, Tags: map[string]string{"latest": "artrev-c"}}
	raw, err := json.Marshal(a)
	require.NoError(t, err, "marshal Artifact")
	var back artifact.Artifact
	require.NoError(t, json.Unmarshal(raw, &back), "unmarshal Artifact")
	assert.Equal(t, a, back, "Artifact must round-trip through JSON")
}

// TestArtifactKind_OutlivesTheLinksWrittenToIt pins the lifetime promise a
// durable artifact link depends on.
//
// channelkinds.ComposeArtifactViewURL's output is written into records this
// system does not own and cannot revise — a GitHub check run's details link,
// read weeks after the session's pods are gone. That link resolves only for as
// long as this entry does. A TTL added here, or an ArchiveOn signal that made
// the head evictable, would turn every one of those links into a 404 that
// nothing in this repo would notice.
//
// The bound is the session, deliberately: the head lives while its scope does
// and is reclaimed with it, so the link's lifetime is the AgentSession's — not
// the runner pod's, and not a clock's.
func TestArtifactKind_OutlivesTheLinksWrittenToIt(t *testing.T) {
	k, ok := memory.LookupKind("artifact")
	require.True(t, ok)

	r := k.Retention()
	assert.True(t, r.EssentialWhileLive, "a link written to a third party must not outlive what it points at")
	assert.Empty(t, r.ArchiveOn, "no signal may make an artifact head evictable while its session lives")
	assert.Zero(t, r.TTLAfterArchive, "a durable link has no expiry, so neither may the entry it names")
	assert.Zero(t, r.SoftCapPerScope, "a per-scope cap would silently drop the oldest report a check run still links to")
}
