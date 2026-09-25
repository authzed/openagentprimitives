package github

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dustin/go-humanize/english"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Compile-time check: *Kind satisfies the optional trigger-describer interface.
var _ channelkinds.TriggerDescriber = (*Kind)(nil)

// DescribeTrigger names the pull request a session was started by, for the
// opening line of the thread its reply lands in.
//
// A webhook delivery carries no message from a person, so the outbound thread
// would otherwise be rooted by whatever the agent emits first. This is the one
// line that says which pull request that thread is about, and this kind is
// the party that can say it: the pull request is recorded in the binding key
// this kind wrote (channelkey.go), and nothing outside this package may read
// that shape.
//
// WHICH pull request comes from the key alone; WHAT it is comes from the
// delivery. The key is the durable record binding the session to a pull
// request, so identity — repository, number, URL — derives only from it, and
// a key this kind cannot parse is refused (ok=false) whatever the body says.
// The delivery, when it is in hand and names the same pull request, adds the
// things a reader would otherwise have to click through for: the title, the
// author, what happened, and how big the change is. Every failure to read it
// — no body (a replayed or non-webhook inbound), an undecodable one, another
// event type, another pull request — degrades to the key-only line rather
// than refusing: yesterday the key was the whole answer, and a working
// opening beats a withheld one.
//
// The Channel is unused: everything the line names is in the key and the body.
func (k *Kind) DescribeTrigger(_ *spiceboxv1alpha1.Channel, channelKey, event string, body []byte) (string, bool) {
	owner, repo, number, err := parseChannelKey(channelKey)
	if err != nil {
		// Not a key this kind wrote, so it names no pull request. Refused
		// rather than guessed at: the caller posts nothing and the thread roots
		// on the agent's first output, which is strictly better than opening it
		// by announcing a repository nobody named. The refusal is not silent —
		// it is this function's documented ok=false answer, and every caller
		// acts on it.
		return "", false
	}
	segments := []string{fmt.Sprintf("Picked up pull request %s/%s#%d", owner, repo, number)}
	meta, facts := describeDelivery(channelKey, event, body)
	if meta != "" {
		segments = append(segments, meta)
	}
	if facts != "" {
		segments = append(segments, facts)
	}
	// The URL is bare on purpose: this text is kind-neutral (any output kind's
	// sender posts it verbatim), and a raw URL is the one link form every
	// surface renders. github.com is literal for the same reason the toolkit's
	// repo canonicalization pins it — this kind speaks to exactly that host.
	segments = append(segments, fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number))
	return strings.Join(segments, " — "), true
}

// describeDelivery renders the two payload-derived segments of the opening
// line: meta is the submitter-authored half (`"<title>" by <login>`, each
// field sanitized to one display-safe line), facts is the structural half
// ("opened, draft, from a fork, 3 changed files") — booleans, enums and
// counts GitHub itself stamped on the signed envelope, kept apart from the
// submitter's text the same way renderPrompt keeps its structural group
// outside the untrusted block.
//
// Both come back empty for a delivery this kind cannot vouch decorates THIS
// key: absent, undecodable, some other event type, or a pull_request payload
// whose own repo#number renders to a different key. The last check is what
// keeps the key authoritative — a body can add to the line, never re-aim it.
func describeDelivery(channelKey, event string, body []byte) (meta, facts string) {
	if event != "pull_request" || len(body) == 0 {
		return "", ""
	}
	var ev prEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", ""
	}
	if formatChannelKey(ev.Repository.FullName, ev.Number) != channelKey {
		return "", ""
	}

	var metaParts []string
	// TrimSpace after sanitizeUntrusted: a title of pure whitespace (or one
	// the sanitizer folded down to it) must vanish rather than render as a
	// quoted blank.
	if title := strings.TrimSpace(sanitizeUntrusted(ev.PullRequest.Title)); title != "" {
		metaParts = append(metaParts, `"`+title+`"`)
	}
	if login := strings.TrimSpace(sanitizeUntrusted(ev.PullRequest.User.Login)); login != "" {
		metaParts = append(metaParts, "by "+login)
	}

	var factParts []string
	if w := actionWord(ev.Action); w != "" {
		factParts = append(factParts, w)
	}
	if ev.PullRequest.Draft {
		factParts = append(factParts, "draft")
	}
	if ev.PullRequest.Head.Repo.Fork {
		factParts = append(factParts, "from a fork")
	}
	if n := ev.PullRequest.ChangedFiles; n > 0 {
		factParts = append(factParts, english.Plural(n, "changed file", ""))
	}
	return strings.Join(metaParts, " "), strings.Join(factParts, ", ")
}

// actionWord renders a pull_request action for a reader who is not steeped in
// GitHub's event vocabulary. Only the two genuinely jargon-shaped actions are
// translated; the rest ("opened", "reopened", "edited") already read as plain
// English and pass through — including any action a Channel's spec.github.
// events list admits that this table has never heard of, which is structural
// text from the signed envelope, not submitter prose.
func actionWord(action string) string {
	switch action {
	case "synchronize":
		return "updated"
	case "ready_for_review":
		return "marked ready for review"
	default:
		return action
	}
}
