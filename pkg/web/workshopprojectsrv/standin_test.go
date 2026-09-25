package workshopprojectsrv

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// This file is a white-box companion to workshopprojectsrv_test.go (which is
// package workshopprojectsrv_test): it exercises createOrUpdateStandin and
// findStandin directly, without the full ResolveAgentsInThread/reachability
// join, because the collision-safety test below needs two DIFFERENT source
// namespaces sharing one bare stand-in name — a shape the
// agents-in-thread join cannot produce today (threadParticipants only ever
// lists peers within the BUILDER's own namespace, so two genuinely reachable
// sources can never actually collide on a bare name through the full HTTP
// path). Testing createOrUpdateStandin directly proves the collision rule
// itself, independent of whether the join can reach this shape today.

const (
	standinTestSessNS   = "builder-ns"
	standinTestSessName = "builder-sess"
	standinTestWSNS     = "ws-standin-test"
)

// standinTestWorkshop builds the Workshop CR createOrUpdateStandin resolves
// via wsKey = {standinTestSessNS, WorkshopName(standinTestSessName)},
// seeded with whatever status.standins entries a test needs already
// recorded.
func standinTestWorkshop(standins ...spiceboxv1alpha1.WorkshopStandin) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: standinTestSessNS,
			Name:      spiceboxv1alpha1.WorkshopName(standinTestSessName),
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session: spiceboxv1alpha1.NamespacedRef{Namespace: standinTestSessNS, Name: standinTestSessName},
			Limits:  spiceboxv1alpha1.WorkshopLimits{MaxAge: metav1.Duration{}, MaxObjectsPerKind: 10, MaxObjects: 50, MaxConcurrentProbes: 2},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Namespace: standinTestWSNS,
			Phase:     spiceboxv1alpha1.WorkshopPhaseReady,
			Standins:  standins,
		},
	}
}

func standinTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
}

func standinTestWSKey() types.NamespacedName {
	return types.NamespacedName{Namespace: standinTestSessNS, Name: spiceboxv1alpha1.WorkshopName(standinTestSessName)}
}

// TestCreateOrUpdateStandin_DifferentSourceSameBareName_RefusedNotUpdated:
// Workshop.status.standins already records bare name "shared-name" as
// standing in for source
// {"namespace-q", "shared-name"}. A projection call for a DIFFERENT
// reachable source ({"namespace-p", "shared-name"}) that happens to share
// the bare name must be refused — never silently converge the existing
// stand-in onto P's own spec, whatever annotation the object at that
// identity happens to carry.
func TestCreateOrUpdateStandin_DifferentSourceSameBareName_RefusedNotUpdated(t *testing.T) {
	existingStandin := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: standinTestWSNS,
			Name:      "shared-name",
			Annotations: map[string]string{
				AnnotationStandinSource: "namespace-q/shared-name",
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "Q's Own Agent",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "I am Q's agent"},
		},
	}
	ws := standinTestWorkshop(spiceboxv1alpha1.WorkshopStandin{
		Name: "shared-name", SourceNamespace: "namespace-q", SourceName: "shared-name",
	})
	c := standinTestClient(t, ws, existingStandin)

	pSource := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace-p", Name: "shared-name"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "P's Totally Different Agent",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "I am P's agent, a completely different one"},
		},
	}
	standin := projectStandin(pSource, standinTestWSNS, "namespace-p", "shared-name", nil, nil)

	err := createOrUpdateStandin(context.Background(), c, standinTestWSKey(), standin, "namespace-p", "shared-name", logr.Discard())

	var collision *errCollision
	require.ErrorAs(t, err, &collision, "a bare name status records for a DIFFERENT source must be refused, not converged")

	var stillThere spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: standinTestWSNS, Name: "shared-name"}, &stillThere))
	assert.Equal(t, "Q's Own Agent", stillThere.Spec.DisplayName,
		"the recorded source's own stand-in must be completely untouched by P's projection attempt")
	assert.Equal(t, spiceboxv1alpha1.PromptSource{Inline: "I am Q's agent"}, stillThere.Spec.SystemPrompt,
		"P's prompt must never land on Q's recorded stand-in")

	var gotWS spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), standinTestWSKey(), &gotWS))
	require.Len(t, gotWS.Status.Standins, 1, "the refused projection must not add or rewrite a status entry")
	assert.Equal(t, "namespace-q", gotWS.Status.Standins[0].SourceNamespace, "the recorded source must still be Q, never overwritten by P's attempt")
}

