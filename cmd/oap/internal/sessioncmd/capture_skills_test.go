package sessioncmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

const (
	skillNS      = "default"
	skillOwnedBy = "demo-skills"
	skillRef     = "github.com/demo-org/demo-skills//plugins/review/skills/review-pr@master"
	clusterRef   = "github.com/demo-org/cluster-skills//skills/audit@main"
)

func skillClass(refs ...string) *spiceboxv1alpha1.AgentClass {
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: skillNS},
	}
	for i, r := range refs {
		class.Spec.Skills = append(class.Spec.Skills,
			spiceboxv1alpha1.AgentSkill{Name: "skill" + string(rune('a'+i)), Ref: r})
	}
	return class
}

func ownedSkill(name, canonicalName, owner string) *spiceboxv1alpha1.Skill {
	sk := &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: skillNS},
		Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: canonicalName, Body: "# body\n"},
	}
	if owner != "" {
		sk.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
			Kind:       "SkillSource",
			Name:       owner,
			UID:        "live-uid",
			Controller: ptr.To(true),
		}}
	}
	return sk
}

func skillSource(name, repoURL string) *spiceboxv1alpha1.SkillSource {
	return &spiceboxv1alpha1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: skillNS},
		Spec:       spiceboxv1alpha1.SkillSourceSpec{RepoURL: repoURL},
	}
}

func gatherSkillsInto(t *testing.T, class *spiceboxv1alpha1.AgentClass, objs ...client.Object) (steelthread.FixtureInput, error) {
	t.Helper()
	var in steelthread.FixtureInput
	b := &kube.Bundle{Controller: fakeChannelClient(t, objs...), Namespace: skillNS}
	return in, gatherSkills(context.Background(), b, class, &in)
}

// TestGatherSkills_GathersTheSkillAndItsOwningSource is the pair the replay
// needs: neither object alone lets the Skill reach Valid=True, because the
// provenance gate demands a controller owner-ref to a SkillSource that EXISTS.
func TestGatherSkills_GathersTheSkillAndItsOwningSource(t *testing.T) {
	in, err := gatherSkillsInto(t, skillClass(skillRef),
		ownedSkill("review-pr-aaaaaaaa", skillRef, skillOwnedBy),
		skillSource(skillOwnedBy, "https://github.com/demo-org/demo-skills"),
	)
	require.NoError(t, err)

	require.Len(t, in.Skills, 1)
	assert.Equal(t, skillRef, in.Skills[0].Spec.CanonicalName)
	require.Len(t, in.SkillSources, 1)
	assert.Equal(t, skillOwnedBy, in.SkillSources[0].Name)
}

// TestGatherSkills_MatchesByCanonicalNameNotMetadataName pins the lookup the
// AgentClass reconciler itself performs. A Skill's metadata.name is a
// content-derived slug; the class references the canonical name, and a gather
// keyed on the wrong one would find nothing for every real skill.
func TestGatherSkills_MatchesByCanonicalNameNotMetadataName(t *testing.T) {
	in, err := gatherSkillsInto(t, skillClass(skillRef),
		ownedSkill("a-name-that-is-not-the-ref", skillRef, skillOwnedBy),
		skillSource(skillOwnedBy, "https://github.com/demo-org/demo-skills"),
	)
	require.NoError(t, err)
	require.Len(t, in.Skills, 1)
	assert.Equal(t, "a-name-that-is-not-the-ref", in.Skills[0].Name)
}

// TestGatherSkills_AnUnresolvedRefIsSkippedNotAnError is the one place this
// differs from gatherSandboxCRs, and the reason is the message a reader gets.
// A ClusterSkill-backed ref legitimately resolves to no namespaced Skill;
// aborting here would report a bare Get failure, while skipping lets the
// self-check name every unresolved ref together as CodeSkillsNotCaptured.
func TestGatherSkills_AnUnresolvedRefIsSkippedNotAnError(t *testing.T) {
	in, err := gatherSkillsInto(t, skillClass(skillRef, clusterRef),
		ownedSkill("review-pr-aaaaaaaa", skillRef, skillOwnedBy),
		skillSource(skillOwnedBy, "https://github.com/demo-org/demo-skills"),
	)
	require.NoError(t, err, "an unresolved ref must not abort the gather")
	require.Len(t, in.Skills, 1, "the resolvable one is still captured")
	assert.Equal(t, skillRef, in.Skills[0].Spec.CanonicalName)
}

// TestGatherSkills_AMissingOwnerSourceIsAnError: the live Skill was Valid=True
// (its class spawned this session), so the source existed when that verdict was
// reached. Emitting the Skill with an owner-ref to a source the fixture cannot
// write produces a bundle that parks at Valid=False/InvalidSpec — a worse
// failure than the one the refusal replaces, and one nothing else reports.
func TestGatherSkills_AMissingOwnerSourceIsAnError(t *testing.T) {
	_, err := gatherSkillsInto(t, skillClass(skillRef),
		ownedSkill("review-pr-aaaaaaaa", skillRef, skillOwnedBy),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), skillOwnedBy)
	assert.Contains(t, err.Error(), "provenance gate",
		"the error has to say WHY a missing source is fatal, or it reads as an unrelated Get failure")
}

