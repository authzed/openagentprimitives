package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// The bundle SpiceboxSession must carry a snapshot of the owning class's
// config, so the toolcall controller can expose it to constraint CEL. This is
// the value the AgentSession reconciler passes as AgentClass.spec.config.
func TestBuildBundleSessionStampsToolConfig(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "default", UID: "uid-1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "gitlike", Class: "demo-reviewbot-gitlike"}
	classCfg := map[string]apiextensionsv1.JSON{
		"allowedRepos": {Raw: []byte(`["demo-org/*"]`)},
	}

	got := agentsession.BuildBundleSession(sess, bundle, "id", "", nil, nil, classCfg)
	assert.Equal(t, classCfg, got.Spec.ToolConfig,
		"the SpiceboxSession must carry a snapshot of the class config")
}

// No class config ⇒ no ToolConfig stamped (nil, not an empty map that would
// change SSA payloads).
func TestBuildBundleSessionNoConfigLeavesToolConfigNil(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "review", Namespace: "default", UID: "uid-1"},
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "gitlike", Class: "demo-reviewbot-gitlike"}

	got := agentsession.BuildBundleSession(sess, bundle, "id", "", nil, nil, nil)
	assert.Nil(t, got.Spec.ToolConfig)
}
