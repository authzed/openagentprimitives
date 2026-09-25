package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// webdDevDoc builds a minimal webd Deployment doc in the *unstructured form
// manifests.Split yields, with the given baseline args on the webd container.
func webdDevDoc(args ...string) *unstructured.Unstructured {
	ifaceArgs := make([]any, len(args))
	for i, a := range args {
		ifaceArgs[i] = a
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      webdDeploymentName,
			"namespace": "agentprimitives-system",
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": webdContainerName, "args": ifaceArgs},
					},
				},
			},
		},
	}}
}

// webdContainerArgs reads back the webd container's args as []string.
func webdContainerArgs(t *testing.T, d *unstructured.Unstructured) []string {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found, "containers must be present")
	for _, raw := range containers {
		c := raw.(map[string]any)
		if c["name"] == webdContainerName {
			got, err := stringSlice(c["args"])
			require.NoError(t, err)
			return got
		}
	}
	t.Fatalf("no container named %q", webdContainerName)
	return nil
}

func TestInjectWebdFlags(t *testing.T) {
	t.Run("--web-dev appended once to the webd container (idempotent)", func(t *testing.T) {
		doc := webdDevDoc() // production baseline: no args
		docs := []*unstructured.Unstructured{doc}

		require.NoError(t, injectWebdFlags(docs, webDevFlag))
		assert.Equal(t, []string{"--web-dev"}, webdContainerArgs(t, doc))

		// Second call is a no-op: --web-dev is not double-added.
		require.NoError(t, injectWebdFlags(docs, webDevFlag))
		assert.Equal(t, []string{"--web-dev"}, webdContainerArgs(t, doc))
	})

	t.Run("--allow-shared-origin appended for the local flow", func(t *testing.T) {
		doc := webdDevDoc()
		require.NoError(t, injectWebdFlags([]*unstructured.Unstructured{doc}, allowSharedOriginFlag))
		assert.Equal(t, []string{"--allow-shared-origin"}, webdContainerArgs(t, doc))
	})

	t.Run("no flags leaves the webd container args untouched (production default)", func(t *testing.T) {
		doc := webdDevDoc() // baseline carries no shared-origin/dev flags
		require.NoError(t, injectWebdFlags([]*unstructured.Unstructured{doc}))
		assert.Empty(t, webdContainerArgs(t, doc), "a plain install injects nothing → no insecure flags")
	})

	t.Run("multiple flags appended, idempotent on the present subset", func(t *testing.T) {
		doc := webdDevDoc(allowSharedOriginFlag) // shared-origin already there
		require.NoError(t, injectWebdFlags([]*unstructured.Unstructured{doc}, allowSharedOriginFlag, webDevFlag))
		assert.Equal(t, []string{"--allow-shared-origin", "--web-dev"}, webdContainerArgs(t, doc),
			"only the absent flag is added")
	})

	t.Run("non-webd docs are untouched", func(t *testing.T) {
		// A Deployment with a different name and a different container.
		other := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "spicebox-operator", "namespace": "agentprimitives-system"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "operator", "args": []any{"--leader-elect"}}},
			}}},
		}}
		require.NoError(t, injectWebdFlags([]*unstructured.Unstructured{other}, webDevFlag))

		containers, _, err := unstructured.NestedSlice(other.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		got, err := stringSlice(containers[0].(map[string]any)["args"])
		require.NoError(t, err)
		assert.Equal(t, []string{"--leader-elect"}, got, "non-webd Deployment args must be untouched")
	})
}

// operatorDoc builds a minimal spicebox-operator Deployment doc with the given
// env entries on the operator container (each entry a {name,value} or
// {name,valueFrom} map, in the unstructured form manifests.Split yields).
func operatorDoc(env ...map[string]any) *unstructured.Unstructured {
	ifaceEnv := make([]any, len(env))
	for i, e := range env {
		ifaceEnv[i] = e
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      operatorDeploymentName,
			"namespace": "agentprimitives-system",
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": operatorContainerName, "env": ifaceEnv},
					},
				},
			},
		},
	}}
}

