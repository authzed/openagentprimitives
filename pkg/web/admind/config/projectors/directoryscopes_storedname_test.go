package projectors

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// The section builder is kind-agnostic on purpose — it never learns which sync
// produced a row. These cases prove that claim for the sources that store
// their OWN names (Slack, 1Password) rather than pointing at a URL-keyed
// bridge, because "it is kind-agnostic" is exactly the sort of claim that goes
// stale the first time a second shape appears and nobody re-checks it.

// A stored channel name becomes the Title and pushes the raw id down into the
// subtitle — DOWN, never out: the name is what a human came to read, and the
// `definition:id` is what an operator needs in hand to go query SpiceDB for the
// same row.
//
// Href stays empty, and that is the point rather than an omission: a
// directory-authored name has no link target, so there is nothing for a
// spoofed one to be.
func TestDirectoryScopesSection_StoredNameBecomesTheTitleAndKeepsTheRawID(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{{
			Definition: "slack_channel",
			ScopeIDs:   []string{"C0123", "C0456"},
			Total:      2,
			Source:     "Slack",
			// Sparse by design: C0456 has no stored name.
			Labels: map[string]spicedb.ScopeLabel{"C0123": {Title: "demo-channel"}},
		}},
	}, nil)

	require.Len(t, sec.Items, 2)

	assert.Equal(t, "demo-channel", sec.Items[0].Title)
	assert.Equal(t, "slack_channel:C0123 · synced by Slack", sec.Items[0].Subtitle,
		"the raw id an operator needs must survive the renaming, in the subtitle")
	assert.Empty(t, sec.Items[0].Href, "a directory-authored name is never a link")

	assert.Equal(t, "slack_channel:C0456", sec.Items[1].Title, "a scope with no stored name renders raw, as before")
	assert.Empty(t, sec.Text, "one unnamed row among named ones is not a failure and must raise no alarm")
}

// The binding constraint for the new kinds: an unavailable label read must
// never render as a silently unlabelled list. Same notice, same wording, for a
// stored-name probe as for a bridged one — and the probe it names is the SCOPE
// definition, because that is the definition the read actually ran against.
func TestDirectoryScopesSection_FailedStoredNameReadKeepsRawIDsAndSaysSo(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "onepassword_group", ScopeIDs: []string{"11111111-2222"}, Total: 1, Source: "1Password"},
		},
		LabelsUnavailable: []spicedb.UnavailableProbe{{
			Source: "1Password", Definition: "onepassword_group", Relation: "label",
			Err: "rpc error: code = Unavailable",
		}},
	}, nil)

	require.Len(t, sec.Items, 1, "every scope the sync wrote must still render")
	assert.Equal(t, "onepassword_group:11111111-2222", sec.Items[0].Title, "a failed read degrades to the raw id")
	require.NotEmpty(t, sec.Text, "an unlabelled list that FAILED to resolve names must never be silent")
	assert.Contains(t, sec.Text, "raw ids")
	assert.Contains(t, sec.Text, "onepassword_group#label",
		"the probe named must be the one that ran, so an operator looks at the right definition")
	assert.Contains(t, sec.Text, "rpc error: code = Unavailable", "with the reason, not just the fact")
	assert.NotContains(t, sec.Text, "INCOMPLETE", "the LIST is complete; calling it incomplete is a false alarm")
}
