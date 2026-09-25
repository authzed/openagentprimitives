package agentcaps

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// classWith builds an AgentClass whose spec.capabilities holds one key.
// rawJSON == "" means "key present with a nil value"; omit == true means
// "key absent entirely".
func classWith(t *testing.T, name, rawJSON string, omit bool) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "ns"}}
	if omit {
		return ac
	}
	ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{
		name: {Raw: []byte(rawJSON)},
	}
	return ac
}

func TestEnabledFrom(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		enabled bool
		wantErr bool
	}{
		{name: "empty object: enabled=true, no error", raw: `{}`, enabled: true},
		{name: "nil raw: enabled=true, no error", raw: ``, enabled: true},
		{name: "explicit false: enabled=false, no error", raw: `{"enabled":false}`, enabled: false},
		{name: "explicit true with extra fields: enabled=true", raw: `{"enabled":true,"maxResults":5}`, enabled: true},
		{name: "malformed json: enabled=false AND error (fail closed)", raw: `{`, enabled: false, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			enabled, err := EnabledFrom(raw)
			if tc.wantErr {
				require.Error(t, err, "malformed JSON must surface an error, never be swallowed")
				assert.False(t, enabled, "malformed JSON must fail closed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.enabled, enabled)
		})
	}
}

func TestGrantOf(t *testing.T) {
	cases := []struct {
		name        string
		rawJSON     string
		omit        bool
		nilClass    bool
		wantGranted bool
		wantEnabled bool
		wantErr     bool
	}{
		{name: "nil class: not granted", nilClass: true},
		{name: "key absent: not granted", omit: true},
		{name: "key present, empty object: granted+enabled", rawJSON: `{}`, wantGranted: true, wantEnabled: true},
		{name: "key present, enabled=false: granted, not enabled", rawJSON: `{"enabled":false}`, wantGranted: true},
		{name: "key present, sub-config only: granted+enabled", rawJSON: `{"renderers":["html"]}`, wantGranted: true, wantEnabled: true},
		{name: "key present, malformed: granted, NOT enabled, error returned", rawJSON: `{`, wantGranted: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var class *spiceboxv1alpha1.AgentClass
			if !tc.nilClass {
				class = classWith(t, "artifacts", tc.rawJSON, tc.omit)
			}
			g, err := GrantOf(class, "artifacts")
			if tc.wantErr {
				require.Error(t, err, "a malformed {enabled} blob must surface, never be dropped")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantGranted, g.Granted)
			assert.Equal(t, tc.wantEnabled, g.Enabled)
		})
	}
}

func TestActive(t *testing.T) {
	cases := []struct {
		name      string
		defaultOn bool
		grant     Grant
		want      bool
	}{
		{name: "default-on, absent: active", defaultOn: true, grant: Grant{Granted: false, Enabled: true}, want: true},
		{name: "default-on, granted+enabled: active", defaultOn: true, grant: Grant{Granted: true, Enabled: true}, want: true},
		{name: "default-on, granted+disabled: inactive", defaultOn: true, grant: Grant{Granted: true, Enabled: false}, want: false},
		{name: "opt-in, absent: inactive", defaultOn: false, grant: Grant{Granted: false, Enabled: true}, want: false},
		{name: "opt-in, granted+enabled: active", defaultOn: false, grant: Grant{Granted: true, Enabled: true}, want: true},
		{name: "opt-in, granted+disabled: inactive", defaultOn: false, grant: Grant{Granted: true, Enabled: false}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Active(tc.defaultOn, tc.grant))
		})
	}
}

// TestOptInRequiresExplicitKey is the security-relevant property: an opt-in
// capability is NEVER active unless the AgentClass explicitly lists its key.
func TestOptInRequiresExplicitKey(t *testing.T) {
	absent := classWith(t, "session_views", "", true)
	g, err := GrantOf(absent, "session_views")
	require.NoError(t, err)
	assert.False(t, Active(false, g), "an opt-in capability with no key must never be active")
}
