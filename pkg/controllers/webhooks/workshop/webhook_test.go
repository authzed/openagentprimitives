package workshop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Fixture identity shared by every test in this file: session builder-s1 in
// namespace "default" owns a Ready Workshop provisioned at namespace "ws-abc".
const (
	sessNS   = "default"
	session  = "builder-s1"
	wsSAUser = "system:serviceaccount:default:builder-s1-workshop-sa"
	wsNS     = "ws-abc"
)

// fakeChecker is the WorkshopBuildChecker test double. err, when set, wins
// over allow — a test asserting the tuple-error path must not also need
// allow:false to make the point.
type fakeChecker struct {
	allow bool
	err   error
}

func (f fakeChecker) CheckWorkshopBuild(_ context.Context, _, _, _ string) (bool, error) {
	return f.allow, f.err
}

func baseWorkshop(muts ...func(*v1.Workshop)) *v1.Workshop {
	ws := &v1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: v1.WorkshopName(session)},
		Spec: v1.WorkshopSpec{
			Session:        v1.NamespacedRef{Namespace: sessNS, Name: session},
			SidecarToolbox: "workshop-sidecar",
			Limits:         v1.WorkshopLimits{MaxObjectsPerKind: 5, MaxObjects: 20},
		},
		Status: v1.WorkshopStatus{Namespace: wsNS, Phase: v1.WorkshopPhaseReady},
	}
	for _, m := range muts {
		m(ws)
	}
	return ws
}

func baseNamespace(muts ...func(*corev1.Namespace)) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: wsNS,
			Labels: map[string]string{
				v1.LabelWorkshopSessionNamespace: sessNS,
				v1.LabelWorkshopSessionName:      session,
			},
		},
	}
	for _, m := range muts {
		m(ns)
	}
	return ns
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// newWebhook builds a Webhook over a fresh fake client seeded with exactly
// fixtures — callers pass baseWorkshop()/baseNamespace() (or mutated
// variants) plus whatever else the case needs. Each test constructs its own
// client so List-based limit counts in one case can never see another case's
// objects.
func newWebhook(t *testing.T, checker WorkshopBuildChecker, fixtures ...client.Object) *Webhook {
	t.Helper()
	return newWebhookWithRegistry(t, checker, "", fixtures...)
}

// newWebhookWithRegistry is newWebhook plus an explicit trustedImageRegistry
// — used by the SidecarToolbox image-gate rows, where the registry itself is
// the thing under test.
func newWebhookWithRegistry(t *testing.T, checker WorkshopBuildChecker, trustedImageRegistry string, fixtures ...client.Object) *Webhook {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(fixtures...).Build()
	return New(c, checker, trustedImageRegistry, admission.NewDecoder(s))
}

func reqFor(t *testing.T, kind, ns, username string, op admissionv1.Operation, obj runtime.Object) admission.Request {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Kind: kind},
		Operation: op,
		Namespace: ns,
		UserInfo:  authenticationv1.UserInfo{Username: username},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

// --- per-kind object factories, defaulted to pass every content rule -------

func agentClass(name string, muts ...func(*v1.AgentClass)) *v1.AgentClass {
	ac := &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNS, Name: name},
		Spec: v1.AgentClassSpec{
			SystemPrompt: v1.PromptSource{Inline: "you are a helpful workshop-built agent"},
		},
	}
	for _, m := range muts {
		m(ac)
	}
	return ac
}

func mcpServer(name string, muts ...func(*v1.MCPServer)) *v1.MCPServer {
	m := &v1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNS, Name: name},
		Spec: v1.MCPServerSpec{
			Name: name, Version: "v1",
			Server: v1.MCPServerServer{URL: "https://example.com/mcp"},
		},
	}
	for _, mu := range muts {
		mu(m)
	}
	return m
}

// adapterConfigFixture is a syntactically valid apiadapter.Config with one
// operation ("get_account") — the shape TestParse_Valid in
// pkg/tools/apiadapter/config_test.go pins, trimmed to one operation and
// re-hosted at a fixture (".test" TLD) hostname that still clears
// safehttp.GuardHost. auth: none needs no credential envVar, keeping the
// fixture focused on what these rows test (operation-name/spec.tools
// agreement), not auth wiring apiadapter's own tests already cover.
const adapterConfigFixture = `
baseURL: https://api.example.test
auth: {type: none}
operations:
  - name: get_account
    description: Fetch one account by id.
    method: GET
    path: /accounts/{id}
    params:
      - {name: id, in: path, required: true, type: string}
`

