package cloud

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsGcloudAlreadyExists ports the isAlreadyExistsErr cases from
// cmd/oap/internal/installcmd (the google-managed TLS test) — same logic, exported name.
func TestIsGcloudAlreadyExists(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ALREADY_EXISTS in gcloud stderr", errors.New("ERROR: (gcloud) Resource 'x' ALREADY_EXISTS"), true},
		{"already exists lowercase phrase", errors.New("the resource already exists"), true},
		{"unrelated error", errors.New("PERMISSION_DENIED"), false},
		{"nil error", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsGcloudAlreadyExists(tc.err))
		})
	}
}

// TestGcloudSignature_Compiles pins Gcloud's shape —
// Gcloud(ctx, Reporter, ...string) (string, error) — so a signature change
// breaks here rather than at each of its call sites across the gke package.
// The closure is never invoked: unit tests do not exec gcloud, and the
// blank-identifier assignment is the whole assertion, made by the compiler.
func TestGcloudSignature_Compiles(t *testing.T) {
	var _ func() (string, error) = func() (string, error) {
		return Gcloud(t.Context(), NopReporter{}, "version")
	}
}

func TestIsGcloudNotFound(t *testing.T) {
	assert.True(t, IsGcloudNotFound(errors.New("gcloud ...: exit status 1: ERROR: (gcloud) Could not fetch resource: - The resource 'x' was not found")))
	assert.True(t, IsGcloudNotFound(errors.New("ERROR: NOT_FOUND: no such NEG")))
	// The storage surface: the fresh-install "does this bucket exist?" probe.
	// Regression for a 404 being mis-classified as a hard error, failing install.
	assert.True(t, IsGcloudNotFound(errors.New("gcloud storage buckets describe gs://ap-artifacts-x: exit status 1: ERROR: (gcloud.storage.buckets.describe) gs://ap-artifacts-x not found: 404.")))
	assert.False(t, IsGcloudNotFound(nil))
	assert.False(t, IsGcloudNotFound(errors.New("some other failure")))
}
