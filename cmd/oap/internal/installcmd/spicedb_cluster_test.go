package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func decodeCluster(t *testing.T, mode SpiceDBClusterMode) map[string]any {
	t.Helper()
	doc, err := buildSpiceDBClusterDoc(mode)
	require.NoError(t, err, "buildSpiceDBClusterDoc")
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(doc, &m), "unmarshal cluster doc")
	return m
}

func TestSpiceDBClusterMemory_EngineMemoryOneReplicaNoDatastoreURI(t *testing.T) {
	m := decodeCluster(t, SpiceDBMemory)
	assert.Equal(t, "authzed.com/v1alpha1", m["apiVersion"])
	assert.Equal(t, "SpiceDBCluster", m["kind"])
	meta := m["metadata"].(map[string]any)
	assert.Equal(t, "spicebox-spicedb", meta["name"])
	assert.Equal(t, "agentprimitives-system", meta["namespace"])
	spec := m["spec"].(map[string]any)
	assert.Equal(t, "spicebox-spicedb-token", spec["secretName"])
	assert.Equal(t, "ghcr.io/authzed/spicedb", spec["baseImage"], "SpiceDB image must be pinned for reproducible + mirrorable installs")
	assert.Equal(t, "v1.56.2", spec["version"], "pinned to the vendored operator's default-channel HEAD version")
	cfg := spec["config"].(map[string]any)
	assert.Equal(t, "memory", cfg["datastoreEngine"])
	assert.Equal(t, float64(1), cfg["replicas"])
}

func TestSpiceDBClusterPostgres_EnginePostgresTwoReplicas(t *testing.T) {
	m := decodeCluster(t, SpiceDBPostgres)
	spec := m["spec"].(map[string]any)
	assert.Equal(t, "ghcr.io/authzed/spicedb", spec["baseImage"], "SpiceDB image must be pinned for reproducible + mirrorable installs")
	assert.Equal(t, "v1.56.2", spec["version"], "pinned to the vendored operator's default-channel HEAD version")
	cfg := spec["config"].(map[string]any)
	assert.Equal(t, "postgres", cfg["datastoreEngine"], "must be 'postgres', never 'postgresql'")
	assert.Equal(t, float64(2), cfg["replicas"])
}

// The correctness invariant this file exists to protect.
//
// SpiceDB's --datastore-bootstrap-files applies ONLY to an empty datastore and
// is FATAL against a non-empty one (--datastore-bootstrap-overwrite defaults to
// false). It is therefore safe only when the datastore is guaranteed empty on
// every boot — an ephemeral, process-local one. Setting it against a shared or
// persistent datastore makes (a) multi-replica startup a race exactly one
// replica can win and (b) every pod restart after first boot fatal.
//
// This is not a preference; getting it wrong takes authorization down
// cluster-wide. See 2026-07-06 / 2026-07-27 incident reports.
func TestSpiceDBClusterBootstrapFilesOnlyOnEphemeralDatastore(t *testing.T) {
	cases := []struct {
		name          string
		mode          SpiceDBClusterMode
		wantBootstrap bool
	}{
		{
			name:          "memory: datastore is empty every boot, so bootstrap files are set",
			mode:          SpiceDBMemory,
			wantBootstrap: true,
		},
		{
			name:          "postgres: datastore is shared+persistent, so bootstrap files must NOT be set",
			mode:          SpiceDBPostgres,
			wantBootstrap: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := decodeCluster(t, tc.mode)
			spec := m["spec"].(map[string]any)
			cfg := spec["config"].(map[string]any)

			bootstrap, hasBootstrap := cfg["datastoreBootstrapFiles"]
			_, hasPatches := spec["patches"]

			if !tc.wantBootstrap {
				assert.False(t, hasBootstrap,
					"datastoreBootstrapFiles must not be set against a shared datastore — it is fatal for every replica after the first and for every restart")
				assert.False(t, hasPatches,
					"the bootstrap ConfigMap mount exists only to serve datastoreBootstrapFiles; drop it with the flag rather than leaving an unread volume")
				return
			}

			assert.Equal(t, "/etc/spicedb/schema.zed", bootstrap)
			require.True(t, hasPatches, "spec.patches must mount the bootstrap ConfigMap when bootstrap files are set")
			raw, err := yaml.Marshal(spec["patches"])
			require.NoError(t, err)
			s := string(raw)
			assert.Contains(t, s, "spicebox-spicedb-bootstrap", "patch references the bootstrap ConfigMap")
			assert.Contains(t, s, "/etc/spicedb", "patch mounts at /etc/spicedb")
			assert.Contains(t, s, "Deployment", "patch targets the Deployment kind")
		})
	}
}