// authedAdapterConfigFixture is adapterConfigFixture's auth-required
// sibling — same one operation, but auth.type "bearer" with an explicit
// envVar — so MINOR-2's upstreamAuth.envVar/auth.envVar agreement check has
// something to check (adapterConfigFixture's auth.type "none" never reaches
// it, per apiadapter.Config.UpstreamEnvVarMismatch).
const authedAdapterConfigFixture = `
baseURL: https://api.example.test
auth: {type: bearer, envVar: UPSTREAM_TOKEN}
operations:
  - name: get_account
    description: Fetch one account by id.
    method: GET
    path: /accounts/{id}
    params:
      - {name: id, in: path, required: true, type: string}
`

// sidecarToolbox defaults to the bare local-dev image form
// (apimage.Image.LocalRef's shape, "<name>:<tag>", no registry) so it passes
// checkSidecarImage under the default "" trustedImageRegistry every other
// content-rule row in this table runs with. Rows that test the image gate
// itself override both the image and the webhook's trustedImageRegistry
// together — see newWebhookWithRegistry.
func sidecarToolbox(name string, muts ...func(*v1.SidecarToolbox)) *v1.SidecarToolbox {
	st := &v1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNS, Name: name},
		Spec: v1.SidecarToolboxSpec{
			Name: name, Version: "v1",
			Source:  v1.SidecarToolboxSource{Image: "ap-workshop:dev"},
			Sandbox: v1.SidecarToolboxSandbox{Class: "allowed-class"},
		},
	}
	for _, m := range muts {
		m(st)
	}
	return st
}

func subagentRequest(name string, muts ...func(*v1.SubagentRequest)) *v1.SubagentRequest {
	sr := &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNS, Name: name},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: sessNS, Name: session},
			Class:  "some-child-class",
			Task:   "do one bounded thing",
			Mode:   v1.SubagentModeSingleTurn,
		},
	}
	for _, m := range muts {
		m(sr)
	}
	return sr
}

func spiceboxToolspec(name string, muts ...func(*v1.SpiceboxToolspec)) *v1.SpiceboxToolspec {
	ts := &v1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1.LabelWorkshopNamespace: wsNS},
		},
		Spec: v1.SpiceboxToolspecSpec{
			Toolkit:          v1.ToolspecToolkitRef{Name: "bash", Revision: "v1"},
			AllowSubcommands: []string{"ls"},
		},
	}
	for _, m := range muts {
		m(ts)
	}
	return ts
}

func spiceboxToolkit(name string, muts ...func(*v1.SpiceboxToolkit)) *v1.SpiceboxToolkit {
	tk := &v1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1.LabelWorkshopNamespace: wsNS},
		},
		Spec: v1.SpiceboxToolkitSpec{
			Name: "bash", ToolkitRevision: "v1",
		},
	}
	for _, m := range muts {
		m(tk)
	}
	return tk
}

func spiceboxClass(name, kind string) *v1.SpiceboxClass {
	return &v1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.SpiceboxClassSpec{Sandbox: v1.SandboxBackend{Kind: kind}},
	}
}

// --- attribution (step 1) ---------------------------------------------------

