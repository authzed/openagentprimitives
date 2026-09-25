package toolkits_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The reviewbot writesRelationships blocks (examples/reviewbot/manifests/
// toolspecs.yaml, test/e2e/testdata/agent-pr-identity-e2e/02-toolspec.yaml)
// read `gh pr view`'s stdout on the assumption that it is the forge's own
// JSON response, unmodified. Both toolspecs' no-jq/no-template constraint
// (pinned as ghPRViewNoJqCEL in pkg/tools/toolspec/reviewbot_pr_relwrites_test.go)
// denies the two flags THIS toolkit revision is known to make stdout-shaping
// — --jq (declared on `pr view` today) and --template (not declared, refused
// ahead of need). That constraint is only as good as this list staying
// exhaustive: a third stdout-shaping flag declared on `pr view` and not
// added to the constraint would silently let a model reshape the tuple the
// blocks resolve from result.stdoutJSON.
//
// This test pins the CURRENT flag set exactly, so a future edit to `pr
// view`'s declared flags in this file must redden it — forcing whoever adds
// a flag to ask "does this shape stdout?" rather than letting the toolkit
// definition drift silently past the constraint that assumes it hasn't.
func TestGhToolkit_PrViewDeclaresOnlyItsCurrentFlags(t *testing.T) {
	sc := findSubcommand(t, findToolkit(t, "gh"), []string{"pr", "view"})

	got := make(map[string]bool, len(sc.Flags))
	for _, f := range sc.Flags {
		got[f.Long] = true
	}

	// The set `gh pr view` declares as of this toolkit's pinned revision.
	// --jq is the one stdout-shaping flag it already has; --template is
	// deliberately absent — see the no-jq/no-template constraint's own
	// comment for why it is refused anyway, ahead of need.
	want := map[string]bool{
		"repo":     true,
		"json":     true,
		"jq":       true,
		"comments": true,
		"web":      true,
	}
	assert.Equal(t, want, got,
		"`gh pr view`'s declared flag set changed — if this added an output-shaping "+
			"flag (anything that can replace or filter stdout, the way --jq and --template "+
			"do), the reviewbot writesRelationships constraint denying --jq/--template must "+
			"widen to deny it too, in BOTH the shipped toolspec and the e2e fixture, or a "+
			"model can reshape the tuple those blocks resolve from result.stdoutJSON")
}
