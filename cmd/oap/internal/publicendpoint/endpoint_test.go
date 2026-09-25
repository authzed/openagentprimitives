package publicendpoint

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// demoLocalURL is a stand-in for whatever address a caller says webd answers on
// from the host. The value is not special here — what each caller's real one
// has to agree with is pinned where that caller lives (installcmd's
// TestPublicEndpointLocalURLMatchesTheConfigMapSeedByteForByte).
const demoLocalURL = "http://localhost:8080"

// TestEnsureWebd_IsRefusedOnAKindWithRealIngress is the
// creation-side half of the policy. The reconciler refuses too, but by then the
// CR exists and someone has been told a tunnel is coming.
func TestEnsureWebd_IsRefusedOnAKindWithRealIngress(t *testing.T) {
	for _, key := range []string{cloud.KeyDefault, cloud.KeyGKE, cloud.KeyEKS, cloud.KeyAKS} {
		t.Run(key+": refused, and nothing is written", func(t *testing.T) {
			ctx := context.Background()
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

			err := EnsureWebd(ctx, &bytes.Buffer{}, c, cloud.MustFor(key), demoLocalURL, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key, "the refusal must name the kind that answered")
			assert.Contains(t, err.Error(), "ingress", "and say what to use instead")

			var got v1alpha1.PublicEndpointList
			require.NoError(t, c.List(ctx, &got))
			assert.Empty(t, got.Items, "a refused creation must write nothing at all")
		})
	}
}

// TestEnsureWebd_CreatesOneTargetingWebdOnATunnelKind pins the
// whole created spec, because every field of it is load-bearing: the target is
// what makes this endpoint the ConfigMap's owner, the provider is a registry
// key the operator resolves, and localURL is the value the controller
// publishes while no tunnel is up.
func TestEnsureWebd_CreatesOneTargetingWebdOnATunnelKind(t *testing.T) {
	for _, key := range []string{cloud.KeyLocal, cloud.KeyDesktop} {
		t.Run(key+": one endpoint, targeting webd", func(t *testing.T) {
			ctx := context.Background()
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

			require.NoError(t, EnsureWebd(ctx, &bytes.Buffer{}, c, cloud.MustFor(key), demoLocalURL, nil))

			var pe v1alpha1.PublicEndpoint
			require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &pe))
			assert.Equal(t, cloud.WebdServiceNamespace, pe.Spec.Target.Namespace)
			assert.Equal(t, cloud.WebdServiceName, pe.Spec.Target.Service)
			assert.Equal(t, cloud.WebdServicePort, pe.Spec.Target.Port)
			assert.True(t, cloud.IsWebdTarget(pe.Spec.Target.Namespace, pe.Spec.Target.Service),
				"only an endpoint the controller recognizes as webd's may publish webd's external URL")
			assert.Equal(t, tunnelProviderNgrok, pe.Spec.Provider)
			assert.Equal(t, cloud.WebdServiceNamespace, pe.Spec.AuthTokenRef.Namespace,
				"the ref must carry a namespace: this CRD is cluster-scoped")
			assert.Equal(t, NgrokAuthTokenSecret, pe.Spec.AuthTokenRef.Name)
			assert.Equal(t, NgrokAuthTokenKey, pe.Spec.AuthTokenRef.Key)
			assert.Equal(t, demoLocalURL, pe.Spec.LocalURL)
			assert.Empty(t, pe.Spec.ReservedDomain, "install pins no domain; the provider assigns one per session")
		})
	}
}

