package aptest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The GVRs a fake dynamic client has to be told about by hand: it cannot
// derive them from a scheme the way the real RESTMapper does. Spellings match
// kube.Apply's CRD-derived pluralization, which is what the commands under
// test actually address.
var (
	ChannelGVR = schema.GroupVersionResource{
		Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "channels",
	}
	AgentClassGVR = schema.GroupVersionResource{
		Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentclasses",
	}
	ClusterAgentSettingsGVR = schema.GroupVersionResource{
		Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "clusteragentsettings",
	}
	SecretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
)

// WriteTempYAML writes body to a doc.yaml in a fresh temp dir and returns its
// path, for the commands that take a -f/<path> manifest argument.
func WriteTempYAML(t *testing.T, body string) string {
	t.Helper()
	return WriteFile(t, t.TempDir(), "doc.yaml", body)
}

// WriteFile writes content to dir/name and returns the path, for the commands
// that need several named files in one directory.
func WriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600), "write %s", p)
	return p
}
