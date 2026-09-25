package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// TestSelfCheck_AnUncapturedSkillIsRefused pins the refusal, and the reason it
// is HARD rather than a note.
//
// An AgentClass whose spec.skills names a canonical skill that no Skill or
// ClusterSkill in the namespace carries is parked by its own reconciler at
// Valid=False/AgentClassSkillMissing; the session reconciler will not spawn
// against an invalid class; and the replay driver fails at its readiness
// barrier having exercised nothing the bundle describes. That is exactly the
// looks-fine-replays-wrong shape the self-check exists to refuse, and a warning
// would let it into a repo.
//
// This was found by replaying a real capture, not by reading the code: the
// bundle emitted clean and died 37 seconds into the suite.
func TestSelfCheck_AnUncapturedSkillIsRefused(t *testing.T) {
	const ref = "github.com/demo-org/demo-skills//plugins/review/skills/review-pr@master"

	in := cleanCapture(t)
	in.ClassSkills = []string{ref}

	f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSkillsNotCaptured)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, ref,
		"the finding must NAME the skills, or a reader has nothing to hand-add")
	assert.True(t, steelthread.HasHardFinding(steelthread.SelfCheck(in)))
}

// TestSelfCheck_ACapturedSkillIsNotRefused is the whole point of the capture
// change, and it is the assertion the old per-class refusal could not make: a
// class that opts into a skill the fixture DOES carry emits.
func TestSelfCheck_ACapturedSkillIsNotRefused(t *testing.T) {
	const ref = "github.com/demo-org/demo-skills//plugins/review/skills/review-pr@master"

	in := cleanCapture(t)
	in.ClassSkills = []string{ref}
	in.CapturedSkills = []string{ref}

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeSkillsNotCaptured, f.Code,
			"the fixture emits a Skill for this ref; refusing anyway refuses every skills capture")
	}
}

// TestSelfCheck_OnlyTheUNCAPTUREDSkillsAreNamed is the difference between a
// per-class refusal and a per-ref one. A class with one captured skill and one
// that resolved to nothing must be refused for the SECOND, by name — naming
// both would send a reader to re-emit a manifest that is already there.
func TestSelfCheck_OnlyTheUNCAPTUREDSkillsAreNamed(t *testing.T) {
	const (
		captured = "github.com/demo-org/demo-skills//a@main"
		missing  = "github.com/other-org/cluster-skills//b@main"
	)

	in := cleanCapture(t)
	in.ClassSkills = []string{captured, missing}
	in.CapturedSkills = []string{captured}

	f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSkillsNotCaptured)
	assert.Contains(t, f.Message, missing)
	assert.NotContains(t, f.Message, captured,
		"a captured skill named in the refusal sends the reader to fix what is not broken")
	assert.Contains(t, f.Message, "1 skill(s)")
}

// TestSelfCheck_OneFindingForEverySkill is the counterpart to
// checkUnmappedTurns reporting each hole separately, and the reason the two
// differ.
//
// There, each hole is independently fixable and collapsing them would make a
// reader fix the first and re-run to discover the second. Here a single cause
// is behind every one of them — refs that resolved to no namespaced Skill — so
// N findings would be N copies of one sentence.
func TestSelfCheck_OneFindingForEverySkill(t *testing.T) {
	in := cleanCapture(t)
	in.ClassSkills = []string{
		"github.com/demo-org/demo-skills//a@main",
		"github.com/demo-org/demo-skills//b@main",
	}

	var n int
	for _, f := range steelthread.SelfCheck(in) {
		if f.Code == steelthread.CodeSkillsNotCaptured {
			n++
			assert.Contains(t, f.Message, "//a@main")
			assert.Contains(t, f.Message, "//b@main")
		}
	}
	assert.Equal(t, 1, n, "one finding naming both, not one per skill")
}

// TestSelfCheck_AClassWithNoSkillsIsUntouched is the negative control the
// refusal above is worthless without: every capture taken before skills existed
// must keep emitting.
func TestSelfCheck_AClassWithNoSkillsIsUntouched(t *testing.T) {
	in := cleanCapture(t)
	require.Empty(t, in.ClassSkills, "the clean fixture must declare no skills, or this asserts nothing")

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeSkillsNotCaptured, f.Code)
		assert.NotEqual(t, steelthread.CodeSkillBundleNotStaged, f.Code)
	}
}

// TestSelfCheck_ADroppedSkillBundleWarnsButEmits pins the severity split, which
// is the substance of the finding rather than a detail of it.
//
// A missing Skill stops the replay at the readiness barrier. A missing bundle
// ARCHIVE stops nothing: the operator's own staging path treats an uncached
// bundle as a log-and-skip and mounts the composed SKILL.md alone, so the
// bundle replays — it just replays a skill directory holding instructions and
// no executables.
func TestSelfCheck_ADroppedSkillBundleWarnsButEmits(t *testing.T) {
	const ref = "github.com/demo-org/demo-skills//plugins/review/skills/review-pr@master"

	in := cleanCapture(t)
	in.ClassSkills = []string{ref}
	in.CapturedSkills = []string{ref}
	in.SkillsWithoutBundle = []string{ref}

	findings := steelthread.SelfCheck(in)
	f := findByCode(t, findings, steelthread.CodeSkillBundleNotStaged)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
	assert.Contains(t, f.Message, ref, "a reader has to know WHICH skill lost its scripts")
	assert.False(t, steelthread.HasHardFinding(findings),
		"a skill that stages instruction-only still replays; refusing would refuse every "+
			"capture of a class using a bundled skill")
}

// TestClassSkillRefs covers the derivation the check reads, including the two
// inputs that must say NOTHING — a class with no skills, and a nil class. Both
// would otherwise refuse every capture in the tree.
func TestClassSkillRefs(t *testing.T) {
	cases := []struct {
		name  string
		class *spiceboxv1alpha1.AgentClass
		want  []string
	}{
		{name: "nil class: nothing", class: nil},
		{name: "no skills: nothing", class: &spiceboxv1alpha1.AgentClass{}},
		{
			name: "two skills: their canonical refs, in order",
			class: &spiceboxv1alpha1.AgentClass{
				Spec: spiceboxv1alpha1.AgentClassSpec{
					Skills: []spiceboxv1alpha1.AgentSkill{
						{Name: "review-pr", Ref: "github.com/demo-org/demo-skills//review@main"},
						{Name: "diff-review", Ref: "github.com/demo-org/demo-skills//diff@main"},
					},
				},
			},
			want: []string{
				"github.com/demo-org/demo-skills//review@main",
				"github.com/demo-org/demo-skills//diff@main",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, steelthread.ClassSkillRefsForTest(tc.class))
		})
	}
}
