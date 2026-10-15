package installcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