func TestHandle_Attribution(t *testing.T) {
	cases := []struct {
		name     string
		username string
		allowed  bool
		contains string
	}{
		{
			name:     "not a workshop SA at all (a runner SA): allowed, RBAC is its control",
			username: "system:serviceaccount:default:builder-s1-runner-sa",
			allowed:  true,
		},
		{
			name:     "human administrator: allowed, RBAC is its control",
			username: "kubernetes-admin",
			allowed:  true,
		},
		{
			// Fail closed: looks like a workshop SA, does not parse into a
			// session. A guard whose unrecognized direction is permissive is
			// not a guard.
			name:     "malformed workshop-sa principal: denied",
			username: "system:serviceaccount:default:-workshop-sa",
			allowed:  false, contains: "cannot be attributed",
		},
		{
			// Same fail-closed property one input shape earlier: the prefix
			// and suffix both match (the same two checks matchConditions
			// would use) but there is no colon separating namespace from name.
			name:     "workshop-sa-shaped principal with no namespace/name colon: denied",
			username: "system:serviceaccount:foo-workshop-sa",
			allowed:  false, contains: "cannot be attributed",
		},
	}
	w := newWebhook(t, fakeChecker{allow: true}, baseWorkshop(), baseNamespace())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := w.Handle(context.Background(),
				reqFor(t, "AgentClass", wsNS, tc.username, admissionv1.Create, agentClass("c1")))
			assert.Equal(t, tc.allowed, res.Allowed, "Allowed")
			if tc.contains != "" {
				require.NotNil(t, res.Result)
				assert.Contains(t, res.Result.Message, tc.contains)
			}
		})
	}
}

// --- workshop resolution (step 2) ------------------------------------------

func TestHandle_WorkshopResolution(t *testing.T) {
	t.Run("session has no workshop CR: denied", func(t *testing.T) {
		w := newWebhook(t, fakeChecker{allow: true}) // no Workshop, no Namespace seeded
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "has no workshop")
	})

	t.Run("workshop CR exists but is not yet provisioned: denied", func(t *testing.T) {
		unprovisioned := baseWorkshop(func(ws *v1.Workshop) { ws.Status.Namespace = "" })
		w := newWebhook(t, fakeChecker{allow: true}, unprovisioned, baseNamespace())
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "not yet provisioned")
	})

	t.Run("object written into a namespace that is not this session's workshop: denied", func(t *testing.T) {
		w := newWebhook(t, fakeChecker{allow: true}, baseWorkshop(), baseNamespace())
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", "some-other-ns", wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "own workshop namespace")
	})

	t.Run("workshop namespace exists but is labeled for a different session: denied", func(t *testing.T) {
		mislabeled := baseNamespace(func(ns *corev1.Namespace) {
			ns.Labels[v1.LabelWorkshopSessionName] = "someone-else"
		})
		w := newWebhook(t, fakeChecker{allow: true}, baseWorkshop(), mislabeled)
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "not labeled for session")
	})

	t.Run("namespaced object in the right, correctly-labeled workshop namespace: allowed", func(t *testing.T) {
		w := newWebhook(t, fakeChecker{allow: true}, baseWorkshop(), baseNamespace())
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.True(t, res.Allowed)
	})
}

// --- the SpiceDB standing tuple (step 3) ------------------------------------

func TestHandle_Tuple(t *testing.T) {
	t.Run("tuple confirms build: allowed", func(t *testing.T) {
		w := newWebhook(t, fakeChecker{allow: true}, baseWorkshop(), baseNamespace())
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.True(t, res.Allowed)
	})

	t.Run("tuple says false regardless of otherwise-compliant content: denied", func(t *testing.T) {
		w := newWebhook(t, fakeChecker{allow: false}, baseWorkshop(), baseNamespace())
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "does not hold build permission")
	})

	t.Run("tuple check errors: denied (fail closed, not a maybe)", func(t *testing.T) {
		w := newWebhook(t, fakeChecker{allow: true, err: assert.AnError}, baseWorkshop(), baseNamespace())
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("c1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "could not confirm workshop")
	})
}

// --- per-kind content rules (step 4) ----------------------------------------

