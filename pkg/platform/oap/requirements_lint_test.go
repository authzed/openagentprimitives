package oap

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skillFixtureManifests returns a minimal one-CR bundle (just an AgentClass)
// whose spec.skills is exactly skills (a YAML list of {name, ref} entries),
// spliced in verbatim. Fabricated names throughout (demo-console prefix) —
// none shared with any bundled example.
func skillFixtureManifests(skills string) []byte {
	return []byte(fmt.Sprintf(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-console
spec:
  description: "Fixture AgentClass for the skill-pinning lint test."
  systemPrompt:
    inline: "You are a fixture agent used only by this test."
  skills:
%s
`, skills))
}

// TestLintSkillPinningNegativeControl proves the rule actually fires on a
// violating fixture AND stays silent on a clean one — the "construct a
// violation, confirm it's caught; construct a clean case, confirm silence"
// shape every lint rule here must satisfy, rather than a test that could pass
// whether or not the rule does anything.
func TestLintSkillPinningNegativeControl(t *testing.T) {
	cases := []struct {
		name    string
		skills  string // YAML list body spliced under spec.skills
		want    string // substring that must appear in exactly one finding; "" means none
		wantErr bool   // true when the entry itself should be reported as unparseable
	}{
		{
			name: "unpinned canonical name: flagged",
			skills: `    - name: x
      ref: "github.com/demo-org/demo-skills//skills/x"`,
			want: "no @ref",
		},
		{
			name: "frozen (sha) pin: clean",
			skills: `    - name: x
      ref: "github.com/demo-org/demo-skills//skills/x@abc1234"`,
		},
		{
			name: "named (tag/branch) pin: clean — a branch still counts as pinned syntactically",
			skills: `    - name: x
      ref: "github.com/demo-org/demo-skills//skills/x@main"`,
		},
		{
			name: "malformed canonical name (no // separator): flagged, different reason",
			skills: `    - name: bad
      ref: "not-a-canonical-name"`,
			want: "not a valid canonical skill name",
		},
		{
			name: "one bad among two good: exactly one finding",
			skills: `    - name: a
      ref: "github.com/demo-org/demo-skills//skills/a@v1.0.0"
    - name: b
      ref: "github.com/demo-org/demo-skills//skills/b"`,
			want: "no @ref",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{Manifests: skillFixtureManifests(tc.skills)}
			findings, err := LintSkillPinning(b)
			require.NoError(t, err, "LintSkillPinning")

			if tc.want == "" {
				assert.Empty(t, findings, "findings: %v", findings)
				return
			}
			assert.Equal(t, 1, countContainingRequirement(findings, tc.want),
				"want exactly one finding containing %q; got: %v", tc.want, findings)
		})
	}
}

// TestLintSkillPinningNoAgentClass pins that a bundle with no AgentClass at
// all lints clean rather than erroring.
func TestLintSkillPinningNoAgentClass(t *testing.T) {
	b := &Bundle{Manifests: []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data:
  x: "y"
`)}
	findings, err := LintSkillPinning(b)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestLintSkillPinningNilBundle(t *testing.T) {
	findings, err := LintSkillPinning(nil)
	require.Error(t, err)
	assert.Nil(t, findings)
}

// TestLintSkillsShapeOldFormatShowsTheRewrite pins the actual content of the
// finding, not merely that one was produced: a message that only said
// "invalid skills" and stopped would pass a weaker assertion but leaves an
// operator with nothing to act on. The old shape — a bare canonical-name
// string — is what Task 5 broke; the finding must show the exact object
// {name, ref, target} it becomes, including a name suggestion derived from
// the ref's last path segment.
func TestLintSkillsShapeOldFormatShowsTheRewrite(t *testing.T) {
	const oldRef = "github.com/demo-org/demo-skills//skills/code-review@v1.0.0"
	b := &Bundle{Manifests: skillFixtureManifests(fmt.Sprintf("    - %s", oldRef))}

	findings, err := LintSkillsShape(b)
	require.NoError(t, err, "LintSkillsShape")
	require.NotEmpty(t, findings, "the old shape must not pass silently")

	reason := findings[0].Reason
	assert.Contains(t, reason, "name: code-review",
		"the finding must show the suggested rewrite, not merely report a problem")
	assert.Contains(t, reason, "ref: "+oldRef)
	assert.Contains(t, reason, "target: agent")
	assert.Equal(t, "demo-console", findings[0].AgentClass)
	assert.Equal(t, "spec.skills[0]", findings[0].Path)
}

// TestLintSkillsShapeOldFormatUnparseableRefStillShowsTheRewrite covers the
// entry that isn't even a valid canonical name: no suggested name is
// possible, but the finding must still show the {name, ref, target} shape
// rather than going silent because the suggestion step failed.
func TestLintSkillsShapeOldFormatUnparseableRefStillShowsTheRewrite(t *testing.T) {
	b := &Bundle{Manifests: skillFixtureManifests("    - not-a-canonical-name")}

	findings, err := LintSkillsShape(b)
	require.NoError(t, err, "LintSkillsShape")
	require.NotEmpty(t, findings)

	reason := findings[0].Reason
	assert.Contains(t, reason, "name:")
	assert.Contains(t, reason, "ref: not-a-canonical-name")
}

// TestLintSkillsShapeNewFormatIsClean proves the detector fires on the old
// shape and ONLY the old shape: a check that also flags the current
// {name, ref, target} object shape would block every valid bundle and is
// worse than no check at all.
func TestLintSkillsShapeNewFormatIsClean(t *testing.T) {
	b := &Bundle{Manifests: skillFixtureManifests(`    - name: code-review
      ref: "github.com/demo-org/demo-skills//skills/code-review@v1.0.0"
      target: sandbox`)}

	findings, err := LintSkillsShape(b)
	require.NoError(t, err, "LintSkillsShape")
	assert.Empty(t, findings, "findings: %v", findings)
}

// TestLintSkillsShapeNoAgentClass pins that a bundle with no AgentClass at
// all lints clean rather than erroring.
func TestLintSkillsShapeNoAgentClass(t *testing.T) {
	b := &Bundle{Manifests: []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data:
  x: "y"
`)}
	findings, err := LintSkillsShape(b)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestLintSkillsShapeNilBundle(t *testing.T) {
	findings, err := LintSkillsShape(nil)
	require.Error(t, err)
	assert.Nil(t, findings)
}

// artifactsFixtureManifests returns a bundle with one AgentClass whose
// systemPrompt.inline is promptBody and whose spec.capabilities is spliced in
// verbatim ("" omits the field entirely, decoding to a nil map — as if an
// author never wrote it).
func artifactsFixtureManifests(promptBody, capsYAML string) []byte {
	caps := ""
	if capsYAML != "" {
		caps = "  capabilities:\n" + capsYAML
	}
	return []byte(fmt.Sprintf(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-console
spec:
  description: "Fixture AgentClass for the artifacts-capability lint test."
  systemPrompt:
    inline: %q
%s
`, promptBody, caps))
}

