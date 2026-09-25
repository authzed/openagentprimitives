package manifests_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// secret_bootstrap_test.go guards the "install bootstraps every Secret the
// bundle requires" invariant.
//
// A workload that references a Secret WITHOUT optional:true cannot start until
// that Secret exists — a missing one yields CreateContainerConfigError forever.
// Some component Secrets are shipped in the bundle; others (tokens, generated
// keys) are minted at install time by the ensure* helpers in
// cmd/oap/internal/installcmd/{install,init}.go. This test asserts every non-optional Secret
// reference in the always-applied manifests is satisfied by one or the other.
//
// A non-optional secretKeyRef with no creator wedges its component in
// CreateContainerConfigError on every fresh install — invisible to the envtest
// suites, which have no kubelet and never schedule a real pod.
//
// installTimeSecrets is the hand-maintained source of truth, mirroring the
// ensure* calls in cmd/oap. Adding a non-optional Secret reference to a shipped
// manifest means either shipping the Secret in the bundle or adding its creator
// both to cmd/oap and to this set — otherwise this test goes red, which is the
// point.
//
// The VALUE is load-bearing, not a comment: TestInstallTimeSecretsMapIsLoadBearing
// resolves each row over cmd/oap's AST and requires the named creator to actually
// reach this Secret name — as a string literal or a constant resolving to it, in
// its own body or in a function it calls — and requires the bundle to still
// reference the Secret. Keep the value shaped `<creatorFunc> (<file>)`; the
// first token is parsed as the identifier to look up.
var installTimeSecrets = map[string]string{
	"spicebox-channelsd-memory-token":     "ensureChannelsdMemoryToken (init.go)",
	"spicebox-webd-memory-token":          "ensureWebdMemoryToken (init.go)",
	"agentprimitives-authzd-memory-token": "ensureAuthzdMemoryToken (init.go)",
	"spicebox-spicedb-token":              "ensureSpiceDBToken (install.go)",
	"spicebox-channelsd-nats-creds":       "ensureNATSClientCreds (install.go)",
	"spicebox-webd-nats-creds":            "ensureNATSClientCreds (install.go)",
	"spicebox-passthrough-link-key":       "ensurePassthroughLinkSigningKey (install.go)",
	"spicebox-webhook-tls":                "ensureWebhookTLSWithClient (install.go)",
	// No row for spicebox-anthropic-api-key: nothing in the repo creates it.
	// Every reference to it (authzd's env, channelsd's volume) is optional:true,
	// so a row would waive nothing today and would pre-waive the first reference
	// that stopped being optional, wedging the install with nothing red to show.
}

// secretRef is one Secret consumed by a workload, plus where it is consumed
// (for a legible failure message).
type secretRef struct {
	name  string
	where string
}

func TestInstallBootstrapsAllSecrets(t *testing.T) {
	// The always-applied core: the embedded install bundle + the channelsd
	// manifests (applied from their own embed.FS by `oap install`). Optional
	// backends (postgres/neo4j/graphiti) are flag-gated and their Secrets are
	// generated separately; they are intentionally out of scope here.
	docBytes := [][]byte{manifests.Install}
	ch, err := manifests.ChannelsD()
	require.NoError(t, err, "load channelsd manifests")
	docBytes = append(docBytes, ch...)

	shipped := map[string]bool{}
	var refs []secretRef
	for _, b := range docBytes {
		objs, err := manifests.Split(b)
		require.NoError(t, err, "split manifest bundle")
		for _, u := range objs {
			if u.GetKind() == "Secret" {
				shipped[u.GetName()] = true
				continue
			}
			ps, ok := podSpecOf(u)
			if !ok {
				continue
			}
			refs = append(refs, nonOptionalSecretRefs(u.GetName(), ps)...)
		}
	}

	require.NotEmpty(t, refs, "expected to find some non-optional Secret references; manifest parsing likely broke")

	for _, r := range refs {
		satisfied := shipped[r.name]
		creator, ensured := installTimeSecrets[r.name]
		if !assert.Truef(t, satisfied || ensured,
			"%s references non-optional Secret %q, but nothing creates it: it is neither shipped as a Secret in the bundle nor minted at install time. Add an ensure* helper in cmd/oap and a row in installTimeSecrets, or set optional:true.",
			r.where, r.name) {
			continue
		}
		if ensured {
			// The claim this reference rests on, quoted where a reader of the
			// failure above will look next. TestInstallTimeSecretsMapIsLoadBearing
			// is what checks it is true.
			assert.NotEmptyf(t, creator,
				"%s is satisfied only by installTimeSecrets[%q], whose creator note is empty — there is nothing to check the "+
					"claim against", r.where, r.name)
		}
	}
}