// operatorEnvValue reads back the literal value of the named env var on the
// operator container, or "" if absent / not a literal value.
func operatorEnvValue(t *testing.T, d *unstructured.Unstructured, name string) string {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found, "containers must be present")
	for _, raw := range containers {
		c := raw.(map[string]any)
		if c["name"] != operatorContainerName {
			continue
		}
		env, ok := c["env"].([]any)
		if !ok {
			return ""
		}
		for _, e := range env {
			m := e.(map[string]any)
			if m["name"] == name {
				v, _ := m["value"].(string)
				return v
			}
		}
	}
	return ""
}

func TestSetOperatorMemoryBackend(t *testing.T) {
	t.Run("--local overrides the bundle's postgres default to sqlite", func(t *testing.T) {
		// The embedded bundle carries MEMORY_BACKEND=postgres.
		doc := operatorDoc(map[string]any{"name": memoryBackendEnvName, "value": "postgres"})
		require.NoError(t, setOperatorMemoryBackend([]*unstructured.Unstructured{doc}, memoryBackendSqliteValue))
		assert.Equal(t, "sqlite", operatorEnvValue(t, doc, memoryBackendEnvName))
	})

	t.Run("appends MEMORY_BACKEND when absent, preserving other env", func(t *testing.T) {
		doc := operatorDoc(map[string]any{"name": "GOTRACEBACK", "value": "all"})
		require.NoError(t, setOperatorMemoryBackend([]*unstructured.Unstructured{doc}, memoryBackendInmemValue))
		assert.Equal(t, "inmem", operatorEnvValue(t, doc, memoryBackendEnvName))
		assert.Equal(t, "all", operatorEnvValue(t, doc, "GOTRACEBACK"), "existing env is preserved")
	})

	t.Run("replacing a valueFrom entry drops valueFrom and sets a literal value", func(t *testing.T) {
		doc := operatorDoc(map[string]any{
			"name":      memoryBackendEnvName,
			"valueFrom": map[string]any{"configMapKeyRef": map[string]any{"name": "x", "key": "y"}},
		})
		require.NoError(t, setOperatorMemoryBackend([]*unstructured.Unstructured{doc}, memoryBackendInmemValue))
		containers, _, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		env := containers[0].(map[string]any)["env"].([]any)
		m := env[0].(map[string]any)
		assert.Equal(t, "inmem", m["value"])
		_, hasValueFrom := m["valueFrom"]
		assert.False(t, hasValueFrom, "valueFrom must be dropped when a literal value is set")
	})

	t.Run("non-operator docs are untouched", func(t *testing.T) {
		webd := webdDevDoc("--x")
		require.NoError(t, setOperatorMemoryBackend([]*unstructured.Unstructured{webd}, memoryBackendInmemValue))
		assert.Empty(t, operatorEnvValue(t, webd, memoryBackendEnvName),
			"a non-operator Deployment gets no MEMORY_BACKEND")
	})

	t.Run("same value: doc is unchanged (SSA no-op)", func(t *testing.T) {
		// RunInstall now calls setOperatorMemoryBackend unconditionally,
		// including on a non-local install where the value it passes
		// (p.MemoryBackend()) already matches the base manifest's own
		// "postgres" default. That re-apply must be byte-identical — no
		// SSA field-ownership churn — or every non-local install would
		// re-trigger a spurious reconcile on this field.
		doc := operatorDoc(map[string]any{"name": memoryBackendEnvName, "value": "postgres"})
		before := doc.DeepCopy()
		require.NoError(t, setOperatorMemoryBackend([]*unstructured.Unstructured{doc}, "postgres"))
		assert.Equal(t, before.Object, doc.Object, "re-applying the same value must leave the doc byte-identical")
	})
}

// deploymentDoc builds a minimal single-container Deployment doc (in the
// unstructured form manifests.Split yields) for the given Deployment/
// container name pair, with the given env entries. Used by setDeploymentEnv
// and flipSpiceDBInsecureForExternalTLS tests to build several distinct
// Deployments in one doc set and assert only the targeted one is mutated.
func deploymentDoc(deploymentName, containerName string, env ...map[string]any) *unstructured.Unstructured {
	ifaceEnv := make([]any, len(env))
	for i, e := range env {
		ifaceEnv[i] = e
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      deploymentName,
			"namespace": "agentprimitives-system",
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": containerName, "env": ifaceEnv},
					},
				},
			},
		},
	}}
}