// Bootstrap files are only ever safe at a single replica: even an ephemeral
// datastore is shared by every replica of a Deployment that points at one.
// Guards against a future replica bump to a bootstrapping mode.
func TestSpiceDBClusterBootstrapImpliesSingleReplica(t *testing.T) {
	for _, mode := range []SpiceDBClusterMode{SpiceDBMemory, SpiceDBPostgres} {
		m := decodeCluster(t, mode)
		cfg := m["spec"].(map[string]any)["config"].(map[string]any)
		if _, ok := cfg["datastoreBootstrapFiles"]; !ok {
			continue
		}
		assert.Equal(t, float64(1), cfg["replicas"],
			"a bootstrapping mode must run exactly one replica — a second replica races the first into a fatal non-empty-datastore exit")
	}
}

// The tripwire that keeps the unsafe combination from being reintroduced by a
// future edit. It is unreachable through buildSpiceDBClusterDoc today by
// construction — which is exactly why it is exercised directly.
func TestValidateBootstrapSafety(t *testing.T) {
	const bootstrap = "/etc/spicedb/schema.zed"
	cases := []struct {
		name    string
		mode    SpiceDBClusterMode
		config  map[string]any
		wantErr string
	}{
		{
			name:   "no bootstrap file: nothing to check",
			mode:   SpiceDBPostgres,
			config: map[string]any{"datastoreEngine": "postgres", "replicas": 2},
		},
		{
			name:   "memory at one replica: the one safe combination",
			mode:   SpiceDBMemory,
			config: map[string]any{"datastoreEngine": "memory", "replicas": 1, "datastoreBootstrapFiles": bootstrap},
		},
		{
			name:    "bootstrap against a shared datastore is refused",
			mode:    SpiceDBPostgres,
			config:  map[string]any{"datastoreEngine": "postgres", "replicas": 2, "datastoreBootstrapFiles": bootstrap},
			wantErr: "fatal against the non-ephemeral",
		},
		{
			name:    "bootstrap at more than one replica is refused even when ephemeral",
			mode:    SpiceDBMemory,
			config:  map[string]any{"datastoreEngine": "memory", "replicas": 2, "datastoreBootstrapFiles": bootstrap},
			wantErr: "races replicas",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBootstrapSafety(tc.mode, tc.config)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err, "the unsafe combination must not be emitted")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// The install's local-vs-remote decision is the single source of truth for the
// datastore mode: the CR (buildCoreComponents) and the bootstrap ConfigMap
// (RunInstall) both gate on it and must not drift apart.
func TestSpiceDBModeFor(t *testing.T) {
	assert.Equal(t, SpiceDBMemory, spicedbModeFor(cloud.DatastoreMemory), "--local runs an in-process datastore")
	assert.Equal(t, SpiceDBPostgres, spicedbModeFor(cloud.DatastorePostgres), "a remote install shares one Postgres datastore")

	assert.True(t, spicedbModeFor(cloud.DatastoreMemory).bootstrapsSchemaAtStartup(),
		"memory: empty on every boot, so SpiceDB may seed its own schema")
	assert.False(t, spicedbModeFor(cloud.DatastorePostgres).bootstrapsSchemaAtStartup(),
		"postgres: non-empty after first boot, so seeding at startup is fatal")
}
