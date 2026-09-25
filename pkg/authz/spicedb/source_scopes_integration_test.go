//go:build integration

package spicedb

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// This file needs no relsource.Register / init() fixture, and so needs no
// "!integration && !e2e" build constraint the way subject_probes_test.go
// does: ListSourceScopes takes a relsource.Source VALUE directly rather than
// resolving one from the global registry, so a test can construct one
// in-line, naming an absent definition, without ever touching relsource.All()
// or the never-reset process-global registry other tests in this binary
// share.

// writeScopeSentinel writes <definition>:<scopeID>#relhash@string:<digest> —
// the sentinel pkg/platform/relsync/sync.go's Pass TOUCHes exactly once per
// scope it processes. ListSourceScopes never reads the subject value, only
// the resource id, so a fixed literal digest is fine.
func writeScopeSentinel(t *testing.T, c *Client, definition, scopeID string) {
	t.Helper()
	_, err := c.cl.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: definition, ObjectId: scopeID},
				Relation: relsource.SentinelRelation,
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{ObjectType: "string", ObjectId: "fixture-digest"},
				},
			},
		}},
	})
	require.NoError(t, err, "write %s:%s#%s", definition, scopeID, relsource.SentinelRelation)
}

// A directory sync's own last-pass footprint — one #relhash tuple per scope
// it wrote — is what this reader answers from, on the RESOURCE side (compare
// ListSubjectIdentities' complementary subject-side read). onepassword_group
// is the only scope-bearing definition the bare scaffold declares
// (schema.zed), so it stands in for github_org/slack_channel/etc. here.
func TestListSourceScopes_ReadsCappedScopesAndReportsTheFullTotal(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	src := relsource.Source{
		Name:        "fixture-scopes-source",
		DisplayName: "Fixture Directory",
		Claims:      []string{"onepassword_group#member", "onepassword_group#relhash"},
	}

	writeScopeSentinel(t, c, "onepassword_group", uniq(t, "grp-a-"))
	writeScopeSentinel(t, c, "onepassword_group", uniq(t, "grp-b-"))
	writeScopeSentinel(t, c, "onepassword_group", uniq(t, "grp-c-"))

	got, err := c.ListSourceScopes(ctx, src, nil, 2)
	require.NoError(t, err)

	require.Len(t, got.Scopes, 1)
	sc := got.Scopes[0]
	assert.Equal(t, "onepassword_group", sc.Definition)
	assert.Len(t, sc.ScopeIDs, 2, "capped at 2")
	assert.Equal(t, 3, sc.Total, "the full count is still reported even though the list was capped")
	assert.Equal(t, "Fixture Directory", sc.Source, "Source is src.Display() — DisplayName here, not the wire Name")
	assert.Empty(t, got.Unavailable)
}

// A probe against a definition the live schema does not declare must SHORTEN
// the answer, not destroy it — same partial-failure contract
// ListSubjectIdentities has, proven here on the resource side.
func TestListSourceScopes_AbsentDefinitionShortensRatherThanBlanks(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	src := relsource.Source{
		Name: "fixture-scopes-source-absent",
		Claims: []string{
			"onepassword_group#relhash",
			"no_such_scope_definition#relhash",
		},
	}
	writeScopeSentinel(t, c, "onepassword_group", uniq(t, "grp-"))

	got, err := c.ListSourceScopes(ctx, src, nil, 100)
	require.NoError(t, err, "a probe failure must not fail the whole read")

	require.Len(t, got.Scopes, 1, "the definition that COULD be read is still returned")
	assert.Equal(t, "onepassword_group", got.Scopes[0].Definition)

	require.Len(t, got.Unavailable, 1, "the probe that could not be read is reported, not swallowed")
	assert.Equal(t, "no_such_scope_definition", got.Unavailable[0].Definition)
	assert.Equal(t, "relhash", got.Unavailable[0].Relation)
	assert.Equal(t, "fixture-scopes-source-absent", got.Unavailable[0].Source,
		"src has no DisplayName, so Display() falls back to Name")
	assert.NotEmpty(t, got.Unavailable[0].Err, "the reason must survive to the caller")
}