// TestEnsureWebd_LeavesAnExistingEndpointAlone: a re-install must
// not stomp a live tunnel's spec, which by then may carry a reservedDomain or a
// provider an operator chose.
func TestEnsureWebd_LeavesAnExistingEndpointAlone(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	strat := cloud.MustFor(cloud.KeyLocal)

	require.NoError(t, EnsureWebd(ctx, &bytes.Buffer{}, c, strat, demoLocalURL, nil))

	var pe v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &pe))
	pe.Spec.ReservedDomain = "demo-org.ngrok.app"
	require.NoError(t, c.Update(ctx, &pe))

	require.NoError(t, EnsureWebd(ctx, &bytes.Buffer{}, c, strat, "http://127.0.0.1:17080", nil),
		"a second install must succeed")

	var after v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &after))
	assert.Equal(t, "demo-org.ngrok.app", after.Spec.ReservedDomain,
		"re-install must not discard a reserved domain an operator set")
	assert.Equal(t, demoLocalURL, after.Spec.LocalURL,
		"nor rewrite the local URL from a different caller's idea of it")

	var all v1alpha1.PublicEndpointList
	require.NoError(t, c.List(ctx, &all))
	assert.Len(t, all.Items, 1, "the cluster has exactly one front door")
}

// TestEnsureWebd_RefusesAnEmptyLocalURL. The CRD marks the field
// required, so the apiserver would refuse too — but the fake client does not
// run CRD validation, and neither does a caller that forgot to pass one. A
// guessed or empty local address 404s webd for every route.
func TestEnsureWebd_RefusesAnEmptyLocalURL(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	err := EnsureWebd(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyLocal), "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "localURL")

	var got v1alpha1.PublicEndpointList
	require.NoError(t, c.List(ctx, &got))
	assert.Empty(t, got.Items)
}

func TestEnsureNgrokAuthTokenSecret(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		preSeed func(*fake.ClientBuilder) *fake.ClientBuilder
		want    string // "" means "no Secret at all"
	}{
		{
			name: "no NGROK_AUTHTOKEN: no Secret, and no error — the endpoint stays Pending",
			env:  "",
			want: "",
		},
		{
			name: "NGROK_AUTHTOKEN set: forwarded into the Secret the endpoint references",
			env:  "demo-authtoken-value",
			want: "demo-authtoken-value",
		},
		{
			name: "an existing Secret is never overwritten from a stale shell",
			env:  "demo-authtoken-value",
			preSeed: func(b *fake.ClientBuilder) *fake.ClientBuilder {
				sec := &corev1.Secret{}
				sec.Namespace = cloud.WebdServiceNamespace
				sec.Name = NgrokAuthTokenSecret
				sec.Data = map[string][]byte{NgrokAuthTokenKey: []byte("rotated-by-the-operator")}
				return b.WithObjects(sec)
			},
			want: "rotated-by-the-operator",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ngrokAuthTokenEnv, tc.env)
			ctx := context.Background()
			b := fake.NewClientBuilder().WithScheme(kube.Scheme)
			if tc.preSeed != nil {
				b = tc.preSeed(b)
			}
			c := b.Build()

			var out bytes.Buffer
			require.NoError(t, ensureNgrokAuthTokenSecret(ctx, &out, c, nil))

			var sec corev1.Secret
			err := c.Get(ctx, types.NamespacedName{
				Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
			}, &sec)
			if tc.want == "" {
				assert.Error(t, err, "no token means no Secret to create")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(sec.Data[NgrokAuthTokenKey]))
			assert.NotContains(t, out.String(), tc.want, "a credential must never be narrated")
		})
	}
}

