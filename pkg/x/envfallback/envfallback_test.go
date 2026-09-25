package envfallback_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/x/envfallback"
)

func TestGet(t *testing.T) {
	const newName, oldName = "ENVFALLBACK_TEST_NEW", "ENVFALLBACK_TEST_OLD"

	cases := []struct {
		name    string
		newVal  string
		newSet  bool
		oldVal  string
		oldSet  bool
		wantVal string
		wantDep bool
	}{
		{
			name:    "new set, old unset: returns new, not deprecated",
			newVal:  "new-value",
			newSet:  true,
			wantVal: "new-value",
			wantDep: false,
		},
		{
			name:    "old set only: returns old, marked deprecated",
			oldVal:  "old-value",
			oldSet:  true,
			wantVal: "old-value",
			wantDep: true,
		},
		{
			name:    "both set: new wins, not deprecated",
			newVal:  "new-value",
			newSet:  true,
			oldVal:  "old-value",
			oldSet:  true,
			wantVal: "new-value",
			wantDep: false,
		},
		{
			name:    "neither set: empty, not deprecated",
			wantVal: "",
			wantDep: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.newSet {
				t.Setenv(newName, tc.newVal)
			}
			if tc.oldSet {
				t.Setenv(oldName, tc.oldVal)
			}

			gotVal, gotDep := envfallback.Get(newName, oldName)
			assert.Equal(t, tc.wantVal, gotVal)
			assert.Equal(t, tc.wantDep, gotDep)
		})
	}
}