// TestInstallTimeSecretsMapIsLoadBearing makes the waiver map's VALUE mean
// something. Without it the map is a bare name list whose entries can only ever
// weaken the guard above, silently.
//
// The hazard is pre-waiving. installTimeSecrets is consulted only for Secrets
// referenced NON-optionally, so a row whose claim is false costs nothing — and
// stays invisible — while every reference to that Secret is optional. The
// moment one stops being optional it waives a Secret nothing creates and the
// install wedges in CreateContainerConfigError forever, the exact failure the
// guard above exists to catch.
//
// So each row must earn its place two ways: the bundle must still reference the
// Secret, and the creator the row names must actually reach that Secret name in
// code (goIndex.mints — the AST, never a comment).
//
// The reciprocal check spans ALL references, optional included, not just the
// non-optional ones this map is consulted for. A row covering a today-optional
// reference is inert but TRUE, so it is no hazard; the hazard is a FALSE row,
// which would wave a wedged install through the moment its reference stops
// being optional.
func TestInstallTimeSecretsMapIsLoadBearing(t *testing.T) {
	src := newGoIndex(t,
		filepath.Join("..", "..", "..", "cmd", "oap"),
		filepath.Join("..", "..", "apis", "v1alpha1"),
	)
	referenced := allReferencedSecretNames(t)

	for name, creator := range installTimeSecrets {
		t.Run(name, func(t *testing.T) {
			assert.Containsf(t, referenced, name,
				"installTimeSecrets[%q] names a Secret no workload in the always-applied bundle references at all — not even "+
					"optionally. The row can only pre-waive a future reference that reuses the name. Delete it.", name)

			require.NotEmptyf(t, creator, "installTimeSecrets[%q] must name its creator; the value is what this test checks", name)
			fn, _, _ := strings.Cut(strings.TrimSpace(creator), " ")
			ok, why := src.mints(fn, name)
			assert.Truef(t, ok,
				"installTimeSecrets[%q] names creator %q, but %s. Either nothing creates the Secret (delete the row; the "+
					"reference must then be optional), the creator was renamed or deleted, or minting moved out of cmd/oap "+
					"(update this test's scan root).", name, fn, why)
		})
	}
}

// TestCreatorClaimsResolveStructurally pins the resolver the map above rests
// on, against a fixture that contains every shape of claim — including the
// false one.
//
// The guard is only as strong as this: it was raw text greps over cmd/oap's
// CONCATENATED source, comments included, so a row passed on the strength of
// somebody having written the Secret name in a doc comment somewhere in the
// package, and any function of that name existing anywhere. That cannot refute
// the fiction class it was written for — `spicebox-passthrough-link-key` sails
// through on one comment while the code names v1alpha1's constant — and it goes
// red on a harmless comment reword. Resolution is structural instead: the name
// must be reachable from THAT creator's body, as a literal or as a constant
// that resolves to it.
func TestCreatorClaimsResolveStructurally(t *testing.T) {
	ix := newGoIndex(t,
		filepath.Join("testdata", "creatorclaims", "creators"),
		filepath.Join("testdata", "creatorclaims", "apis"),
	)

	cases := []struct {
		name   string
		fn     string
		secret string
		want   bool
	}{
		{
			name: "name appears only in the creator's doc comment: NOT minted",
			fn:   "ensureCommentOnly", secret: "demo-comment-secret", want: false,
		},
		{
			name: "string literal in the creator's own body: minted",
			fn:   "ensureLiteral", secret: "demo-literal-secret", want: true,
		},
		{
			name: "same-package constant naming the Secret: minted",
			fn:   "ensureViaLocalConst", secret: "demo-local-secret", want: true,
		},
		{
			name: "another package's constant naming the Secret: minted",
			fn:   "ensureViaImportedConst", secret: "demo-imported-secret", want: true,
		},
		{
			name: "creator delegates one hop to a helper that mints it: minted",
			fn:   "ensureDelegating", secret: "demo-delegated-secret", want: true,
		},
		{
			name: "the literal lives in an unrelated function: NOT minted",
			fn:   "ensureLiteral", secret: "demo-delegated-secret", want: false,
		},
		{
			name: "no function of that name exists: NOT minted",
			fn:   "ensureRenamedAway", secret: "demo-literal-secret", want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := ix.mints(tc.fn, tc.secret)
			assert.Equal(t, tc.want, got, "why: %s", why)
			if !got {
				assert.NotEmpty(t, why, "a refused claim must say what was missing")
			}
		})
	}
}

