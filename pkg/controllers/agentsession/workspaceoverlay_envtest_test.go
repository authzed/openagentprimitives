//go:build integration

// pkg/controllers/agentsession/workspaceoverlay_envtest_test.go
//
// Exercises ensureWorkspaceOverlay directly against the shared envtest
// apiserver client, rather than driving a full AgentSession reconcile (which
// would pull in the runner factory, RBAC, and secret-token machinery that
// this helper doesn't touch). package agentsession (internal) so the
// unexported method is callable.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestEnsureWorkspaceOverlay drives ensureWorkspaceOverlay through its two
// observable phases: (1) the overlay-cut Job gets created and the reconciler
// waits, (2) once the Job reports success the resolved status flips
// OverlayCut=true.
//
// TestEnsureWorkspaceOverlay_NoStorage and TestEnsureWorkspaceOverlay_MissingSource
// (below) cover two of the markBootFailed failure paths against this same
// minimal fixture: markBootFailed -> applyEvent tolerates a nil
// LifecycleMemory (it no-ops the signed append and still sets Phase=Failed),
// so those paths ARE reachable here without the append-only audit-signing
// infra. The failed-Job path is not covered here — see
// sidecar_reconcile_test.go / workspace_test.go for adjacent boot-fail
// coverage.
func TestEnsureWorkspaceOverlay(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	// WorkspaceSource, manually driven Ready with a base claim — the
	// materialization controller's job (git-clone into a base PVC) is out of
	// scope for this helper.
	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{
				Kind:    "git",
				Locator: "https://example.com/demo-source.git",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws), "create WorkspaceSource")

	ws.Status.BaseClaimName = "ws-base-demo-source"
	ws.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.WorkspaceSourceConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Materialized",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark WorkspaceSource Ready")

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-class", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			WorkspaceSource: &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "demo-source"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Only the fields ensureWorkspaceOverlay itself reads: Client (Get/Create/
	// status writes) and the two snapshot Job settings.
	r := &Reconciler{
		Client:                  env.Client,
		SnapshotImage:           "busybox:1.36",
		SnapshotServiceAccount:  "ap-snapshotter",
		WorkspaceReconcileImage: "my-registry/git@sha256:pinned",
	}

	// Call 1: WorkspaceSource is Ready, so the helper creates the overlay-cut
	// Job and requeues to wait for it.
	res, done, err := r.ensureWorkspaceOverlay(ctx, sess, ac, "sess1-workspace")
	require.NoError(t, err, "first ensureWorkspaceOverlay call")
	assert.False(t, done, "overlay-cut Job was just created, not yet complete")
	assert.Greater(t, res.RequeueAfter, time.Duration(0), "should requeue while waiting for the Job")

	var job batchv1.Job
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ws-overlay-sess1-workspace"}, &job),
		"overlay-cut Job should exist")

	require.NotNil(t, sess.Status.ResolvedWorkspaceSource, "ResolvedWorkspaceSource should be set")
	assert.Equal(t, "ws-base-demo-source", sess.Status.ResolvedWorkspaceSource.BaseClaimName)
	assert.False(t, sess.Status.ResolvedWorkspaceSource.OverlayCut, "not cut yet")
	// #2: the operator resolves the reconcile image (from --materialize-image)
	// and SA (from --snapshot-service-account) onto status so the runner runs
	// the operator-configured image, not a hard-coded one.
	assert.Equal(t, "my-registry/git@sha256:pinned", sess.Status.ResolvedWorkspaceSource.ReconcileImage,
		"operator must snapshot the resolved reconcile image")
	assert.Equal(t, "ap-snapshotter", sess.Status.ResolvedWorkspaceSource.ReconcileServiceAccount,
		"operator must snapshot the resolved reconcile ServiceAccount")

	// Fake the Job succeeding.
	job.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &job), "fake overlay-cut Job success")

	// Call 2: the Job is now Succeeded, so the helper reports done and marks
	// the overlay cut.
	res, done, err = r.ensureWorkspaceOverlay(ctx, sess, ac, "sess1-workspace")
	require.NoError(t, err, "second ensureWorkspaceOverlay call")
	assert.True(t, done, "overlay cut should be complete once the Job succeeded")
	require.NotNil(t, sess.Status.ResolvedWorkspaceSource)
	assert.True(t, sess.Status.ResolvedWorkspaceSource.OverlayCut, "OverlayCut should be true")

	// Call 3 (#6): a mid-session rebind must be FROZEN, not re-cut. Re-point the
	// AgentClass at a different (here, nonexistent) source and re-run: because
	// OverlayCut is already true, the helper returns done WITHOUT re-cutting or
	// even looking up the new source — the session keeps the workspace it
	// started with (re-seeding would clobber the agent's live edits).
	ac.Spec.WorkspaceSource.Ref = "some-other-source"
	res, done, err = r.ensureWorkspaceOverlay(ctx, sess, ac, "sess1-workspace")
	require.NoError(t, err, "rebind must not error (the frozen source is never looked up)")
	assert.True(t, done, "an already-cut overlay stays done across a rebind")
	assert.Zero(t, res.RequeueAfter, "no requeue — nothing to do for a frozen overlay")
	assert.Equal(t, "demo-source", sess.Status.ResolvedWorkspaceSource.Ref,
		"resolved source stays frozen at the session's original binding, not the rebind target")
	assert.True(t, sess.Status.ResolvedWorkspaceSource.OverlayCut, "OverlayCut stays true after a rebind")
}

// TestEnsureWorkspaceOverlay_NoStorage covers the dstClaim=="" boot-fail path:
// a WorkspaceSource is bound on the AgentClass but no shared-workspace
// StorageClass is configured, so there is nowhere to seed the overlay.
// markBootFailed -> applyEvent tolerates a nil LifecycleMemory (this
// Reconciler fixture doesn't wire one), so the failure is reachable without
// the append-only audit-signing infra.
func TestEnsureWorkspaceOverlay_NoStorage(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nostorage-class", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			// The referenced WorkspaceSource need not exist: dstClaim=="" short-
			// circuits before ensureWorkspaceOverlay ever looks it up.
			WorkspaceSource: &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "nostorage-source"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "nostorage-sess", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "nostorage-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	r := &Reconciler{
		Client:                 env.Client,
		SnapshotImage:          "busybox:1.36",
		SnapshotServiceAccount: "ap-snapshotter",
	}

	_, done, err := r.ensureWorkspaceOverlay(ctx, sess, ac, "")
	require.NoError(t, err, "ensureWorkspaceOverlay with empty dstClaim")
	assert.False(t, done, "no storage means the session boot-fails, not completes")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionWorkspaceStorageRequired, sess.Status.FailureReason)
}

// TestEnsureWorkspaceOverlay_MissingSource covers the boot-fail path where
// the AgentClass names a WorkspaceSource that does not exist in the
// session's namespace.
func TestEnsureWorkspaceOverlay_MissingSource(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "missingsource-class", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			WorkspaceSource: &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "does-not-exist-source"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "missingsource-sess", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "missingsource-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	r := &Reconciler{
		Client:                 env.Client,
		SnapshotImage:          "busybox:1.36",
		SnapshotServiceAccount: "ap-snapshotter",
	}

	_, done, err := r.ensureWorkspaceOverlay(ctx, sess, ac, "missingsource-sess-workspace")
	require.NoError(t, err, "ensureWorkspaceOverlay against a missing WorkspaceSource")
	assert.False(t, done, "a missing WorkspaceSource means the session boot-fails, not completes")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceMissing, sess.Status.FailureReason)
}
