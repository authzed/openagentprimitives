package clusterskillsource

import (
	"context"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	bundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillfetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1.ClusterSkillSource{}).
		Build()
}

// newSecretReader builds a Warn-mode SecretReader backed by the fake client
// (serves as both the label-filtered cache and the API reader in tests).
func newSecretReader(c client.Client) *adoptguard.SecretReader {
	return adoptguard.NewSecretReader(c, c, adoptguard.Warn,
		func(types.NamespacedName) bool { return false })
}

// fixtures returns a ClusterSkillSource + the Secret backing its PAT. The
// ClusterSkillSource is cluster-scoped (no namespace) but the auth Secret IS
// namespaced and read directly from spec.Auth.Namespace. The Secret is
// pre-stamped with the adoption label so the guarded SecretReader accepts it.
func fixtures() (*v1.ClusterSkillSource, *corev1.Secret) {
	src := &v1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", UID: "uid-src"},
		Spec: v1.ClusterSkillSourceSpec{
			RepoURL: "https://github.com/someorg/somerepo",
			Ref:     "v1.2.0",
			Subpath: "skills",
			Auth: &v1.ClusterSkillSourceAuth{
				SecretRef: v1.SecretKeyRef{Name: "pat", Key: "token"},
				Namespace: "secrets-ns",
			},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pat", Namespace: "secrets-ns"},
		Data:       map[string][]byte{"token": []byte("ghp_x")},
	}
	adoptguard.WithAdoptedLabel(sec)
	return src, sec
}

// srcKey is the request/lookup key for the fixture ClusterSkillSource
// (cluster-scoped: no namespace).
var srcKey = types.NamespacedName{Name: "src"}

// newReconciler builds a Reconciler over the fake client whose Fetcher always
// returns sha + files. Used by the re-sync tests, which reconcile twice and need
// the second pass to see an unchanged repo.
func newReconciler(c client.Client, store skillbundle.Store, sha string, files map[string][]byte) *Reconciler {
	return &Reconciler{
		Client:       c,
		SecretReader: newSecretReader(c),
		BundleStore:  store,
		Fetcher:      &skillfetch.Fake{Result: skillfetch.Result{SHA: sha, Files: files}},
	}
}

// reconcileSrc reconciles the fixture ClusterSkillSource once.
func reconcileSrc(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err, "Reconcile must not return an error (failures are condition-surfaced)")
}

func TestReconcileRecreatesDeletedClusterSkill(t *testing.T) {
	// A ClusterSkill deleted out from under the controller must be recreated on
	// the next sync even though neither the resolved SHA nor the generation
	// moved. Status reporting Ready/Synced is not evidence that the owned object
	// still exists.
	src, sec := fixtures()
	c := newClient(t, src, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md": []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
	})

	reconcileSrc(t, r)

	var skills v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &skills))
	require.Len(t, skills.Items, 1, "precondition: the first sync materializes one ClusterSkill")
	require.NoError(t, c.Delete(context.Background(), &skills.Items[0]), "delete the materialized ClusterSkill")

	reconcileSrc(t, r)

	var after v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &after))
	assert.Len(t, after.Items, 1, "a deleted ClusterSkill must be recreated on the next sync")

	var got v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &got))
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "Ready stays True after the self-heal")
}

func TestReconcileRepopulatesEmptiedBundleStore(t *testing.T) {
	// The operator gets an in-memory bundle store for every non-postgres memory
	// backend — sqlite installs (oap init --local / oap desktop) included — so a
	// restart empties it. The next sync must re-cache the bundle; otherwise a
	// pinned ref never recovers and the runner silently falls back to
	// instruction-only.
	src, sec := fixtures()
	c := newClient(t, src, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md":       []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
		"skills/skillone/scripts/run.sh": []byte("echo hi"),
	})

	reconcileSrc(t, r)

	var skills v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &skills))
	require.Len(t, skills.Items, 1)
	require.NotNil(t, skills.Items[0].Spec.Bundle, "precondition: a skill with supporting files carries a bundle")
	digest := skills.Items[0].Spec.Bundle.Digest

	present, err := r.BundleStore.Has(context.Background(), digest)
	require.NoError(t, err)
	require.True(t, present, "precondition: the first sync caches the bundle")

	// Operator restart: the process-local store comes back empty.
	r.BundleStore = bundlemem.New()
	reconcileSrc(t, r)

	present, err = r.BundleStore.Has(context.Background(), digest)
	require.NoError(t, err)
	assert.True(t, present, "an emptied bundle store must be repopulated by the next sync")
}