// TestLintArtifactsCapabilityNegativeControl proves the rule fires exactly
// when the prompt calls artifact_prepare AND the capability is missing, and
// stays silent in every other combination — including the case that a
// weaker "capabilities is just empty" check would miss: some OTHER
// capability declared, but not "artifacts".
func TestLintArtifactsCapabilityNegativeControl(t *testing.T) {
	cases := []struct {
		name       string
		promptBody string
		capsYAML   string // spliced verbatim under spec.capabilities; "" omits the field
		want       string
	}{
		{
			name:       "calls artifact_prepare, no capabilities block at all: flagged",
			promptBody: "Call artifact_prepare(kind: \"html\", ...) to render the report.",
			capsYAML:   "",
			want:       "artifacts",
		},
		{
			name:       "calls artifact_prepare, capabilities present but missing artifacts: flagged",
			promptBody: "Call artifact_prepare(kind: \"html\", ...) to render the report.",
			capsYAML:   "    channel_history: {}\n",
			want:       "artifacts",
		},
		{
			name:       "calls artifact_prepare, artifacts capability granted: clean",
			promptBody: "Call artifact_prepare(kind: \"html\", ...) to render the report.",
			capsYAML:   "    artifacts: {}\n",
		},
		{
			// The regression case: the capability KEY is present, but runtime
			// activation (agentcaps.Active) is false because enabled=false. A
			// key-presence check lints this clean; the rule must match runtime
			// and still flag it.
			name:       "calls artifact_prepare, artifacts capability present but enabled:false: flagged",
			promptBody: "Call artifact_prepare(kind: \"html\", ...) to render the report.",
			capsYAML:   "    artifacts: {enabled: false}\n",
			want:       "artifacts",
		},
		{
			name:       "never mentions artifact_prepare, no capabilities: clean — nothing to flag",
			promptBody: "You are a helpful assistant. Answer questions about the repo.",
			capsYAML:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{Manifests: artifactsFixtureManifests(tc.promptBody, tc.capsYAML)}
			findings, err := LintArtifactsCapability(b)
			require.NoError(t, err, "LintArtifactsCapability")

			if tc.want == "" {
				assert.Empty(t, findings, "findings: %v", findings)
				return
			}
			assert.Equal(t, 1, countContainingRequirement(findings, tc.want),
				"want exactly one finding containing %q; got: %v", tc.want, findings)
		})
	}
}