// A source with nothing synced yet gets an empty answer, not an error — and
// a zero-total definition contributes NO row, matching the console section
// builder's "successful empty read renders nothing" contract.
func TestListSourceScopes_NothingSyncedYetIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	src := relsource.Source{
		Name:   "fixture-scopes-source-empty",
		Claims: []string{"onepassword_group#relhash"},
	}

	got, err := c.ListSourceScopes(ctx, src, nil, 100)
	require.NoError(t, err)
	assert.Empty(t, got.Scopes, "no scopes synced is an empty list, not an error")
	assert.Empty(t, got.Unavailable)
}

// The partial failure the console must never render silently: the scope rows
// read fine, and the BRIDGE that would have named them did not. This is the
// live shape of it — github_repo_url is declared only in the gh toolkit
// fragment, so against a bare scaffold the bridge read fails for real rather
// than by a stubbed error.
//
// Both halves are asserted because either one alone is the bug: rows without
// the notice is a confident unlabelled list, and the notice without rows is a
// blanked panel over a cosmetic failure.
func TestListSourceScopes_FailedLabelBridgeKeepsScopesAndReportsSeparately(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	src := relsource.Source{
		Name:        "fixture-scopes-source-bridge",
		DisplayName: "Fixture Directory",
		Claims:      []string{"onepassword_group#relhash"},
	}
	writeScopeSentinel(t, c, "onepassword_group", uniq(t, "grp-"))

	got, err := c.ListSourceScopes(ctx, src, []ScopeLabelBridge{{
		ScopeDefinition:  "onepassword_group",
		BridgeDefinition: "no_such_bridge_definition",
		BridgeRelation:   "names",
		Decoder:          resourcedisplay.DecoderB64URL,
	}}, 100)
	require.NoError(t, err, "a failed label bridge must not fail the whole read")

	require.Len(t, got.Scopes, 1, "every scope row must still render")
	assert.Empty(t, got.Scopes[0].Labels, "nothing could be named, so nothing claims to be")
	assert.Empty(t, got.Unavailable, "the LIST is complete — this is not a missing-rows failure")

	require.Len(t, got.LabelsUnavailable, 1, "an unresolvable name must be reported, never swallowed")
	assert.Equal(t, "no_such_bridge_definition", got.LabelsUnavailable[0].Definition)
	assert.Equal(t, "names", got.LabelsUnavailable[0].Relation)
	assert.Equal(t, "Fixture Directory", got.LabelsUnavailable[0].Source)
	assert.NotEmpty(t, got.LabelsUnavailable[0].Err, "the reason must survive to the caller")
}

// writeScopeLabel writes <definition>:<scopeID>#label@string:<base64url name>
// — the tuple pkg/platform/relsync's ScopeLabelTuple mints and the Slack and
// 1Password kinds return as ordinary scope content.
func writeScopeLabel(t *testing.T, c *Client, definition, scopeID, name string) {
	t.Helper()
	encoded, ok := resourcedisplay.EncodeB64Text(name)
	require.True(t, ok, "precondition: %q must encode", name)
	_, err := c.cl.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: definition, ObjectId: scopeID},
				Relation: relsource.LabelRelation,
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{ObjectType: relsource.LabelSubjectType, ObjectId: encoded},
				},
			},
		}},
	})
	require.NoError(t, err, "write %s:%s#%s", definition, scopeID, relsource.LabelRelation)
}

