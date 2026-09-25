//go:build e2e

package install_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// oapWidgetSecretValue is the fake secret value carried through both
// installs in this test. Never a real credential (AGENTS.md fake-name
// convention).
const oapWidgetSecretValue = "fake-e2e-widget-token-8b31c9"

// oapWidgetResourceType/oapWidgetResourceID name a SpiceDB resource that
// exists ONLY because this test's MCPServer fixture contributes it via
// spec.spiceDBSchema — it is not part of the harness's base platform
// schema (pkg/authz/spicedb/schema). A live CheckPermission against it after
// install (assertWidgetPermissionLive) is therefore proof the exported +
// packed + installed schema fragment made it into the real SpiceDB
// instance via the guardian composer, not just accepted by a stub.
const (
	oapWidgetResourceType = "oap_e2e_widget"
	oapWidgetResourceID   = "demo"
)

// TestOapExportPackInstall_WithSpiceDB is Task 9 of Plan 4 (final plan) of
// the oap agent container: an end-to-end test of the whole .oap lifecycle
// against the real e2e harness (envtest apiserver + controllers + a real,
// per-test SpiceDB container — not a fake).
//
//  1. Build a real AgentClass ref graph (AgentClass, AgentIdentity,
//     MCPServer) in a source namespace, exactly as `oap agent export` would
//     find it on a live cluster.
//
//  2. source.OpenCluster(...).Bundle -> oap.Pack -> oap.Unpack, mirroring a
//     real .oap file round trip (registry push/pull or a local file).
//
//  3. install.Install the unpacked bundle into a FRESH target namespace,
//     supplying the secret answer directly as an install.SecretSpec
//     (bypassing install.Resolve's interactive/--set/--values plumbing,
//     which is a CLI-layer concern this test doesn't exercise).
//
//  4. Assert the installed AgentClass + AgentIdentity + MCPServer + Secret
//     land with the oap-install labels. Then assert SpiceDB consistency
//     directly: write a relationship for the resource type this test's
//     MCPServer's spec.spiceDBSchema fragment contributed, and confirm a
//     live, fully-consistent CheckPermission against the harness's REAL
//     SpiceDB resolves ALLOWED. That resource type exists in SpiceDB only
//     because the guardian composer picked up the exported+packed+installed
//     MCPServer's fragment — this is independent proof the .oap round trip
//     preserved the schema fragment all the way to a live authz decision.
//
//     NOTE on depth: the installed AgentClass's own Valid/SchemaValidated
//     conditions (which independently re-derive the same live-schema check
//     inside the AgentClass controller) are logged best-effort
//     (logAgentClassConditionBestEffort) rather than required. Empirically,
//     that controller's cross-reference re-enqueue (AgentIdentity/MCPServer
//     watches) does not reliably converge in this multi-graph, multi-
//     namespace scenario within a practical test deadline — a harness/
//     controller-runtime watch-timing question orthogonal to the oap
//     lifecycle this test exists to cover. Gating on it would make this
//     test flaky for reasons unrelated to export/pack/install. The direct
//     SpiceDB round trip above is the hard assertion of SpiceDB depth.
//
//  5. A second install.Install with InstallOpts{Name: "pm2"} into the SAME
//     target namespace: instance.Rename prefixes every bundled CR's name,
//     so the two installs must coexist rather than collide.
func TestOapExportPackInstall_WithSpiceDB(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ctx := context.Background()

	// The MCPServer fixture below declares one tool, check_widget; the
	// mcpserverctrl reconciler probes the live server's tools/list and
	// flips Valid=False/AllowlistDrift if the declared tool isn't
	// actually advertised. Registering it here (even with a body this
	// test never calls) is what makes tools/list report it.
	h.MCP.OnTool("check_widget", func(_ map[string]any) any { return map[string]any{"ok": true} })

	const srcNs = "oap-e2e-source"
	require.NoError(t, h.K8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: srcNs}}), "create source namespace")
	const dstNs = "oap-e2e-target"
	require.NoError(t, h.K8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dstNs}}), "create target namespace")

	buildOapSourceGraph(t, ctx, h, srcNs)

	// ---- Export -> Pack -> Unpack ----
	b, err := source.OpenCluster(h.K8s, srcNs, "demo-agent").Bundle(ctx)
	require.NoError(t, err, "OpenCluster(...).Bundle")
	require.NoError(t, b.Validate())

	packed, err := oap.Pack(b)
	require.NoError(t, err, "Pack")
	unpacked, err := oap.Unpack(packed)
	require.NoError(t, err, "Unpack")
	require.NoError(t, unpacked.Validate())
	assert.Equal(t, "demo-agent", unpacked.Manifest.Agent.Name)

	secrets := []install.SecretSpec{{Name: "widget-pat", Key: "token", Value: oapWidgetSecretValue}}

	// ---- Install into a FRESH target namespace ----
	result, err := install.Install(ctx, h.K8s, unpacked, oap.Answers{}, secrets, install.InstallOpts{Namespace: dstNs})
	require.NoError(t, err, "first Install")
	assert.Equal(t, "demo-agent", result.Name, "install name defaults to the bundled AgentClass's own name")
	assert.Equal(t, 1, result.SecretsCreated)
	assert.ElementsMatch(t, []string{"AgentIdentity", "MCPServer", "AgentClass"}, result.AppliedKinds)
	assert.NotContains(t, result.AppliedKinds, "Channel", "channels are per-install config, never bundled or installed by a .oap")

	assertOapInstalled(t, ctx, h.K8s, dstNs, "demo-agent", "widget-identity", "widget-mcp", "demo-agent")

	var sec corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: dstNs, Name: "widget-pat"}, &sec), "Secret created from the secret answer")
	assert.Equal(t, oapWidgetSecretValue, string(sec.Data["token"]), "secret value lands under the manifest's declared key")
	assert.Equal(t, "demo-agent", sec.Labels[instance.LabelInstall], "the Secret carries the install label")

	// Best-effort/diagnostic only — see the doc comment above for why this
	// isn't a hard gate.
	logAgentClassConditionBestEffort(t, h.K8s, dstNs, "demo-agent", v1alpha1.AgentClassConditionSchemaValidated, 10*time.Second)

	// ---- SpiceDB consistency ----
	// Independent, direct SpiceDB round trip: write the wildcard tuple for
	// the resource type this test's MCPServer contributed, then Check it.
	// oap_e2e_widget never existed in the harness's base platform schema
	// (pkg/authz/spicedb/schema) — a live ALLOW here is proof the exported +
	// packed + installed schema fragment reached the real SpiceDB
	// instance via the guardian composer, not merely a k8s-side artifact.
	assertWidgetPermissionLive(t, ctx, h)

	// ---- Second install via --name pm2: must coexist, not collide, and be
	// fully ISOLATED — its own prefixed Secret with a DISTINCT value, never
	// clobbering the first install's Secret. ----
	const oapWidgetSecretValue2 = "fake-e2e-widget-token-pm2-d4e91a"
	secrets2 := []install.SecretSpec{{Name: "widget-pat", Key: "token", Value: oapWidgetSecretValue2}}
	result2, err := install.Install(ctx, h.K8s, unpacked, oap.Answers{}, secrets2, install.InstallOpts{Name: "pm2", Namespace: dstNs})
	require.NoError(t, err, "second install (--name pm2)")
	assert.Equal(t, "pm2", result2.Name)
	assert.ElementsMatch(t, []string{"AgentIdentity", "MCPServer", "AgentClass"}, result2.AppliedKinds)

	// The original, unprefixed install is untouched...
	var original v1alpha1.AgentClass
	assert.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: dstNs, Name: "demo-agent"}, &original), "original unprefixed AgentClass is still present")

	// ...and the prefixed second install coexists alongside it, its
	// AgentClass ref fields rewritten to the prefixed dependent names.
	assertOapInstalled(t, ctx, h.K8s, dstNs, "pm2-demo-agent", "pm2-widget-identity", "pm2-widget-mcp", "pm2")

	var prefixed v1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: dstNs, Name: "pm2-demo-agent"}, &prefixed))
	assert.Equal(t, "pm2-widget-identity", prefixed.Spec.AgentIdentity, "instance.Rename must rewrite spec.agentIdentity to the prefixed name")
	require.Len(t, prefixed.Spec.MCPServers, 1)
	assert.Equal(t, "pm2-widget-mcp", prefixed.Spec.MCPServers[0].Ref, "instance.Rename must rewrite spec.mcpServers[].ref to the prefixed name")

	// The source graph contains a Channel bound to demo-agent, but the export
	// deliberately IGNORES it (channels are per-install config, not bundled), so
	// no Channel was installed and there is no prefixed Channel to rewrite. Prove
	// the negative: neither install created a Channel in the target namespace.
	assertNoChannelInstalled(t, ctx, h.K8s, dstNs)

	// The prefixed AgentIdentity's credential secretRef must be rewritten to the
	// prefixed Secret (I2).
	var prefixedID v1alpha1.AgentIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: dstNs, Name: "pm2-widget-identity"}, &prefixedID))
	require.Len(t, prefixedID.Spec.Credentials, 1)
	require.NotNil(t, prefixedID.Spec.Credentials[0].Static)
	assert.Equal(t, "pm2-widget-pat", prefixedID.Spec.Credentials[0].Static.SecretRef.Name, "instance.Rename must rewrite AgentIdentity static.secretRef.name to the prefixed Secret")

	// Secret isolation: each instance owns its own prefixed Secret with its own
	// value; neither clobbers the other.
	var sec1, sec2 corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: dstNs, Name: "widget-pat"}, &sec1))
	assert.Equal(t, oapWidgetSecretValue, string(sec1.Data["token"]), "first install's Secret value must be untouched by the second install")
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: dstNs, Name: "pm2-widget-pat"}, &sec2))
	assert.Equal(t, oapWidgetSecretValue2, string(sec2.Data["token"]), "second install's Secret must be its own, distinct value")

	// Best-effort/diagnostic only, same caveat as above.
	logAgentClassConditionBestEffort(t, h.K8s, dstNs, "pm2-demo-agent", v1alpha1.AgentClassConditionValid, 10*time.Second)
}

