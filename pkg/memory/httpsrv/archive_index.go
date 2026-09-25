package httpsrv

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/dustin/go-humanize"
)

// The readability vocabulary. CLOSED, and the agent is told to rely on it, so
// it is derived in exactly one place (readabilityOf) and rendered in exactly
// one place (renderArchiveIndex).
//
// It exists because a member never gets a manifest line of its own — a support
// bundle of 187 files must not become 187 lines — so the index is the ONLY
// place the agent learns what it can do with each one. A row that leaves that
// implicit invites a fetch of binary noise.
const (
	// readText: an extractor claimed this MIME; the row's handle returns text.
	readText = "text"
	// readRaw: no extractor, but the bytes are already text-shaped, so
	// fetch_artifact on the handle returns something usable.
	readRaw = "raw"
	// readImage: fetch_artifact would return bytes a model cannot use. Pin it
	// with show_attachment instead.
	readImage = "image"
	// readArchive: a nested archive, stored but NOT expanded (nesting depth is
	// 0 by design). Called out so the agent does not read the handle as an
	// invitation.
	readArchive = "archive"
	// readUnreadable: stored, but nothing here turns it into something usable.
	// Named explicitly so the agent does not fetch it and reason from noise.
	readUnreadable = "unreadable"
)

// memberResult is one exploded member as the inbound-asset route records it.
type memberResult struct {
	Name        string `json:"name"`
	MIME        string `json:"mime,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	Ref         string `json:"ref"`
	TextRef     string `json:"textRef,omitempty"`
	Pages       int    `json:"pages,omitempty"`
	Readability string `json:"readability"`
}

// readabilityOf classifies one member. Derived here and nowhere else, so the
// index and any future consumer cannot disagree about what a member is.
//
// explodable outranks everything: a nested archive with extracted text is
// still an archive we did not open, and saying "text" would describe the
// wrong thing.
func readabilityOf(m memberResult, explodable bool) string {
	switch {
	case explodable:
		return readArchive
	case m.TextRef != "":
		return readText
	case strings.HasPrefix(m.MIME, "image/"):
		return readImage
	case strings.HasPrefix(m.MIME, "text/"), strings.HasPrefix(m.MIME, "application/json"):
		return readRaw
	default:
		return readUnreadable
	}
}

// handleFor is the handle a row advertises: the TEXT handle when there is one,
// since that is what the agent should read, and the raw ref otherwise.
func handleFor(m memberResult) string {
	if m.TextRef != "" {
		return m.TextRef
	}
	return m.Ref
}

// indexNameMaxRunes caps how much of a member name a row renders. Names are
// attacker-controlled text landing in the agent's context.
const indexNameMaxRunes = 120

// sanitizeIndexName renders a member name for a row: control characters
// stripped, truncated to a cap, and QUOTED.
//
// Quoting is not cosmetic. Stripping newlines stops a name forging a new ROW,
// but a name full of spaces still forges FIELDS — "a.log  text  mem://x"
// renders a row in which the agent cannot tell which token is the real
// handle. Quotes make the name's extent unambiguous, which is the same
// reason quotedNames exists on the runner's attachment notes.
func sanitizeIndexName(name string) string {
	var b strings.Builder
	count := 0
	truncated := false
	for _, r := range name {
		if count >= indexNameMaxRunes {
			truncated = true
			break
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		count++
	}
	out := b.String()
	if truncated {
		out += "…"
	}
	return strconv.Quote(out)
}

// renderArchiveIndex is the archive's extracted text: a header stating the
// totals, then one row per member giving its name, size, readability and
// handle.
//
// A fixed-column text table rather than JSON, because at 256 entries the
// punctuation is a real share of the tokens and the agent reads this before it
// knows which members matter.
//
// The header is what makes truncation and skips impossible to miss. An agent
// that reads only the first few lines still learns the archive was partial,
// which is the whole reason a byte-bound overrun is never reported as success.
func renderArchiveIndex(archiveName string, members []memberResult, sum ExplodeSummary) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s — %d files", sanitizeIndexName(archiveName), len(members))
	if sum.Truncated {
		reason := sum.TruncatedReason
		if reason == "" {
			reason = "a limit was reached"
		}
		fmt.Fprintf(&b, ", TRUNCATED (%s): this archive was only partly opened", reason)
	}
	if len(sum.Skipped) > 0 {
		reasons := make([]string, 0, len(sum.Skipped))
		for r := range sum.Skipped {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		parts := make([]string, 0, len(reasons))
		for _, r := range reasons {
			parts = append(parts, fmt.Sprintf("%s=%d", r, sum.Skipped[r]))
		}
		// Counts, never names: a skipped member's name is file content.
		fmt.Fprintf(&b, "; skipped %s", strings.Join(parts, " "))
	}
	b.WriteString("\n")

	byClass := map[string]int{}
	for _, m := range members {
		byClass[m.Readability]++
	}
	if len(byClass) > 0 {
		classes := make([]string, 0, len(byClass))
		for c := range byClass {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		parts := make([]string, 0, len(classes))
		for _, c := range classes {
			parts = append(parts, fmt.Sprintf("%s=%d", c, byClass[c]))
		}
		fmt.Fprintf(&b, "by readability: %s\n", strings.Join(parts, " "))
	}
	b.WriteString("\n")

	for _, m := range members {
		size := ""
		if m.SizeBytes > 0 {
			size = humanize.Bytes(uint64(m.SizeBytes))
		}
		note := ""
		if m.Readability == readArchive {
			note = "  (nested archive, not expanded)"
		}
		fmt.Fprintf(&b, "  %s  %s  %s  %s%s\n",
			sanitizeIndexName(m.Name), size, m.Readability, handleFor(m), note)
	}
	return b.String()
}
