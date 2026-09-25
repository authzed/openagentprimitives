package settings

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func specWith(class, key, rawValue string, lock bool) v1.SettingsSpec {
	spec := v1.SettingsSpec{
		ClassUserPreferences: make(map[string]map[string]v1.PreferenceGlobal),
	}
	if class != "" || key != "" {
		if spec.ClassUserPreferences[class] == nil {
			spec.ClassUserPreferences[class] = make(map[string]v1.PreferenceGlobal)
		}
		g := v1.PreferenceGlobal{Lock: lock}
		if rawValue != "" {
			g.Value = apiextv1.JSON{Raw: []byte(rawValue)}
		}
		spec.ClassUserPreferences[class][key] = g
	}
	return spec
}

func TestClassUserPreferencesError(t *testing.T) {
	cases := []struct {
		name      string
		spec      v1.SettingsSpec
		isCluster bool
		wantMsg   string // "" = valid
	}{
		{name: "empty: valid", spec: v1.SettingsSpec{}},
		{name: "namespace tier with values: valid (shape only — class may not exist yet)",
			spec: specWith("reviewbot", "language", `"de"`, false)},
		{name: "cluster tier non-empty: rejected",
			spec: specWith("reviewbot", "language", `"de"`, false), isCluster: true,
			wantMsg: "namespace tier only"},
		{name: "empty class name: rejected",
			spec: specWith("", "language", `"de"`, false), wantMsg: "class name"},
		{name: "empty key: rejected",
			spec: specWith("reviewbot", "", `"de"`, false), wantMsg: "preference key"},
		{name: "empty value: rejected",
			spec: specWith("reviewbot", "language", ``, false), wantMsg: "value"},
		{name: "null value: rejected",
			spec: specWith("reviewbot", "language", `null`, false), wantMsg: "must not be null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ClassUserPreferencesError(&tc.spec, tc.isCluster)
			if tc.wantMsg == "" {
				assert.Empty(t, msg)
			} else {
				assert.Contains(t, msg, tc.wantMsg)
			}
		})
	}
}