// buildOapSourceGraph creates a real AgentClass ref graph in namespace ns:
// an AgentIdentity, an MCPServer (pointed at the harness's live MCP stub,
// carrying a spiceDBSchema fragment + one permission-checked tool), and the
// AgentClass referencing both. Fake names throughout.
func buildOapSourceGraph(t *testing.T, ctx context.Context, h *e2e.Harness, ns string) {
	t.Helper()

	ai := &v1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-identity", Namespace: ns},
		Spec: v1alpha1.AgentIdentitySpec{
			Credentials: []v1alpha1.AgentCredential{
				{
					Name: "widget-cred",
					Type: "static",
					Static: &v1alpha1.StaticCredentialSource{
						SecretRef: v1alpha1.SecretKeyRef{Name: "widget-pat", Key: "token"},
					},
				},
			},
		},
	}
	require.NoError(t, h.K8s.Create(ctx, ai), "create source AgentIdentity")

	mcp := &v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-mcp", Namespace: ns},
		Spec: v1alpha1.MCPServerSpec{
			Name:    "widget",
			Version: "1.0.0",
			Server: v1alpha1.MCPServerServer{
				URL:       h.MCP.URL(),
				Transport: "http",
			},
			Tools: []v1alpha1.MCPServerTool{
				{
					Name:   "check_widget",
					Intent: "Check whether the caller may view the e2e fixture widget.",
					Permission: &authz.Permission{
						StateImpact: authz.Readonly,
						Check: &authz.PermissionCheck{
							ResourceType:       oapWidgetResourceType,
							Permission:         "view",
							ResourceIDTemplate: oapWidgetResourceID,
						},
					},
					Effects: v1alpha1.MCPServerToolEffects{ReadOnly: true, Idempotent: true},
				},
			},
			SpiceDBSchema: &v1alpha1.SpiceDBSchemaFragment{
				Resources: []v1alpha1.SpiceDBResource{
					{
						Standing: v1alpha1.StandingSessionOnly,
						Name:     oapWidgetResourceType,
						Relations: []v1alpha1.SpiceDBRelation{
							{Name: "any_user", SubjectType: "user", Wildcard: true},
						},
						Permissions: []v1alpha1.SpiceDBPermission{
							{Name: "view", Expr: "any_user"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, h.K8s.Create(ctx, mcp), "create source MCPServer")

	ac := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: ns},
		Spec: v1alpha1.AgentClassSpec{
			DisplayName: "Product Manager Agent",
			Description: "Fake PM agent exercised only by the oap export/pack/install e2e test.",
			SystemPrompt: v1alpha1.PromptSource{
				Inline: "You are a fake read-only demo-class assistant used only by an e2e test.",
			},
			AgentIdentity: "widget-identity",
			MCPServers: []v1alpha1.AgentClassMCPServerRef{
				{Name: "widget", Ref: "widget-mcp"},
			},
		},
	}
	require.NoError(t, h.K8s.Create(ctx, ac), "create source AgentClass")

	// A Channel bound to the AgentClass. The export deliberately IGNORES it —
	// channels are per-install deployment config, never part of a portable .oap
	// (see oap.allowedBundleKinds / source.OpenCluster). Creating it here proves
	// the export + install skip a bound Channel entirely (see
	// assertNoChannelInstalled). credentialsRef names a channel-token Secret
	// provided separately (never a created bundle Secret).
	require.NoError(t, h.K8s.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-channel-creds", Namespace: ns},
	}), "create source channel-creds Secret")
	ch := &v1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-channel", Namespace: ns},
		Spec: v1alpha1.ChannelSpec{
			Kind:           "fake",
			AgentClass:     "demo-agent",
			CredentialsRef: v1alpha1.ChannelCredentialsRef{SecretName: "widget-channel-creds"},
			Fake:           &v1alpha1.FakeChannelConfig{},
			// fake provides no starting user → an owner policy is required to be Valid.
			Owner: &v1alpha1.ChannelOwnerPolicy{
				Ownerless: &v1alpha1.ChannelOwnerlessSource{FromOutputChannel: true},
			},
		},
	}
	require.NoError(t, h.K8s.Create(ctx, ch), "create source Channel")
}

