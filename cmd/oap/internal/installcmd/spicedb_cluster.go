package installcmd

import (
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// SpiceDBClusterMode selects the datastore backing for the operator-managed
// SpiceDBCluster: memory (local dev, single replica) or postgres (remote, HA).
type SpiceDBClusterMode int

const (
	SpiceDBMemory SpiceDBClusterMode = iota
	SpiceDBPostgres
)

// spicedbModeFor maps the kind's chosen datastore onto the CR's mode. It stays
// the single source of truth for that mapping: both the CR (buildCoreComponents)
// and the bootstrap ConfigMap (RunInstall) gate on it, and they must not drift —
// a ConfigMap without the flag is harmless, but a flag without the ConfigMap is
// a crashloop.
func spicedbModeFor(d cloud.SpiceDBDatastore) SpiceDBClusterMode {
	if d == cloud.DatastoreMemory {
		return SpiceDBMemory
	}
	return SpiceDBPostgres
}

// bootstrapsSchemaAtStartup reports whether SpiceDB may seed its own base schema
// via --datastore-bootstrap-files on this mode.
//
// SpiceDB's bootstrap-files feature applies ONLY to an empty datastore. With
// --datastore-bootstrap-overwrite=false (the default) a process that finds a
// non-empty datastore during bootstrap exits fatally. So it is safe exactly when
// the datastore is guaranteed empty on every boot: an ephemeral, process-local
// one at a single replica. That is `memory`, and only `memory`.
//
// Against a shared/persistent datastore it is doubly fatal: with N>1 replicas
// exactly one wins the race and the rest crashloop permanently (the loser's
// killer — a non-empty datastore — was created by its own sibling and never goes
// away), and even at one replica the pod dies on its first restart, taking
// authorization down cluster-wide long after the install that planted it.
//
// The base schema is not lost by skipping it: the AP operator's guardian
// composer (pkg/authz/guardian/schema.RunAll — "the inputs are the authoritative
// source, not the live schema") owns the schema and continuously reconciles a
// strict superset of the bootstrap file.
func (m SpiceDBClusterMode) bootstrapsSchemaAtStartup() bool {
	return m == SpiceDBMemory
}

// validateBootstrapSafety refuses to emit a SpiceDBCluster carrying the
// combination that caused a production outage, rather than trusting the caller's
// branching to stay correct through a future edit.
//
// A first-boot-only initialization step must never run in a replicated or
// restartable serving container. If a mode ever needs a seed it cannot get this
// way, it belongs in a Job or an operator reconcile — the spicedb-operator
// already runs createdb/migrate as exactly such Jobs.
func validateBootstrapSafety(mode SpiceDBClusterMode, config map[string]any) error {
	if _, ok := config["datastoreBootstrapFiles"]; !ok {
		return nil
	}
	if !mode.bootstrapsSchemaAtStartup() {
		return fmt.Errorf("refusing to build SpiceDBCluster: datastoreBootstrapFiles is fatal against the non-ephemeral %v datastore once it is non-empty", config["datastoreEngine"])
	}
	if replicas, ok := config["replicas"].(int); ok && replicas != 1 {
		return fmt.Errorf("refusing to build SpiceDBCluster: datastoreBootstrapFiles with replicas=%d races replicas into a fatal non-empty-datastore exit", replicas)
	}
	return nil
}

// buildSpiceDBClusterDoc renders the authzed.com/v1alpha1 SpiceDBCluster the
// operator reconciles. The Service it derives is the bare cluster name
// (spicebox-spicedb), keeping the endpoint DNS unchanged.
//
// Unknown spec.config keys are forwarded to the SpiceDB pod as SPICEDB_* env
// vars, so datastoreBootstrapFiles becomes SPICEDB_DATASTORE_BOOTSTRAP_FILES on
// every replica. That is set only for modes where it is safe — see
// bootstrapsSchemaAtStartup — and the ConfigMap mount that serves it is emitted
// with it rather than left behind as an unread volume.
func buildSpiceDBClusterDoc(mode SpiceDBClusterMode) ([]byte, error) {
	engine, replicas := "memory", 1
	if mode == SpiceDBPostgres {
		engine, replicas = "postgres", 2
	}

	config := map[string]any{
		"datastoreEngine": engine,
		"replicas":        replicas,
	}
	spec := map[string]any{
		"secretName": "spicebox-spicedb-token",
		// Pinned to the vendored operator's (v1.27.0) default-channel HEAD
		// version so installs are reproducible and `--mirror-dependencies`
		// can copy a known ref (see pkg/platform/apimage.DependencyImages). Bump this
		// deliberately alongside the vendored bundle, not implicitly.
		"baseImage": "ghcr.io/authzed/spicedb",
		"version":   "v1.56.2",
		"config":    config,
	}

	if mode.bootstrapsSchemaAtStartup() {
		config["datastoreBootstrapFiles"] = "/etc/spicedb/schema.zed"
		// Strategic-merge patch: add the bootstrap ConfigMap volume + mount to
		// the operator-generated Deployment. The operator auto-detects
		// strategic-merge vs JSON6902; a map-shaped patch is treated as
		// strategic-merge. The container name the operator uses is "spicedb".
		spec["patches"] = []any{map[string]any{
			"kind": "Deployment",
			"patch": map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"volumes": []any{map[string]any{
								"name": "bootstrap-schema",
								"configMap": map[string]any{
									"name": "spicebox-spicedb-bootstrap",
								},
							}},
							"containers": []any{map[string]any{
								"name": "spicedb",
								"volumeMounts": []any{map[string]any{
									"name":      "bootstrap-schema",
									"mountPath": "/etc/spicedb",
									"readOnly":  true,
								}},
							}},
						},
					},
				},
			},
		}}
	}

	if err := validateBootstrapSafety(mode, config); err != nil {
		return nil, err
	}

	cluster := map[string]any{
		"apiVersion": "authzed.com/v1alpha1",
		"kind":       "SpiceDBCluster",
		"metadata": map[string]any{
			"name":      "spicebox-spicedb",
			"namespace": "agentprimitives-system",
		},
		"spec": spec,
	}
	return yaml.Marshal(cluster)
}
