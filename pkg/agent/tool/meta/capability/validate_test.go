package capability

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateGrant(t *testing.T) {
	resetRegistryForTest(t)
	Register(&fakeCap{name: "planning", def: true})
	Register(&fakeCap{name: "artifacts", parseFn: func(raw json.RawMessage) (Config, error) {
		var cfg struct {
			Renderers []string `json:"renderers"`
		}
		if len(raw) == 0 {
			return cfg, nil
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}})

	cases := []struct {
		name        string
		capName     string
		raw         string
		wantErr     bool
		wantUnknown bool
	}{
		{
			name:    "valid config → nil",
			capName: "planning",
			raw:     `{}`,
		},
		{
			name:    "unknown capability name → error",
			capName: "teleport",
			raw:     `{}`,
			wantErr: true, wantUnknown: true,
		},
		{
			name:    "malformed {enabled} field → error",
			capName: "planning",
			raw:     `{"enabled":"not-a-bool"}`,
			wantErr: true, wantUnknown: false,
		},
		{
			name:    "malformed capability-specific config → error",
			capName: "artifacts",
			raw:     `{"renderers":"not-a-list"}`,
			wantErr: true, wantUnknown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			err := ValidateGrant(tc.capName, raw)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantUnknown, IsUnknownCapability(err), "IsUnknownCapability classification")
			if tc.wantUnknown {
				assert.True(t, errors.Is(err, ErrUnknownCapability), "errors.Is(err, ErrUnknownCapability)")
			}
		})
	}
}
