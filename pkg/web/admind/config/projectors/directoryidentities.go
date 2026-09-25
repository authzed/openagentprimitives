package projectors

import (
	"context"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// This file is named separately from identities.go (the AgentIdentity CRD
// list Projector) on purpose: this concern is unrelated to that CRD — it
// renders the EXTERNAL identity objects linked to a user, sourced from SpiceDB
// rather than any CRD list.
//
// "Directory" names where most of them come from, not all: the useridentity
// reconciler's own account attestation (github_user#user / #sole_user) is a
// user↔external-identity link too, and appears here alongside what the syncs
// wrote. See spicedb.ListSubjectIdentities for the full set and its limits.

// directoryIdentitiesSection renders the external identity objects linked to a
// user.
//
// There are THREE answers here, not two, and none of them may render as
// another:
//
//   - a failed read is text ("unavailable: <reason>"), which appendSection
//     keeps. Presenting it as an empty list would be the confident claim "this
//     person has no linked identities", the silent-error failure this repo has
//     paid for.
//   - a successful empty read is a zero Section, which appendSection drops.
//   - a PARTIAL read — some probes failed, the rest returned rows — is a list
//     carrying BOTH the rows and a notice naming what is missing. Dropping the
//     notice would present a short list as a complete one; dropping the rows
//     would hide what is actually known over one absent definition.
func directoryIdentitiesSection(ids spicedb.SubjectIdentities, err error) config.Section {
	if err != nil {
		return textSection("directoryidentities", "Directory identities",
			"unavailable: "+err.Error())
	}
	items := make([]config.ListItem, 0, len(ids.Identities))
	for _, id := range ids.Identities {
		items = append(items, config.ListItem{
			Title:    id.Definition + ":" + id.ObjectID,
			Subtitle: "asserted by " + id.Source + " · " + id.Relation,
			Badges:   []config.Badge{{Key: "source", Value: id.Source}},
		})
	}
	sec := listSection("directoryidentities", "Directory identities", items)
	sec.Text = unavailableNotice(ids.Unavailable)
	return sec
}

// unavailableNotice is the warning a partially-read list carries alongside its
// rows, or "" when every probe succeeded. A list Section's Text is rendered
// above its items by the frontend (SectionRenderer), so the notice cannot be
// mistaken for the list itself — and a Section with a notice and no rows is
// still non-empty, so appendSection keeps it rather than dropping the one
// answer that says the list is short.
func unavailableNotice(unavailable []spicedb.UnavailableProbe) string {
	if len(unavailable) == 0 {
		return ""
	}
	parts := make([]string, 0, len(unavailable))
	for _, u := range unavailable {
		parts = append(parts, u.Source+" ("+u.Definition+"#"+u.Relation+"): "+u.Err)
	}
	return "This list is INCOMPLETE — some sources could not be read: " + strings.Join(parts, "; ")
}

// SubjectIdentityReaderFunc reads the external identities linked to a user, as
// usersDetailProjector.Detail needs them.
//
// subject is the SpiceDB subject reference as it appears on
// UserIdentity.Spec.Subject — "user:<canonical>", INCLUDING the prefix, not a
// bare canonical id. The strip to a typed identity.CanonicalUserID happens in
// admind.New's adapter, which is where the import of pkg/platform/identity
// lives; taking a plain string here is what keeps this package free of it.
type SubjectIdentityReaderFunc func(ctx context.Context, subject string) (spicedb.SubjectIdentities, error)

// subjectIdentityReader is the package-level injection point
// usersDetailProjector.Detail consults. The zero value (nil) is what every
// test binary that never calls SetSubjectIdentityReader sees, and nil leaves
// the Directory identities section off the page entirely — the panel is
// additive, so a build without a reader shows exactly what it showed before.
var subjectIdentityReader SubjectIdentityReaderFunc

// SetSubjectIdentityReader installs the reader the users detail projector
// consults. Called once by admind.New. Nil leaves the Directory identities
// section off the page entirely — the panel is additive, and a build without a
// reader shows exactly what it showed before.
func SetSubjectIdentityReader(r SubjectIdentityReaderFunc) { subjectIdentityReader = r }
