package agentcaps

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ResolveSessionViews replaced three independent copies of this resolution —
// pkg/web/webui/interact's POST and GET handlers, internal/cmd/webd's artifactview deps, and
// the capability registry's ParseConfig. Those copies happened to agree; this
// table is what keeps the single survivor honest, so it enumerates the union of
// what all three depended on, including the malformed-config row none of them
// covered.
func TestResolveSessionViews(t *testing.T) {
	cases := []struct {
		name             string
		class            *spiceboxv1alpha1.AgentClass
		wantActive       bool
		wantInteractions []string
		wantErr          bool
	}{
		{
			name:  "nil class: inactive, no error (an absent class is not a malformed one)",
			class: nil,
		},
		{
			name:  "no capabilities map: inactive (opt-in — absent grant grants nothing)",
			class: classWith(t, SessionViewsCapability, "", true),
		},
		{
			name:  "granted but enabled=false: inactive even with interactions listed",
			class: classWith(t, SessionViewsCapability, `{"enabled":false,"interactions":["user_message"]}`, false),
		},
		{
			name:             "granted as {}: ACTIVE but no interactions (read-only transcript)",
			class:            classWith(t, SessionViewsCapability, `{}`, false),
			wantActive:       true,
			wantInteractions: nil,
		},
		{
			name:             "granted with one interaction: active, that kind only",
			class:            classWith(t, SessionViewsCapability, `{"interactions":["user_message"]}`, false),
			wantActive:       true,
			wantInteractions: []string{"user_message"},
		},
		{
			name:             "granted with enabled=true and several interactions: active, all of them, order preserved",
			class:            classWith(t, SessionViewsCapability, `{"enabled":true,"interactions":["user_message","annotation_batch"]}`, false),
			wantActive:       true,
			wantInteractions: []string{"user_message", "annotation_batch"},
		},
		{
			name:             "granted with an empty interactions array: active, no kinds (same as {})",
			class:            classWith(t, SessionViewsCapability, `{"interactions":[]}`, false),
			wantActive:       true,
			wantInteractions: []string{},
		},
		{
			name:    "malformed envelope: zero resolution AND error (callers fail closed)",
			class:   classWith(t, SessionViewsCapability, `{`, false),
			wantErr: true,
		},
		{
			name:    "interactions is not an array: zero resolution AND error, never a partial parse",
			class:   classWith(t, SessionViewsCapability, `{"interactions":"user_message"}`, false),
			wantErr: true,
		},
		{
			name:  "a DIFFERENT capability granted: inactive (the key is not confused for another)",
			class: classWith(t, "artifacts", `{"interactions":["user_message"]}`, false),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveSessionViews(tc.class)
			if tc.wantErr {
				require.Error(t, err, "a malformed value must be reported, never tolerated")
				assert.Equal(t, SessionViewsResolution{}, got,
					"an error must carry the ZERO resolution so no caller can act on a half-parsed value")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, got.Active)
			assert.Equal(t, tc.wantInteractions, got.Interactions)
		})
	}
}

// Active and Interactions must stay independently readable: pkg/web/webui/interact
// reports "session views are not enabled for this class" and "this interaction
// kind is not permitted" as different 403s, which it can only do while the two
// states are distinguishable. A resolver returning a bare []string would make
// them identical.
func TestResolveSessionViewsDistinguishesInactiveFromGrantedReadOnly(t *testing.T) {
	inactive, err := ResolveSessionViews(classWith(t, SessionViewsCapability, "", true))
	require.NoError(t, err)

	readOnly, err := ResolveSessionViews(classWith(t, SessionViewsCapability, `{}`, false))
	require.NoError(t, err)

	assert.Empty(t, inactive.Interactions)
	assert.Empty(t, readOnly.Interactions, "both permit no interaction kind…")
	assert.NotEqual(t, inactive.Active, readOnly.Active,
		"…but only one of them has the capability at all, and callers must be able to say which")
}

func TestParseSessionViewsConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    SessionViewsConfig
		wantErr bool
	}{
		{name: "nil raw: zero config, no error", raw: ``},
		{name: "empty object: zero config, no error (granted read-only)", raw: `{}`},
		{
			name: "interactions listed: decoded in order",
			raw:  `{"interactions":["user_message","annotation_batch"]}`,
			want: SessionViewsConfig{Interactions: []string{"user_message", "annotation_batch"}},
		},
		{
			name: "unknown sibling fields are ignored, not rejected",
			raw:  `{"interactions":["user_message"],"somethingNew":42}`,
			want: SessionViewsConfig{Interactions: []string{"user_message"}},
		},
		{name: "truncated json: zero config AND error", raw: `{"interactions":`, wantErr: true},
		{name: "wrong type for interactions: zero config AND error", raw: `{"interactions":7}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSessionViewsConfig(json.RawMessage(tc.raw))
			if tc.wantErr {
				require.Error(t, err)
				assert.Equal(t, SessionViewsConfig{}, got, "never return a partially-populated config")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
