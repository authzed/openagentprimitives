package agentcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// writeLintFixtureFolder writes a minimal folder-source .oap bundle whose
// single AgentClass's spec.skills is exactly skills — a raw YAML list body,
// spliced in verbatim, so the caller can supply either the pre-migration
// bare-string shape or the current {name, ref, target} object shape.
// Fabricated names throughout (demo-org/demo-reviewbot); none shared with
// any bundled example.
func writeLintFixtureFolder(t *testing.T, skills string) string {
	t.Helper()
	dir := t.TempDir()
	// agent.name is omitted: it is inherited from the bundled AgentClass below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	manifest := "apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
		"kind: AgentClass\n" +
		"metadata:\n" +
		"  name: demo-reviewbot-class\n" +
		"spec:\n" +
		"  description: \"Fixture AgentClass for the lint-ordering test.\"\n" +
		"  systemPrompt:\n" +
		"    inline: \"You are a fixture agent used only by this test.\"\n" +
		"  skills:\n" + skills + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(manifest), 0o644))
	return dir
}

// runLint executes `oap agent lint <dir>` — the real, fully-wired command,
// exactly as an operator invokes it — and returns everything it printed to
// stdout. The command's own error return is deliberately ignored: a
// non-nil error IS the expected outcome for an old-shape fixture, and this
// helper's callers care about what got printed before the command gave up,
// not its exit status.
func runLint(t *testing.T, dir string) string {
	t.Helper()
	out, _ := runLintErr(t, dir)
	return out
}

// runLintErr is runLint for a caller that also cares whether the command
// failed — a finding an author must fix is reported by a non-nil error as
// well as by a printed line, and a check that printed but did not fail would
// leave a broken bundle installable.
func runLintErr(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := newAgentLintCmd(&apcmd.Globals{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{dir})
	err := cmd.Execute()
	return out.String(), err
}

// TestAgentLint_OldSkillsShapeLeadsWithTheRewrite pins the ordering
// invariant documented in newAgentLintCmd's RunE (lint.go): LintSkillsShape
// must run, and short-circuit, before LintAgentUIs, LintSkillPinning, and
// LintArtifactsCapability — all three decode the bundled AgentClass strictly
// into the typed AgentSkill shape and, against a pre-migration bare-string
// skills entry, fail with a raw "cannot unmarshal string into Go struct
// field" error that names no fix. That ordering bug is not hypothetical: it
// is exactly what happened when LintSkillsShape was first wired in after
// LintAgentUIs instead of before it — `oap agent lint` reported only the
// unmarshal error and never reached the rewrite at all.
//
// This test does not pin the literal call sequence in lint.go — a
// legitimate future reordering that preserves the observable behaviour
// stays green. It pins the OUTCOME: running the real, fully-wired `oap
// agent lint` command over an old-shape bundle must print the
// {name, ref, target} rewrite, and must never print the raw decode error.
func TestAgentLint_OldSkillsShapeLeadsWithTheRewrite(t *testing.T) {
	dir := writeLintFixtureFolder(t, "    - github.com/demo-org/demo-skills//skills/code-review@v1.0.0")
	out := runLint(t, dir)

	assert.Contains(t, out, "name: code-review",
		"the rewrite must be shown, not merely a problem reported")
	assert.Contains(t, out, "ref: github.com/demo-org/demo-skills//skills/code-review@v1.0.0")
	assert.NotContains(t, out, "cannot unmarshal",
		"the raw decode error must never reach the operator ahead of (or instead of) the rewrite")
}

// writeChannelLintFixtureFolder writes a folder-source bundle that declares
// the required channels and carries an AgentIdentity reading credsSecret — the
// pairing `oap agent lint` now has to keep honest. Fabricated names
// throughout; nothing here reads an example bundle.
//
// The role=output declaration beside the input one is not decoration: B-R14
// refuses an input channel with no delivery target, so a single-input fixture
// would raise a finding of its own and the credentials assertions below would
// stop being about the credentials.
func writeChannelLintFixtureFolder(t *testing.T, channelName, credsSecret string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"+
			"requires:\n  channels:\n    - kind: github\n      role: input\n      name: "+channelName+"\n"+
			"    - kind: fake\n      role: output\n      name: demo-agent-out\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	manifest := "apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
		"kind: AgentIdentity\n" +
		"metadata:\n" +
		"  name: demo-agent-id\n" +
		"spec:\n" +
		"  credentials:\n" +
		"    - name: github-app\n" +
		"      type: githubApp\n" +
		"      githubApp:\n" +
		"        secretRef:\n" +
		"          name: " + credsSecret + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "identity.yaml"), []byte(manifest), 0o644))
	// A .oap carries exactly one AgentClass (Bundle.Validate enforces it); this
	// one names the identity above so the graph is coherent.
	agentClass := "apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
		"kind: AgentClass\n" +
		"metadata:\n" +
		"  name: demo-reviewbot\n" +
		"spec:\n" +
		"  agentIdentity: demo-agent-id\n" +
		"  systemPrompt:\n    inline: hi\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(agentClass), 0o644))
	return dir
}

