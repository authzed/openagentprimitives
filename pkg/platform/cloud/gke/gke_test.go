package gke

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// ---- Strategy facts ----

func TestStrategyFacts(t *testing.T) {
	s := Strategy{}
	assert.Equal(t, "gke", s.Key())
	assert.Equal(t, "GKE", s.DisplayName())
	assert.True(t, s.IsManaged())
	assert.Equal(t, "gce://", s.ProviderIDPrefix())
	assert.Equal(t, []string{"169.254.0.0/16"}, s.DNSEgressCIDRs())
	assert.Equal(t, []string{"35.191.0.0/16", "130.211.0.0/22"}, s.GatewayBackendIngressCIDRs())
	// GKE's managed global L7 first-provision runs ~10m, so it asks for a larger
	// address-wait budget than the bundled-Envoy clouds (8m/7m).
	dl, eta := s.GatewayAddressWait()
	assert.Equal(t, 15*time.Minute, dl)
	assert.Equal(t, 10*time.Minute, eta)
	assert.Greater(t, dl, eta, "deadline must exceed the expected-ready ETA")
	assert.NotNil(t, s.TLS())
	assert.Equal(t, "google-managed", s.TLS().Name())
	assert.NotNil(t, s.WorkspaceStorage())
}

// ---- RegistryFromProviderID ----

func TestRegistryFromProviderID(t *testing.T) {
	s := Strategy{}
	cases := []struct {
		name       string
		providerID string
		want       string
	}{
		{"GKE zonal → AR in region", "gce://my-proj/us-east1-c/gke-n1", "us-east1-docker.pkg.dev/my-proj/ap"},
		{"GKE another region", "gce://acme-infra/europe-west4-a/node", "europe-west4-docker.pkg.dev/acme-infra/ap"},
		{"wrong prefix → empty", "aws:///us-west-2a/i-0abc", ""},
		{"GKE but wrong prefix → empty", "aws:///x/i-0", ""},
		{"GKE malformed (too few segments) → empty", "gce://only-proj", ""},
		{"GKE empty providerID → empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, s.RegistryFromProviderID(tc.providerID))
		})
	}
}

// ---- ProjectFromProviderID ----

func TestProjectFromProviderID(t *testing.T) {
	s := Strategy{}
	cases := []struct {
		name       string
		providerID string
		want       string
	}{
		{"GKE → project", "gce://my-proj/us-east1-c/gke-n1", "my-proj"},
		{"wrong prefix → empty", "aws:///us-west-2a/i-0abc", ""},
		{"GKE wrong prefix → empty", "aws:///x/i-0", ""},
		{"GKE too few segments → empty", "gce://only-proj/zone", ""},
		{"GKE empty providerID → empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, s.ProjectFromProviderID(tc.providerID))
		})
	}
}

// ---- workspaceStorage.Resolve ----

const wardenWebhook = "warden-validating.common-webhooks.networking.gke.io"

func filestoreStorageClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: "filestore.csi.storage.gke.io",
	}
}

// multishareFilestoreClass is a Filestore class that shares ONE instance across
// many PVCs — GKE marks these with parameters.multishare=true. Used by the
// enable-capability tests (workspacecontroller_test.go); Resolve itself no
// longer inspects Filestore classes.
func multishareFilestoreClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: "filestore.csi.storage.gke.io",
		Parameters:  map[string]string{"multishare": "true"},
	}
}

func wardenWebhookObj() *admissionregistrationv1.ValidatingWebhookConfiguration {
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: wardenWebhook},
	}
}

// TestWorkspaceStorageResolve pins the reverted default: the bundled node-local
// local-path class is what Resolve recommends on GKE Standard, EVEN WHEN a
// durable Filestore class exists on the cluster. Filestore is an NFS mount and
// makes git operations (many small files) ~10x slower, so it is opt-in only —
// reachable through an explicit --workspace-storage-class (handled upstream in
// cmd/oap, never in Resolve). Resolve therefore never auto-prefers Filestore;
// on Autopilot, where the bundled hostPath provisioner is denied, it degrades
// to isolated with guidance to name a Filestore class explicitly.
func TestWorkspaceStorageResolve(t *testing.T) {
	cases := []struct {
		name         string
		objs         []runtime.Object
		wantDegraded bool
		wantBundled  bool
		wantVerify   cloud.WorkspaceVerify
		wantClass    string
		wantMsgHas   string // Message contains this substring (when set)
	}{
		{
			name:        "GKE Standard (no Warden webhook), no Filestore → bundled NeedsBundled ProbeBeforeUse",
			objs:        nil,
			wantClass:   cloud.BundledWorkspaceStorageClass,
			wantBundled: true,
			wantVerify:  cloud.WorkspaceProbeBeforeUse,
		},
		{
			// The revert: a Filestore class existing no longer wins. Filestore is
			// an NFS mount (~10x slower git), so the node-local bundled class is
			// the default; Filestore is available only via --workspace-storage-class.
			name: "GKE Standard + Filestore StorageClass → bundled (Filestore is opt-in only now)",
			objs: []runtime.Object{
				filestoreStorageClass("standard-rwx"),
			},
			wantClass:   cloud.BundledWorkspaceStorageClass,
			wantBundled: true,
			wantVerify:  cloud.WorkspaceProbeBeforeUse,
		},
		{
			// Autopilot denies the bundled hostPath provisioner (Warden), and we
			// no longer auto-pick Filestore, so there is no free default: degrade
			// to isolated and tell the operator how to opt into Filestore.
			name: "GKE Autopilot + Filestore StorageClass → Degraded (bundled denied, Filestore opt-in)",
			objs: []runtime.Object{
				wardenWebhookObj(),
				filestoreStorageClass("premium-rwx"),
			},
			wantDegraded: true,
			wantMsgHas:   "--workspace-storage-class",
		},
		{
			name: "GKE Autopilot + no Filestore → Degraded",
			objs: []runtime.Object{
				wardenWebhookObj(),
			},
			wantDegraded: true,
			wantMsgHas:   "--workspace-storage-class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			ws := workspaceStorage{}
			got, err := ws.Resolve(context.Background(), cloud.WorkspaceParams{
				Clients:  cloud.Clients{Typed: kc},
				Reporter: cloud.NopReporter{},
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantDegraded, got.Degraded, "Degraded")
			assert.Equal(t, tc.wantBundled, got.NeedsBundled, "NeedsBundled")
			assert.Equal(t, tc.wantVerify, got.Verify, "Verify")
			assert.Equal(t, tc.wantClass, got.ClassName, "ClassName")
			// Resolve never carries a cost warning now — Filestore's cost confirm
			// only happens on the explicit-class path in cmd/oap.
			assert.Empty(t, got.CostWarning, "Resolve must never emit a CostWarning")
			if tc.wantDegraded {
				assert.NotEmpty(t, got.Message, "expected non-empty Message on Degraded decision")
			}
			if tc.wantMsgHas != "" {
				assert.Contains(t, got.Message, tc.wantMsgHas, "Message substring")
			}
		})
	}
}
