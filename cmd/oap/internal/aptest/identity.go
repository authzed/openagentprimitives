package aptest

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliidentity"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// FakeIdentityConfigDir points the CLI's identity cache at a per-test temp
// directory and restores the real one on cleanup, so a test can seed or clear
// a cached assertion without touching the developer's own ~/.config.
func FakeIdentityConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	orig := cliidentity.ConfigDirFn
	cliidentity.ConfigDirFn = func() (string, error) { return dir, nil }
	t.Cleanup(func() { cliidentity.ConfigDirFn = orig })
}

// NewClient returns a fake controller-runtime client over kube.Scheme — the
// same scheme every oap command's real client is built with — seeded with objs.
func NewClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(kube.Scheme)
	if len(objs) > 0 {
		b = b.WithObjects(objs...)
	}
	return b.Build()
}
