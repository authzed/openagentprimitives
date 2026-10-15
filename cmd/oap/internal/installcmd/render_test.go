package installcmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func TestWriteOverlay_DeterministicAndBuildable(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	base := RenderConfig{ClusterKind: cloud.KeyLocal, Strat: cloud.MustFor(cloud.KeyLocal)}

	cfg := base
	cfg.OutputDir = dir1
	require.NoError(t, WriteOverlay(cfg))
	cfg.OutputDir = dir2
	require.NoError(t, WriteOverlay(cfg))

	assert.FileExists(t, filepath.Join(dir1, "base", "install.yaml"))
	assert.FileExists(t, filepath.Join(dir1, "kustomization.yaml"))

	for _, rel := range []string{"kustomization.yaml", filepath.Join("base", "install.yaml")} {
		b1, err := os.ReadFile(filepath.Join(dir1, rel))
		require.NoError(t, err)
		b2, err := os.ReadFile(filepath.Join(dir2, rel))
		require.NoError(t, err)
		assert.Equal(t, string(b1), string(b2), "re-render of %s is byte-identical", rel)
	}

	out, err := buildOverlay(dir1)
	require.NoError(t, err, "emitted overlay must kustomize build")
	assert.Contains(t, string(out), "kind: Deployment")
}

func TestRunRender_OfflineSkipsDigestResolution(t *testing.T) {
	g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) {
		panic("render must not touch the cluster offline")
	}}
	// A GitOps-valid (persistent-datastore) kind with --no-digest-pin: render must
	// succeed without any remote call. gke, not local — local is a memory-datastore
	// kind and validateRenderFlags refuses it (see TestValidateRenderFlags_*).
	cfg := RenderConfig{OutputDir: t.TempDir(), ClusterKind: cloud.KeyGKE, NoDigestPin: true}
	require.NoError(t, runRender(context.Background(), io.Discard, g, cfg, false))
}

func TestRender_StampsClusterKindAndImages(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{
		OutputDir:   dir,
		ClusterKind: cloud.KeyGKE,
		Strat:       cloud.MustFor(cloud.KeyGKE),
		Tags: manifests.Tags{
			Registry: "reg.example.com",
			Digests: map[string]string{
				"spicebox-operator":      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				"agentprimitives-runner": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			},
		},
	}
	require.NoError(t, WriteOverlay(cfg))
	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.GreaterOrEqual(t, strings.Count(s, "AP_CLUSTER_KIND"), 2, "AP_CLUSTER_KIND stamped on operator + webd")
	assert.Contains(t, s, cloud.KeyGKE, "cluster-kind value present")
	assert.Contains(t, s, "reg.example.com/spicebox-operator", "operator image rewritten to the registry")
	assert.Contains(t, s, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "operator image pinned by digest")
}

func TestRender_EmitsRoutingWhenHostnameGiven(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	cfg.Routing.trustedHostname = "webd.example.com"
	cfg.Routing.sandboxHostname = "sandbox.example.com"
	cfg.Routing.gatewayClass = "eg"
	require.NoError(t, WriteOverlay(cfg))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "kind: Gateway")
	assert.Contains(t, s, "kind: HTTPRoute")
	assert.Contains(t, s, "webd.example.com")
	assert.Contains(t, s, "sandbox.example.com")
	assert.Contains(t, s, cloud.WebdTrustedCertSecretName, "trusted listener references the cert Secret")
	assert.Contains(t, s, cloud.WebdSandboxCertSecretName, "sandbox listener references the cert Secret")
}

func TestRender_NoRoutingWithoutHostname(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	require.NoError(t, WriteOverlay(cfg))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "kind: HTTPRoute", "no --trusted-hostname → bring-your-own ingress, no routing emitted")
}

func TestValidateRenderFlags_RoutingRequiresGatewayClass(t *testing.T) {
	cfg := RenderConfig{ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	cfg.Routing.trustedHostname = "webd.example.com"
	err := validateRenderFlags(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gateway-class")
}

func TestRender_WithPostgresComponentInjectsStorageClass(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{
		OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE),
		WithPostgres: true, StorageClass: "fast-ssd",
	}
	require.NoError(t, WriteOverlay(cfg))
	assert.FileExists(t, filepath.Join(dir, "components", "postgres", "kustomization.yaml"))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "kind: PersistentVolumeClaim", "postgres PVC pulled in by the component")
	assert.Contains(t, s, "storageClassName: fast-ssd", "--storage-class injected into the postgres PVC")
}

