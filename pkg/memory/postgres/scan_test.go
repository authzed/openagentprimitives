package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// fakeRow drives scanEntry without a live database. It assigns the nine
// columns memory_entry yields in select order; per-column overrides let a test
// inject a corrupt links/provenance blob the way a tampered or truncated
// postgres row would.
type fakeRow struct {
	scope    memory.Scope
	kind, id string
	created  time.Time
	tags     []string
	content  json.RawMessage
	linksRaw []byte
	provRaw  []byte
}

var _ pgx.Row = (*fakeRow)(nil)

// Scan mirrors the dest types scanEntry passes, in select order. Column 6
// (content) is *json.RawMessage, the rest are the plain types backend.go uses.
func (r *fakeRow) Scan(dest ...any) error {
	*(dest[0].(*string)) = r.scope.Kind
	*(dest[1].(*string)) = r.scope.ID
	*(dest[2].(*string)) = r.kind
	*(dest[3].(*string)) = r.id
	*(dest[4].(*time.Time)) = r.created
	*(dest[5].(*[]string)) = r.tags
	*(dest[6].(*json.RawMessage)) = r.content
	*(dest[7].(*[]byte)) = r.linksRaw
	*(dest[8].(*[]byte)) = r.provRaw
	return nil
}

// TestScanEntry_CorruptProvenance is the regression guard for the audit
// finding: a row with a malformed provenance blob MUST surface an error from
// the scan path, not laundered into a silently-nil-provenance entry that reads
// back as "unsigned" and lowers the re-seeded chain head / truncation anchor.
func TestScanEntry_CorruptProvenance(t *testing.T) {
	cases := []struct {
		name     string
		linksRaw []byte
		provRaw  []byte
		wantErr  string
	}{
		{
			name:    "corrupt provenance JSON: scanEntry errors, not nil-provenance",
			provRaw: []byte(`{not valid json`),
			wantErr: "unmarshal provenance",
		},
		{
			name:     "corrupt links JSON: scanEntry errors, not dropped-links",
			linksRaw: []byte(`{not valid json`),
			wantErr:  "unmarshal links",
		},
		{
			name:     "well-formed provenance + links: scanEntry succeeds",
			linksRaw: []byte(`[]`),
			provRaw:  []byte(`{"publisher":"system:test","keyID":"abcd","seq":3}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &fakeRow{
				scope:    memory.Scope{Kind: "session", ID: "ns/a"},
				kind:     "transcript",
				id:       "t-1",
				created:  time.Now().UTC(),
				linksRaw: tc.linksRaw,
				provRaw:  tc.provRaw,
			}

			e, err := scanEntry(row)

			if tc.wantErr == "" {
				require.NoError(t, err)
				if tc.provRaw != nil {
					require.NotNil(t, e.Provenance, "well-formed provenance must decode")
					assert.Equal(t, "system:test", e.Provenance.Publisher)
				}
				return
			}

			require.Error(t, err, "corrupt blob must surface, not be swallowed")
			assert.ErrorContains(t, err, tc.wantErr)
			// The error must carry the row identity so an operator can locate it.
			assert.ErrorContains(t, err, "kind=transcript")
			assert.ErrorContains(t, err, "id=t-1")
			// Critically: the corrupt provenance is NOT silently nil-on-success.
			assert.Nil(t, e.Provenance,
				"on error scanEntry must not return a usable nil-provenance entry as if it succeeded")
		})
	}
}

// TestUnmarshalProvenance_ErrorNotSwallowed exercises the helper directly: a
// decode failure returns a non-nil error and does NOT collapse to a nil
// envelope masquerading as a benign "unsigned" entry.
func TestUnmarshalProvenance_ErrorNotSwallowed(t *testing.T) {
	p, err := unmarshalProvenance([]byte(`{garbage`))
	require.Error(t, err)
	assert.Nil(t, p)

	// Empty input is the genuine "no provenance" case: nil, no error.
	p, err = unmarshalProvenance(nil)
	require.NoError(t, err)
	assert.Nil(t, p)
}