// TestCreateOrUpdateStandin_SameSourceReprojected_UpdatesInPlace is the
// positive control for the test above: re-projecting the EXACT source
// status already names for a bare name converges in place, so the fix does
// not turn every re-projection into a false collision.
func TestCreateOrUpdateStandin_SameSourceReprojected_UpdatesInPlace(t *testing.T) {
	existingStandin := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: standinTestWSNS,
			Name:      "shared-name",
			Annotations: map[string]string{
				AnnotationStandinSource: "namespace-q/shared-name",
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "Q's Own Agent",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "an older prompt"},
		},
	}
	ws := standinTestWorkshop(spiceboxv1alpha1.WorkshopStandin{
		Name: "shared-name", SourceNamespace: "namespace-q", SourceName: "shared-name",
	})
	c := standinTestClient(t, ws, existingStandin)

	qSource := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace-q", Name: "shared-name"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "Q's Own Agent, Updated",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "an updated prompt"},
		},
	}
	standin := projectStandin(qSource, standinTestWSNS, "namespace-q", "shared-name", nil, nil)

	err := createOrUpdateStandin(context.Background(), c, standinTestWSKey(), standin, "namespace-q", "shared-name", logr.Discard())
	require.NoError(t, err, "re-projecting the exact source status already names must converge, not collide")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: standinTestWSNS, Name: "shared-name"}, &got))
	assert.Equal(t, spiceboxv1alpha1.PromptSource{Inline: "an updated prompt"}, got.Spec.SystemPrompt)
}

// TestCreateOrUpdateStandin_RecordedEntryObjectDeleted_ReprojectionSucceeds
// is m1's own test: status.standins still names "shared-name" for source
// {"namespace-q","shared-name"}, but the backing AgentClass was deleted
// directly (e.g. workshop_delete) — a dangling registry entry. Re-projecting
// the SAME source must succeed (create the object fresh) rather than 500 on
// a NotFound Get, and the dangling entry must be pruned rather than left to
// keep pointing at nothing.
func TestCreateOrUpdateStandin_RecordedEntryObjectDeleted_ReprojectionSucceeds(t *testing.T) {
	// No existingStandin object seeded — only the status entry, simulating a
	// direct delete of the AgentClass without clearing the registry.
	ws := standinTestWorkshop(spiceboxv1alpha1.WorkshopStandin{
		Name: "shared-name", SourceNamespace: "namespace-q", SourceName: "shared-name",
	})
	c := standinTestClient(t, ws)

	qSource := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace-q", Name: "shared-name"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "Q's Own Agent, Re-projected",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "re-created after deletion"},
		},
	}
	standin := projectStandin(qSource, standinTestWSNS, "namespace-q", "shared-name", nil, nil)

	err := createOrUpdateStandin(context.Background(), c, standinTestWSKey(), standin, "namespace-q", "shared-name", logr.Discard())
	require.NoError(t, err, "re-projecting a source whose prior stand-in object was deleted must succeed, not 500 on NotFound")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: standinTestWSNS, Name: "shared-name"}, &got),
		"the stand-in must actually have been (re-)created")
	assert.Equal(t, spiceboxv1alpha1.PromptSource{Inline: "re-created after deletion"}, got.Spec.SystemPrompt)
}

