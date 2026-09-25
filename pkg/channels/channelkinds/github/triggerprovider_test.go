package github

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/checkruns"
)

// TestTriggerProviderStateIn_ReadsBackWhatRefWrote is the round trip that makes
// the extractor trustworthy.
//
// ref and checkRunIDPattern are ONE fact spelled twice — a formatter and its
// inverse — and the failure mode of a pair like that is silent: reword ref and
// the pattern quietly matches nothing, the capture records no minted id, and
// the replay's stand-in hands back an id of its own. Nothing would report that;
// the bundle would simply diverge on a step nobody could explain. So the test
// drives the REAL formatter rather than a copy of its output.
func TestTriggerProviderStateIn_ReadsBackWhatRefWrote(t *testing.T) {
	s := &checkRunSurface{
		repo:   checkruns.Repo{Owner: "demo-org", Name: "platform"},
		number: 42,
		name:   "demo-reviewbot",
	}

	for _, id := range []int64{1, 7, 99044729080} {
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			got := Kind{}.TriggerProviderStateIn(s.ref(id))
			assert.Equal(t, []string{strconv.FormatInt(id, 10)}, got.MintedIDs,
				"ref rendered an id this pattern cannot read back; the pair has drifted")
			assert.Empty(t, got.SurfaceRevision, "a ref names no commit")
		})
	}
}

// TestTriggerProviderStateIn_ReadsBackWhatRenderPromptWrote is the same round
// trip for the other half, and it matters more: the head commit is what the
// stand-in provider reports as the pull request's current head, and without one
// the surface refuses outright with "response carried no head commit".
func TestTriggerProviderStateIn_ReadsBackWhatRenderPromptWrote(t *testing.T) {
	const sha = "ba03f5969a9e29334d669472f4346f29ea7247fe"
	var ev prEvent
	ev.Repository.FullName = "demo-org/platform"
	ev.Number = 42
	ev.Action = "opened"
	ev.PullRequest.Head.SHA = sha
	ev.PullRequest.Base.SHA = "2321b6df5a31724b7e516dd3398a3cbe39f74e18"
	ev.PullRequest.Title = "Add rate limiting"
	ev.PullRequest.User.Login = "demo-user"

	prompt := renderPrompt(ev)
	require.Contains(t, prompt, sha, "the fixture must actually put the sha in the prompt")

	got := Kind{}.TriggerProviderStateIn(prompt)
	assert.Equal(t, sha, got.SurfaceRevision,
		"renderPrompt wrote a head commit this pattern cannot read back; the pair has drifted")
	assert.Empty(t, got.MintedIDs,
		"the BASE commit is on the line below and is not a mint; nor is the pull request number")
}

// TestTriggerProviderStateIn_IgnoresNumbersItDidNotWrite is the negative half,
// and it is the one that protects the SEQUENCE.
//
// A recorded id that never happened is worse than a missed one: the stand-in
// hands them back in order, so one spurious entry shifts every later id by a
// position and the run addresses an object the recorded one did not.
func TestTriggerProviderStateIn_IgnoresNumbersItDidNotWrite(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "a pull request number alone", text: "demo-org/platform#4321 was opened."},
		{name: "prose about a check run with no id", text: "the GitHub check run on demo-org/platform#42"},
		{name: "a sandbox tool printing a diff stat", text: "3 files changed, 128 insertions(+), 4 deletions(-)"},
		{name: "an id-shaped run with the wrong preamble", text: "workflow run 99044729080 on demo-org/platform#42"},
		{name: "an id-shaped run with the wrong postamble", text: " check run 99044729080 for demo-org/platform#42"},
		{name: "a head commit label carrying a word", text: "Head commit: unknown\n"},
		{name: "a head commit label carrying a short sha", text: "Head commit: ba03f59\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Kind{}.TriggerProviderStateIn(tc.text)
			assert.Empty(t, got.MintedIDs, "nothing here was minted by the provider")
			assert.Empty(t, got.SurfaceRevision, "nothing here is a resolved head commit")
		})
	}
}

// TestTriggerProviderStateIn_KeepsMintOrder covers the contract the sequence
// rests on: a stand-in draws these in order, so the order they are read in is
// the order they were minted in.
func TestTriggerProviderStateIn_KeepsMintOrder(t *testing.T) {
	s := &checkRunSurface{repo: checkruns.Repo{Owner: "demo-org", Name: "platform"}, number: 42, name: "demo-reviewbot"}
	text := strings.Join([]string{s.ref(11), "…later…", s.ref(22), s.ref(33)}, "\n")

	got := Kind{}.TriggerProviderStateIn(text)
	assert.Equal(t, []string{"11", "22", "33"}, got.MintedIDs)
}

// TestTriggerProviderState_MergeDedupsAndKeepsFirstRevision pins the fold the
// capture applies across a whole transcript: the same ref is echoed by a
// notice, a log line and a summary, and counting those as three mints would
// exhaust the stand-in's sequence two ids early.
func TestTriggerProviderState_MergeDedupsAndKeepsFirstRevision(t *testing.T) {
	s := &checkRunSurface{repo: checkruns.Repo{Owner: "demo-org", Name: "platform"}, number: 42, name: "demo-reviewbot"}
	k := Kind{}

	got := k.TriggerProviderStateIn("Head commit: " + strings.Repeat("a", 40) + "\n").
		Merge(k.TriggerProviderStateIn(s.ref(11))).
		Merge(k.TriggerProviderStateIn("echoing " + s.ref(11) + " back")).
		Merge(k.TriggerProviderStateIn("Head commit: " + strings.Repeat("b", 40) + "\n")).
		Merge(k.TriggerProviderStateIn(s.ref(22)))

	assert.Equal(t, []string{"11", "22"}, got.MintedIDs, "the echo is not a second mint")
	assert.Equal(t, strings.Repeat("a", 40), got.SurfaceRevision,
		"the FIRST revision is the one the surface resolved before any work happened")
}
