package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateArtifactStoreURL covers the fail-closed ARTIFACT_STORE_URL
// selector: an unset URL is a fatal configuration error (no silent inmem/mem
// fallback), and a schemeless value is rejected loudly. `oap install` always
// injects a value; a bare `kubectl apply` of the bundle intentionally lands
// on the unset-fails-closed path.
func TestValidateArtifactStoreURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "unset fails closed with guidance", url: "", wantErr: true},
		{name: "schemeless fails", url: "/var/lib/ap", wantErr: true},
		{name: "gs URL ok", url: "gs://ap-artifacts-x-abcd1234", wantErr: false},
		{name: "file URL with driver query ok", url: "file:///var/lib/ap/artifacts?create_dir=true", wantErr: false},
		{name: "mem ok (dev only)", url: "mem://", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArtifactStoreURL(tc.url)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "ARTIFACT_STORE_URL", "error must name the env var")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