// TestEveryPublicEndpointCreationGoesThroughTheCheckedHelper is the structural
// guarantee that no creation path can skip cloud.CheckPublicEndpointAllowed.
//
// One caller being missed is exactly how this class of defect survives: the
// check is correct, one path calls it, and the second path added six weeks
// later does not. So rather than asserting that today's callers call it, this
// asserts there is only one place a PublicEndpoint can be NAMED at all —
// EnsureWebd's file, whose helper begins with the check.
//
// THE EXEMPTION IS ONE PATH, AND IT MOVES RATHER THAN MULTIPLYING. A second
// caller that needs to name the type routes through this package instead, and
// if the door itself ever moves again, this string moves with it. A second
// exempt file re-opens the hole the guard exists to close.
//
// Two things it is careful about, because a guard with a hole is worse than no
// guard — it reads as coverage:
//
//   - The exemption is the ONE repo-relative path, never a base name. A base
//     name would exempt any file called endpoint.go anywhere, which is a name
//     several packages under cmd/ could plausibly pick — and precisely the
//     file this must not wave through.
//   - It flags any REFERENCE to the type, not only composite literals.
//     `pe := new(v1alpha1.PublicEndpoint); pe.Spec.Provider = "ngrok";
//     c.Create(ctx, pe)` builds one without a literal anywhere, and new(expr)
//     is idiomatic in this repo. Naming the type is the thing to notice; a
//     read that genuinely needs to name it (a Get into a local) is rare enough
//     to be worth adding here on purpose.
//
// Scoped to cmd/ and internal/ (both roots — a scan rooted at cmd/ alone would
// keep passing while guarding nothing on the server side). pkg/ is excluded on
// purpose: the reconciler and the API types legitimately name the object, and
// the reconciler runs its own copy of the gate.
func TestEveryPublicEndpointCreationGoesThroughTheCheckedHelper(t *testing.T) {
	// The one exempt file, as a repo-relative path.
	theOneDoor := filepath.Join("cmd", "oap", "internal", "publicendpoint", "endpoint.go")

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	require.NoError(t, err)

	// Sanity: the exemption must name a file that exists. A typo'd path would
	// make this test pass by exempting nothing and finding nothing, which is
	// the same green as real coverage.
	_, statErr := os.Stat(filepath.Join(repoRoot, theOneDoor))
	require.NoError(t, statErr, "the exempt path must name a real file")

	var offenders []string
	for _, root := range []string{"cmd", "internal"} {
		dir := filepath.Join(repoRoot, root)
		require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(repoRoot, path)
			if rerr != nil {
				rel = path
			}
			if rel == theOneDoor {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				// A file this test cannot parse is a file it cannot clear.
				offenders = append(offenders, rel+": "+perr.Error())
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if typeNameOf(n) == "PublicEndpoint" {
					offenders = append(offenders, rel)
					return false
				}
				return true
			})
			return nil
		}))
	}

	assert.Empty(t, offenders,
		"a PublicEndpoint is named outside %s, so it can be built and created without "+
			"cloud.CheckPublicEndpointAllowed; route the creation through EnsureWebdPublicEndpoint instead",
		theOneDoor)
}

// typeNameOf renders the bare type name an expression names —
// "PublicEndpoint" for `v1alpha1.PublicEndpoint`, `PublicEndpoint{}`,
// `&v1alpha1.PublicEndpoint{}` and `new(v1alpha1.PublicEndpoint)` alike, and
// "PublicEndpointList" (a different name, deliberately not matched) for a list
// read. Anything else yields "".
func typeNameOf(n ast.Node) string {
	switch t := n.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.CompositeLit:
		return typeNameOf(t.Type)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}

// TestEnsureWebd_WritesNoCredentialOnARefusedKind. The Secret
// used to be materialized beside the call site, before the refusal ran — safe
// only because a second gate happened to exclude the refusing kinds, and
// invisible to the AST guard, which watches for PublicEndpoint construction
// and not for Secrets. Behind the check, an ngrok token cannot land on a
// cluster that then refuses every tunnel it could open.
func TestEnsureWebd_WritesNoCredentialOnARefusedKind(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "demo-authtoken-value")
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.Error(t, EnsureWebd(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyGKE), demoLocalURL, nil))

	var sec corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
	}, &sec)
	assert.Error(t, err, "a refused kind must not be left holding a tunnel credential")

	// And the allowed kind still gets one, so the assertion above is about the
	// refusal and not about the Secret never being written at all.
	allowed := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	require.NoError(t, EnsureWebd(ctx, &bytes.Buffer{}, allowed, cloud.MustFor(cloud.KeyLocal), demoLocalURL, nil))
	require.NoError(t, allowed.Get(ctx, types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
	}, &sec))
	assert.Equal(t, "demo-authtoken-value", string(sec.Data[NgrokAuthTokenKey]))
}