func TestHandle_ContentRules(t *testing.T) {
	cases := []struct {
		name                 string
		kind                 string
		obj                  runtime.Object
		fixtures             []client.Object
		trustedImageRegistry string // "" (the zero value) is local-dev: bare images only.
		allowed              bool
		contains             string
	}{
		// MCPServer -----------------------------------------------------
		{
			name:    "MCPServer: no spicedbSchema, safe host: allowed",
			kind:    "MCPServer",
			obj:     mcpServer("m1"),
			allowed: true,
		},
		{
			name: "MCPServer: spicedbSchema fragment present: denied",
			kind: "MCPServer",
			obj: mcpServer("m1", func(m *v1.MCPServer) {
				m.Spec.SpiceDBSchema = &v1.SpiceDBSchemaFragment{RawZed: "definition foo {}"}
			}),
			allowed: false, contains: "spicedbSchema must be absent",
		},
		{
			name: "MCPServer: server.url is a cloud-metadata address: denied",
			kind: "MCPServer",
			obj: mcpServer("m1", func(m *v1.MCPServer) {
				m.Spec.Server.URL = "http://169.254.169.254/latest/meta-data/"
			}),
			allowed: false, contains: "is refused",
		},

		// SidecarToolbox --------------------------------------------------
		{
			name:     "SidecarToolbox: first-party image, no secrets, allowed sandbox class: allowed",
			kind:     "SidecarToolbox",
			obj:      sidecarToolbox("t1"),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  true,
		},
		{
			name: "SidecarToolbox: image not in the first-party allowlist: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "docker.io/library/anything-else:latest"
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "not one of the first-party workshop images",
		},
		// Fix round 1 (CRITICAL): the pre-fix name-basename match admitted
		// any registry as long as the last path segment was "ap-workshop" —
		// confirmed empirically against the exact removed function: all
		// three refs below returned allowed=true under it. These rows pin
		// the full registry, not just the image name.
		{
			name: "SidecarToolbox (fix round 1): image name matches but registry is a foreign one: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "someregistry.io/evil/ap-workshop:v1"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "ghcr.io/example",
			allowed:              false, contains: "not from this cluster's trusted first-party registry",
		},
		{
			name: "SidecarToolbox (fix round 1): digest-pinned ref from a foreign registry, same org shape: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "myevil.io/authzed/ap-workshop@sha256:" + strings.Repeat("ab", 32)
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "ghcr.io/example",
			allowed:              false, contains: "not from this cluster's trusted first-party registry",
		},
		{
			name: "SidecarToolbox (fix round 1): bare name (no registry) once a trusted registry is configured: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-workshop:dev"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "ghcr.io/example",
			allowed:              false, contains: "names no registry",
		},
		{
			name: "SidecarToolbox (fix round 1): the real trusted registry, exact match: allowed",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ghcr.io/example/ap-workshop:v1"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "ghcr.io/example",
			allowed:              true,
		},
		{
			name: "SidecarToolbox (fix round 1): bare name on a local cluster with no configured registry: allowed",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-workshop:dev"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "",
			allowed:              true,
		},
		// Fix round 2 (CRITICAL): round 1 detected "no explicit registry" via
		// the PARSED repo.RegistryStr() == name.DefaultRegistry, but
		// go-containerregistry normalizes a genuinely bare ref AND an
		// EXPLICIT "docker.io/…"/"index.docker.io/…" one to the identical
		// value — so under an empty trustedImageRegistry (every local/desktop
		// install), a Docker-Hub-hosted "attacker/ap-workshop" was admitted
		// exactly like the real bare name. Confirmed empirically: reproducing
		// the exact round-1 checkSidecarImage (commit f9c29ea5d) in a
		// throwaway test and running it against these three refs returned ""
		// (admitted) for all three, before it was deleted.
		{
			name: "SidecarToolbox (fix round 2): explicit docker.io ref under empty trust: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "docker.io/attacker/ap-workshop:v1"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "",
			allowed:              false, contains: "not from this cluster's trusted first-party registry",
		},
		{
			name: "SidecarToolbox (fix round 2): explicit index.docker.io ref under empty trust: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "index.docker.io/attacker/ap-workshop:v1"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "",
			allowed:              false, contains: "not from this cluster's trusted first-party registry",
		},
		{
			name: "SidecarToolbox (fix round 2): implicit-docker.io org ref (no registry segment, two path segments) under empty trust: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "authzed/ap-workshop:v1"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "",
			allowed:              false, contains: "names no registry",
		},
		{
			name: "SidecarToolbox (fix round 2): explicit docker.io ref while a trusted registry IS configured: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "docker.io/attacker/ap-workshop:v1"
			}),
			fixtures:             []client.Object{spiceboxClass("allowed-class", "")},
			trustedImageRegistry: "ghcr.io/example",
			allowed:              false, contains: "not from this cluster's trusted first-party registry",
		},
		{
			name: "SidecarToolbox: secretInputs set: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.SecretInputs = []v1.SidecarToolboxSecretInput{{Name: "TOKEN", Deliver: "env", From: "producer-secret"}}
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "secretInputs must be empty",
		},
		{
			name:     "SidecarToolbox: sandbox class resolves to a non-allowlisted kind: denied",
			kind:     "SidecarToolbox",
			obj:      sidecarToolbox("t1", func(st *v1.SidecarToolbox) { st.Spec.Sandbox.Class = "denied-class" }),
			fixtures: []client.Object{spiceboxClass("denied-class", "agent-sandbox")},
			allowed:  false, contains: "not in the allowed set",
		},
		{
			name: "SidecarToolbox: inline/adapter source (image empty): denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source = v1.SidecarToolboxSource{Inline: &v1.SidecarToolboxInlineSource{
					BaseImage:  "attacker.example/base:latest",
					Entrypoint: []string{"/run.sh"},
				}}
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "inline is not permitted",
		},

		// SidecarToolbox: ap-api-adapter config carrier (plan 7b task 3) -----
		{
			name: "SidecarToolbox: ap-api-adapter with a valid config whose operations exactly match spec.tools: allowed",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = adapterConfigFixture
				st.Spec.Tools = []v1.MCPServerTool{{Name: "get_account"}}
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  true,
		},
		{
			name: "SidecarToolbox: ap-api-adapter with NO config: denied, a config is required for that image",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "config is required for image ap-api-adapter",
		},
		{
			name: "SidecarToolbox: ap-api-adapter config fails apiadapter.Parse: denied with the parser's own words",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = "baseURL: https://api.example.test\nauth: {type: bogus}\noperations: []"
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			// Distinctive fragment proving the DENIAL carries the parser's own
			// message verbatim, not a re-derived summary: the "apiadapter:"
			// prefix (config.go's error convention) together with the
			// offending field ("auth.type") the parser itself named.
			allowed: false, contains: "apiadapter: auth.type",
		},
		{
			name: "SidecarToolbox: ap-api-adapter config baseURL is a cloud-metadata/link-local address: denied with the SSRF guard's own refusal",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = "baseURL: http://169.254.169.254\nauth: {type: none}\n"
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			// Distinctive fragment proving the denial carries safehttp.GuardHost's
			// own words (mirrors the MCPServer 169.254 row above): apiadapter.Parse
			// runs the same SSRF guard as every other baseURL/URL in this webhook.
			allowed: false, contains: "is a blocked (private/loopback/link-local) address",
		},
		{
			name: "SidecarToolbox: ap-api-adapter valid config but an operation is missing from spec.tools: denied naming it",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = adapterConfigFixture
				// spec.tools left empty: the config's one operation, get_account,
				// has no matching tools entry.
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "config has [get_account]",
		},
		{
			name: "SidecarToolbox: ap-api-adapter valid config but spec.tools names an extra operation: denied naming it",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = adapterConfigFixture
				st.Spec.Tools = []v1.MCPServerTool{{Name: "get_account"}, {Name: "extra_tool"}}
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "extra_tool",
		},
		{
			name: "SidecarToolbox: ap-api-adapter auth.envVar agrees with spec.upstreamAuth.envVar: allowed",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = authedAdapterConfigFixture
				st.Spec.UpstreamAuth = v1.SidecarToolboxUpstream{Provider: "some-provider", EnvVar: "UPSTREAM_TOKEN"}
				st.Spec.Tools = []v1.MCPServerTool{{Name: "get_account"}}
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  true,
		},
		{
			name: "SidecarToolbox (MINOR-2): ap-api-adapter auth.envVar disagrees with spec.upstreamAuth.envVar: denied — would otherwise only fail at sidecar boot",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Source.Image = "ap-api-adapter:dev"
				st.Spec.Config = authedAdapterConfigFixture
				st.Spec.UpstreamAuth = v1.SidecarToolboxUpstream{Provider: "some-provider", EnvVar: "WRONG_VAR_NAME"}
				st.Spec.Tools = []v1.MCPServerTool{{Name: "get_account"}}
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "UPSTREAM_TOKEN",
		},
		{
			name: "SidecarToolbox: non-adapter first-party image (ap-workshop) with a non-empty config: denied",
			kind: "SidecarToolbox",
			obj: sidecarToolbox("t1", func(st *v1.SidecarToolbox) {
				st.Spec.Config = adapterConfigFixture
			}),
			fixtures: []client.Object{spiceboxClass("allowed-class", "")},
			allowed:  false, contains: "spec.config is not consumed by image ap-workshop",
		},

		// AgentClass --------------------------------------------------------
		{
			name: "AgentClass: systemPrompt.configMapRef set: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.SystemPrompt = v1.PromptSource{ConfigMapRef: &v1.ConfigMapKeyRef{Name: "cm", Key: "prompt"}}
			}),
			allowed: false, contains: "systemPrompt must be inline",
		},
		{
			name:    "AgentClass: subagents entry names a bare in-namespace class: allowed",
			kind:    "AgentClass",
			obj:     agentClass("c1", func(ac *v1.AgentClass) { ac.Spec.Subagents = []string{"child-class"} }),
			allowed: true,
		},
		{
			name:    "AgentClass: subagents entry looks like a qualified/foreign reference: denied",
			kind:    "AgentClass",
			obj:     agentClass("c1", func(ac *v1.AgentClass) { ac.Spec.Subagents = []string{"other-ns/child-class"} }),
			allowed: false, contains: "workshop namespace",
		},
		{
			name: "AgentClass: capabilities grants agent_builder: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{
					agentBuilderCapabilityKey: {Raw: []byte("true")},
				}
			}),
			allowed: false, contains: "agent_builder",
		},
		{
			name: "AgentClass: authz.session.allowedStarters set: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.Authz = &v1.AuthzBlock{Session: &v1.SessionAuthz{AllowedStarters: []string{"user:someone"}}}
			}),
			allowed: false, contains: "allowedStarters must be empty",
		},
		{
			name: "AgentClass: authz.session.interactPermission set: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.Authz = &v1.AuthzBlock{Session: &v1.SessionAuthz{InteractPermission: "group:eng#member"}}
			}),
			allowed: false, contains: "interactPermission must be empty",
		},
		// Fix round 1 (IMPORTANT): spec.skills[].ref was never inspected, so a
		// workshop-authored class could name a git-sourced or cluster-scoped
		// (ClusterSkill) skill — a cluster-scoped reference exactly like an
		// unbound SpiceboxClass, and the outside-the-build-space fetch spec §1
		// forbids. Only the reserved "local" authority is permitted.
		{
			name: "AgentClass (ANY-object cluster ref, fix round 1): skills[].ref names the local authority: allowed",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.Skills = []v1.AgentSkill{{Name: "s1", Ref: "local//foo"}}
			}),
			allowed: true,
		},
		{
			name: "AgentClass (ANY-object cluster ref, fix round 1): skills[].ref names a git-sourced skill outside the build space: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.Skills = []v1.AgentSkill{{Name: "s1", Ref: "github.com/org/repo//skills/x@v1"}}
			}),
			allowed: false, contains: "this agent references a skill from outside its build space",
		},
		{
			name: "AgentClass (ANY-object cluster ref): toolBundles[].class names a SpiceboxClass directly: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.ToolBundles = []v1.ToolBundle{{Name: "b1", Class: "some-cluster-class", Toolspecs: []string{}}}
			}),
			allowed: false, contains: "cluster-scoped reference",
		},
		{
			name: "AgentClass (ANY-object cluster ref): toolBundles[].toolspecs names this workshop's own toolspec: allowed",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.ToolBundles = []v1.ToolBundle{{Name: "b1", Toolspecs: []string{"ws-abc-mytool"}}}
			}),
			fixtures: []client.Object{spiceboxToolspec("ws-abc-mytool")},
			allowed:  true,
		},
		{
			name: "AgentClass (ANY-object cluster ref): toolBundles[].toolspecs names a toolspec NOT owned by this workshop: denied",
			kind: "AgentClass",
			obj: agentClass("c1", func(ac *v1.AgentClass) {
				ac.Spec.ToolBundles = []v1.ToolBundle{{Name: "b1", Toolspecs: []string{"ws-abc-someone-elses-tool"}}}
			}),
			// Correctly prefixed for THIS workshop's id but labeled for a
			// different workshop namespace — the prefix alone is not proof of
			// ownership, the label match is what the reference check demands.
			fixtures: []client.Object{spiceboxToolspec("ws-abc-someone-elses-tool", func(ts *v1.SpiceboxToolspec) {
				ts.Labels[v1.LabelWorkshopNamespace] = "ws-other"
			})},
			allowed: false, contains: "must equal",
		},

		// SpiceboxToolspec / SpiceboxToolkit (cluster-scoped) ----------------
		{
			name:    "SpiceboxToolspec: correctly prefixed name and label: allowed",
			kind:    "SpiceboxToolspec",
			obj:     spiceboxToolspec("ws-abc-mytool"),
			allowed: true,
		},
		{
			name:    "SpiceboxToolspec: name missing the workshop prefix: denied",
			kind:    "SpiceboxToolspec",
			obj:     spiceboxToolspec("mytool"),
			allowed: false, contains: "must be prefixed",
		},
		{
			name: "SpiceboxToolspec: correctly prefixed but labeled for a different workshop: denied",
			kind: "SpiceboxToolspec",
			obj: spiceboxToolspec("ws-abc-mytool", func(ts *v1.SpiceboxToolspec) {
				ts.Labels[v1.LabelWorkshopNamespace] = "ws-other"
			}),
			allowed: false, contains: "must equal",
		},
		{
			name:    "SpiceboxToolkit: correctly prefixed name and label: allowed",
			kind:    "SpiceboxToolkit",
			obj:     spiceboxToolkit("ws-abc-mytoolkit"),
			allowed: true,
		},
		{
			name:    "SpiceboxToolkit: name missing the workshop prefix: denied",
			kind:    "SpiceboxToolkit",
			obj:     spiceboxToolkit("mytoolkit"),
			allowed: false, contains: "must be prefixed",
		},

		// SubagentRequest -----------------------------------------------------
		{
			name:    "SubagentRequest: parent is the workshop's own session, mode single_turn: allowed",
			kind:    "SubagentRequest",
			obj:     subagentRequest("sr1"),
			allowed: true,
		},
		{
			name: "SubagentRequest: parent names a foreign session: denied",
			kind: "SubagentRequest",
			obj: subagentRequest("sr1", func(sr *v1.SubagentRequest) {
				sr.Spec.Parent = v1.NamespacedRef{Namespace: sessNS, Name: "some-other-session"}
			}),
			allowed: false, contains: "must be the workshop's own session",
		},
		{
			name:    "SubagentRequest: mode=task: allowed",
			kind:    "SubagentRequest",
			obj:     subagentRequest("sr1", func(sr *v1.SubagentRequest) { sr.Spec.Mode = v1.SubagentModeTask }),
			allowed: true,
		},
		{
			name:    "SubagentRequest: mode=attended (human-directed, not agent-to-agent): allowed",
			kind:    "SubagentRequest",
			obj:     subagentRequest("sr1", func(sr *v1.SubagentRequest) { sr.Spec.Mode = v1.SubagentModeAttended }),
			allowed: true,
		},
		{
			name:    "SubagentRequest: mode=chat (unbounded two-way): denied",
			kind:    "SubagentRequest",
			obj:     subagentRequest("sr1", func(sr *v1.SubagentRequest) { sr.Spec.Mode = v1.SubagentModeChat }),
			allowed: false, contains: "is not permitted from a workshop",
		},

		// Kinds with no extra content rule beyond attribution/namespace/tuple.
		{
			name: "AgentIdentity: no extra content rule, otherwise-compliant object: allowed",
			kind: "AgentIdentity",
			obj: &v1.AgentIdentity{
				ObjectMeta: metav1.ObjectMeta{Namespace: wsNS, Name: "id1"},
			},
			allowed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixtures := append([]client.Object{baseWorkshop(), baseNamespace()}, tc.fixtures...)
			w := newWebhookWithRegistry(t, fakeChecker{allow: true}, tc.trustedImageRegistry, fixtures...)
			res := w.Handle(context.Background(),
				reqFor(t, tc.kind, wsNS, wsSAUser, admissionv1.Create, tc.obj))
			assert.Equal(t, tc.allowed, res.Allowed, "Allowed")
			if tc.contains != "" {
				require.NotNil(t, res.Result, "a denial must carry a Result naming the rule")
				assert.Contains(t, res.Result.Message, tc.contains)
			}
		})
	}
}