// The stored-name shape, end to end against a real SpiceDB: the SCOPE is the
// tuple's resource and the encoded name is its subject, which is the opposite
// arrangement from the URL bridge above.
//
// This is the half a pure-logic test structurally cannot reach. The joiner is
// unit-tested, but the FILTER — which definition is streamed, and which subject
// type it is narrowed to — only exists here, and getting it backwards is
// silent: the read succeeds, returns rows for the wrong definition or none at
// all, resolves nothing, and every row renders its raw id, which is
// indistinguishable from a directory that stores no names.
//
// onepassword_group is the one scope-bearing definition the bare scaffold
// declares, and it now declares `label` alongside `relhash` — so this exercises
// the real shipped schema, not a fixture definition.
func TestListSourceScopes_ResolvesAStoredNameFromTheScopesOwnLabelRelation(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	src := relsource.Source{
		Name:        "fixture-scopes-source-label",
		DisplayName: "Fixture Directory",
		Claims:      []string{"onepassword_group#relhash", "onepassword_group#label"},
	}

	named := uniq(t, "grp-named-")
	unnamed := uniq(t, "grp-unnamed-")
	writeScopeSentinel(t, c, "onepassword_group", named)
	writeScopeSentinel(t, c, "onepassword_group", unnamed)
	writeScopeLabel(t, c, "onepassword_group", named, "demo engineers")

	got, err := c.ListSourceScopes(ctx, src, []ScopeLabelBridge{{
		ScopeDefinition:  "onepassword_group",
		BridgeDefinition: relsource.LabelSubjectType,
		BridgeRelation:   relsource.LabelRelation,
		Shape:            ShapeNameOnSubject,
		Decoder:          resourcedisplay.DecoderB64Text,
	}}, 100)
	require.NoError(t, err)
	require.Len(t, got.Scopes, 1)
	assert.Empty(t, got.LabelsUnavailable, "the relation is in the live schema; nothing should have failed")

	labels := got.Scopes[0].Labels
	require.Contains(t, labels, named, "a scope with a stored label must resolve to its name")
	assert.Equal(t, "demo engineers", labels[named].Title,
		"a name with a space is exactly what a bare object id could not have held")
	assert.Empty(t, labels[named].Href, "a directory-authored name is never a link target")

	assert.NotContains(t, labels, unnamed,
		"absence is the single signal to render the raw id; an empty title would render a blank row")
}

// The notice half, for a stored-name bridge: when the read fails, every row
// still renders AND the page is told the names could not be resolved. The
// probe reported must name the definition that was actually READ — the SCOPE
// definition — because reporting the `string` subject type would send an
// operator to a definition with no relations at all.
func TestListSourceScopes_FailedStoredNameReadReportsTheScopeDefinition(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	src := relsource.Source{
		Name:        "fixture-scopes-source-label-fail",
		DisplayName: "Fixture Directory",
		Claims:      []string{"onepassword_group#relhash"},
	}
	writeScopeSentinel(t, c, "onepassword_group", uniq(t, "grp-"))

	// A relation the live schema does not declare on a definition that DOES
	// exist: the read fails for real, rather than by a stubbed error.
	got, err := c.ListSourceScopes(ctx, src, []ScopeLabelBridge{{
		ScopeDefinition:  "onepassword_group",
		BridgeDefinition: relsource.LabelSubjectType,
		BridgeRelation:   "no_such_label_relation",
		Shape:            ShapeNameOnSubject,
		Decoder:          resourcedisplay.DecoderB64Text,
	}}, 100)
	require.NoError(t, err, "a failed label read must not fail the whole read")

	require.Len(t, got.Scopes, 1, "every scope row must still render")
	assert.Empty(t, got.Scopes[0].Labels, "nothing could be named, so nothing claims to be")
	assert.Empty(t, got.Unavailable, "the LIST is complete — this is not a missing-rows failure")

	require.Len(t, got.LabelsUnavailable, 1, "an unresolvable name must be reported, never swallowed")
	assert.Equal(t, "onepassword_group", got.LabelsUnavailable[0].Definition,
		"the definition READ, not the string type the name rides on")
	assert.Equal(t, "no_such_label_relation", got.LabelsUnavailable[0].Relation)
	assert.NotEmpty(t, got.LabelsUnavailable[0].Err, "the reason must survive to the caller")
}
