package toolcall

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestConfigFromSession(t *testing.T) {
	t.Run("decodes JSON values to native for the config CEL root", func(t *testing.T) {
		sess := &spiceboxv1alpha1.SpiceboxSession{
			Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
				ToolConfig: map[string]apiextensionsv1.JSON{
					"allowedRepos": {Raw: []byte(`["demo-org/*","owner/repo"]`)},
					"maxDepth":     {Raw: []byte(`3`)},
				},
			},
		}
		got, err := configFromSession(sess)
		require.NoError(t, err)
		assert.Equal(t, []any{"demo-org/*", "owner/repo"}, got["allowedRepos"])
		assert.EqualValues(t, 3, got["maxDepth"])
	})

	t.Run("no ToolConfig yields a non-nil empty map", func(t *testing.T) {
		got, err := configFromSession(&spiceboxv1alpha1.SpiceboxSession{})
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("undecodable value fails closed", func(t *testing.T) {
		sess := &spiceboxv1alpha1.SpiceboxSession{
			Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
				ToolConfig: map[string]apiextensionsv1.JSON{
					"bad": {Raw: []byte(`{not json`)},
				},
			},
		}
		_, err := configFromSession(sess)
		assert.Error(t, err, "a session carrying undecodable config must fail, not proceed with empty config")
	})
}
