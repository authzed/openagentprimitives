package graphiti_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	kggraphiti "github.com/authzed/openagentprimitives/pkg/memory/kg/graphiti"
	searchgraphiti "github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
)

// pathRecordingServer records the escaped path of every request it receives
// and answers each with an empty JSON object.
func pathRecordingServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"uuid": "x"})
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// TestGetEntity_UUIDIsValidatedAndEscaped is the defect: GetEntity
// interpolated a caller-supplied uuid straight into the request path with no
// escaping and no validation, so the value could re-target the request within
// the Graphiti host — dot segments are resolved by net/url before the request
// is sent, so "../../nodes/all" leaves /entity-edge/ entirely.
//
// The read is scope-gated (the caller's approval authorizes the scope in the
// request URL, not the UUID in the path), so an unvalidated URL component is
// the wrong shape for it regardless of what the current Graphiti routes
// happen to expose.
func TestGetEntity_UUIDIsValidatedAndEscaped(t *testing.T) {
	cases := []struct {
		name      string
		uuid      string
		wantPaths []string
	}{
		{
			name:      "a well-formed UUID reaches /entity-edge/{uuid}",
			uuid:      testEntityUUID,
			wantPaths: []string{"/entity-edge/" + testEntityUUID},
		},
		{
			name:      "path traversal: refused before any request is issued",
			uuid:      "../../nodes/all",
			wantPaths: nil,
		},
		{
			name:      "an appended query string: refused before any request is issued",
			uuid:      testEntityUUID + "?group_id=other",
			wantPaths: nil,
		},
		{
			name:      "empty: refused before any request is issued",
			uuid:      "",
			wantPaths: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, paths := pathRecordingServer(t)
			p := kggraphiti.New(searchgraphiti.New(srv.URL))

			_, err := p.GetEntity(context.Background(), tc.uuid)
			if tc.wantPaths == nil {
				require.Error(t, err, "a non-UUID entity id must be refused")
				assert.ErrorIs(t, err, memory.ErrInvalidQuery,
					"a malformed entity id is a caller bug (400), not a retried 500")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantPaths, *paths, "requests actually issued")
		})
	}
}