func TestReconcileIsQuiescentWhenNothingChanged(t *testing.T) {
	// Guard for the fix above: materializing on every pass is only safe if a
	// no-change pass writes NOTHING. The controller watches its own object with
	// no predicate, so a status write would re-enqueue the reconcile — and its
	// git fetch — forever. resourceVersion is the assertion that discriminates:
	// lastSyncTime alone does not, because it lands second-truncated and two
	// passes of one test share a second.
	src, sec := fixtures()
	c := newClient(t, src, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md":       []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
		"skills/skillone/scripts/run.sh": []byte("echo hi"),
	})

	reconcileSrc(t, r)

	var first v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &first))
	require.NotNil(t, first.Status.LastSyncTime, "precondition: the first sync stamps lastSyncTime")

	reconcileSrc(t, r)

	var second v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &second))
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a no-change pass must not write status at all, or the self-watch re-enqueues forever")
	require.NotNil(t, second.Status.LastSyncTime)
	assert.Equal(t, *first.Status.LastSyncTime, *second.Status.LastSyncTime)
	assert.Equal(t, first.Status.ResolvedSHA, second.Status.ResolvedSHA)
	assert.Equal(t, first.Status.DiscoveredSkills, second.Status.DiscoveredSkills)
}

func TestReconcileEmptySecretValueSetsNotReady(t *testing.T) {
	// A present-but-EMPTY auth value must fail closed. skillfetch documents an
	// empty Token as "clone anonymously", so falling through would clone as
	// nobody and report Synced (public repo) or FetchFailed (private) instead of
	// AuthResolveFailed — the namespaced SkillSource already rejects this.
	src, sec := fixtures()
	sec.Data["token"] = []byte("")
	c := newClient(t, src, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md": []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
	})

	reconcileSrc(t, r)

	var got v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &got))
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v1.ReasonSkillSourceAuthResolveFailed, ready.Reason)

	var skills v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &skills))
	assert.Empty(t, skills.Items, "nothing may materialize from a clone the credential never authorized")
}

func TestReconcileMaterializesClusterSkills(t *testing.T) {
	src, sec := fixtures()
	c := newClient(t, src, sec)

	r := &Reconciler{
		Client:       c,
		SecretReader: newSecretReader(c),
		BundleStore:  bundlemem.New(),
		Fetcher: &skillfetch.Fake{Result: skillfetch.Result{
			SHA: "abc123def456",
			Files: map[string][]byte{
				"skills/skillone/SKILL.md": []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
			},
		}},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "src"},
	})
	require.NoError(t, err)

	// A ClusterSkill was created (cluster-scoped: no namespace), owned by the
	// ClusterSkillSource, with provenance set.
	var skills v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &skills))
	require.Len(t, skills.Items, 1)
	sk := skills.Items[0]
	assert.Empty(t, sk.Namespace, "ClusterSkill is cluster-scoped")
	assert.Equal(t, "github.com/someorg/somerepo//skills/skillone@v1.2.0", sk.Spec.CanonicalName)
	assert.Equal(t, "Body.", sk.Spec.Body)
	require.NotNil(t, sk.Spec.Source)
	assert.Equal(t, "abc123def456", sk.Spec.Source.ResolvedSHA)
	assert.Equal(t, "src", sk.Spec.Source.SourceName)
	require.Len(t, sk.OwnerReferences, 1)
	assert.Equal(t, "src", sk.OwnerReferences[0].Name)
	assert.Equal(t, "ClusterSkillSource", sk.OwnerReferences[0].Kind)

	// Status reflects the sync.
	var got v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "src"}, &got))
	assert.Equal(t, "abc123def456", got.Status.ResolvedSHA)
	assert.Equal(t, int32(1), got.Status.DiscoveredSkills)
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

func TestReconcileSurfacesDiscoveryProblemsInStatus(t *testing.T) {
	// One VALID skill + one INVALID skill (frontmatter name ≠ directory). The
	// valid skill materializes; the invalid one is skipped but surfaced in
	// status.discoveryProblems with Ready=True.
	src, sec := fixtures()
	c := newClient(t, src, sec)

	r := &Reconciler{
		Client:       c,
		SecretReader: newSecretReader(c),
		BundleStore:  bundlemem.New(),
		Fetcher: &skillfetch.Fake{Result: skillfetch.Result{
			SHA: "abc123def456",
			Files: map[string][]byte{
				"skills/skillone/SKILL.md": []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
				// Directory is "skilltwo" but frontmatter name is "wrongname".
				"skills/skilltwo/SKILL.md": []byte("---\nname: wrongname\ndescription: Use for two.\n---\nBody."),
			},
		}},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "src"},
	})
	require.NoError(t, err)

	// Only the valid skill materialized.
	var skills v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &skills))
	require.Len(t, skills.Items, 1, "only the valid skill should materialize")
	assert.Equal(t, "github.com/someorg/somerepo//skills/skillone@v1.2.0", skills.Items[0].Spec.CanonicalName)

	// Status: Ready=True, DiscoveryProblems names the skipped skill.
	var got v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "src"}, &got))
	assert.Equal(t, int32(1), got.Status.DiscoveredSkills)
	require.NotEmpty(t, got.Status.DiscoveryProblems, "skipped skill must surface in status.discoveryProblems")
	assert.Contains(t, got.Status.DiscoveryProblems[0], "skilltwo", "problem should name the skipped skill")

	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "Ready stays True despite a skipped skill")
	assert.Contains(t, ready.Message, "status.discoveryProblems", "Ready message points at status, not logs")
}

