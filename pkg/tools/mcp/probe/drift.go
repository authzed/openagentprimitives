package probe

import (
	"fmt"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/x/stringsx"
)

// maxServedNamesInDrift bounds how many of a server's tool names a drift
// message renders. The message lands on an AgentSession status condition and in
// an append-only log entry, so it cannot be a dump of a server with hundreds of
// tools; forty sorted names is enough for a reader to spot the one that was
// renamed, which is what the list is for.
const maxServedNamesInDrift = 40

// maxDriftNameRunes bounds a single tool name rendered into a drift message.
// A served name is the connector's own untrusted upstream input, so one
// pathological name must not be able to dominate the message on its own —
// this holds even before the whole-message cap below is applied.
const maxDriftNameRunes = 120

// maxDriftMessageRunes bounds the WHOLE rendered message. It lands on an
// AgentSession Failed condition — whose CRD field has maxLength 32768, but a
// status patch that exceeds it fails the write and crash-loops the runner
// into RunnerCrash rather than truncating gracefully — and it is appended to
// the session's append-only signed lifecycle log, which cannot delete an
// entry once written. Both writers need the cap enforced HERE, before either
// one ever sees the text.
const maxDriftMessageRunes = 2000

// AllowlistDrift compares the tool names a spec PINS against the ones a server
// actually serves, and renders the finding.
//
// missing is every pinned name the server does not serve, sorted and
// deduplicated; it is nil when the pin holds. message is empty in that case,
// and otherwise names BOTH halves of the finding: the pinned names that are
// absent, and what the server does offer instead.
//
// Naming what IS served is the whole point of this function. "pinned tools not
// served: [get_pull_request]" tells a reader that something is wrong and
// nothing about what to do; the same line with the server's own list beside it
// usually shows the tool under its new name, which turns a dead end into a
// one-line spec edit. A server that serves NOTHING is called out in words
// rather than as an empty list — it usually means the credential was refused,
// not that the pin is stale, and those want different fixes.
func AllowlistDrift(ref string, pinned []string, live []Tool) (missing []string, message string) {
	servedByName := make(map[string]bool, len(live))
	servedNames := make([]string, 0, len(live))
	for _, t := range live {
		if servedByName[t.Name] {
			continue
		}
		servedByName[t.Name] = true
		servedNames = append(servedNames, t.Name)
	}

	seen := make(map[string]bool, len(pinned))
	for _, name := range pinned {
		if servedByName[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil, ""
	}
	slices.Sort(missing)
	slices.Sort(servedNames)

	var offers string
	switch {
	case len(servedNames) == 0:
		offers = "the server offers no tools at all, which usually means its credential was refused"
	case len(servedNames) > maxServedNamesInDrift:
		offers = fmt.Sprintf("the server offers %d, first %d: [%s]",
			len(servedNames), maxServedNamesInDrift,
			strings.Join(cappedNames(servedNames[:maxServedNamesInDrift]), ", "))
	default:
		offers = fmt.Sprintf("the server offers: [%s]", strings.Join(cappedNames(servedNames), ", "))
	}
	msg := fmt.Sprintf("MCPServer/%s: pinned tools not served: [%s]; %s",
		ref, strings.Join(cappedNames(missing), ", "), offers)
	return missing, stringsx.CapRunes(msg, maxDriftMessageRunes)
}

// cappedNames rune-caps each name before it is joined into a rendered
// message. names is returned unmutated by AllowlistDrift's own return value
// (missing) — only the message text goes through this.
func cappedNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = stringsx.CapRunes(n, maxDriftNameRunes)
	}
	return out
}