// TestCreateOrUpdateStandin_ForgedAnnotationOnAuthoredClass_RefusedAsCollision:
// an AgentClass the workshop's builder authored itself, carrying a FORGED
// AnnotationStandinSource (a builder's own apply tool can server-side-apply
// arbitrary annotations; the admission webhook never inspects them), must
// still be refused as a collision when a DIFFERENT source projects onto its
// bare name — status, not the annotation, is what createOrUpdateStandin
// trusts, and status records nothing at this identity at all.
func TestCreateOrUpdateStandin_ForgedAnnotationOnAuthoredClass_RefusedAsCollision(t *testing.T) {
	forged := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: standinTestWSNS,
			Name:      "support-class",
			Annotations: map[string]string{
				// A forged marker: this class was never actually projected by
				// this route, so status.standins (below) never recorded it.
				AnnotationStandinSource: "some-other-namespace/some-other-class",
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "The Builder's Own Draft, Wearing a Forged Marker",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "this is real, authored work"},
		},
	}
	ws := standinTestWorkshop() // status.standins deliberately empty
	c := standinTestClient(t, ws, forged)

	source := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace-p", Name: "support-class"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "A Genuinely Different Agent",
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "a genuinely different prompt"},
		},
	}
	standin := projectStandin(source, standinTestWSNS, "namespace-p", "support-class", nil, nil)

	err := createOrUpdateStandin(context.Background(), c, standinTestWSKey(), standin, "namespace-p", "support-class", logr.Discard())

	var collision *errCollision
	require.ErrorAs(t, err, &collision, "a forged AnnotationStandinSource must not let a projection treat a builder-authored class as a prior stand-in it may overwrite")

	var stillThere spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: standinTestWSNS, Name: "support-class"}, &stillThere))
	assert.Equal(t, "The Builder's Own Draft, Wearing a Forged Marker", stillThere.Spec.DisplayName,
		"the builder's own authored class, forged annotation and all, must be completely untouched")
}

// TestCreateOrUpdateStandin_AtPerKindCap_RefusedWithActionableMessage:
// this route writes as the trusted operator and so bypasses the admission
// webhook's checkLimits (which only ever fires for a write attributed to
// the sidecar's own SA). A FRESH projection (a bare name status has no
// entry for at all) must be refused once the workshop namespace already
// holds MaxObjectsPerKind AgentClass objects — counted against every
// AgentClass actually there (m7), not len(status.standins) alone, which
// would undercount against a workshop that also holds builder-authored
// drafts.
func TestCreateOrUpdateStandin_AtPerKindCap_RefusedWithActionableMessage(t *testing.T) {
	ws := standinTestWorkshop(spiceboxv1alpha1.WorkshopStandin{
		Name: "already-recorded", SourceNamespace: "namespace-q", SourceName: "already-recorded",
	})
	ws.Spec.Limits.MaxObjectsPerKind = 1 // already at the cap with one entry
	// The recorded entry's own backing AgentClass must actually exist —
	// otherwise the dead-entry prune (m1) would correctly remove it and the
	// cap, now counted against real objects, would no longer be at capacity.
	alreadyRecorded := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: standinTestWSNS, Name: "already-recorded"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "A Prior Stand-in"},
	}
	c := standinTestClient(t, ws, alreadyRecorded)

	source := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace-p", Name: "a-new-name"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "A New Agent"},
	}
	standin := projectStandin(source, standinTestWSNS, "namespace-p", "a-new-name", nil, nil)

	err := createOrUpdateStandin(context.Background(), c, standinTestWSKey(), standin, "namespace-p", "a-new-name", logr.Discard())
	require.Error(t, err, "a fresh projection must be refused once the workshop's AgentClass count is at the per-kind cap")
	assert.Contains(t, err.Error(), "limit", "the refusal must name the limit so a builder can act on it")

	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(standinTestWSNS)))
	assert.Len(t, list.Items, 1, "a capped-out projection must not create anything beyond what already existed")
}