// --- limits (step 5) --------------------------------------------------------

func TestHandle_Limits(t *testing.T) {
	t.Run("per-kind limit already met: denied", func(t *testing.T) {
		tight := baseWorkshop(func(ws *v1.Workshop) {
			ws.Spec.Limits = v1.WorkshopLimits{MaxObjectsPerKind: 1, MaxObjects: 100}
		})
		existing := agentClass("existing-1")
		w := newWebhook(t, fakeChecker{allow: true}, tight, baseNamespace(), existing)
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("new-1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "per-kind limit")
	})

	t.Run("under the per-kind limit: allowed", func(t *testing.T) {
		roomy := baseWorkshop(func(ws *v1.Workshop) {
			ws.Spec.Limits = v1.WorkshopLimits{MaxObjectsPerKind: 5, MaxObjects: 100}
		})
		existing := agentClass("existing-1")
		w := newWebhook(t, fakeChecker{allow: true}, roomy, baseNamespace(), existing)
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("new-1")))
		assert.True(t, res.Allowed)
	})

	t.Run("total object limit already met: denied even with per-kind room", func(t *testing.T) {
		tight := baseWorkshop(func(ws *v1.Workshop) {
			ws.Spec.Limits = v1.WorkshopLimits{MaxObjectsPerKind: 10, MaxObjects: 2}
		})
		w := newWebhook(t, fakeChecker{allow: true}, tight, baseNamespace(),
			agentClass("existing-1"), mcpServer("existing-2"))
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Create, agentClass("new-1")))
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "total limit")
	})

	t.Run("UPDATE never counts against limits", func(t *testing.T) {
		tight := baseWorkshop(func(ws *v1.Workshop) {
			ws.Spec.Limits = v1.WorkshopLimits{MaxObjectsPerKind: 1, MaxObjects: 1}
		})
		existing := agentClass("existing-1")
		w := newWebhook(t, fakeChecker{allow: true}, tight, baseNamespace(), existing)
		res := w.Handle(context.Background(),
			reqFor(t, "AgentClass", wsNS, wsSAUser, admissionv1.Update, agentClass("existing-1")))
		assert.True(t, res.Allowed)
	})
}