// TestPublicEndpointGuardExemptsOnlyTheOneRepoPath drives the guard's own
// walker over a fixture tree, because both of its escape hatches were closed
// on the strength of an argument and an argument is not evidence. A guard with
// a hole is worse than no guard: it reads as coverage.
func TestPublicEndpointGuardExemptsOnlyTheOneRepoPath(t *testing.T) {
	cases := []struct {
		name string
		file string
		src  string
		want bool // true = must be flagged
	}{
		{
			name: "a composite literal is flagged",
			file: "creator.go",
			src:  "package p\nimport v1 \"x\"\nfunc f() any { return &v1.PublicEndpoint{} }\n",
			want: true,
		},
		{
			name: "new(T) with no literal anywhere is flagged too",
			file: "creator.go",
			src:  "package p\nimport v1 \"x\"\nfunc f() any { pe := new(v1.PublicEndpoint); return pe }\n",
			want: true,
		},
		{
			name: "a file merely named endpoint.go in another package is NOT exempt",
			file: "endpoint.go",
			src:  "package p\nimport v1 \"x\"\nfunc f() any { return &v1.PublicEndpoint{} }\n",
			want: true,
		},
		{
			name: "reading a PublicEndpointList is not naming a PublicEndpoint",
			file: "reader.go",
			src:  "package p\nimport v1 \"x\"\nfunc f() any { var l v1.PublicEndpointList; return &l }\n",
			want: false,
		},
		{
			name: "an unrelated type is not flagged",
			file: "other.go",
			src:  "package p\nimport v1 \"x\"\nfunc f() any { return &v1.Channel{} }\n",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), tc.file, tc.src, 0)
			require.NoError(t, err)
			var flagged bool
			ast.Inspect(f, func(n ast.Node) bool {
				if typeNameOf(n) == "PublicEndpoint" {
					flagged = true
					return false
				}
				return true
			})
			assert.Equal(t, tc.want, flagged)
		})
	}
}

// ————————————————————————————————————————————————————————————————————————
// The on-demand path: everything install answers from its flags, answered
// from the cluster instead.
// ————————————————————————————————————————————————————————————————————————

const demoDesktopURL = "http://127.0.0.1:17080"

func TestPrepareWebdOnDemandIsReadOnlyAndKeepsTheTokenPrivate(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "")
	mutations := 0
	record := func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		mutations++
		return cli.Create(ctx, obj, opts...)
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap(demoDesktopURL)).
		WithInterceptorFuncs(interceptor.Funcs{Create: record}).Build()
	const token = "private-planned-ngrok-token"
	asked := 0

	planned, err := PrepareWebdOnDemand(context.Background(), c, cloud.MustFor(cloud.KeyDesktop), func(context.Context) (string, error) {
		asked++
		return token, nil
	})
	require.NoError(t, err)
	assert.True(t, planned.NeedsReadyURL())
	assert.Equal(t, 1, asked)
	assert.Zero(t, mutations, "planning may Get/List, but must never Create/Patch/Update/Delete")
	assert.NotContains(t, fmt.Sprintf("%#v", planned), token)
	assert.Contains(t, planned.SensitiveValues(), token)
}

func TestPrepareWebdOnDemandRecordsDeclineWithoutKeepingTheLoopbackAnswer(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap(demoDesktopURL)).Build()

	planned, err := PrepareWebdOnDemand(context.Background(), c, cloud.MustFor(cloud.KeyDesktop), func(context.Context) (string, error) {
		return "", nil
	})

	require.NoError(t, err)
	assert.False(t, planned.NeedsReadyURL())
	assert.True(t, planned.ReplacesLocalURL(), "the channel wizard must ask for a reachable URL during planning")
	assert.Empty(t, planned.SensitiveValues())
}

