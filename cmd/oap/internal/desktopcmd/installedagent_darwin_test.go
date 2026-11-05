//go:build darwin && arm64

package desktopcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/linkinstall"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

func TestInstalledAgentClass(t *testing.T) {
	key := client.ObjectKey{Namespace: demoChatNamespace, Name: "demo-agent"}
	withSource := func(src instance.OapSource) *spiceboxv1alpha1.AgentClass {
		raw, err := instance.MarshalOapSource(src)
		require.NoError(t, err)
		return &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace, Name: key.Name,
			Annotations: map[string]string{instance.AnnotationOapSource: raw},
		}}
	}
	statusOnly := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Status:     spiceboxv1alpha1.AgentClassStatus{OapInstall: &spiceboxv1alpha1.OapInstallStatus{Version: "1.3.0", Digest: "sha256:aa", SourceKind: "registry"}},
	}
	bare := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	malformed := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{
		Namespace: key.Namespace, Name: key.Name,
		Annotations: map[string]string{instance.AnnotationOapSource: "{not json"},
	}}

	cases := []struct {
		name    string
		live    *spiceboxv1alpha1.AgentClass
		want    linkinstall.Installed
		wantErr bool
	}{
		{name: "absent: not installed, no error", want: linkinstall.Installed{}},
		{name: "annotated: version from oap-source", live: withSource(instance.OapSource{Version: "1.4.0", SourceKind: "registry"}), want: linkinstall.Installed{Found: true, Version: "1.4.0"}},
		{name: "annotation stripped: version from status mirror", live: statusOnly, want: linkinstall.Installed{Found: true, Version: "1.3.0"}},
		{name: "not from a bundle: found, version unknown", live: bare, want: linkinstall.Installed{Found: true}},
		{name: "malformed annotation: error, so the link is refused", live: malformed, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).WithStatusSubresource(&spiceboxv1alpha1.AgentClass{})
			if tc.live != nil {
				b = b.WithObjects(tc.live)
			}
			got, err := installedAgentClass(t.Context(), b.Build(), key)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
