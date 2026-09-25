package channelplan_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// declaredChannelBundle is a bundle in the shape LintRequiredChannels blesses:
// declared channels, and one bundled AgentIdentity whose credential reads the
// "<channel-name>-creds" Secret one of those channels' wizards will write. The
// two strings agree, which is the whole property this file is about.
//
// The second declaration is a delivery target the first one needs: B-R14
// refuses an input channel with no role=output sibling, so an input-only
// bundle is not one the lint blesses and would be the wrong fixture for a file
// whose subject is what rename does to a BLESSED bundle.
func declaredChannelBundle(t *testing.T) *oap.Bundle {
	t.Helper()
	return bundleWith(t, `requires:
  channels:
    - kind: github
      role: input
      name: demo-agent-gh
    - kind: slack
      role: output
      name: demo-agent-out
`,
		`apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-agent
spec:
  agentIdentity: demo-agent-id
`,
		agentIdentityReferencingSecret(t, "demo-agent-gh-creds"),
	)
}

// credSecretRef reads the one githubApp credential's secretRef back off the
// (possibly renamed) AgentIdentity.
func credSecretRef(t *testing.T, crs []*unstructured.Unstructured) string {
	t.Helper()
	for _, cr := range crs {
		if cr.GetKind() != "AgentIdentity" {
			continue
		}
		creds, found, err := unstructured.NestedSlice(cr.Object, "spec", "credentials")
		require.NoError(t, err)
		require.True(t, found, "the fixture AgentIdentity must carry a credential")
		require.Len(t, creds, 1)
		elem, ok := creds[0].(map[string]any)
		require.True(t, ok)
		ref, found, err := unstructured.NestedString(elem, "githubApp", "secretRef", "name")
		require.NoError(t, err)
		require.True(t, found, "the fixture credential must carry a secretRef")
		return ref
	}
	t.Fatal("the fixture must contain an AgentIdentity")
	return ""
}

// crNamed reports whether a CR of this Kind and name is in the set.
func crNamed(crs []*unstructured.Unstructured, kind, name string) bool {
	for _, cr := range crs {
		if cr.GetKind() == kind && cr.GetName() == name {
			return true
		}
	}
	return false
}

// TestRenameKeepsTheCredentialPointingAtTheDeclaredChannelsSecret guards ONE
// HALF of the "<channel-name>-creds" correspondence under a named install: the
// half instance.Rename owns.
//
// SCOPE, STATED HONESTLY, because an earlier version of this comment claimed
// to be two-sided and was not. Rename is handed CRs and never sees the
// manifest, so nothing here can observe a change that prefixes the DECLARED
// CHANNEL NAME — that would live in install/apply.go or a bundle-aware helper,
// and this test would stay green through it. What it does catch is the other
// direction: prefixing the credential's secretRef while the declaration stays
// put, which dangles the credential just as surely.
//
// The declared half is guarded elsewhere, and deliberately: PlanChannels
// refuses a named install of a channel-declaring bundle outright (B-R9), so
// there is no supported path on which a declared name could acquire a prefix.
// The assertion at the end of this test pins those two guards together, so
// removing the refusal cannot quietly leave this half uncovered.
//
// LintRequiredChannels proves the two strings agree in the bundle AS AUTHORED;
// it runs before Rename and never sees the renamed CRs.
//
// Both expectations are LITERALS. Reading the declared name back out of the
// manifest under test would move the expectation with the value, which proves
// the assertion's arithmetic rather than the behaviour.
func TestRenameKeepsTheCredentialPointingAtTheDeclaredChannelsSecret(t *testing.T) {
	b := declaredChannelBundle(t)

	// As authored: the lint is satisfied, which is what makes the two strings
	// below a correspondence rather than a coincidence.
	findings, err := channelplan.LintRequiredChannels(b)
	require.NoError(t, err)
	require.Empty(t, findings, "the fixture must be a bundle the lint blesses")

	crs, err := b.CRs()
	require.NoError(t, err)
	require.NoError(t, instance.Rename(crs, "my-bot-"), "a named install renames every bundled CR")

	// Rename really ran: the AgentClass carries the prefix.
	assert.True(t, crNamed(crs, "AgentClass", "my-bot-demo-agent"),
		"the prefix must actually have been applied, or every assertion below is vacuous")

	assert.Equal(t, "demo-agent-gh-creds", credSecretRef(t, crs),
		"the credential must still read the Secret the DECLARED channel's wizard writes; prefixing it "+
			"while the declaration stays put is the dangling-creds failure the lint cannot see")

	// The other half of the correspondence, pinned to the guard that actually
	// holds it. Without this, deleting B-R9's refusal would leave the declared
	// name free to acquire a prefix with nothing in the tree objecting.
	_, err = channelplan.PlanChannels(context.Background(), nil, b, "default", "demo-agent", "my-bot")
	require.Error(t, err,
		"the declared half is guarded by the refusal, not by this file: a named install of a "+
			"channel-declaring bundle must stay refused, or this test covers only one direction")
}

