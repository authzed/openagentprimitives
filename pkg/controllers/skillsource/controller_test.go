package skillsource

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	bundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
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

	// Registers the static/oauth/federated credkind Kinds: resolveToken's and
	// credentialSecretName's credkindregistry.Get dispatch resolve in this
	// package's (untagged) test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SkillSource{}).
		Build()
}

// newSecretReader builds a Warn-mode SecretReader backed by the fake client
// (serves as both the label-filtered cache and the API reader in tests).
func newSecretReader(c client.Client) *adoptguard.SecretReader {
	return adoptguard.NewSecretReader(c, c, adoptguard.Warn,
		func(types.NamespacedName) bool { return false })
}

// fixtures returns a SkillSource + the AgentIdentity/Secret backing its PAT.
// The Secret is pre-stamped with the adoption label so the guarded SecretReader
// accepts it during reconcile.
func fixtures() (*v1.SkillSource, *v1.AgentIdentity, *corev1.Secret) {
	src := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns", UID: "uid-src"},
		Spec: v1.SkillSourceSpec{
			RepoURL: "https://github.com/someorg/somerepo",
			Ref:     "v1.2.0",
			Subpath: "skills",
			Auth:    &v1.SkillSourceAuth{AgentIdentity: "id", Credential: "github_pat"},
		},
	}
	id := &v1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id", Namespace: "ns"},
		Spec: v1.AgentIdentitySpec{
			Credentials: []v1.AgentCredential{{
				Name: "github_pat", Type: "static",
				Static: &v1.StaticCredentialSource{SecretRef: v1.SecretKeyRef{Name: "pat", Key: "token"}},
				// Scoped to the host spec.repoURL names. A clone credential
				// with no allowedHosts is refused outright: repoURL and auth
				// are two independent tenant-writable fields, so without a
				// scope the operator would send this PAT to whatever host the
				// same tenant wrote.
				AllowedHosts: []string{"github.com"},
			}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pat", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("ghp_x")},
	}
	adoptguard.WithAdoptedLabel(sec)
	return src, id, sec
}

// srcKey is the request/lookup key for the fixture SkillSource.
var srcKey = types.NamespacedName{Namespace: "ns", Name: "src"}

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

// reconcileSrc reconciles the fixture SkillSource once.
func reconcileSrc(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err, "Reconcile must not return an error (failures are condition-surfaced)")
}

func TestReconcileRecreatesDeletedSkill(t *testing.T) {
	// A Skill deleted out from under the controller must be recreated on the next
	// sync even though neither the resolved SHA nor the generation moved. Status
	// reporting Ready/Synced is not evidence that the owned object still exists.
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md": []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
	})

	reconcileSrc(t, r)

	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	require.Len(t, skills.Items, 1, "precondition: the first sync materializes one Skill")
	require.NoError(t, c.Delete(context.Background(), &skills.Items[0]), "delete the materialized Skill")

	reconcileSrc(t, r)

	var after v1.SkillList
	require.NoError(t, c.List(context.Background(), &after, client.InNamespace("ns")))
	assert.Len(t, after.Items, 1, "a deleted Skill must be recreated on the next sync")

	var got v1.SkillSource
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
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md":       []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
		"skills/skillone/scripts/run.sh": []byte("echo hi"),
	})

	reconcileSrc(t, r)

	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
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
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md":       []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
		"skills/skillone/scripts/run.sh": []byte("echo hi"),
	})

	reconcileSrc(t, r)

	var first v1.SkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &first))
	require.NotNil(t, first.Status.LastSyncTime, "precondition: the first sync stamps lastSyncTime")

	reconcileSrc(t, r)

	var second v1.SkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &second))
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a no-change pass must not write status at all, or the self-watch re-enqueues forever")
	require.NotNil(t, second.Status.LastSyncTime)
	assert.Equal(t, *first.Status.LastSyncTime, *second.Status.LastSyncTime)
	assert.Equal(t, first.Status.ResolvedSHA, second.Status.ResolvedSHA)
	assert.Equal(t, first.Status.DiscoveredSkills, second.Status.DiscoveredSkills)
}

