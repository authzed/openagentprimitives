package settings

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func boolp(b bool) *bool { return &b }

func TestResolveReportSessionCost(t *testing.T) {
	cases := []struct {
		name    string
		cluster *bool
		ns      *bool
		want    bool
	}{
		{"default on when all nil", nil, nil, true},
		{"cluster off", boolp(false), nil, false},
		{"cluster on", boolp(true), nil, true},
		{"namespace overrides cluster", boolp(false), boolp(true), true},
		{"namespace off over cluster on", boolp(true), boolp(false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Inputs{}
			if tc.cluster != nil {
				in.Cluster = &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{ReportSessionCost: tc.cluster}}
			}
			if tc.ns != nil {
				in.Namespace = &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{ReportSessionCost: tc.ns}}
			}
			eff, _ := Resolve(in)
			assert.Equal(t, tc.want, eff.ReportSessionCost)
		})
	}
}