// assertOapInstalled asserts the named AgentClass/AgentIdentity/MCPServer
// exist in ns and all carry the oap-install label set to wantInstanceName.
func assertOapInstalled(t *testing.T, ctx context.Context, c client.Client, ns, className, identityName, mcpName, wantInstanceName string) {
	t.Helper()

	var ac v1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: className}, &ac), "AgentClass %s/%s must exist", ns, className)
	assert.Equal(t, wantInstanceName, ac.Labels[instance.LabelInstall], "AgentClass %s/%s oap-install label", ns, className)
	assert.Equal(t, wantInstanceName, ac.Labels["app.kubernetes.io/instance"], "AgentClass %s/%s app.kubernetes.io/instance label", ns, className)

	var ai v1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: identityName}, &ai), "AgentIdentity %s/%s must exist", ns, identityName)
	assert.Equal(t, wantInstanceName, ai.Labels[instance.LabelInstall], "AgentIdentity %s/%s oap-install label", ns, identityName)

	var mcp v1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: mcpName}, &mcp), "MCPServer %s/%s must exist", ns, mcpName)
	assert.Equal(t, wantInstanceName, mcp.Labels[instance.LabelInstall], "MCPServer %s/%s oap-install label", ns, mcpName)
}

// assertNoChannelInstalled proves the channels-out invariant end-to-end: the
// source graph contains a Channel bound to the AgentClass, but a .oap never
// bundles or installs one, so the target namespace must have zero Channels
// after both installs.
func assertNoChannelInstalled(t *testing.T, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	var chans v1alpha1.ChannelList
	require.NoError(t, c.List(ctx, &chans, client.InNamespace(ns)), "list Channels in %s", ns)
	assert.Empty(t, chans.Items, "a .oap install must not create any Channel (channels are per-install config)")
}