func TestRender_IncludesChannelsdAndNats(t *testing.T) {
	// channelsd + nats are applied by `oap install` from separate embedded
	// bundles (not config/), so the render must add them explicitly or the
	// bundle deploys an install that cannot function.
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	require.NoError(t, WriteOverlay(cfg))
	assert.FileExists(t, filepath.Join(dir, "base", "channelsd.yaml"))
	assert.FileExists(t, filepath.Join(dir, "base", "nats.yaml"))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "spicebox-channelsd", "channelsd workload present")
	assert.Contains(t, s, "spicebox-nats", "nats workload present")
}

func TestRender_EmitsBootstrapJob(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	require.NoError(t, WriteOverlay(cfg))
	assert.FileExists(t, filepath.Join(dir, "bootstrap-job.yaml"))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "kind: Job")
	assert.Contains(t, s, "spicebox-bootstrap-secrets")
	assert.Contains(t, s, "bootstrap-secrets", "runs the operator bootstrap-secrets entrypoint")
	assert.Contains(t, s, "serviceAccountName: spicebox-operator", "reuses the operator SA's RBAC")
	assert.NotContains(t, s, "--skip-spicedb-token", "mints the bundled spicedb token by default")
}

func TestRender_BootstrapJobSkipsExternalTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE), ExternalSpiceDB: true, ExternalPostgres: true}
	require.NoError(t, WriteOverlay(cfg))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "--skip-spicedb-token", "external SpiceDB: Job does not mint the token")
	assert.Contains(t, s, "--skip-postgres-token", "external Postgres: Job does not mint the token")
}

func TestRenderValidate_ExternalSpiceDBStillValidates(t *testing.T) {
	// With --external-spicedb the Job does not mint spicedb-token, but the token's
	// k8s refs are optional (the app requires the value at runtime, not the kubelet
	// at mount), so --validate must still pass — the externalized instance does not
	// make the overlay structurally incomplete.
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE), NoDigestPin: true, ExternalSpiceDB: true}
	require.NoError(t, WriteOverlay(cfg))

	var buf bytes.Buffer
	require.NoError(t, runRenderValidate(context.Background(), &buf, cfg, dir), "external spicedb does not break validation")
}

func TestRender_EmitsWebhookCertManagerByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	require.NoError(t, WriteOverlay(cfg))
	assert.FileExists(t, filepath.Join(dir, "webhook-cert.yaml"))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "kind: Certificate")
	assert.Contains(t, s, "kind: Issuer")
	assert.Contains(t, s, "spicebox-webhook.agentprimitives-system.svc", "cert SANs cover the webhook Service")
	assert.Contains(t, s, "cert-manager.io/inject-ca-from: agentprimitives-system/spicebox-webhook-tls", "VWC gets the CA-injector annotation")
}

func TestRender_ExternalWebhookTLSSkipsCertManager(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE), ExternalWebhookTLS: true}
	require.NoError(t, WriteOverlay(cfg))
	assert.NoFileExists(t, filepath.Join(dir, "webhook-cert.yaml"))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "cert-manager.io/inject-ca-from", "no CA-injector annotation when the user provides webhook-tls")
}

func TestRender_NoComponentsByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	require.NoError(t, WriteOverlay(cfg))
	assert.NoDirExists(t, filepath.Join(dir, "components"), "no --with-* flag → no components dir")
}

func TestRender_LinksExistingMemoryTokenSecret(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{OutputDir: dir, ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	cfg.MemoryToken.Channelsd = "my-channelsd-secret"
	require.NoError(t, WriteOverlay(cfg))

	out, err := buildOverlay(dir)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "my-channelsd-secret", "channelsd memory-token references rewritten to the linked Secret")
	assert.NotContains(t, s, "spicebox-channelsd-memory-token", "default channelsd token name fully replaced (operator mount + channelsd volume)")
	assert.Contains(t, s, "spicebox-webd-memory-token", "unlinked webd token still uses the operator-minted default")
}