func TestReconcileEmptySecretValueSetsNotReady(t *testing.T) {
	// A present-but-EMPTY credential value must fail closed. skillfetch
	// documents an empty Token as "clone anonymously", so falling through would
	// clone as nobody and report Synced (public repo) or FetchFailed (private)
	// instead of AuthResolveFailed. Pins the namespaced half of the shared
	// TokenFromSecret check, whose cluster-scoped twin had drifted.
	src, id, sec := fixtures()
	sec.Data["token"] = []byte("")
	c := newClient(t, src, id, sec)
	r := newReconciler(c, bundlemem.New(), "abc123def456", map[string][]byte{
		"skills/skillone/SKILL.md": []byte("---\nname: skillone\ndescription: Use for one.\n---\nBody."),
	})

	reconcileSrc(t, r)

	var got v1.SkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &got))
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v1.ReasonSkillSourceAuthResolveFailed, ready.Reason)

	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	assert.Empty(t, skills.Items, "nothing may materialize from a clone the credential never authorized")
}

func TestReconcileMaterializesSkills(t *testing.T) {
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)

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
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
	})
	require.NoError(t, err)

	// A Skill was created, owned by the SkillSource, with provenance set.
	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	require.Len(t, skills.Items, 1)
	sk := skills.Items[0]
	assert.Equal(t, "github.com/someorg/somerepo//skills/skillone@v1.2.0", sk.Spec.CanonicalName)
	assert.Equal(t, "Body.", sk.Spec.Body)
	require.NotNil(t, sk.Spec.Source)
	assert.Equal(t, "abc123def456", sk.Spec.Source.ResolvedSHA)
	assert.Equal(t, "src", sk.Spec.Source.SourceName)
	require.Len(t, sk.OwnerReferences, 1)
	assert.Equal(t, "src", sk.OwnerReferences[0].Name)

	// Status reflects the sync.
	var got v1.SkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "src"}, &got))
	assert.Equal(t, "abc123def456", got.Status.ResolvedSHA)
	assert.Equal(t, int32(1), got.Status.DiscoveredSkills)
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

func TestReconcileSurfacesDiscoveryProblemsInStatus(t *testing.T) {
	// A fetched tree with one VALID skill and one INVALID skill (its
	// frontmatter name does not match its directory basename → rejected by
	// validate.Skill). The valid skill must materialize; the invalid one is
	// skipped but surfaced in status.discoveryProblems, and Ready stays True.
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)

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
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
	})
	require.NoError(t, err)

	// Only the valid skill materialized.
	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	require.Len(t, skills.Items, 1, "only the valid skill should materialize")
	assert.Equal(t, "github.com/someorg/somerepo//skills/skillone@v1.2.0", skills.Items[0].Spec.CanonicalName)

	// Status: Ready=True, DiscoveryProblems names the skipped skill.
	var got v1.SkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "src"}, &got))
	assert.Equal(t, int32(1), got.Status.DiscoveredSkills)
	require.NotEmpty(t, got.Status.DiscoveryProblems, "skipped skill must surface in status.discoveryProblems")
	assert.Contains(t, got.Status.DiscoveryProblems[0], "skilltwo", "problem should name the skipped skill")

	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "Ready stays True despite a skipped skill")
	assert.Contains(t, ready.Message, "status.discoveryProblems", "Ready message points at status, not logs")
}

func TestReconcileSubpathMatchingNothingIsNotReady(t *testing.T) {
	// The whole symptom, end to end: spec.subpath names a directory the fetched
	// tree does not have, so the pass materializes nothing. The clone worked and
	// no SKILL.md was rejected, so before this the source reported Ready=True /
	// Synced with an empty status.discoveryProblems — and the only thing that
	// looked wrong anywhere was the AgentClass opting into the skill, whose
	// diagnostic sends the operator to that empty problem list.
	src, id, sec := fixtures() // spec.subpath is "skills"
	c := newClient(t, src, id, sec)

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

	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	assert.Empty(t, skills.Items, "nothing under spec.subpath means nothing materializes")

	var got v1.SkillSource
	require.NoError(t, c.Get(context.Background(), srcKey, &got))
	assert.Equal(t, int32(0), got.Status.DiscoveredSkills)
	require.Len(t, got.Status.DiscoveryProblems, 1,
		"the subpath that matched nothing must be named in status.discoveryProblems")
	assert.Contains(t, got.Status.DiscoveryProblems[0], "skills",
		"the problem names the subpath")

	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status,
		"a source that materialized nothing is not Ready")
	assert.Equal(t, v1.ReasonSkillSourceNoSkillsDiscovered, ready.Reason)
}