// TestGatherSkills_AHandAuthoredSkillNeedsNoSource: a local// skill's
// provenance gate passes on the reserved authority alone, so demanding an owner
// would refuse a legitimate configuration.
func TestGatherSkills_AHandAuthoredSkillNeedsNoSource(t *testing.T) {
	in, err := gatherSkillsInto(t, skillClass("local//hand-written"),
		ownedSkill("hand-written-cccccccc", "local//hand-written", ""),
	)
	require.NoError(t, err)
	require.Len(t, in.Skills, 1)
	assert.Empty(t, in.SkillSources)
}

// TestGatherSkills_TwoSkillsSharingOneSourceGatherItOnce keeps the emitted
// fixture free of a duplicate document, which the harness would apply twice.
func TestGatherSkills_TwoSkillsSharingOneSourceGatherItOnce(t *testing.T) {
	const second = "github.com/demo-org/demo-skills//skills/other@master"
	in, err := gatherSkillsInto(t, skillClass(skillRef, second),
		ownedSkill("review-pr-aaaaaaaa", skillRef, skillOwnedBy),
		ownedSkill("other-bbbbbbbb", second, skillOwnedBy),
		skillSource(skillOwnedBy, "https://github.com/demo-org/demo-skills"),
	)
	require.NoError(t, err)
	assert.Len(t, in.Skills, 2)
	assert.Len(t, in.SkillSources, 1)
}

// TestGatherSkills_AClassWithNoSkillsReadsNothing is the negative control every
// capture taken before skills existed depends on.
func TestGatherSkills_AClassWithNoSkillsReadsNothing(t *testing.T) {
	in, err := gatherSkillsInto(t, skillClass())
	require.NoError(t, err)
	assert.Empty(t, in.Skills)
	assert.Empty(t, in.SkillSources)
}

// TestGatherManifests_ReachesTheSkills spans the join between the gather and
// its caller, and it exists because a mutation survived without it.
//
// Every case above drives gatherSkills directly, so none of them notices when
// gatherManifests stops calling it. That mutation leaves in.Skills empty for
// every capture, which the self-check then reports as CodeSkillsNotCaptured —
// a refusal naming the class's own skills, with nothing anywhere saying the
// gather was never run. It is the same shape as the two-tasks-half-a-contract
// gap: each half correct, the wiring between them untested.
func TestGatherManifests_ReachesTheSkills(t *testing.T) {
	class := skillClass(skillRef)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: skillNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class.Name},
	}
	cli := fakeChannelClient(t, class, sess,
		ownedSkill("review-pr-aaaaaaaa", skillRef, skillOwnedBy),
		skillSource(skillOwnedBy, "https://github.com/demo-org/demo-skills"),
	)

	in, err := gatherManifests(context.Background(),
		&kube.Bundle{Controller: cli, Namespace: skillNS}, sess, false)
	require.NoError(t, err)

	require.Len(t, in.Skills, 1, "gatherManifests did not reach the class's skills")
	assert.Equal(t, skillRef, in.Skills[0].Spec.CanonicalName)
	require.Len(t, in.SkillSources, 1, "the Skill rode through without the source its provenance gate needs")
	assert.Equal(t, skillOwnedBy, in.SkillSources[0].Name)
}

// TestControllerSkillSourceName covers the owner match, on exactly the fields
// pkg/tools/skills/materialize matches on. A ref this reports that the gate
// would not find sends the capture after a SkillSource the replay never
// consults; one it misses drops the source the replay requires.
func TestControllerSkillSourceName(t *testing.T) {
	cases := []struct {
		name string
		ref  metav1.OwnerReference
		want string
	}{
		{
			name: "controller SkillSource ref: found",
			ref: metav1.OwnerReference{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "SkillSource", Name: "src", Controller: ptr.To(true),
			},
			want: "src",
		},
		{
			name: "not the controller: the gate ignores it, so this must too",
			ref: metav1.OwnerReference{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "SkillSource", Name: "src",
			},
		},
		{
			name: "another kind: an AgentClass owner is not provenance",
			ref: metav1.OwnerReference{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "AgentClass", Name: "src", Controller: ptr.To(true),
			},
		},
		{
			name: "another group: a same-named CRD elsewhere is a different object",
			ref: metav1.OwnerReference{
				APIVersion: "other.example.com/v1", Kind: "SkillSource",
				Name: "src", Controller: ptr.To(true),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk := &spiceboxv1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{tc.ref}},
			}
			assert.Equal(t, tc.want, controllerSkillSourceName(sk))
		})
	}
}
