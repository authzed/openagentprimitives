package github

import (
	"fmt"
	"strconv"
	"strings"
)

// channelKeyPrefix marks a binding key as naming a pull request. Held as a
// constant because both the writer (Translate) and the reader
// (parseChannelKey) derive from it: a renamed prefix that only one side knew
// about would leave every existing session's trigger unaddressable, silently.
const channelKeyPrefix = "pr:"

// formatChannelKey renders the ChannelKey for one pull request. It is the ONLY
// place this shape is written.
//
// The key is not merely a correlation string: it is the durable record of WHICH
// pull request a session is about, and the trigger-status surface
// (triggerstatus.go) addresses github's API with what it parses back out. That
// makes the format a two-sided contract inside this package, which is why the
// writer and the reader sit in one file with one round-trip test over them.
func formatChannelKey(repoFullName string, number int) string {
	return fmt.Sprintf("%s%s#%d", channelKeyPrefix, repoFullName, number)
}

// formatPRSubjectID renders the factcontent.Subject.ResourceID for a
// "github_pr" subject. TriggerFacts is the only writer today; kept beside
// formatChannelKey because a later reader (a gate looking a fact back up)
// must reconstruct this EXACT shape, the same two-sided-contract reasoning
// that keeps formatChannelKey and parseChannelKey in this one file.
//
// repoFullName is the pull request's BASE repository, never its head. The two
// differ only for a fork PR — which is the exact case the fork gate exists
// for — and every other reader of `owner/repo#n` in this package means the
// base repo. An `observes` block deriving the same id from a tool result must
// therefore read the base repository too; that agreement is not enforceable by
// any type, so TestEnvelopeAndObservedPathsKeyTheSamePullRequestIdentically
// pins it directly.
//
// The id is RAW, pre-transform: no slot's ValueTransforms have run on it. See
// pkg/memory/kinds/factcontent's package doc for what that obliges a reader
// to do.
//
// Deliberately NOT channelKeyPrefix-prefixed: a Subject id is not a
// ChannelKey, and the two must stay visually distinct so a value from one
// namespace is never mistaken for, or looked up through, the other.
func formatPRSubjectID(repoFullName string, number int) string {
	return fmt.Sprintf("%s#%d", repoFullName, number)
}

// parseChannelKey recovers the repository and pull request number from a
// binding key formatChannelKey produced.
//
// Strict on purpose. A key this cannot parse is not a pull request — a session
// bound to some other kind's key, or a hand-edited CR — and guessing at it
// would address a repository nobody named. Callers surface the refusal; none of
// them may proceed without both halves.
func parseChannelKey(key string) (owner, repo string, number int, err error) {
	rest, ok := strings.CutPrefix(key, channelKeyPrefix)
	if !ok {
		return "", "", 0, fmt.Errorf("channel key %q does not name a pull request", key)
	}
	fullName, num, ok := strings.Cut(rest, "#")
	if !ok {
		return "", "", 0, fmt.Errorf("channel key %q does not name a pull request number", key)
	}
	owner, repo, ok = strings.Cut(fullName, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", 0, fmt.Errorf("channel key %q does not name an owner/repository", key)
	}
	number, err = strconv.Atoi(num)
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("channel key %q does not name a positive pull request number", key)
	}
	return owner, repo, number, nil
}
