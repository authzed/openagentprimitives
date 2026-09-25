package credhost

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func credScopedTo(hosts ...string) v1.AgentCredential {
	return v1.AgentCredential{Name: "org-pat", Type: "static", AllowedHosts: hosts}
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name    string
		cred    v1.AgentCredential
		url     string
		wantErr bool
	}{
		{
			name: "unscoped credential permits any host, so upgrades do not break",
			cred: v1.AgentCredential{Name: "org-pat", Type: "static"},
			url:  "https://attacker.example/x.git",
		},
		{
			name: "the scoped host itself",
			cred: credScopedTo("github.com"), url: "https://github.com/org/repo.git",
		},
		{
			name: "a different host — the exfiltration this exists to refuse",
			cred: credScopedTo("github.com"), url: "https://attacker.example/x.git",
			wantErr: true,
		},
		{
			name: "case is not authority",
			cred: credScopedTo("GitHub.com"), url: "https://github.com/org/repo.git",
		},
		{
			name: "a wildcard matches a subdomain",
			cred: credScopedTo("*.internal.example"), url: "https://git.internal.example/a.git",
		},
		{
			name: "a wildcard matches a deeper subdomain",
			cred: credScopedTo("*.internal.example"), url: "https://a.b.internal.example/a.git",
		},
		{
			name: "a wildcard does NOT match the apex — an author who wants both lists both",
			cred: credScopedTo("*.internal.example"), url: "https://internal.example/a.git",
			wantErr: true,
		},
		{
			name: "a wildcard does NOT match a non-boundary suffix",
			cred: credScopedTo("*.example.com"), url: "https://notexample.com/a.git",
			wantErr: true,
		},
		{
			name: "a port is part of the authority and must match",
			cred: credScopedTo("git.example:8443"), url: "https://git.example:8443/a.git",
		},
		{
			name: "a scoped credential is not sent to the same name on a different port",
			cred: credScopedTo("git.example:8443"), url: "https://git.example:9999/a.git",
			wantErr: true,
		},
		{
			name: "a destination with no host cannot be matched, so it is refused",
			cred: credScopedTo("github.com"), url: "/relative/path",
			wantErr: true,
		},
		{
			name: "an unparseable destination is refused rather than defaulted",
			cred: credScopedTo("github.com"), url: "ht tp://%zz",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.cred, tc.url)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