func TestValidateRenderFlags_RefusesWebdLinkEqualToOthers(t *testing.T) {
	// webd is browser-facing and its memory token is READ-ONLY by value; linking
	// it to the same Secret as channelsd/authzd (whose tokens are write-capable)
	// would hand webd write access to the audit log. Refuse it.
	base := RenderConfig{ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}

	c := base
	c.MemoryToken = MemoryTokenLinks{Channelsd: "shared", Webd: "shared"}
	require.Error(t, validateRenderFlags(c), "webd == channelsd link must be refused")

	c = base
	c.MemoryToken = MemoryTokenLinks{Authzd: "shared", Webd: "shared"}
	require.Error(t, validateRenderFlags(c), "webd == authzd link must be refused")

	// The bypass: linking webd to a DEFAULT-minted name while the matching
	// component is left to operator-mint (its link == ""). The Job mints the
	// default names regardless of links, so webd would mount the write-capable
	// channelsd/authzd token. The guard must compare EFFECTIVE names (link if
	// set, else the default), not just the raw link values.
	c = base
	c.MemoryToken = MemoryTokenLinks{Webd: bootstrap.MemoryTokenSecretNames[0]} // channelsd default, channelsd unlinked
	require.Error(t, validateRenderFlags(c), "webd linked to channelsd's default-minted token must be refused")

	c = base
	c.MemoryToken = MemoryTokenLinks{Webd: bootstrap.MemoryTokenSecretNames[2]} // authzd default, authzd unlinked
	require.Error(t, validateRenderFlags(c), "webd linked to authzd's default-minted token must be refused")

	// Distinct links are fine.
	c = base
	c.MemoryToken = MemoryTokenLinks{Channelsd: "ch", Webd: "wd", Authzd: "az"}
	require.NoError(t, validateRenderFlags(c))

	// The all-default (no-link) case must stay valid: the operator mints three
	// distinct tokens, so webd never collides.
	c = base
	c.MemoryToken = MemoryTokenLinks{}
	require.NoError(t, validateRenderFlags(c), "all-default links must remain valid")
}

func TestRenderCmd_NoGraphitiOrNeo4jFlags(t *testing.T) {
	// graphiti/neo4j require a user-supplied LLM API key (spicebox-graphiti-config)
	// that the overlay cannot mint — fundamentally incompatible with render's
	// "every internal Secret is provisioned, nothing external by default"
	// property. Rather than ship flags that emit a non-functional bundle (and a
	// --validate that hard-fails with no remedy), they are omitted until a
	// follow-up adds the external-secret story. Keep --with-postgres (its token IS
	// Job-minted, so it works end to end).
	flags := NewRenderCmd(&apcmd.Globals{}).Flags()
	assert.NotNil(t, flags.Lookup("with-postgres"), "postgres is Job-provisionable and stays")
	assert.Nil(t, flags.Lookup("with-graphiti"), "with-graphiti must not be offered (non-functional bundle)")
	assert.Nil(t, flags.Lookup("with-neo4j"), "with-neo4j must not be offered (non-functional bundle)")
}

func TestBootstrapJobName_TracksImageAndArgs(t *testing.T) {
	// The Job's pod template is immutable once applied. A GitOps re-sync after an
	// operator-image bump (new digest) would otherwise try to patch spec.template
	// and be rejected (SyncFailed). Naming the Job by a hash of its
	// image+args means a changed template is a NEW object (prune-and-create),
	// while a byte-identical re-render keeps the same name (no-op apply).
	// A real remote GitOps render pins by registry+digest, which is exactly the
	// ref that changes on an operator upgrade.
	base := RenderConfig{ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}
	base.Tags.Registry = "reg.example.com"
	base.Tags.Digests = map[string]string{apimage.Operator.Name: "sha256:" + strings.Repeat("a", 64)}

	n1, err := bootstrapJobName(base)
	require.NoError(t, err)
	n1b, err := bootstrapJobName(base)
	require.NoError(t, err)
	assert.Equal(t, n1, n1b, "deterministic: identical config → identical Job name")
	assert.True(t, strings.HasPrefix(n1, "spicebox-bootstrap-secrets-"), "keeps the recognizable prefix, got %q", n1)

	bumped := base
	bumped.Tags.Digests = map[string]string{apimage.Operator.Name: "sha256:" + strings.Repeat("b", 64)}
	nb, err := bootstrapJobName(bumped)
	require.NoError(t, err)
	assert.NotEqual(t, n1, nb, "a bumped operator image digest must yield a new Job object")

	skipped := base
	skipped.ExternalSpiceDB = true
	ns, err := bootstrapJobName(skipped)
	require.NoError(t, err)
	assert.NotEqual(t, n1, ns, "changed args (--skip-spicedb-token) must yield a new Job object")
}

func TestValidateRenderFlags_RefusesMemoryDatastoreKind(t *testing.T) {
	// local/desktop use an in-memory SpiceDB datastore whose schema is bootstrapped
	// at startup (bootstrapsSchemaAtStartup) — fatal against a durable, multi-replica
	// GitOps target. Render must refuse it rather than emit an unsafe bundle.
	err := validateRenderFlags(RenderConfig{ClusterKind: cloud.KeyLocal, Strat: cloud.MustFor(cloud.KeyLocal)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory", "error explains the memory datastore is the problem")

	// A persistent-datastore kind is accepted.
	require.NoError(t, validateRenderFlags(RenderConfig{ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE)}))
}
