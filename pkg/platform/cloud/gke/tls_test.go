package gke

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestSanitizeCMName(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{"dots become hyphens", "webd.example.com", "webd-example-com"},
		{"uppercase lowered", "Webd.Example.COM", "webd-example-com"},
		{"leading/trailing non-alnum trimmed", "-webd.example-", "webd-example"},
		{"runs collapse to one hyphen", "a..b__c", "a-b-c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeCMName(tc.host))
		})
	}
}

func TestCertManagerResourceNames(t *testing.T) {
	const host = "webd.example.com"
	assert.Equal(t, "spicebox-webd-webd-example-com-auth", certManagerAuthName(host))
	assert.Equal(t, "spicebox-webd-webd-example-com-cert", certManagerCertName(host))
	assert.Equal(t, "spicebox-webd-webd-example-com", certManagerEntryName(host))
}

// TestGoogleManagedPrepare_NoProject fails closed when the GCP project couldn't
// be derived (so the gcloud calls would target no project).
func TestGoogleManagedPrepare_NoProject(t *testing.T) {
	_, _, proceed, err := GoogleManagedTLS{}.Prepare(context.Background(), cloud.PrepareParams{
		TrustedHostname: "webd.example.com",
	})
	require.Error(t, err)
	assert.False(t, proceed)
	assert.Contains(t, err.Error(), "GCP project")
}

func TestGoogleManagedComplete_NoOp(t *testing.T) {
	require.NoError(t, GoogleManagedTLS{}.Complete(context.Background(), cloud.CompleteParams{}))
}

// TestGoogleManagedTLS_Name verifies the strategy's stable identifier.
func TestGoogleManagedTLS_Name(t *testing.T) {
	assert.Equal(t, "google-managed", GoogleManagedTLS{}.Name())
}
