package originfmt_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestForBrowser(t *testing.T) {
	t.Run("uses Ref (the CR name), not Name (the LLM prefix)", func(t *testing.T) {
		rt := spiceboxv1alpha1.ResolvedBrowserToolbox{Name: "demo", Ref: "demo-browser"}
		assert.Equal(t, "browsertoolbox/demo-browser", originfmt.ForBrowser(rt))
	})
	t.Run("empty Ref yields empty string, mirroring Origin()'s empty case", func(t *testing.T) {
		assert.Equal(t, "", originfmt.ForBrowser(spiceboxv1alpha1.ResolvedBrowserToolbox{Name: "demo"}))
	})
	t.Run("kind constant is browsertoolbox", func(t *testing.T) {
		assert.Equal(t, "browsertoolbox", originfmt.KindBrowserToolbox)
	})
}
