package passthrough_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
)

func TestIconURL(t *testing.T) {
	cases := []struct {
		name        string
		externalURL string
		credName    string
		want        string
	}{
		{name: "happy path", externalURL: "https://identity.example.com", credName: "linear", want: "https://identity.example.com/icon/linear"},
		{name: "trailing slash trimmed", externalURL: "https://identity.example.com/", credName: "linear", want: "https://identity.example.com/icon/linear"},
		{name: "multiple trailing slashes trimmed", externalURL: "https://identity.example.com///", credName: "linear", want: "https://identity.example.com/icon/linear"},
		{name: "subpath preserved", externalURL: "https://example.com/identity", credName: "github-token", want: "https://example.com/identity/icon/github-token"},
		{name: "empty externalURL returns empty", externalURL: "", credName: "linear", want: ""},
		{name: "empty credName returns empty", externalURL: "https://identity.example.com", credName: "", want: ""},
		{name: "both empty returns empty", externalURL: "", credName: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, passthrough.IconURL(tc.externalURL, tc.credName))
		})
	}
}
