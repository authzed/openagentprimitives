package projectors

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// This file is the RESOURCE-side counterpart to directoryidentities.go. That
// file answers "what upstream objects has a directory sync linked to THIS
// person" from the SUBJECT side, which structurally cannot see a membership
// tuple whose subject is a userset — github_org:acme#member@github_user:
// 123#sole_user is invisible to a user-subject filter, so an org or channel
// membership never shows up there even though a sync wrote it. This file
// answers the complementary — and, for monitoring a sync, more useful —
// question from the RESOURCE side: what did this sync actually write into
// SpiceDB, read back by the #relhash sentinel every synced scope carries. See
// spicedb.ListSourceScopes for the mechanism and its own limits.

// directoryScopesCapPerDefinition bounds how many scope ids
// directoryDetailProjector asks the reader for, per scope-bearing
// definition. ResourceDetail has no pagination, and a large org's repo list
// is otherwise unbounded; SourceScope.Total still carries the full count
// observed, so the section can say "showing N of M" rather than silently
// truncating.
const directoryScopesCapPerDefinition = 100

// directoryScopesSection renders one RelationshipSource's synced scopes.
//
// Same three-answer shape directoryIdentitiesSection uses, and for the same
// reason:
//
//   - a failed read is text ("unavailable: <reason>"), which appendSection
//     keeps. Presenting it as an empty list would be the confident claim
//     "this sync wrote nothing", the silent-error failure mode this repo has
//     already paid for.
//   - a successful empty read (no scope-bearing definition has synced
//     anything yet) is a zero Section, which appendSection drops.
//   - a PARTIAL read — some definitions failed, the rest returned rows — is a
//     list carrying BOTH the rows and a notice naming what is missing.
//
// A fourth degradation rides on the third: the rows are all there, but the
// names for some of them could not be resolved (SourceScopes.LabelsUnavailable).
// That is not an incomplete list and must not be worded as one — but it may
// not be silent either, because a row showing a raw id looks identical whether
// nothing stored a name for it or the read that would have found one failed.
func directoryScopesSection(scopes spicedb.SourceScopes, err error) config.Section {
	if err != nil {
		return textSection("directoryscopes", "Scopes", "unavailable: "+err.Error())
	}
	var items []config.ListItem
	for _, s := range scopes.Scopes {
		for _, id := range s.ScopeIDs {
			raw := s.Definition + ":" + id
			// Subtitle names the SOURCE, not the definition again — an
			// unlabelled Title already carries the definition, so repeating it
			// there would be the third copy of the same string (see the badge
			// below). Mirrors directoryIdentitiesSection's "asserted by
			// <Source>" rows on the subject side.
			item := config.ListItem{
				Title:    raw,
				Subtitle: "synced by " + s.Source,
				Badges:   []config.Badge{{Key: "source", Value: s.Source}},
			}
			// A resolved name becomes the Title and pushes the raw id down into
			// the subtitle — DOWN, never out. The name is what a human came to
			// read; the `definition:id` is what an operator needs in hand to go
			// query SpiceDB for the same row, and dropping it would trade one
			// audience's problem for the other's.
			if lbl, ok := s.Labels[id]; ok {
				item.Title = lbl.Title
				item.Subtitle = raw + " · synced by " + s.Source
				item.Href = lbl.Href
			}
			items = append(items, item)
		}
		if s.Total > len(s.ScopeIDs) {
			// The cap truncated this definition's list. A synthetic row rather
			// than a per-row badge: repeating "N of M" on every one of the
			// (already capped) rows would say the same thing len(ScopeIDs)
			// times and say it about the wrong row each time — this one row
			// names the gap once, against the definition it belongs to.
			items = append(items, config.ListItem{
				Title:    fmt.Sprintf("+ %d more %s", s.Total-len(s.ScopeIDs), s.Definition),
				Subtitle: fmt.Sprintf("showing %d of %d", len(s.ScopeIDs), s.Total),
				Badges:   []config.Badge{{Key: "source", Value: s.Source}},
			})
		}
	}
	sec := listSection("directoryscopes", "Scopes", items)
	sec.Text = joinNotices(unavailableNotice(scopes.Unavailable), labelsUnavailableNotice(scopes.LabelsUnavailable))
	return sec
}

// labelsUnavailableNotice says that the rows are complete but some of them
// are unnamed because the read that resolves names failed.
//
// Deliberately NOT worded as unavailableNotice is. "This list is INCOMPLETE"
// would be false here — every scope the sync wrote is on the page — and a
// false alarm about missing data is its own defect. What it must convey is
// the thing an operator cannot see for themselves: a raw id on this page
// means either "nothing stores a name for this" or "the name exists and could
// not be read", and only this sentence tells them which.
func labelsUnavailableNotice(unavailable []spicedb.UnavailableProbe) string {
	if len(unavailable) == 0 {
		return ""
	}
	parts := make([]string, 0, len(unavailable))
	for _, u := range unavailable {
		parts = append(parts, u.Source+" ("+u.Definition+"#"+u.Relation+"): "+u.Err)
	}
	return "Scope names could not be resolved — rows below show raw ids: " + strings.Join(parts, "; ")
}

// joinNotices runs the non-empty notices together into the one string a list
// Section carries. Space-joined rather than newline-joined because the
// frontend renders the notice as flowing alert text, where a newline collapses
// to nothing and would silently run two sentences together.
func joinNotices(notices ...string) string {
	kept := make([]string, 0, len(notices))
	for _, n := range notices {
		if n != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, " ")
}

// SourceScopeReaderFunc reads what one RelationshipSource's directory sync
// last wrote into SpiceDB, as directoryDetailProjector.Detail needs it.
//
// kind is the RelationshipSource's spec.kind ("github", "slack",
// "onepassword") verbatim — the resolution from that string to the
// relsync.Kind (and the relsource.Source ListSourceScopes actually reads)
// happens in admind.New's adapter, which is where the import of
// pkg/platform/relsync lives; taking a plain string here is what keeps this
// package free of it, mirroring SubjectIdentityReaderFunc's own split for the
// subject side.
type SourceScopeReaderFunc func(ctx context.Context, kind string, capPerDefinition int) (spicedb.SourceScopes, error)

// sourceScopeReader is the package-level injection point
// directoryDetailProjector.Detail consults. The zero value (nil) is what
// every test binary that never calls SetSourceScopeReader sees, and nil
// leaves the Scopes section off the page entirely — the panel is additive,
// so a build without a reader shows exactly what it showed before.
var sourceScopeReader SourceScopeReaderFunc

// SetSourceScopeReader installs the reader the directory detail projector
// consults. Called once by admind.New. Nil leaves the Scopes section off the
// page entirely — the panel is additive, and a build without a reader shows
// exactly what it showed before.
func SetSourceScopeReader(r SourceScopeReaderFunc) { sourceScopeReader = r }
