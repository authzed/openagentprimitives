package gke

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// parseARRegistry decides whether oap will try to create an Artifact Registry
// repository and, if so, which one — so getting a segment wrong means either
// creating a repo in the wrong project/location or silently skipping the whole
// step and failing later at `docker push`.
func TestParseARRegistry(t *testing.T) {
	cases := []struct {
		name     string
		registry string
		wantOK   bool
		want     arRegistry
	}{
		{
			name:     "regional AR reference parses into host/location/project/repo",
			registry: "us-east1-docker.pkg.dev/demo-project/ap-images",
			wantOK:   true,
			want: arRegistry{
				host:     "us-east1-docker.pkg.dev",
				location: "us-east1",
				project:  "demo-project",
				repo:     "ap-images",
			},
		},
		{
			name:     "trailing image path is ignored: AR repos are single-level",
			registry: "europe-west4-docker.pkg.dev/demo-project/ap-images/runner/base",
			wantOK:   true,
			want: arRegistry{
				host:     "europe-west4-docker.pkg.dev",
				location: "europe-west4",
				project:  "demo-project",
				repo:     "ap-images",
			},
		},
		{
			name:     "non-AR host is not ours to create in: ok=false",
			registry: "ghcr.io/demo-org/ap-images",
			wantOK:   false,
		},
		{
			name:     "host with no project or repo segment: ok=false",
			registry: "us-east1-docker.pkg.dev",
			wantOK:   false,
		},
		{
			name:     "host plus project but no repo: ok=false",
			registry: "us-east1-docker.pkg.dev/demo-project",
			wantOK:   false,
		},
		{
			name:     "empty project segment: ok=false",
			registry: "us-east1-docker.pkg.dev//ap-images",
			wantOK:   false,
		},
		{
			name:     "empty repo segment: ok=false",
			registry: "us-east1-docker.pkg.dev/demo-project/",
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseARRegistry(tc.registry)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestRegistryHostAndIsGKEArtifactRegistry(t *testing.T) {
	cases := []struct {
		name     string
		registry string
		wantHost string
		wantAR   bool
	}{
		{"full AR reference: host is the first segment and is AR", "us-east1-docker.pkg.dev/p/r", "us-east1-docker.pkg.dev", true},
		{"bare AR host with no path is still AR", "us-east1-docker.pkg.dev", "us-east1-docker.pkg.dev", true},
		{"ghcr reference: host parsed, not AR", "ghcr.io/demo-org/img", "ghcr.io", false},
		{"docker hub short name: whole string is the host, not AR", "ap-runner", "ap-runner", false},
		{"a host merely containing the suffix mid-string is not AR", "docker.pkg.dev.example.com/p/r", "docker.pkg.dev.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := RegistryHost(tc.registry)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantAR, IsGKEArtifactRegistry(host))
		})
	}
}

// CredHelperConfigured gates whether oap runs `gcloud auth configure-docker`.
// A false positive skips auth setup and the push fails later with a 401.
func TestCredHelperConfigured(t *testing.T) {
	const host = "us-east1-docker.pkg.dev"
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"helper registered for this host", `{"credHelpers":{"us-east1-docker.pkg.dev":"gcloud"}}`, true},
		{"helper registered for a DIFFERENT host only", `{"credHelpers":{"ghcr.io":"gh"}}`, false},
		{"credHelpers present but this host maps to an empty helper", `{"credHelpers":{"us-east1-docker.pkg.dev":""}}`, false},
		{"no credHelpers key at all", `{"auths":{"us-east1-docker.pkg.dev":{}}}`, false},
		{"malformed JSON is not a configured helper", `{"credHelpers":`, false},
		{"empty config", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CredHelperConfigured([]byte(tc.json), host))
		})
	}
}