// TestAgentLint_DanglingChannelCredsReferenceFailsTheCommand proves the check
// is WIRED, not merely written: an AgentIdentity reading a Secret no declared
// channel produces has to reach the operator's terminal and fail the command.
func TestAgentLint_DanglingChannelCredsReferenceFailsTheCommand(t *testing.T) {
	dir := writeChannelLintFixtureFolder(t, "demo-agent-gh", "demo-agent-github-creds")
	out, err := runLintErr(t, dir)

	require.Error(t, err, "a dangling credentials reference must fail the command, not merely print")
	assert.Contains(t, out, "AgentIdentity/demo-agent-id spec.credentials[0].githubApp.secretRef.name:",
		"the finding must render against the CR that holds the reference")
	assert.Contains(t, out, `"demo-agent-github-creds"`, "the dangling reference")
	// The closing quote is load-bearing: "demo-agent-gh" is a prefix of
	// "demo-agent-github-creds", so a bare substring check would pass on the
	// dangling name alone.
	assert.Contains(t, out, `"demo-agent-gh"`, "and the declared channel name")
	assert.NotContains(t, out, "no required-channel findings")
}

// The negative control for the test above, and for the wiring's clean-run
// line: the same bundle with the two names agreeing must lint clean.
func TestAgentLint_MatchingChannelCredsReferenceIsClean(t *testing.T) {
	dir := writeChannelLintFixtureFolder(t, "demo-agent-gh", "demo-agent-gh-creds")
	out, err := runLintErr(t, dir)

	require.NoError(t, err)
	assert.Contains(t, out, "no required-channel findings")
}

// TestAgentLint_AnInputChannelWithNoDeliveryTargetFailsTheCommand proves
// B-R14 is wired into the command an AUTHOR runs, which is the moment it is
// cheapest to act on: the alternative is a clean install whose input Channel
// reports OutputBindingUnresolvable and whose agent never delivers anything.
//
// The `both` variant is the one worth a case of its own. "both" reads like
// "does everything", so it is the spelling an author reaches for — and
// outputbind refuses it deliberately, which means the bundle installs and
// fails exactly as if no delivery channel had been declared at all.
func TestAgentLint_AnInputChannelWithNoDeliveryTargetFailsTheCommand(t *testing.T) {
	for _, tc := range []struct{ name, deliveryRole string }{
		{name: "no second channel at all", deliveryRole: ""},
		{name: "a role=both sibling, which is not a delivery target", deliveryRole: "both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			manifest := "oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n" +
				"requires:\n  channels:\n    - kind: github\n      role: input\n      name: demo-agent-gh\n"
			if tc.deliveryRole != "" {
				manifest += "    - kind: slack\n      role: " + tc.deliveryRole + "\n      name: demo-agent-chat\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(manifest), 0o644))
			writeMinimalAgentClass(t, dir)

			out, err := runLintErr(t, dir)

			require.Error(t, err, "an agent with nowhere to deliver must fail the command, not merely print")
			assert.Contains(t, out, "oap.yaml requires.channels[0].role:",
				"the finding must point at the input declaration that has nowhere to deliver")
			assert.Contains(t, out, "nowhere to deliver it")
			assert.Contains(t, out, "OutputBindingUnresolvable",
				"and name the condition the operator would otherwise have to find on the Channel itself")
			assert.NotContains(t, out, "no required-channel findings")
		})
	}
}

// TestAgentLint_AnInputWithARealDeliveryTargetIsClean is the negative control
// for the two cases above: the shape B-R14 blesses — an input and a role=output
// beside it, which is reviewbot's own — must stay clean, or the rule would be
// refusing every channel-declaring bundle rather than the broken ones.
func TestAgentLint_AnInputWithARealDeliveryTargetIsClean(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(
		"oapFormatVersion: \"1\"\nagent:\n  version: \"1.0.0\"\n"+
			"requires:\n  channels:\n    - kind: github\n      role: input\n      name: demo-agent-gh\n"+
			"    - kind: slack\n      role: output\n      name: demo-agent-chat\n"), 0o644))
	writeMinimalAgentClass(t, dir)

	out, err := runLintErr(t, dir)

	require.NoError(t, err)
	assert.Contains(t, out, "no required-channel findings")
}

// writeMinimalAgentClass drops a single, dependency-free AgentClass into
// <dir>/manifests so the fixture satisfies Bundle.Validate's one-class rule
// without introducing any other lint finding.
func writeMinimalAgentClass(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(
		"apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-reviewbot\nspec:\n  systemPrompt:\n    inline: hi\n"), 0o644))
}

// TestAgentLint_NewSkillsShapeIsClean is the negative control for the
// invariant above: the current {name, ref, target} shape must lint clean.
// Without this, a guard that fired on every bundle — old shape or new —
// would make the positive test above pass for the wrong reason.
func TestAgentLint_NewSkillsShapeIsClean(t *testing.T) {
	dir := writeLintFixtureFolder(t,
		"    - name: code-review\n      ref: \"github.com/demo-org/demo-skills//skills/code-review@v1.0.0\"\n      target: sandbox")
	out := runLint(t, dir)

	assert.Contains(t, out, "no skills-shape findings")
	assert.NotContains(t, out, "cannot unmarshal")
}