func TestReconcileAdoptsPreExistingSkill(t *testing.T) {
	// A Skill with the correct slug but NO owner references already exists (e.g.
	// created manually or from a previous run that predates GC adoption). After
	// reconcile the SkillSource must be set as the controller owner so k8s GC
	// and self-heal work correctly.
	src, id, sec := fixtures()

	// Compute the slug the same way upsertSkill will.
	canonName := "github.com/someorg/somerepo//skills/skillone@v1.2.0"
	parsed, err := canonical.Parse(canonName)
	require.NoError(t, err, "parse canonical name for pre-existing skill")
	slug := parsed.SafeSlug()

	// Pre-create the Skill with no owner refs.
	preExisting := &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slug,
			Namespace: "ns",
			// OwnerReferences intentionally empty
		},
		Spec: v1.SkillSpec{CanonicalName: canonName},
	}

	c := newClient(t, src, id, sec, preExisting)

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

	var reconcileErr error
	_, reconcileErr = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
	})
	require.NoError(t, reconcileErr)

	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	require.Len(t, skills.Items, 1)
	sk := skills.Items[0]

	// The SkillSource must now be the controller owner.
	require.Len(t, sk.OwnerReferences, 1, "pre-existing Skill must be adopted with one owner ref")
	ownerRef := sk.OwnerReferences[0]
	assert.Equal(t, "src", ownerRef.Name)
	assert.Equal(t, "uid-src", string(ownerRef.UID))
	assert.Equal(t, "SkillSource", ownerRef.Kind)
	require.NotNil(t, ownerRef.Controller)
	assert.True(t, *ownerRef.Controller, "owner ref must be the controller owner")
}

func TestReconcileAuthFailureSetsNotReady(t *testing.T) {
	src, id, _ := fixtures() // no Secret → adopt+SecretRead fails → Ready=False
	c := newClient(t, src, id)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(), Fetcher: &skillfetch.Fake{}}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
	})
	require.NoError(t, err) // reconcile errors are surfaced as conditions, not returned

	var got v1.SkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "src"}, &got))
	ready := apimeta.FindStatusCondition(got.Status.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v1.ReasonSkillSourceAuthResolveFailed, ready.Reason)
}

func TestResolveTokenUnregisteredCredentialTypeNamesTheSkillSource(t *testing.T) {
	// NOTE: the old switch's fallthrough already failed closed on an
	// unregistered type — as of the Task 5 credresolve migration,
	// credresolve.ResolveSecretValue itself dispatches through the
	// credkind registry and errors on an unknown type, and the old
	// switch's default path called exactly that. So an assertion of
	// "err is non-nil" (or that it merely names the type) passes against
	// BOTH the old switch and the new registry-based resolveToken and
	// proves nothing about this change.
	//
	// What Task 7 actually changed here: one locate/adopt/read path
	// instead of two hand-copied static/oauth blocks plus a fallthrough,
	// and an error that names the SkillSource itself (namespace/name) —
	// the old fallthrough wrapped only auth.Credential, the bare
	// credential name, with no SkillSource identity at all. That's the
	// value this test pins: assert the ns/name wrap, which only the new
	// code produces (verified by reading the old code at git rev
	// c011ee73f: its fallthrough is
	// `fmt.Errorf("resolve credential %q: %w", auth.Credential, err)` —
	// no "skillsource ns/name" substring appears in that message).
	src, id, _ := fixtures()
	id.Spec.Credentials[0].Type = "nosuch"
	id.Spec.Credentials[0].Static = nil
	c := newClient(t, src, id)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	got, err := r.resolveToken(context.Background(), src)
	require.Error(t, err, "an unregistered credential type must error, not fall through to an empty token")
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), fmt.Sprintf("skillsource %s/%s", src.Namespace, src.Name),
		"the error must name the SkillSource itself (namespace/name), which only the new locate step produces — the old fallthrough named just the credential")
}

func TestResolveTokenOAuthCredentialReadsAccessToken(t *testing.T) {
	// The regression this guards: credkind.SecretRef.Key is deliberately
	// empty for oauth (a fixed multi-key Secret shape — access_token,
	// refresh_token, expires_at, ...). SecretRef locates the Secret;
	// ReadStoredValue (here, via credresolve.ResolveSecretValue) is what
	// extracts the value, because only the oauth Kind knows its own key
	// convention. Indexing the Secret directly with SecretRef.Key would
	// silently read sec.Data[""] — always absent — and this path had no
	// test before this change, oauth or otherwise.
	src := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns", UID: "uid-src"},
		Spec: v1.SkillSourceSpec{
			RepoURL: "https://github.com/someorg/somerepo",
			Ref:     "v1.2.0",
			Auth:    &v1.SkillSourceAuth{AgentIdentity: "id", Credential: "gh_oauth"},
		},
	}
	id := &v1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id", Namespace: "ns"},
		Spec: v1.AgentIdentitySpec{
			Credentials: []v1.AgentCredential{{
				Name: "gh_oauth", Type: "oauth",
				OAuth:        &v1.OAuthCredentialSource{SecretRef: v1.SecretRef{Name: "gh-oauth-secret"}},
				AllowedHosts: []string{"github.com"},
			}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-oauth-secret", Namespace: "ns"},
		Data:       map[string][]byte{"access_token": []byte("gho_livetoken")},
	}
	adoptguard.WithAdoptedLabel(sec)
	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	got, err := r.resolveToken(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, "gho_livetoken", got,
		"resolveToken must return the oauth Secret's access_token value, not an empty string from indexing with SecretRef's (deliberately empty) Key")
}