// TestCreateOrUpdateStandin_StatusRecordFailsEveryRetry_CompensatingDeleteRemovesOrphan:
// the AgentClass CREATE lands, but
// EVERY attempt to record it on Workshop.status.standins fails (an
// interceptor fails every SubResourceUpdate, standing in for the ordinary
// case — five other writers touch this same Workshop's status). The route
// must not leave the just-created AgentClass behind as an orphan the
// registry has no record of: it must be deleted, and the failure must say
// so.
func TestCreateOrUpdateStandin_StatusRecordFailsEveryRetry_CompensatingDeleteRemovesOrphan(t *testing.T) {
	ws := standinTestWorkshop() // no prior stand-ins recorded
	statusUpdateCalls := 0
	// A CONFLICT on every attempt: retry.RetryOnConflict only retries a
	// conflict (correctly — retrying a non-transient error is pointless), so
	// this is what an ordinary "five other writers touch this Workshop's
	// status" pile-up looks like when it never clears within the retry
	// budget, not a synthetic non-conflict error RetryOnConflict would
	// (rightly) refuse to retry at all.
	conflictErr := apierrors.NewConflict(schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "workshops"}, standinTestWSKey().Name, errors.New("stale resourceVersion"))
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(ws).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusUpdateCalls++
				return conflictErr
			},
		}).
		Build()

	source := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace-p", Name: "new-agent"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "A New Agent"},
	}
	standin := projectStandin(source, standinTestWSNS, "namespace-p", "new-agent", nil, nil)

	err := createOrUpdateStandin(context.Background(), c, standinTestWSKey(), standin, "namespace-p", "new-agent", logr.Discard())
	require.Error(t, err, "a status record that never succeeds must surface as a failure, not a silent success")
	assert.Contains(t, err.Error(), "orphan", "the failure must say the just-created AgentClass was compensated for, not merely that the status write failed")
	assert.Greater(t, statusUpdateCalls, 1, "recordStandinStatus must actually retry, not fail on the first attempt alone")

	var list spiceboxv1alpha1.AgentClassList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(standinTestWSNS)))
	assert.Empty(t, list.Items, "no orphan AgentClass may survive a status record that never succeeded")

	var gotWS spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), standinTestWSKey(), &gotWS))
	assert.Empty(t, gotWS.Status.Standins, "a failed record must never appear to have succeeded")
}

// TestUpdateStandinWithRetry_RetriesOnceOnConflict:
// createOrUpdateStandin's update-in-place path must retry once on a
// conflicting ResourceVersion rather than surfacing a raw error the first
// time two calls interleave — Task 2's project_agent tool makes concurrent
// re-projection of the same source reachable.
func TestUpdateStandinWithRetry_RetriesOnceOnConflict(t *testing.T) {
	existing := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: standinTestWSNS, Name: "shared-name"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "Old"},
	}
	calls := 0
	conflictErr := apierrors.NewConflict(schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "agentclasses"}, "shared-name", errors.New("stale resourceVersion"))
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				calls++
				if calls == 1 {
					return conflictErr
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()

	standin := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: standinTestWSNS, Name: "shared-name"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "New"},
	}
	err := updateStandinWithRetry(context.Background(), c, standin)
	require.NoError(t, err, "a single conflict must be retried, not surfaced")
	assert.Equal(t, 2, calls, "exactly one retry: a fresh Get+Update after the first conflict")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: standinTestWSNS, Name: "shared-name"}, &got))
	assert.Equal(t, "New", got.Spec.DisplayName, "the retried update must actually land")
}

// TestUpdateStandinWithRetry_MergesAnnotationsRatherThanReplacing: an
// existing stand-in carrying some OTHER annotation (stamped by something
// other than this route) must keep it after a re-projection updates
// AnnotationStandinSource — a bare `existing.Annotations =
// standin.Annotations` would silently discard it.
func TestUpdateStandinWithRetry_MergesAnnotationsRatherThanReplacing(t *testing.T) {
	existing := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: standinTestWSNS, Name: "shared-name",
			Annotations: map[string]string{
				AnnotationStandinSource:                       "namespace-q/shared-name",
				"kubectl.kubernetes.io/some-other-annotation": "keep-me",
			},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{DisplayName: "Old"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(existing).Build()

	standin := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: standinTestWSNS, Name: "shared-name",
			Annotations: map[string]string{AnnotationStandinSource: "namespace-q/shared-name"},
		},
		Spec: spiceboxv1alpha1.AgentClassSpec{DisplayName: "New"},
	}
	require.NoError(t, updateStandinWithRetry(context.Background(), c, standin))

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: standinTestWSNS, Name: "shared-name"}, &got))
	assert.Equal(t, "New", got.Spec.DisplayName)
	assert.Equal(t, "keep-me", got.Annotations["kubectl.kubernetes.io/some-other-annotation"],
		"an annotation this route did not itself stamp must survive a re-projection update")
	assert.Equal(t, "namespace-q/shared-name", got.Annotations[AnnotationStandinSource])
}