// TestLintArtifactsCapabilityConfigMapRef exercises the ConfigMapRef half of
// prompt resolution: the prompt text lives in a bundled ConfigMap, not
// inline, and the rule must still find it there.
func TestLintArtifactsCapabilityConfigMapRef(t *testing.T) {
	manifests := []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-prompt-cm
data:
  prompt.txt: "Call artifact_prepare(kind: \"html\") when the review is ready."
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-console
spec:
  description: "Fixture AgentClass whose prompt is sourced from a ConfigMap."
  systemPrompt:
    configMapRef:
      name: demo-prompt-cm
      key: prompt.txt
`)
	b := &Bundle{Manifests: manifests}
	findings, err := LintArtifactsCapability(b)
	require.NoError(t, err)
	assert.Equal(t, 1, countContainingRequirement(findings, "artifacts"), "findings: %v", findings)
}

// TestLintArtifactsCapabilityUnresolvableConfigMapRef pins that a
// configMapRef pointing at a ConfigMap the bundle does NOT carry makes no
// claim either way (the prompt text isn't visible, so absence of a finding
// here is "cannot tell", not "verified clean") — it must not panic or error.
func TestLintArtifactsCapabilityUnresolvableConfigMapRef(t *testing.T) {
	manifests := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-console
spec:
  description: "Fixture AgentClass whose prompt ConfigMap is not bundled."
  systemPrompt:
    configMapRef:
      name: not-bundled
      key: prompt.txt
`)
	b := &Bundle{Manifests: manifests}
	findings, err := LintArtifactsCapability(b)
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestLintArtifactsCapabilityNilBundle(t *testing.T) {
	findings, err := LintArtifactsCapability(nil)
	require.Error(t, err)
	assert.Nil(t, findings)
}

// TestRequirementFindingString pins Finding.String()'s exact rendering, since
// `oap agent lint` prints it verbatim.
func TestRequirementFindingString(t *testing.T) {
	cases := []struct {
		name    string
		finding RequirementFinding
		want    string
	}{
		{
			name:    "an AgentClass's finding: the AgentClass/ prefix",
			finding: RequirementFinding{AgentClass: "demo-console", Path: "spec.skills[0]", Reason: "something is wrong"},
			want:    "AgentClass/demo-console spec.skills[0]: something is wrong",
		},
		{
			name:    "another CR's finding: Resource replaces the prefix entirely",
			finding: RequirementFinding{Resource: "AgentIdentity/demo-agent-id", Path: "spec.credentials[0]", Reason: "something is wrong"},
			want:    "AgentIdentity/demo-agent-id spec.credentials[0]: something is wrong",
		},
		{
			name:    "the manifest's own finding: no CR named at all",
			finding: RequirementFinding{Resource: "oap.yaml", Path: "requires.channels[0].kind", Reason: "something is wrong"},
			want:    "oap.yaml requires.channels[0].kind: something is wrong",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.finding.String())
		})
	}
}

// countContainingRequirement mirrors countContaining (agentui_lint_test.go)
// for RequirementFinding — kept separate rather than made generic, since the
// two Finding shapes are deliberately distinct types (see RequirementFinding's
// doc comment).
func countContainingRequirement(findings []RequirementFinding, substr string) int {
	n := 0
	for _, f := range findings {
		if strings.Contains(f.String(), substr) {
			n++
		}
	}
	return n
}