func TestPrepareWebdOnDemandDoesNotTreatNoTerminalAsADecline(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap(demoDesktopURL)).Build()

	planned, err := PrepareWebdOnDemand(context.Background(), c, cloud.MustFor(cloud.KeyDesktop), nil)

	require.NoError(t, err)
	assert.True(t, planned.NeedsReadyURL(), "execution must preserve the existing bounded missing-credential failure")
	assert.True(t, planned.ReplacesLocalURL())
	assert.Empty(t, planned.SensitiveValues())
}

func TestWebdOnDemandPlanRollbackDeletesOnlyPrerequisitesCreatedByThisApply(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "private-rollback-token")
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap(demoDesktopURL)).Build()
	planned, err := PrepareWebdOnDemand(ctx, c, cloud.MustFor(cloud.KeyDesktop), nil)
	require.NoError(t, err)

	receipt, err := planned.Apply(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyDesktop), time.Millisecond)
	require.Error(t, err, "the fake controller never publishes a URL")
	require.NotNil(t, receipt, "a partial resolution still needs a cleanup receipt")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &v1alpha1.PublicEndpoint{}))
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
	}, &corev1.Secret{}))

	require.NoError(t, receipt.Rollback(ctx, c))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: WebdName}, &v1alpha1.PublicEndpoint{})))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
	}, &corev1.Secret{})))
}

func TestWebdOnDemandPlanRollbackRefusesAChangedPrerequisite(t *testing.T) {
	t.Setenv(ngrokAuthTokenEnv, "private-precondition-token")
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap(demoDesktopURL)).Build()
	planned, err := PrepareWebdOnDemand(ctx, c, cloud.MustFor(cloud.KeyDesktop), nil)
	require.NoError(t, err)
	receipt, err := planned.Apply(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyDesktop), time.Millisecond)
	require.Error(t, err)

	var changed v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &changed))
	changed.Annotations = map[string]string{"changed-after-resolution": "true"}
	require.NoError(t, c.Update(ctx, &changed))

	err = receipt.Rollback(ctx, c)
	require.ErrorContains(t, err, "ResourceVersion")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &v1alpha1.PublicEndpoint{}),
		"a prerequisite changed after observation is never an authorized rollback target")
}

func TestWebdOnDemandPlanRollbackNeverTargetsPreexistingObjects(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: NgrokAuthTokenSecret, Namespace: cloud.WebdServiceNamespace},
		Data:       map[string][]byte{NgrokAuthTokenKey: []byte("preexisting-token")},
	}
	endpoint := &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: WebdName},
		Status: v1alpha1.PublicEndpointStatus{
			Phase: v1alpha1.PublicEndpointPhaseReady, URL: "https://preexisting.example.test",
		},
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(secret, endpoint).Build()
	planned, err := PrepareWebdOnDemand(ctx, c, cloud.MustFor(cloud.KeyDesktop), nil)
	require.NoError(t, err)
	receipt, err := planned.Apply(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyDesktop), time.Second)
	require.NoError(t, err)

	require.NoError(t, receipt.Rollback(ctx, c))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &v1alpha1.PublicEndpoint{}))
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: NgrokAuthTokenSecret,
	}, &corev1.Secret{}))
}

// externalURLConfigMap is webd's external-URL ConfigMap holding trustedURL —
// the value that says both where webd is reachable locally and whether
// anything else already owns the answer.
func externalURLConfigMap(trustedURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.WebdExternalURLConfigMap,
			Namespace: cloud.WebdServiceNamespace,
		},
		Data: map[string]string{
			v1alpha1.WebdTrustedURLKey: trustedURL,
			v1alpha1.WebdSandboxURLKey: trustedURL,
		},
	}
}

