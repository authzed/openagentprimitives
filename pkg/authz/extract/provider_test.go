package extract_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
)

// stubProvider implements Provider for the test-fixture sanity checks.
type stubProvider struct {
	out []extract.ExtractedEntity
	err error
}

func (s *stubProvider) Extract(_ context.Context, _ extract.ExtractInput) ([]extract.ExtractedEntity, error) {
	return s.out, s.err
}

func TestExtractInput_RoundtripFieldsPreserved(t *testing.T) {
	in := extract.ExtractInput{
		UserMessage: "merge PR 17 in foo/bar",
		EntityTypes: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read"},
		},
		AlreadyBound: []extract.SessionBinding{
			{ResourceType: "github_repo", ResourceID: "baz/qux"},
		},
	}
	assert.Equal(t, "merge PR 17 in foo/bar", in.UserMessage)
	require.Len(t, in.EntityTypes, 1)
	assert.Equal(t, "github_repo", in.EntityTypes[0].ResourceType)
	require.Len(t, in.AlreadyBound, 1)
	assert.Equal(t, "baz/qux", in.AlreadyBound[0].ResourceID)
}

func TestStubProvider_ReturnsConfiguredEntities(t *testing.T) {
	p := &stubProvider{out: []extract.ExtractedEntity{
		{ResourceType: "github_repo", ResourceID: "foo/bar", SourceText: "foo/bar"},
	}}
	got, err := p.Extract(context.Background(), extract.ExtractInput{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "foo/bar", got[0].ResourceID)
}
