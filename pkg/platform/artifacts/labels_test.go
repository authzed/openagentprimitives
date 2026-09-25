package artifacts_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func TestLabelKeys_FollowFeatureConvention(t *testing.T) {
	for _, k := range []string{
		artifacts.LabelArtifactID, artifacts.AnnoParentRevision, artifacts.AnnoChangeDescription,
		artifacts.AnnoAppliedTags, artifacts.AnnoArtifactName, artifacts.AnnoArtifactDescription,
	} {
		assert.Truef(t, strings.HasPrefix(k, "artifact.agentprimitives.authzed.com/"),
			"key %q must use the feature-prefixed convention", k)
	}
	assert.Equal(t, "latest", artifacts.TagLatest)
}