// TestEnsureWebdOnDemand_TakesTheLocalURLFromTheConfigMap. spec.localURL is
// required and deliberately not defaulted, and the on-demand callers cannot
// know it: the desktop binds 127.0.0.1 on a port it picks at runtime. The
// cluster already records it, and a guessed address 404s webd for every route.
func TestEnsureWebdOnDemand_TakesTheLocalURLFromTheConfigMap(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap(demoDesktopURL)).Build()

	ensured, err := EnsureWebdOnDemand(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyDesktop), nil)
	require.NoError(t, err)
	assert.True(t, ensured)

	var pe v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: WebdName}, &pe))
	assert.Equal(t, demoDesktopURL, pe.Spec.LocalURL,
		"the endpoint publishes this while no tunnel is up; it must be the address webd actually answers on")
}

// TestEnsureWebdOnDemand_StandsDownWhenWebdsExternalURLHasAnOwner is
// webdExternalURLHasAnotherOwner's question asked of the cluster. An endpoint
// created against either of these would hand the two ConfigMap keys to a
// controller that re-applies them under ForceOwnership every reconcile,
// rewriting a working public hostname to a loopback address.
func TestEnsureWebdOnDemand_StandsDownWhenWebdsExternalURLHasAnOwner(t *testing.T) {
	cases := []struct {
		name       string
		trustedURL string
		wantSaid   string
	}{
		{
			name:       "a real https host: the Gateway owns it, and it already works",
			trustedURL: "https://webd.demo.test",
			wantSaid:   "already reachable",
		},
		{
			name:       "empty: the --trusted-hostname seed, still waiting for its Gateway",
			trustedURL: "",
			wantSaid:   "spoken for",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).
				WithObjects(externalURLConfigMap(tc.trustedURL)).Build()

			var out bytes.Buffer
			ensured, err := EnsureWebdOnDemand(ctx, &out, c, cloud.MustFor(cloud.KeyDesktop), nil)
			require.NoError(t, err, "somebody else owning the URL is an answer, not a failure")
			assert.False(t, ensured)
			assert.Contains(t, out.String(), tc.wantSaid, "standing down silently is the same as forgetting to")

			var got v1alpha1.PublicEndpointList
			require.NoError(t, c.List(ctx, &got))
			assert.Empty(t, got.Items)
		})
	}
}

// TestEnsureWebdOnDemand_AnExistingEndpointShortCircuitsTheOwnershipQuestion.
// Once an endpoint is up, the ConfigMap carries the TUNNEL's own public URL —
// which reads as "somebody else owns this" to the check above, and would stand
// this path down against its own endpoint.
func TestEnsureWebdOnDemand_AnExistingEndpointShortCircuitsTheOwnershipQuestion(t *testing.T) {
	ctx := context.Background()
	live := &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: WebdName},
		Spec:       v1alpha1.PublicEndpointSpec{LocalURL: demoDesktopURL},
		Status: v1alpha1.PublicEndpointStatus{
			Phase: v1alpha1.PublicEndpointPhaseReady,
			URL:   "https://demo-tunnel.demo.test",
		},
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(externalURLConfigMap("https://demo-tunnel.demo.test"), live).Build()

	ensured, err := EnsureWebdOnDemand(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyDesktop), nil)
	require.NoError(t, err)
	assert.True(t, ensured, "an endpoint that already exists is an endpoint to wait on, not a URL somebody else owns")
}

// TestEnsureWebdOnDemand_RefusesOnAKindWithRealIngress: the refusal is asked
// FIRST so the operator is told the actionable thing. Without it a durable
// cluster hears that its external-URL ConfigMap is missing — a true sentence
// about the wrong question.
func TestEnsureWebdOnDemand_RefusesOnAKindWithRealIngress(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	ensured, err := EnsureWebdOnDemand(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyGKE), nil)
	require.Error(t, err)
	assert.False(t, ensured)
	assert.Contains(t, err.Error(), cloud.KeyGKE, "the refusal must name the kind that answered")
	assert.Contains(t, err.Error(), "ingress")
}