// TestRenameLeavesAChannelCredsSecretRefAlone characterizes WHY the
// correspondence above holds, because the reason is not obvious and the
// obvious guess is wrong.
//
// instance.Rename rewrites an AgentIdentity credential's secretRef only when
// the Secret it names is one this bundle materializes: renamedNames is built
// from the CRs and SecretSpecs in the rename set, and a lookup that misses
// falls through the "an EXTERNAL Secret this bundle does not create; leave it
// alone" branch. A "<channel>-creds" Secret is written by the CHANNEL WIZARD
// at channel-create time, never by install, so it is never in that map and the
// ref is never prefixed.
//
// Both halves therefore stay unprefixed TOGETHER, and that is what keeps them
// equal. Prefixing the declared channel name to match a renamed secretRef
// would not fix a mismatch — it would create one.
func TestRenameLeavesAChannelCredsSecretRefAlone(t *testing.T) {
	b := declaredChannelBundle(t)
	crs, err := b.CRs()
	require.NoError(t, err)
	require.NoError(t, instance.Rename(crs, "my-bot-"))

	t.Logf("AgentClass name                    = %q", crName(t, crs, "AgentClass"))
	t.Logf("AgentIdentity name                 = %q", crName(t, crs, "AgentIdentity"))
	t.Logf("AgentClass spec.agentIdentity      = %q", agentClassIdentityRef(t, crs))
	t.Logf("credential secretRef.name          = %q", credSecretRef(t, crs))
	t.Logf("manifest requires.channels[0].name = %q", b.Manifest.Requires.Channels[0].Name)

	assert.Equal(t, "my-bot-demo-agent", crName(t, crs, "AgentClass"),
		"a CR the bundle carries IS prefixed")
	assert.Equal(t, "my-bot-demo-agent-id", agentClassIdentityRef(t, crs),
		"and a ref naming a bundled CR IS rewritten")
	assert.Equal(t, "demo-agent-gh-creds", credSecretRef(t, crs),
		"but a Secret the bundle does not create is left alone — the branch the correspondence rests on")
	assert.Equal(t, "demo-agent-gh", b.Manifest.Requires.Channels[0].Name,
		"and the declared channel name is not a CR, so Rename never sees it")
}

// crName returns the name of the single CR of this Kind in the set.
func crName(t *testing.T, crs []*unstructured.Unstructured, kind string) string {
	t.Helper()
	for _, cr := range crs {
		if cr.GetKind() == kind {
			return cr.GetName()
		}
	}
	t.Fatalf("no %s in the fixture", kind)
	return ""
}

// agentClassIdentityRef reads spec.agentIdentity off the bundle's AgentClass.
func agentClassIdentityRef(t *testing.T, crs []*unstructured.Unstructured) string {
	t.Helper()
	for _, cr := range crs {
		if cr.GetKind() != "AgentClass" {
			continue
		}
		ref, found, err := unstructured.NestedString(cr.Object, "spec", "agentIdentity")
		require.NoError(t, err)
		require.True(t, found, "the fixture AgentClass must name an AgentIdentity")
		return ref
	}
	t.Fatal("no AgentClass in the fixture")
	return ""
}