// allReferencedSecretNames collects every Secret name any workload in the
// always-applied manifests consumes, optional or not.
func allReferencedSecretNames(t *testing.T) []string {
	t.Helper()
	docBytes := [][]byte{manifests.Install}
	ch, err := manifests.ChannelsD()
	require.NoError(t, err, "load channelsd manifests")
	docBytes = append(docBytes, ch...)

	var names []string
	for _, b := range docBytes {
		objs, err := manifests.Split(b)
		require.NoError(t, err, "split manifest bundle")
		for _, u := range objs {
			ps, ok := podSpecOf(u)
			if !ok {
				continue
			}
			for _, c := range append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...) {
				for _, e := range c.Env {
					if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
						names = append(names, e.ValueFrom.SecretKeyRef.Name)
					}
				}
				for _, ef := range c.EnvFrom {
					if ef.SecretRef != nil {
						names = append(names, ef.SecretRef.Name)
					}
				}
			}
			for _, v := range ps.Volumes {
				if v.Secret != nil {
					names = append(names, v.Secret.SecretName)
				}
			}
		}
	}
	return names
}

// goIndex answers one question about a body of Go source: does the creator
// function named fn mint the Secret named secret?
//
// It resolves that STRUCTURALLY, over the AST — the name must be reachable from
// that function's body — because the text-grep version it replaced could not
// tell a claim from a comment. The map documents itself as "mirroring the
// ensure* calls in cmd/oap", so cmd/oap is where the claim is checked; if secret
// minting ever moves elsewhere this test goes red rather than quietly stopping
// to check anything.
type goIndex struct {
	// funcs are the function declarations found under funcDir, by name.
	funcs map[string]*ast.FuncDecl
	// consts are the string constants a body may name a Secret with instead of
	// spelling it out, keyed by the bare identifier — which is what both a
	// same-package reference (localSecretName) and a qualified one
	// (v1alpha1.PassthroughLinkSigningKeySecret) resolve through.
	consts map[string]string
}

// newGoIndex parses every non-test Go source under funcDir (where creators are
// declared) and constDirs (packages whose string constants a creator may name a
// Secret with). Comments are deliberately not requested: a claim written in
// prose is exactly what this index must not accept as evidence.
func newGoIndex(t *testing.T, funcDir string, constDirs ...string) goIndex {
	t.Helper()
	ix := goIndex{funcs: map[string]*ast.FuncDecl{}, consts: map[string]string{}}
	fset := token.NewFileSet()
	for _, root := range append([]string{funcDir}, constDirs...) {
		collectFuncs := root == funcDir
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			for _, decl := range f.Decls {
				switch gd := decl.(type) {
				case *ast.FuncDecl:
					if collectFuncs && gd.Body != nil {
						ix.funcs[gd.Name.Name] = gd
					}
				case *ast.GenDecl:
					if gd.Tok == token.CONST {
						ix.collectConsts(gd)
					}
				}
			}
			return nil
		})
		require.NoErrorf(t, err, "parse Go sources under %s", root)
	}
	require.NotEmptyf(t, ix.funcs, "found no function declarations under %s; the scan root moved", funcDir)
	return ix
}

// collectConsts records every string-valued constant in one const declaration.
func (ix goIndex) collectConsts(d *ast.GenDecl) {
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil {
				ix.consts[name.Name] = v
			}
		}
	}
}

// mints reports whether fn mints the Secret named secret — the name appears in
// its body as a string literal or as a constant that resolves to it — and, when
// it does not, why not.
//
// The search follows calls to other functions in the index, because a creator
// legitimately does the work one hop down: cmd/oap's ensureSpiceDBToken and
// ensureNATSClientCreds each build a client and hand off to a *WithClient
// sibling that holds the name. A visited set keeps the walk finite. On success
// the note is the call path, so a reader can see where the claim was satisfied
// rather than taking the test's word for it.
func (ix goIndex) mints(fn, secret string) (bool, string) {
	if _, ok := ix.funcs[fn]; !ok {
		return false, "no function of that name is declared in the scanned sources"
	}
	seen := map[string]bool{}
	var walk func(name string, path []string) []string
	walk = func(name string, path []string) []string {
		decl, ok := ix.funcs[name]
		if !ok || seen[name] {
			return nil
		}
		seen[name] = true

		var found []string
		var calls []string
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if v, err := strconv.Unquote(x.Value); err == nil && v == secret {
						found = path
					}
				}
			case *ast.Ident:
				// Covers a bare constant and the Sel half of a qualified one
				// alike: ast.Inspect descends into SelectorExpr.
				if ix.consts[x.Name] == secret {
					found = path
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok {
					calls = append(calls, id.Name)
				}
			}
			return true
		})
		if found != nil {
			return found
		}
		for _, c := range calls {
			if p := walk(c, append(append([]string{}, path...), c)); p != nil {
				return p
			}
		}
		return nil
	}
	if p := walk(fn, []string{fn}); p != nil {
		return true, "minted via " + strings.Join(p, " -> ")
	}
	return false, "its body names that Secret nowhere — not as a string literal, not through a string constant, " +
		"and not in any function it calls (a mention in a comment is not evidence)"
}