// TestEnsureWebdOnDemand_RefusesWhenTheConfigMapIsMissing. There is no
// defaulting the local address, so "I do not know where webd answers" has to be
// said rather than guessed at.
func TestEnsureWebdOnDemand_RefusesWhenTheConfigMapIsMissing(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	ensured, err := EnsureWebdOnDemand(ctx, &bytes.Buffer{}, c, cloud.MustFor(cloud.KeyDesktop), nil)
	require.Error(t, err)
	assert.False(t, ensured)
	assert.Contains(t, err.Error(), v1alpha1.WebdExternalURLConfigMap)
	assert.Contains(t, err.Error(), "oap install")
}

// TestAwaitWebdReady_TimesOutSayingWhatTheEndpointWasLeftDoing. The bound is
// only half of it: a refusal that says "it did not become ready" and stops has
// told the operator nothing they could act on. The Ready condition's reason and
// message are where the controller puts the actionable half.
func TestAwaitWebdReady_TimesOutSayingWhatTheEndpointWasLeftDoing(t *testing.T) {
	ctx := context.Background()
	pending := &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: WebdName},
		Status: v1alpha1.PublicEndpointStatus{
			Phase: v1alpha1.PublicEndpointPhasePending,
			Conditions: []metav1.Condition{{
				Type:    v1alpha1.PublicEndpointConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.ReasonPublicEndpointAuthTokenMissing,
				Message: "no token in the referenced Secret key",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(pending).Build()

	err := awaitWebdReady(ctx, &bytes.Buffer{}, c, 50*time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "public endpoint")
	assert.Contains(t, err.Error(), v1alpha1.PublicEndpointPhasePending, "the phase it was left in")
	assert.Contains(t, err.Error(), v1alpha1.ReasonPublicEndpointAuthTokenMissing, "and why")
	assert.Contains(t, err.Error(), NgrokAuthTokenSecret, "and where to put the thing that fixes it")
}

// TestAwaitWebdReady_ReturnsAsSoonAsTheURLIsPublished, so the bound above is
// about the failing direction and not about the wait always running out.
func TestAwaitWebdReady_ReturnsAsSoonAsTheURLIsPublished(t *testing.T) {
	ctx := context.Background()
	ready := &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: WebdName},
		Status: v1alpha1.PublicEndpointStatus{
			Phase: v1alpha1.PublicEndpointPhaseReady,
			URL:   "https://demo-tunnel.demo.test",
		},
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(ready).Build()

	var out bytes.Buffer
	require.NoError(t, awaitWebdReady(ctx, &out, c, time.Minute))
	assert.Contains(t, out.String(), "https://demo-tunnel.demo.test",
		"the address is what the caller went and got; say it")
}

// TestAwaitWebdReady_RefusesAnUnboundedWait. Zero is not "forever" and not "do
// not wait": both are silent, and one of them is the hang the bound exists to
// prevent.
func TestAwaitWebdReady_RefusesAnUnboundedWait(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	err := awaitWebdReady(context.Background(), &bytes.Buffer{}, c, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no bound")
}

// TestIsLoopbackURL is the whole ownership predicate, and the rows that matter
// are the near-misses: webd dispatches on an exact bare-host match, so a host
// that merely BEGINS with a loopback name is a different origin entirely.
func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{raw: "http://localhost:8080", want: true},
		{raw: "http://127.0.0.1:17080", want: true},
		{raw: "http://[::1]:8080", want: true},
		{raw: "http://127.1.2.3:8080", want: true},
		{raw: "https://webd.demo.test", want: false},
		{raw: "http://localhost.demo.test", want: false},
		{raw: "https://demo-tunnel.ngrok.app", want: false},
		{raw: "", want: false},
		{raw: "not a url at all", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			assert.Equal(t, tc.want, isLoopbackURL(tc.raw))
		})
	}
}

