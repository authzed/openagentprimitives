package installcmd

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// failOnReadReader fails the test the moment stdin is read. It proves
// resolveWorkspaceChoice took the no-prompt marker path rather than launching
// the interactive reselect picker (which reads stdin).
type failOnReadReader struct{ t *testing.T }

func (r failOnReadReader) Read([]byte) (int, error) {
	r.t.Error("resolveWorkspaceChoice read from stdin — a wizard-preconfirmed run must not launch the reselect picker")
	return 0, io.EOF
}

// TestResolveWorkspaceChoice_PreconfirmedSkipsReselect pins the M2/F1 fix. The
// wizard presents the Workspace decision up front and forwards Preconfirmed, so
// an interactive, non-`-y`, marker-present run must NOT re-ask via RunInstall's
// mid-install reselect picker (the double-prompt the wizard's own screen made
// redundant). It flows straight to the marker path (WorkspaceUseDirectly),
// returning the cached class without reading stdin.
func TestResolveWorkspaceChoice_PreconfirmedSkipsReselect(t *testing.T) {
	strat := cloud.MustFor(cloud.KeyLocal)
	var buf bytes.Buffer
	bundle := &kube.Bundle{Typed: fake.NewSimpleClientset(reselectObjs(true)...)}
	rep := progress.New(&buf, strings.NewReader(""), false)
	got, err := resolveWorkspaceChoice(context.Background(), bundle, strat,
		WorkspaceResolveOptions{Preconfirmed: true}, rep, &buf, failOnReadReader{t}, false /*assumeYes*/, true /*interactive*/)
	require.NoError(t, err)
	assert.Equal(t, "ap-workspace-rwx", got.ClassName, "the marker class is used directly; no reselect")
	assert.NotContains(t, buf.String(), "workspace storage (shared RWX)", "the reselect picker must not be shown")
}

func scObject(name, provisioner string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
	}
}

func markerCM(class string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: workspaceMarkerNamespace, Name: workspaceMarkerName},
		Data:       map[string]string{workspaceMarkerKey: class},
	}
}

// The zap2 shape: a prior install's marker pins the bundled node-pinned class,
// and a Filestore class was added to the cluster afterward.
func reselectObjs(withMarker bool) []runtime.Object {
	objs := []runtime.Object{
		scObject("ap-workspace-rwx", "cluster.local/ap-workspace-provisioner"),
		scObject("enterprise-multishare-rwx", "filestore.csi.storage.gke.io"),
	}
	if withMarker {
		objs = append(objs, markerCM("ap-workspace-rwx"))
	}
	return objs
}

// resolveWorkspaceChoice must offer the interactive picker (current
// pre-selected) when a prior marker is present on an interactive, non-`-y` run —
// the gap that made `oap init` silently keep the stale class — while a `-y` or
// non-interactive run, a fresh cluster with no marker, and a forced recheck all
// bypass the picker.
func TestResolveWorkspaceChoice_MarkerReselection(t *testing.T) {
	strat := cloud.MustFor(cloud.KeyLocal)
	ctx := context.Background()

	call := func(objs []runtime.Object, opts WorkspaceResolveOptions, in string, assumeYes, interactive bool) (workspaceChoice, string) {
		var buf bytes.Buffer
		bundle := &kube.Bundle{Typed: fake.NewSimpleClientset(objs...)}
		rep := progress.New(&buf, strings.NewReader(""), assumeYes)
		got, err := resolveWorkspaceChoice(ctx, bundle, strat, opts, rep, &buf, strings.NewReader(in), assumeYes, interactive)
		require.NoError(t, err)
		return got, buf.String()
	}

	t.Run("interactive + marker: picker shown, empty input keeps current (idempotent)", func(t *testing.T) {
		got, out := call(reselectObjs(true), WorkspaceResolveOptions{}, "\n", false, true)
		assert.Equal(t, "ap-workspace-rwx", got.ClassName)
		assert.Contains(t, out, "workspace storage (shared RWX)", "picker menu must be shown")
		assert.Contains(t, strings.ToLower(out), "operator", "roll-operator warning must be shown")
	})

	t.Run("interactive + marker: selecting the Filestore class repoints", func(t *testing.T) {
		// Menu order (fake lists by name): 1) ap-workspace-rwx 2) enterprise-multishare-rwx 3) isolated
		got, _ := call(reselectObjs(true), WorkspaceResolveOptions{}, "2\n", false, true)
		assert.Equal(t, "enterprise-multishare-rwx", got.ClassName)
	})

	t.Run("assumeYes + marker: no picker, silent cached class", func(t *testing.T) {
		got, out := call(reselectObjs(true), WorkspaceResolveOptions{}, "2\n", true, false)
		assert.Equal(t, "ap-workspace-rwx", got.ClassName, "cached class, not the ignored '2' input")
		assert.NotContains(t, out, "Workspace storage class?", "picker prompt must not appear under -y")
	})

	t.Run("interactive, no marker: fresh detection, no picker noise", func(t *testing.T) {
		got, out := call(reselectObjs(false), WorkspaceResolveOptions{}, "2\n", false, true)
		assert.Equal(t, "enterprise-multishare-rwx", got.ClassName, "auto-detected, not picked")
		assert.NotContains(t, out, "workspace storage (shared RWX)", "no picker without a prior marker")
	})

	t.Run("interactive + marker + recheck: bypasses picker for full detection", func(t *testing.T) {
		got, out := call(reselectObjs(true), WorkspaceResolveOptions{Recheck: true}, "\n", false, true)
		assert.Equal(t, "enterprise-multishare-rwx", got.ClassName)
		assert.NotContains(t, out, "workspace storage (shared RWX)", "recheck runs detection, not the picker")
	})
}
