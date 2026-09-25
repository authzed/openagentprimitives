package v1alpha1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goodSpec() SpiceboxToolchainSpec {
	return SpiceboxToolchainSpec{
		Source: ToolchainSource{
			Kind:   "image",
			Image:  "ghcr.io/example/ap-toolchain-go@sha256:aaaa",
			Prefix: "/opt/ap-toolchains/go",
		},
		Bin:       []string{"bin", "tools/bin"},
		Env:       map[string]string{"GOROOT": "{{ .Root }}", "GOCACHE": "{{ .Cache }}/gobuild"},
		SizeBytes: 520000000,
	}
}

func TestValidateToolchainSpec(t *testing.T) {
	cases := []struct {
		name    string
		tcName  string
		mutate  func(*SpiceboxToolchainSpec)
		wantErr string
	}{
		{name: "valid spec passes", tcName: "go", mutate: func(*SpiceboxToolchainSpec) {}},
		{
			name: "name with a dot is rejected", tcName: "go.1.21",
			mutate: func(s *SpiceboxToolchainSpec) {
				s.Source.Prefix = ToolchainRootFor("go.1.21")
			},
			wantErr: "DNS-1123 label",
		},
		{
			name: "name with an uppercase letter is rejected", tcName: "Go",
			mutate: func(s *SpiceboxToolchainSpec) {
				s.Source.Prefix = ToolchainRootFor("Go")
			},
			wantErr: "DNS-1123 label",
		},
		{
			name:   "name that pushes toolchain-<name> over 63 chars is rejected",
			tcName: strings.Repeat("a", 55), // len("toolchain-")+55 = 65 > 63
			mutate: func(s *SpiceboxToolchainSpec) {
				s.Source.Prefix = ToolchainRootFor(strings.Repeat("a", 55))
			},
			wantErr: "exceeds the DNS-1123 label max",
		},
		{
			name: "plain name is accepted", tcName: "go", mutate: func(*SpiceboxToolchainSpec) {},
		},
		{
			name: "empty source.kind is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Source.Kind = "" },
			wantErr: "source.kind is required",
		},
		{
			name: "empty source.image is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Source.Image = "" },
			wantErr: "source.image is required",
		},
		{
			name: "prefix not equal to root/<name> is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Source.Prefix = "/opt/toolchains/go" },
			wantErr: "must equal",
		},
		{
			name: "prefix naming another toolchain is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Source.Prefix = "/opt/ap-toolchains/node" },
			wantErr: "must equal",
		},
		{
			name: "absolute bin entry is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Bin = []string{"/bin"} },
			wantErr: "must be relative",
		},
		{
			name: "bin entry escaping the root is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Bin = []string{"../../etc"} },
			wantErr: "must not contain",
		},
		{
			name: "bin entry containing a PATH separator is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Bin = []string{"bin:/etc"} },
			wantErr: "must not contain",
		},
		{
			name: "zero sizeBytes is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.SizeBytes = 0 },
			wantErr: "sizeBytes must be > 0",
		},
		{
			name: "unknown env template var is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Env = map[string]string{"X": "{{ .Nope }}"} },
			wantErr: "unexpanded template",
		},
		{
			name: "reserved env key PATH is rejected", tcName: "go",
			mutate:  func(s *SpiceboxToolchainSpec) { s.Env = map[string]string{"PATH": "/x"} },
			wantErr: "reserved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSpec()
			tc.mutate(&s)
			err := ValidateToolchainSpec(tc.tcName, s)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestExpandToolchainEnv(t *testing.T) {
	got, err := ExpandToolchainEnv(
		map[string]string{"GOROOT": "{{ .Root }}", "GOCACHE": "{{ .Cache }}/gobuild"},
		"/opt/ap-toolchains/go", "/var/ap-cache")
	require.NoError(t, err, "ExpandToolchainEnv")
	assert.Equal(t, "/opt/ap-toolchains/go", got["GOROOT"])
	assert.Equal(t, "/var/ap-cache/gobuild", got["GOCACHE"])

	_, err = ExpandToolchainEnv(map[string]string{"X": "{{.Root}}"}, "/r", "/c")
	assert.ErrorContains(t, err, "unexpanded template",
		"tight braces are not the supported spelling and must fail closed, not silently pass through")
}