// TestChannelsdPassthroughKeyMountIsNonOptional pins the one mount whose
// optionality is a SECURITY property, not a convenience.
//
// channelsd is the sole custodian of the passthrough-link HMAC key (the
// operator was deliberately kept out of that role — see
// pkg/channels/channelsd/pipeline/credential_update.go's split rationale), and three
// separate comments justify that split with "the kubelet blocks channelsd's pod
// start if the Secret is missing, instead of a process silently degrading
// forever". With optional:true that sentence was simply false: the pod started,
// the signer stayed nil, and both credential watchers self-disabled with no
// retry. Flipping the mount back to optional must fail HERE rather than quietly
// re-falsify those comments.
func TestChannelsdPassthroughKeyMountIsNonOptional(t *testing.T) {
	blobs, err := manifests.ChannelsD()
	require.NoError(t, err, "load channelsd manifests")

	found := false
	for _, b := range blobs {
		objs, err := manifests.Split(b)
		require.NoError(t, err, "split channelsd manifests")
		for _, u := range objs {
			ps, ok := podSpecOf(u)
			if !ok {
				continue
			}
			for _, v := range ps.Volumes {
				if v.Secret == nil || v.Secret.SecretName != "spicebox-passthrough-link-key" {
					continue
				}
				found = true
				assert.Falsef(t, boolVal(v.Secret.Optional),
					"%s volume %q mounts the passthrough-link signing key with optional:true; "+
						"channelsd must fail to START without it, not boot with its credential watchers silently disabled",
					u.GetName(), v.Name)
			}
		}
	}
	require.True(t, found, "no channelsd volume mounts spicebox-passthrough-link-key; the mount was renamed or dropped")
}

// podSpecOf returns the PodSpec embedded in a workload object, decoding the
// unstructured nested template into a typed corev1.PodSpec so the *bool
// Optional fields are read correctly. Returns ok=false for non-workloads.
func podSpecOf(u *unstructured.Unstructured) (corev1.PodSpec, bool) {
	var path []string
	switch u.GetKind() {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
		path = []string{"spec", "template", "spec"}
	case "CronJob":
		path = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	case "Pod":
		path = []string{"spec"}
	default:
		return corev1.PodSpec{}, false
	}
	raw, found, err := unstructured.NestedMap(u.Object, path...)
	if err != nil || !found {
		return corev1.PodSpec{}, false
	}
	var ps corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &ps); err != nil {
		return corev1.PodSpec{}, false
	}
	return ps, true
}

// nonOptionalSecretRefs returns every Secret a PodSpec consumes without
// optional:true — via env secretKeyRef, envFrom secretRef, or a secret volume.
// These are the references that block container creation when the Secret is
// absent. imagePullSecrets are excluded: a missing one is a pull-time failure,
// not CreateContainerConfigError.
func nonOptionalSecretRefs(workload string, ps corev1.PodSpec) []secretRef {
	var out []secretRef
	containers := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
	for _, c := range containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				s := e.ValueFrom.SecretKeyRef
				if !boolVal(s.Optional) {
					out = append(out, secretRef{s.Name, fmt.Sprintf("%s container %q env %q", workload, c.Name, e.Name)})
				}
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && !boolVal(ef.SecretRef.Optional) {
				out = append(out, secretRef{ef.SecretRef.Name, fmt.Sprintf("%s container %q envFrom", workload, c.Name)})
			}
		}
	}
	for _, v := range ps.Volumes {
		if v.Secret != nil && !boolVal(v.Secret.Optional) {
			out = append(out, secretRef{v.Secret.SecretName, fmt.Sprintf("%s volume %q", workload, v.Name)})
		}
	}
	return out
}

func boolVal(p *bool) bool { return p != nil && *p }