func TestReconcileSubpathMatchingNothingIsNotReady(t *testing.T) {
	// The cluster-scoped half of the same claim. Discover and RecordSync are
	// shared with the namespaced controller (see skillsource/sync.go), so this
	// asserts the join — that this controller routes its own pass through them
	// — not the helper logic a second time.
	src, sec := fixtures() // spec.subpath is "skills"
	c := newClient(t, src, sec)

	r := &Reconciler{
		Client:       c,
		SecretReader: newSecretReader(c),
		BundleStore:  bundlemem.New(),
		Fetcher: &skillfetch.Fake{Result: skillfetch.Result{
			SHA: "abc123def456",
			Files: map[string][]byte{
				// A real, valid skill — just not under spec.subpath.
				"plugins/review/skills/review-pr/SKILL.md": []byte("---\nname: review-pr\ndescription: Use when reviewing.\n---\nBody."),
			},
		}},
	}
	reconcileSrc(t, r)

	var skills v1.ClusterSkillList
	require.NoError(t, c.List(context.Background(), &skills))
	assert.Empty(t, skills.Items, "nothing under spec.subpath means nothing materializes")

	var got v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &got))
	assert.Equal(t, int32(0), got.Status.DiscoveredSkills)
	require.Len(t, got.Status.DiscoveryProblems, 1,
		"the subpath that matched nothing must be named in status.discoveryProblems")

	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status,
		"a source that materialized nothing is not Ready")
	assert.Equal(t, v1.ReasonSkillSourceNoSkillsDiscovered, ready.Reason)
}

func TestReconcileAuthFailureSetsNotReady(t *testing.T) {
	src, _ := fixtures() // no Secret → adopt+SecretRead fails → Ready=False
	c := newClient(t, src)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(), Fetcher: &skillfetch.Fake{}}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "src"},
	})
	require.NoError(t, err) // reconcile errors are surfaced as conditions, not returned

	var got v1.ClusterSkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "src"}, &got))
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v1.ReasonSkillSourceAuthResolveFailed, ready.Reason)
}

func TestReconcileClusterSkillRepoInstructions(t *testing.T) {
	// A fetched tree with a root AGENTS.md and one valid SKILL.md. Tests that:
	//   - default (no opt-out): the materialized ClusterSkill has RepoInstructions set.
	//   - DisableRepoInstructions=true: the materialized ClusterSkill's RepoInstructions is nil.
	const agentsMDContent = "# Repo instructions\n\nDo things carefully."
	files := map[string][]byte{
		"AGENTS.md":           []byte(agentsMDContent),
		"skills/foo/SKILL.md": []byte("---\nname: foo\ndescription: A foo skill that does foo things well enough.\n---\nbody"),
	}

	cases := []struct {
		name                    string
		disableRepoInstructions bool
		wantRepoInstr           bool
	}{
		{
			name:          "default (opt-in): RepoInstructions populated from AGENTS.md",
			wantRepoInstr: true,
		},
		{
			name:                    "DisableRepoInstructions=true: RepoInstructions is nil",
			disableRepoInstructions: true,
			wantRepoInstr:           false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, sec := fixtures()
			src.Spec.DisableRepoInstructions = tc.disableRepoInstructions
			c := newClient(t, src, sec)

			r := &Reconciler{
				Client:       c,
				SecretReader: newSecretReader(c),
				BundleStore:  bundlemem.New(),
				Fetcher: &skillfetch.Fake{Result: skillfetch.Result{
					SHA:   "abc123def456",
					Files: files,
				}},
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "src"},
			})
			require.NoError(t, err)

			var skills v1.ClusterSkillList
			require.NoError(t, c.List(context.Background(), &skills))
			require.Len(t, skills.Items, 1)
			sk := skills.Items[0]

			if tc.wantRepoInstr {
				require.NotNil(t, sk.Spec.RepoInstructions, "RepoInstructions must be set")
				assert.Equal(t, "AGENTS.md", sk.Spec.RepoInstructions.SourceFile)
				assert.Equal(t, agentsMDContent, sk.Spec.RepoInstructions.Content)
				assert.False(t, sk.Spec.RepoInstructions.Truncated)
			} else {
				assert.Nil(t, sk.Spec.RepoInstructions, "RepoInstructions must be nil when disabled")
			}
		})
	}
}