// --- misc --------------------------------------------------------------------

// TestHandle_DeleteNotDialed documents that DELETE is NOT this webhook's
// boundary. config/manager/webhook.yaml dials the workshop VWC for CREATE and
// UPDATE only, so Handle is never invoked for a DELETE in production; the
// early-Allow at webhook.go:159 is a defensive no-op for any operation this
// gate does not inspect, not a decision that a workshop MAY delete a tool CR.
//
// The actual boundary against a workshop deleting cluster-scoped tool CRs is
// RBAC: spicebox-workshop-toolwriter grants only create/update/patch (never
// delete or get), which no ValidatingWebhook could gate on a cluster-scoped
// kind anyway. That posture is asserted in the refusing direction by
// TestWorkshopRBACSufficiency (pkg/controllers/workshop) — the DENIED-delete /
// DENIED-get rows — not here. This test only pins that the early-Allow branch
// stays a no-op and never grows a hidden delete-authorizing decision.
func TestHandle_DeleteNotDialed(t *testing.T) {
	w := newWebhook(t, fakeChecker{allow: true}, baseWorkshop(), baseNamespace())
	res := w.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		Kind:      metav1.GroupVersionKind{Kind: "AgentClass"},
		Namespace: wsNS,
		UserInfo:  authenticationv1.UserInfo{Username: wsSAUser},
	}})
	// A no-op path: the webhook neither inspects nor authorizes the delete —
	// it simply is not the gate. The empty message distinguishes this
	// unconditional early-Allow from a content-rule decision.
	assert.True(t, res.Allowed, "the webhook is not dialed for DELETE, so it is a no-op pass, not a delete authorizer")
	require.NotNil(t, res.Result)
	assert.Empty(t, res.Result.Message, "the early-Allow for an un-dialed operation carries no decision message")
}