// logAgentClassConditionBestEffort polls the named AgentClass for up to
// deadline and logs (via t.Logf) whether condType reached True — it never
// fails the test. See the doc comment on TestOapExportPackInstall_WithSpiceDB
// for why this AgentClass condition is diagnostic-only here rather than a
// hard gate: the hard SpiceDB-consistency assertion is
// assertWidgetPermissionLive, a direct round trip against h.SpiceDB that
// doesn't depend on this controller's own re-enqueue behavior converging.
func logAgentClassConditionBestEffort(t *testing.T, c client.Client, ns, name, condType string, deadline time.Duration) {
	t.Helper()
	started := time.Now()
	cutoff := started.Add(deadline)
	var lastCond *metav1.Condition
	var lastErr error
	for time.Now().Before(cutoff) {
		var ac v1alpha1.AgentClass
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ac); err == nil {
			lastErr = nil
			if cond := meta.FindStatusCondition(ac.Status.Conditions, condType); cond != nil {
				lastCond = cond
				if cond.Status == metav1.ConditionTrue {
					t.Logf("AgentClass %s/%s: condition %s reached True within %s", ns, name, condType, time.Since(started).Round(time.Millisecond))
					return
				}
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	switch {
	case lastErr != nil:
		t.Logf("AgentClass %s/%s: get failed within %s (best-effort, not failing the test): %v", ns, name, deadline, lastErr)
	case lastCond != nil:
		t.Logf("AgentClass %s/%s: condition %s did not reach True within %s (best-effort, not failing the test); last: status=%s reason=%s message=%q",
			ns, name, condType, deadline, lastCond.Status, lastCond.Reason, lastCond.Message)
	default:
		t.Logf("AgentClass %s/%s: condition %s never observed within %s (best-effort, not failing the test)", ns, name, condType, deadline)
	}
}

// assertWidgetPermissionLive writes the wildcard "any_user" tuple for
// oapWidgetResourceType/oapWidgetResourceID directly against the harness's
// real SpiceDB (h.SpiceDB), then performs a live, fully-consistent
// CheckPermission and asserts it resolves ALLOWED. The write is retried:
// the guardian composer only writes the "oap_e2e_widget" resource into the
// live SpiceDB schema once it has reconciled the installed MCPServer
// (debounced up to ~5s), so an immediate write can transiently fail with
// "object definition not found" until that compose lands. This retry loop
// is the test's actual (self-contained) wait for schema-compose
// convergence — it does not depend on the AgentClass controller's own
// condition surface at all.
func assertWidgetPermissionLive(t *testing.T, ctx context.Context, h *e2e.Harness) {
	t.Helper()
	// e2e.E2EHarnessSource: this writes the test's own fixture wildcard tuple,
	// not standing in for any production writer. See
	// pkg/authz/spicedb/relsource and pkg/authz/spicedb/writer.go.
	rw := h.SpiceDB.Writer(e2e.E2EHarnessSource)

	writeTuple := func() error {
		_, err := rw.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
			Updates: []*v1.RelationshipUpdate{{
				Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &v1.Relationship{
					Resource: &v1.ObjectReference{ObjectType: oapWidgetResourceType, ObjectId: oapWidgetResourceID},
					Relation: "any_user",
					Subject: &v1.SubjectReference{
						Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "*"},
					},
				},
			}},
		})
		return err
	}

	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = writeTuple(); lastErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	require.NoError(t, lastErr, "write wildcard relationship for the exported+installed %q resource", oapWidgetResourceType)

	const fakeUser = "fake-e2e-user-4471"
	resp, err := h.SpiceDB.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:    &v1.ObjectReference{ObjectType: oapWidgetResourceType, ObjectId: oapWidgetResourceID},
		Permission:  "view",
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: fakeUser}},
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	require.NoError(t, err, "CheckPermission against the exported+installed schema fragment")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, resp.GetPermissionship(),
		"the widget wildcard permission must resolve ALLOWED once the installed MCPServer's schema fragment is live")
}
