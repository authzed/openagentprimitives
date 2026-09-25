package capability

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The point of validating here is WHERE the error lands: on the AgentClass's
// CapabilitiesValid condition at apply time, rather than at the moment a user
// uploads a zip weeks later and gets a transient-looking failure.
func TestAttachmentsParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "absent config is valid", raw: ``},
		{name: "empty object is valid", raw: `{}`},
		{name: "enabled flag alone is valid", raw: `{"enabled": true}`},
		{name: "empty archives stanza is valid", raw: `{"archives":{}}`},
		{name: "a tightened bound is valid", raw: `{"archives":{"maxMembers":32}}`},
		{name: "a bound above the ceiling is valid: it clamps", raw: `{"archives":{"maxMembers":100000}}`},
		{name: "zero is rejected, not read as unlimited", raw: `{"archives":{"maxMembers":0}}`, wantErr: true},
		{name: "negative is rejected", raw: `{"archives":{"maxUncompressedTotal":-1}}`, wantErr: true},
		{name: "a malformed value is rejected", raw: `{"archives":{"maxMembers":"lots"}}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := attachmentsCapability{}.ParseConfig(json.RawMessage(tc.raw))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