// deploymentEnvValue reads back the literal value of the named env var on the
// given Deployment doc's first container, or "" if absent / not a literal
// value.
func deploymentEnvValue(t *testing.T, d *unstructured.Unstructured, name string) string {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found, "containers must be present")
	require.NotEmpty(t, containers, "at least one container must be present")
	c, ok := containers[0].(map[string]any)
	require.True(t, ok, "container[0] must be a map")
	env, ok := c["env"].([]any)
	if !ok {
		return ""
	}
	for _, e := range env {
		m := e.(map[string]any)
		if m["name"] == name {
			v, _ := m["value"].(string)
			return v
		}
	}
	return ""
}

func TestSetDeploymentEnv(t *testing.T) {
	t.Run("sets the env on the named Deployment only; other Deployments untouched", func(t *testing.T) {
		op := deploymentDoc(operatorDeploymentName, operatorContainerName, map[string]any{"name": spicedb.EnvInsecure, "value": "true"})
		az := deploymentDoc(authzdDeploymentName, "authzd", map[string]any{"name": spicedb.EnvInsecure, "value": "true"})
		wb := deploymentDoc(webdDeploymentName, webdContainerName, map[string]any{"name": spicedb.EnvInsecure, "value": "true"})
		docs := []*unstructured.Unstructured{op, az, wb}

		require.NoError(t, setDeploymentEnv(docs, authzdDeploymentName, spicedb.EnvInsecure, "false"))

		assert.Equal(t, "false", deploymentEnvValue(t, az, spicedb.EnvInsecure), "the named Deployment (authzd) is flipped")
		assert.Equal(t, "true", deploymentEnvValue(t, op, spicedb.EnvInsecure), "operator Deployment must be untouched")
		assert.Equal(t, "true", deploymentEnvValue(t, wb, spicedb.EnvInsecure), "webd Deployment must be untouched")
	})

	t.Run("appends the env when absent, preserving other env on the same Deployment", func(t *testing.T) {
		doc := deploymentDoc(webdDeploymentName, webdContainerName, map[string]any{"name": "OTHER", "value": "keep-me"})
		require.NoError(t, setDeploymentEnv([]*unstructured.Unstructured{doc}, webdDeploymentName, spicedb.EnvInsecure, "false"))
		assert.Equal(t, "false", deploymentEnvValue(t, doc, spicedb.EnvInsecure))
		assert.Equal(t, "keep-me", deploymentEnvValue(t, doc, "OTHER"), "existing env is preserved")
	})

	t.Run("Deployment name absent from docs is a no-op, not an error", func(t *testing.T) {
		doc := deploymentDoc(webdDeploymentName, webdContainerName)
		require.NoError(t, setDeploymentEnv([]*unstructured.Unstructured{doc}, "some-other-deployment", "X", "y"))
		assert.Empty(t, deploymentEnvValue(t, doc, "X"), "the non-matching doc set gets no mutation")
	})
}

// newSpiceDBClientDeploymentDocs builds one fake Deployment doc for each of
// the three baseDocs Deployments flipSpiceDBInsecureForExternalTLS targets,
// none carrying a SPICEDB_INSECURE entry yet.
func newSpiceDBClientDeploymentDocs() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		deploymentDoc(operatorDeploymentName, operatorContainerName),
		deploymentDoc(authzdDeploymentName, "authzd"),
		deploymentDoc(webdDeploymentName, webdContainerName),
	}
}

func TestFlipSpiceDBInsecureForExternalTLS(t *testing.T) {
	t.Run("external + TLS (Insecure=false): flips operator/authzd/webd to SPICEDB_INSECURE=false", func(t *testing.T) {
		docs := newSpiceDBClientDeploymentDocs()
		ext := ExternalSpiceDB{Endpoint: "spicedb.example.net:443"}
		require.NoError(t, flipSpiceDBInsecureForExternalTLS(docs, ext))
		for _, d := range docs {
			assert.Equal(t, "false", deploymentEnvValue(t, d, spicedb.EnvInsecure), "%s must be flipped to TLS", d.GetName())
		}
	})

	t.Run("external + --external-spicedb-insecure: no flip", func(t *testing.T) {
		docs := newSpiceDBClientDeploymentDocs()
		ext := ExternalSpiceDB{Endpoint: "spicedb.example.net:50051", Insecure: true}
		require.NoError(t, flipSpiceDBInsecureForExternalTLS(docs, ext))
		for _, d := range docs {
			assert.Empty(t, deploymentEnvValue(t, d, spicedb.EnvInsecure), "%s must be untouched for --external-spicedb-insecure", d.GetName())
		}
	})

	t.Run("not external: no flip", func(t *testing.T) {
		docs := newSpiceDBClientDeploymentDocs()
		require.NoError(t, flipSpiceDBInsecureForExternalTLS(docs, ExternalSpiceDB{}))
		for _, d := range docs {
			assert.Empty(t, deploymentEnvValue(t, d, spicedb.EnvInsecure), "%s must be untouched for a non-external (bundled) install", d.GetName())
		}
	})
}

