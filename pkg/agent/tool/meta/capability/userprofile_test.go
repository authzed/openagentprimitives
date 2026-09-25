package capability

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestUserProfileParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []userprofile.Field
		wantErr string
	}{
		{name: "absent config yields the default field set", raw: ``, want: userprofile.DefaultFields()},
		{name: "empty object yields the default field set", raw: `{}`, want: userprofile.DefaultFields()},
		{name: "enabled:false alone is legal and still parses", raw: `{"enabled":false}`, want: userprofile.DefaultFields()},
		{
			name: "explicit fields parse in order",
			raw:  `{"fields":["title","pronouns"]}`,
			want: []userprofile.Field{userprofile.FieldTitle, userprofile.FieldPronouns},
		},
		{name: "unknown field name is rejected", raw: `{"fields":["salary"]}`, wantErr: `unknown profile field "salary"`},
		{name: "unknown config key is rejected", raw: `{"feilds":["title"]}`, wantErr: "unknown field"},
		{name: "wrong-typed fields value is rejected", raw: `{"fields":"title"}`, wantErr: "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			cfg, err := userProfileCapability{}.ParseConfig(raw)
			if tc.wantErr != "" {
				require.Error(t, err, "a bad config must surface as a CapabilitiesValid condition, never narrow silently")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			upc, ok := cfg.(UserProfileConfig)
			require.True(t, ok, "ParseConfig must yield a UserProfileConfig")
			assert.Equal(t, tc.want, upc.Fields)
		})
	}
}

func TestUserProfileIsOptIn(t *testing.T) {
	assert.False(t, userProfileCapability{}.DefaultOn(),
		"profile data must never be exposed without an explicit grant")
	assert.False(t, userProfileCapability{}.Infrastructural())
	assert.Equal(t, "user_profile", userProfileCapability{}.Name())
}

func classWithCaps(t *testing.T, key, rawJSON string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "ns"}}
	if key != "" {
		ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{key: {Raw: []byte(rawJSON)}}
	}
	return ac
}

func TestActiveWithConfig(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		raw        string
		wantActive bool
		wantFields []userprofile.Field
		wantErr    bool
	}{
		{name: "key absent: inactive", key: "", wantActive: false},
		{name: "other key present: inactive", key: "artifacts", raw: `{}`, wantActive: false},
		{name: "granted empty: active with defaults", key: "user_profile", raw: `{}`, wantActive: true, wantFields: userprofile.DefaultFields()},
		{
			name: "granted with fields: active with those fields", key: "user_profile",
			raw: `{"fields":["title"]}`, wantActive: true,
			wantFields: []userprofile.Field{userprofile.FieldTitle},
		},
		{name: "explicitly disabled: inactive", key: "user_profile", raw: `{"enabled":false}`, wantActive: false},
		{name: "malformed config: inactive AND error", key: "user_profile", raw: `{`, wantActive: false, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, active, err := ActiveWithConfig(classWithCaps(t, tc.key, tc.raw), "user_profile")
			if tc.wantErr {
				require.Error(t, err, "a malformed config must surface, never be dropped")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantActive, active)
			if !tc.wantActive {
				return
			}
			upc, ok := cfg.(UserProfileConfig)
			require.True(t, ok)
			assert.Equal(t, tc.wantFields, upc.Fields)
		})
	}
}

// TestUserProfileOffersNoToolsYet pins this plan's scope: the ambient block is
// the whole deliverable. get_participant_profile arrives in Plan 2.
func TestUserProfileOffersNoToolsYet(t *testing.T) {
	tools, skip := userProfileCapability{}.Offer(OfferContext{})
	assert.Empty(t, tools)
	assert.Nil(t, skip, "contributing no tools is a normal state, not a skip")
}