// webdTargetingEndpoint is an endpoint that owns webd's external URL, under a
// name the caller picks — because the refusal must key on the TARGET and not on
// this package's own WebdName.
func webdTargetingEndpoint(name string) *v1alpha1.PublicEndpoint {
	return &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PublicEndpointSpec{
			Target: v1alpha1.PublicEndpointTarget{
				Namespace: cloud.WebdServiceNamespace,
				Service:   cloud.WebdServiceName,
				Port:      cloud.WebdServicePort,
			},
			Provider: tunnelProviderNgrok,
			LocalURL: demoLocalURL,
		},
	}
}

// TestRefuseWebdEndpointWhenURLIsClaimed_RefusesAnEndpointFromAnEarlierInstall
// is the re-install `oap install`'s creation decision structurally cannot see:
// it reads install's own flags to answer "do I create one", and an endpoint
// left by a previous run is not one of its inputs.
//
// The failure it produces is a SUCCESSFUL install. The Gateway, the certificate
// and the real hostnames all land, and the still-live PublicEndpoint controller
// re-applies the tunnel's address over the two ConfigMap keys under
// ForceOwnership on its next reconcile, with nothing said.
func TestRefuseWebdEndpointWhenURLIsClaimed_RefusesAnEndpointFromAnEarlierInstall(t *testing.T) {
	cases := []struct {
		name         string
		endpointName string
		claimedBy    string
	}{
		{
			name:         "--trusted-hostname over install's own endpoint: refused, naming both",
			endpointName: WebdName,
			claimedBy:    "--trusted-hostname",
		},
		{
			name:         "--manual-webd-routing over a hand-made endpoint: refused by TARGET, not by name",
			endpointName: "demo-operator-made",
			claimedBy:    "--manual-webd-routing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).
				WithObjects(webdTargetingEndpoint(tc.endpointName)).Build()

			err := RefuseWebdEndpointWhenURLIsClaimed(ctx, &bytes.Buffer{}, c, tc.claimedBy)

			require.Error(t, err, "two owners of one address is the failure, not a state to install into")
			assert.Contains(t, err.Error(), tc.endpointName, "the refusal must name what is in the way")
			assert.Contains(t, err.Error(), tc.claimedBy, "and the flag that claimed the address")
			assert.Contains(t, err.Error(), "kubectl delete publicendpoint "+tc.endpointName,
				"and how to remove it, since this refuses rather than tearing down a live tunnel itself")

			var got v1alpha1.PublicEndpointList
			require.NoError(t, c.List(ctx, &got))
			assert.Len(t, got.Items, 1, "a refusal deletes nothing")
		})
	}
}

// TestRefuseWebdEndpointWhenURLIsClaimed_PassesWhenNothingOwnsTheURL is the
// control: a refusal that fired on every install with a hostname would break
// every first install, which is the overwhelmingly common case.
func TestRefuseWebdEndpointWhenURLIsClaimed_PassesWhenNothingOwnsTheURL(t *testing.T) {
	cases := []struct {
		name string
		objs []client.Object
	}{
		{name: "no endpoints at all: the first install of a hostname cluster"},
		{
			name: "an endpoint tunnelling something else: not webd's URL to own",
			objs: []client.Object{&v1alpha1.PublicEndpoint{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-dashboard-tunnel"},
				Spec: v1alpha1.PublicEndpointSpec{
					Target: v1alpha1.PublicEndpointTarget{
						Namespace: "demo-observability",
						Service:   "demo-dashboard",
						Port:      3000,
					},
					Provider: tunnelProviderNgrok,
					LocalURL: demoLocalURL,
				},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(tc.objs...).Build()

			var out bytes.Buffer
			require.NoError(t, RefuseWebdEndpointWhenURLIsClaimed(
				context.Background(), &out, c, "--trusted-hostname"))
			assert.Contains(t, out.String(), "--trusted-hostname",
				"the answer to who owns the URL is said out loud either way")
		})
	}
}
