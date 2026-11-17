package installcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestRenderValidate_PassesWithNothingExternalByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE), NoDigestPin: true}
	require.NoError(t, WriteOverlay(cfg))

	var buf bytes.Buffer
	require.NoError(t, runRenderValidate(context.Background(), &buf, cfg, dir), "a complete default render passes validation")
	// Everything internal is now provisioned by the overlay (Job + cert-manager),
	// so there is nothing to provide yourself.
	assert.NotContains(t, buf.String(), "provide these", "no provide-yourself secrets by default")
	assert.NotContains(t, buf.String(), "spicebox-webhook-tls", "webhook-tls is cert-manager-provisioned, not external")
}

func TestRenderValidate_ExternalWebhookTLSListsIt(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE), NoDigestPin: true, ExternalWebhookTLS: true}
	require.NoError(t, WriteOverlay(cfg))

	var buf bytes.Buffer
	require.NoError(t, runRenderValidate(context.Background(), &buf, cfg, dir), "externalized webhook-tls is listed, not a failure")
	assert.Contains(t, buf.String(), "spicebox-webhook-tls", "with --external-webhook-tls it is surfaced as provide-yourself")
}

func TestRenderValidate_FailsOnUnsatisfiedSecret(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - dep.yaml\n"), 0o644))
	// A Deployment referencing a Secret nothing creates and that is not a known
	// app-internal secret → a hard validation failure.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dep.yaml"), []byte(
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: widget\n  namespace: agentprimitives-system\n"+
			"spec:\n  selector:\n    matchLabels:\n      app: widget\n  template:\n    metadata:\n      labels:\n        app: widget\n"+
			"    spec:\n      containers:\n        - name: c\n          image: busybox\n          env:\n            - name: T\n"+
			"              valueFrom:\n                secretKeyRef:\n                  name: nobody-makes-this\n                  key: token\n"), 0o644))

	cfg := RenderConfig{OutputDir: dir, NoDigestPin: true}
	err := runRenderValidate(context.Background(), new(bytes.Buffer), cfg, dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nobody-makes-this")
	assert.Contains(t, err.Error(), "no creator")
}

// writeOverlayFiles writes a minimal kustomization over the given resource docs.
func writeOverlayFiles(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	k := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n"
	for _, name := range sortedKeys(docs) {
		k += "  - " + name + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(docs[name]), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(k), 0o644))
	return dir
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// spicedbEndpointDoc satisfies the SpiceDB check so a fixture fails only on what
// it is about.
const spicedbEndpointDoc = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: spicebox-spicedb-config\n  namespace: agentprimitives-system\ndata:\n  endpoint: spicedb.example.com:50051\n"

func podDeployment(podSpec string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: widget\n  namespace: agentprimitives-system\n" +
		"spec:\n  selector:\n    matchLabels:\n      app: widget\n  template:\n    metadata:\n      labels:\n        app: widget\n" +
		"    spec:\n" + podSpec
}

func TestRenderValidate_FailsOnMissingOrUnpinned(t *testing.T) {
	cases := []struct {
		name    string
		docs    map[string]string
		noPin   bool
		wantErr string
	}{
		{
			name: "envFrom secretRef nothing creates: no creator",
			docs: map[string]string{"cm.yaml": spicedbEndpointDoc, "dep.yaml": podDeployment(
				"      containers:\n        - name: c\n          image: busybox\n          envFrom:\n            - secretRef:\n                name: envfrom-orphan\n")},
			noPin: true, wantErr: "envfrom-orphan",
		},
		{
			name: "projected-volume secret nothing creates: no creator",
			docs: map[string]string{"cm.yaml": spicedbEndpointDoc, "dep.yaml": podDeployment(
				"      containers:\n        - name: c\n          image: busybox\n" +
					"      volumes:\n        - name: v\n          projected:\n            sources:\n              - secret:\n                  name: projected-orphan\n")},
			noPin: true, wantErr: "projected-orphan",
		},
		{
			name: "one pinned and one unpinned ref to the same image: mutable tag",
			docs: map[string]string{"cm.yaml": spicedbEndpointDoc, "dep.yaml": podDeployment(
				"      containers:\n        - name: a\n          image: reg.example.com/spicebox-operator:dev@sha256:" + strings.Repeat("a", 64) + "\n" +
					"        - name: b\n          image: reg.example.com/spicebox-operator:dev\n")},
			wantErr: "mutable tag",
		},
		{
			name:    "no SpiceDB endpoint ConfigMap: clients have no endpoint",
			docs:    map[string]string{"dep.yaml": podDeployment("      containers:\n        - name: c\n          image: busybox\n")},
			noPin:   true,
			wantErr: "spicebox-spicedb-config",
		},
		{
			name: "in-cluster endpoint without a SpiceDBCluster: nothing serves it",
			docs: map[string]string{"cm.yaml": strings.Replace(spicedbEndpointDoc, "spicedb.example.com:50051", inClusterSpiceDBEndpoint, 1),
				"dep.yaml": podDeployment("      containers:\n        - name: c\n          image: busybox\n")},
			noPin:   true,
			wantErr: "no SpiceDBCluster",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeOverlayFiles(t, tc.docs)
			err := runRenderValidate(context.Background(), new(bytes.Buffer), RenderConfig{OutputDir: dir, NoDigestPin: tc.noPin}, dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