// channelsdDeploymentArgs finds the Deployment doc among ms (the channelsd
// manifest set) and returns its container's args.
func channelsdDeploymentArgs(t *testing.T, ms [][]byte) []string {
	t.Helper()
	for _, m := range ms {
		docs, err := manifests.Split(m)
		require.NoError(t, err)
		for _, d := range docs {
			if d.GetKind() != "Deployment" {
				continue
			}
			containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
			require.NoError(t, err)
			require.True(t, found)
			args, err := stringSlice(containers[0].(map[string]any)["args"])
			require.NoError(t, err)
			return args
		}
	}
	t.Fatal("no Deployment doc found in channelsd manifest set")
	return nil
}

func TestAppendChannelsdInsecureFlag(t *testing.T) {
	t.Run("appends --spicedb-insecure=false to the channelsd container args", func(t *testing.T) {
		ms, err := manifests.ChannelsD()
		require.NoError(t, err)
		require.NoError(t, appendChannelsdInsecureFlag(ms))
		assert.Contains(t, channelsdDeploymentArgs(t, ms), channelsdInsecureFlag)
	})

	t.Run("idempotent: a second call does not duplicate the flag", func(t *testing.T) {
		ms, err := manifests.ChannelsD()
		require.NoError(t, err)
		require.NoError(t, appendChannelsdInsecureFlag(ms))
		require.NoError(t, appendChannelsdInsecureFlag(ms))

		args := channelsdDeploymentArgs(t, ms)
		count := 0
		for _, a := range args {
			if a == channelsdInsecureFlag {
				count++
			}
		}
		assert.Equal(t, 1, count, "the flag must appear exactly once after repeated calls")
	})

	t.Run("baseline args (e.g. --spicedb-endpoint) are preserved", func(t *testing.T) {
		ms, err := manifests.ChannelsD()
		require.NoError(t, err)
		require.NoError(t, appendChannelsdInsecureFlag(ms))
		args := channelsdDeploymentArgs(t, ms)
		assert.Contains(t, args, "--spicedb-endpoint=$(SPICEDB_ENDPOINT)", "existing args must survive the mutation")
	})
}

// operatorArgDoc builds a minimal spicebox-operator Deployment doc with the
// given baseline args on the operator container (the unstructured form
// manifests.Split yields).
func operatorArgDoc(args ...string) *unstructured.Unstructured {
	ifaceArgs := make([]any, len(args))
	for i, a := range args {
		ifaceArgs[i] = a
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      operatorDeploymentName,
			"namespace": "agentprimitives-system",
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": operatorContainerName, "args": ifaceArgs},
					},
				},
			},
		},
	}}
}

// operatorContainerArgs reads back the operator container's args as []string.
func operatorContainerArgs(t *testing.T, d *unstructured.Unstructured) []string {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found, "containers must be present")
	for _, raw := range containers {
		c := raw.(map[string]any)
		if c["name"] == operatorContainerName {
			got, err := stringSlice(c["args"])
			require.NoError(t, err)
			return got
		}
	}
	t.Fatalf("no container named %q", operatorContainerName)
	return nil
}