func TestResolveTokenOAuthCredentialExpiredIsAnError(t *testing.T) {
	// The same path as above, but past expires_at: must fail closed rather
	// than clone with a stale token. This expiry gate is new on this path —
	// the old hardcoded "access_token" read never checked expires_at at
	// all — and is a byproduct of extracting values through the shared
	// ReadStoredValue rather than a regression; still worth pinning since
	// nothing else exercises it for SkillSource.
	src := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns", UID: "uid-src"},
		Spec: v1.SkillSourceSpec{
			RepoURL: "https://github.com/someorg/somerepo",
			Auth:    &v1.SkillSourceAuth{AgentIdentity: "id", Credential: "gh_oauth"},
		},
	}
	id := &v1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id", Namespace: "ns"},
		Spec: v1.AgentIdentitySpec{
			Credentials: []v1.AgentCredential{{
				Name: "gh_oauth", Type: "oauth",
				OAuth:        &v1.OAuthCredentialSource{SecretRef: v1.SecretRef{Name: "gh-oauth-secret"}},
				AllowedHosts: []string{"github.com"},
			}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-oauth-secret", Namespace: "ns"},
		Data: map[string][]byte{
			"access_token": []byte("gho_stale"),
			"expires_at":   []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)),
		},
	}
	adoptguard.WithAdoptedLabel(sec)
	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c)}

	_, err := r.resolveToken(context.Background(), src)
	require.Error(t, err, "an expired oauth token must fail closed rather than clone with a stale credential")
	assert.ErrorIs(t, err, credresolve.ErrExpired)
}

func TestReconcileRepoInstructions(t *testing.T) {
	// A fetched tree with a root AGENTS.md and one valid SKILL.md. Tests that:
	//   - default (no opt-out): the materialized Skill has RepoInstructions set.
	//   - DisableRepoInstructions=true: the materialized Skill's RepoInstructions is nil.
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
			src, id, sec := fixtures()
			src.Spec.DisableRepoInstructions = tc.disableRepoInstructions
			c := newClient(t, src, id, sec)

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
				NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
			})
			require.NoError(t, err)

			var skills v1.SkillList
			require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
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

func TestReconcileRepoInstructionsTruncation(t *testing.T) {
	// An AGENTS.md that exceeds the 64 KiB cap must be truncated and surfaced as
	// a DiscoveryProblem on the SkillSource status.
	overCapContent := strings.Repeat("a", 64*1024+100)
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)

	r := &Reconciler{
		Client:       c,
		SecretReader: newSecretReader(c),
		BundleStore:  bundlemem.New(),
		Fetcher: &skillfetch.Fake{Result: skillfetch.Result{
			SHA: "abc123def456",
			Files: map[string][]byte{
				"AGENTS.md":           []byte(overCapContent),
				"skills/foo/SKILL.md": []byte("---\nname: foo\ndescription: A foo skill that does foo things well enough.\n---\nbody"),
			},
		}},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
	})
	require.NoError(t, err)

	var skills v1.SkillList
	require.NoError(t, c.List(context.Background(), &skills, client.InNamespace("ns")))
	require.Len(t, skills.Items, 1)
	sk := skills.Items[0]

	// The materialized Skill must carry truncated repo instructions.
	require.NotNil(t, sk.Spec.RepoInstructions, "RepoInstructions must be set even for over-cap content")
	assert.True(t, sk.Spec.RepoInstructions.Truncated, "Truncated must be true for over-cap AGENTS.md")
	assert.Contains(t, sk.Spec.RepoInstructions.Content, "truncated",
		"Content must include the truncation marker substring")

	// The SkillSource status must surface a DiscoveryProblem naming the 64 KiB cap.
	var got v1.SkillSource
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "src"}, &got))
	require.NotEmpty(t, got.Status.DiscoveryProblems, "over-cap AGENTS.md must produce a DiscoveryProblem")
	assert.Contains(t, got.Status.DiscoveryProblems[0], "64 KiB",
		"DiscoveryProblem must mention the 64 KiB cap")
}
