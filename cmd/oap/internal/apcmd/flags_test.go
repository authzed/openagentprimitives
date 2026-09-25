package apcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateDryRunMode pins which modes the cluster-mutating commands take.
// "server" is called out explicitly: it is the mode a kubectl user will reach
// for, and refusing it by name is the whole point — a mode that is accepted
// and then quietly downgraded to a client-side print tells the user their
// manifests were checked against the apiserver when they were not.
func TestValidateDryRunMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{name: "empty: not a dry run, accepted", mode: "", wantErr: false},
		{name: "client: the implemented mode, accepted", mode: DryRunClient, wantErr: false},
		{name: "server: not implemented, refused rather than downgraded", mode: "server", wantErr: true},
		{name: "typo: refused", mode: "clientt", wantErr: true},
		{name: "wrong case: refused, the mode is not normalized", mode: "Client", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDryRunMode(tc.mode)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), DryRunClient, "the error must name the mode that does work")
		})
	}
}