// TestInjectOperatorWorkspaceClass covers the fix for the init/install ordering
// bug: the operator Deployment must carry --workspace-storage-class in the SSA
// apply set itself (injected up-front from the resolved class), so a re-apply
// never strips it and an interrupt/failure before the late patchOperator step
// can never leave the operator running in isolated mode with a live workspace
// marker. Mirrors the injectWebdFlags contract.
func TestInjectOperatorWorkspaceClass(t *testing.T) {
	t.Run("sets the flag on the operator container at apply time", func(t *testing.T) {
		doc := operatorArgDoc("--leader-elect=true")
		require.NoError(t, injectOperatorWorkspaceClass([]*unstructured.Unstructured{doc}, "ap-workspace-rwx"))
		assert.Equal(t, []string{"--leader-elect=true", "--workspace-storage-class=ap-workspace-rwx"},
			operatorContainerArgs(t, doc))
	})

	t.Run("empty class injects nothing (isolated) and leaves baseline args", func(t *testing.T) {
		doc := operatorArgDoc("--leader-elect=true")
		require.NoError(t, injectOperatorWorkspaceClass([]*unstructured.Unstructured{doc}, ""))
		assert.Equal(t, []string{"--leader-elect=true"}, operatorContainerArgs(t, doc),
			"an isolated (empty) class deploys the operator with no workspace flag")
	})

	t.Run("replaces a stale workspace-class arg (idempotent re-point)", func(t *testing.T) {
		doc := operatorArgDoc("--leader-elect=true", "--workspace-storage-class=old-class")
		require.NoError(t, injectOperatorWorkspaceClass([]*unstructured.Unstructured{doc}, "ap-workspace-rwx"))
		assert.Equal(t, []string{"--leader-elect=true", "--workspace-storage-class=ap-workspace-rwx"},
			operatorContainerArgs(t, doc), "exactly one workspace-class arg, pointing at the new class")
	})

	t.Run("empty class strips a stale workspace-class arg (degrade to isolated)", func(t *testing.T) {
		doc := operatorArgDoc("--leader-elect=true", "--workspace-storage-class=ap-workspace-rwx")
		require.NoError(t, injectOperatorWorkspaceClass([]*unstructured.Unstructured{doc}, ""))
		assert.Equal(t, []string{"--leader-elect=true"}, operatorContainerArgs(t, doc))
	})

	t.Run("same class: doc unchanged (SSA no-op)", func(t *testing.T) {
		doc := operatorArgDoc("--leader-elect=true", "--workspace-storage-class=ap-workspace-rwx")
		before := doc.DeepCopy()
		require.NoError(t, injectOperatorWorkspaceClass([]*unstructured.Unstructured{doc}, "ap-workspace-rwx"))
		assert.Equal(t, before.Object, doc.Object, "re-injecting the same class must leave the doc byte-identical")
	})

	t.Run("non-operator docs are untouched", func(t *testing.T) {
		webd := webdDevDoc("--serve")
		require.NoError(t, injectOperatorWorkspaceClass([]*unstructured.Unstructured{webd}, "ap-workspace-rwx"))
		assert.Equal(t, []string{"--serve"}, webdContainerArgs(t, webd),
			"a non-operator Deployment gets no workspace flag")
	})
}

// TestInjectWorkspaceClassUpFront covers the audit's Finding 1: the up-front
// operator stamp must be withheld for a class that has NOT yet been proven to
// exist (a ProbeBeforeUse choice — fresh bundled provisioner, explicit
// --workspace-storage-class, or a newly-picked reselect class). Only an
// already-trusted class (cached marker, cost-confirmed managed class,
// kept-current pick — all Probe=false) is stamped up-front; the late
// patchOperatorWorkspaceClass sets the rest after the probe proves them.
func TestInjectWorkspaceClassUpFront(t *testing.T) {
	t.Run("trusted class (marker / cost-confirm, Probe=false): stamp up-front", func(t *testing.T) {
		assert.True(t, injectWorkspaceClassUpFront(workspaceChoice{ClassName: "ap-workspace-rwx"}))
	})
	t.Run("probe-before-use class: withheld until the probe proves the StorageClass exists", func(t *testing.T) {
		assert.False(t, injectWorkspaceClassUpFront(workspaceChoice{ClassName: "fresh-class", Probe: true}),
			"a fresh/explicit/newly-picked class must not be stamped onto the serving operator before it is probed")
	})
	t.Run("isolated (empty, Probe=false): up-front call is a harmless no-op strip", func(t *testing.T) {
		assert.True(t, injectWorkspaceClassUpFront(workspaceChoice{}))
	})
}
