package installcmd

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

const (
	webdDeploymentName    = "spicebox-webd"
	webdContainerName     = "webd"
	webDevFlag            = "--web-dev"
	allowSharedOriginFlag = "--allow-shared-origin"

	// The same name cloud.Stamped reads the cluster kind back from.
	operatorDeploymentName = cloud.OperatorDeploymentName
	operatorContainerName  = "operator"
	memoryBackendEnvName   = "MEMORY_BACKEND"
	// memoryBackendInmemValue is the pure-ephemeral memory backend value.
	// No longer used by `oap init --local` (see memoryBackendSqliteValue) but
	// kept as a documented value for setOperatorMemoryBackend callers.
	memoryBackendInmemValue = "inmem"
	// memoryBackendSqliteValue is the value `oap init --local` sets on the
	// operator's MEMORY_BACKEND env. The bundle default is "postgres"
	// (config/manager/deployment.yaml); the local tunnel-dev flow overrides it
	// to select the persistent single-file backend (backed by the
	// spicebox-operator-memory PVC + MEMORY_SQLITE_PATH env already present in
	// the base manifest), so local installs keep memory across pod restarts.
	memoryBackendSqliteValue = "sqlite"

	// authzdDeploymentName is the base-bundle Deployment name for authzd —
	// unlike operator/webd it has no dedicated *ContainerName constant because
	// setDeploymentEnv (below) mutates env on every container in the named
	// Deployment rather than matching by container name; all three
	// (operator/authzd/webd) ship exactly one container each.
	authzdDeploymentName = "agentprimitives-authzd"

	// channelsdInsecureFlag is the CLI flag `oap install` appends to the
	// channelsd container's args to flip its --spicedb-insecure default
	// (true, set in internal/cmd/channelsd/main.go) to false for an external+TLS
	// SpiceDB. channelsd has no SPICEDB_INSECURE env in the base bundle (see
	// pkg/platform/manifests/channelsd/deployment.yaml), so it cannot be flipped via
	// setDeploymentEnv like operator/authzd/webd — it takes the flag instead.
	channelsdInsecureFlag = "--spicedb-insecure=false"
)

// injectWebdFlags mutates the parsed install docs in place: it finds the webd
// Deployment (spicebox-webd) and idempotently appends each flag in `flags` to
// the webd container's args, so the SSA apply set itself carries them. This
// keeps a single field manager (ap-install) owning the args field in one apply
// pass — no second manager fighting over args, and no extra post-install
// rollout. Already-present flags are not re-added. When `flags` is empty the
// docs are returned unchanged, so a plain `oap install` re-applies the baseline
// args (no dev/shared-origin flags) and SSA cleanly removes any prior flag in
// one rollout — this is what keeps --allow-shared-origin OFF on a production
// install (it is injected only by `oap init --local`, and --web-dev only by
// --develop). Returns an error only if the webd Deployment is found but its
// container shape is malformed (so a real packaging bug surfaces rather than
// silently producing a pod missing an intended flag).
func injectWebdFlags(docs []*unstructured.Unstructured, flags ...string) error {
	if len(flags) == 0 {
		return nil
	}
	for _, d := range docs {
		if d.GetKind() != "Deployment" || d.GetName() != webdDeploymentName {
			continue
		}
		containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return fmt.Errorf("%s: read spec.template.spec.containers: %w", webdDeploymentName, err)
		}
		if !found {
			return fmt.Errorf("%s: no spec.template.spec.containers", webdDeploymentName)
		}
		mutated := false
		for ci, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: container[%d] is not a map", webdDeploymentName, ci)
			}
			if c["name"] != webdContainerName {
				continue
			}
			args, err := stringSlice(c["args"])
			if err != nil {
				return fmt.Errorf("%s: container %q args: %w", webdDeploymentName, webdContainerName, err)
			}
			changed := false
			for _, f := range flags {
				if !containsString(args, f) {
					args = append(args, f)
					changed = true
				}
			}
			if !changed {
				return nil // idempotent: every flag already present
			}
			ifaceArgs := make([]any, len(args))
			for i, a := range args {
				ifaceArgs[i] = a
			}
			c["args"] = ifaceArgs
			containers[ci] = c
			mutated = true
			break
		}
		if !mutated {
			return fmt.Errorf("%s: no container named %q", webdDeploymentName, webdContainerName)
		}
		if err := unstructured.SetNestedSlice(d.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("%s: write spec.template.spec.containers: %w", webdDeploymentName, err)
		}
		return nil
	}
	// No webd Deployment in this doc set is not an error: the caller passes the
	// base region, and a future split could route webd elsewhere. The caller
	// applies the mutation to the doc slice that contains webd today (baseDocs).
	return nil
}

// injectOperatorWorkspaceClass mutates the parsed install docs in place: it
// finds the operator Deployment (spicebox-operator) and sets the operator
// container's --workspace-storage-class arg to className, so the SSA apply set
// itself carries it. The counterpart to injectWebdFlags for the one operator
// flag whose value is resolved at install time.
//
// Injecting up-front (rather than patching the live Deployment after the
// data-plane comes up) is load-bearing. The base manifest carries no
// --workspace-storage-class, so the SSA apply STRIPS any prior value; the
// post-apply patchOperatorWorkspaceClass re-adds it only AFTER the failable
// workspace-provisioning work (bundled-provisioner apply + patient provisioning
// probe). That left a window where an interrupt (Ctrl-C) or a failed dependency
// install aborted the run with the operator already deployed in isolated mode
// and a stale workspace marker still pointing at the RWX class — the operator
// then ran every session isolated, and every stateful (git) tool call hung on a
// snapshot Job whose <session>-workspace PVC never existed. With the flag in the
// applied doc, the operator is deployed with the resolved class from the start
// and a re-apply is idempotent.
//
// An empty className injects nothing (and strips any stale flag), so the
// operator falls back to isolated /work — the deliberate --no-workspace-rwx /
// degraded outcome. Non-operator docs are untouched. Idempotent: re-injecting
// the same class leaves the doc byte-identical (no SSA field-ownership churn).
// Returns an error only if the operator Deployment is found but its container
// shape is malformed, so a real packaging bug surfaces rather than silently
// producing a pod missing the intended flag.
func injectOperatorWorkspaceClass(docs []*unstructured.Unstructured, className string) error {
	for _, d := range docs {
		if d.GetKind() != "Deployment" || d.GetName() != operatorDeploymentName {
			continue
		}
		containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return fmt.Errorf("%s: read spec.template.spec.containers: %w", operatorDeploymentName, err)
		}
		if !found {
			return fmt.Errorf("%s: no spec.template.spec.containers", operatorDeploymentName)
		}
		mutated := false
		for ci, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: container[%d] is not a map", operatorDeploymentName, ci)
			}
			if c["name"] != operatorContainerName {
				continue
			}
			args, err := stringSlice(c["args"])
			if err != nil {
				return fmt.Errorf("%s: container %q args: %w", operatorDeploymentName, operatorContainerName, err)
			}
			// Strip any existing --workspace-storage-class= arg, then append the
			// resolved one (skipped for an empty class → isolated). Mirrors
			// patchOperatorWorkspaceClass's flag handling, applied to the doc.
			updated := make([]string, 0, len(args)+1)
			for _, a := range args {
				if !strings.HasPrefix(a, workspaceFlagPrefix) {
					updated = append(updated, a)
				}
			}
			if className != "" {
				updated = append(updated, workspaceFlagPrefix+className)
			}
			if argsEqual(args, updated) {
				return nil // idempotent no-op: doc left byte-identical
			}
			ifaceArgs := make([]any, len(updated))
			for i, a := range updated {
				ifaceArgs[i] = a
			}
			c["args"] = ifaceArgs
			containers[ci] = c
			mutated = true
			break
		}
		if !mutated {
			return fmt.Errorf("%s: no container named %q", operatorDeploymentName, operatorContainerName)
		}
		if err := unstructured.SetNestedSlice(d.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("%s: write spec.template.spec.containers: %w", operatorDeploymentName, err)
		}
		return nil
	}
	// No operator Deployment in this doc set is not an error: callers pass the
	// base region, which carries the operator Deployment today.
	return nil
}

// stringSlice normalizes an unstructured args value ([]any of strings, a nil,
// or absent) into a []string. It rejects a non-slice or a slice with a
// non-string element so a malformed manifest surfaces as an error rather than
// silently dropping the flag injection.
func stringSlice(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list, got %T", v)
	}
	out := make([]string, 0, len(raw))
	for i, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("element[%d] is %T, not a string", i, e)
		}
		out = append(out, s)
	}
	return out, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// setOperatorMemoryBackend mutates the parsed install docs in place, setting
// the operator container's MEMORY_BACKEND env var to value. Used by
// `oap init --local` to override the bundle's postgres default with sqlite; a
// plain install never calls this, so SSA re-applies the postgres default from
// the embedded manifest. Thin wrapper over setOperatorEnv kept so call sites
// read the same as before the artifact-store env var was added.
func setOperatorMemoryBackend(docs []*unstructured.Unstructured, value string) error {
	return setOperatorEnv(docs, memoryBackendEnvName, value)
}

// setOperatorEnv mutates the parsed install docs in place, setting the
// operator Deployment's (spicebox-operator) container env var `name` to
// value. Thin wrapper over setDeploymentEnv kept so the many pre-existing
// call sites (ARTIFACT_STORE_URL, MEMORY_BACKEND) read the same as before
// setDeploymentEnv generalized the mutation to an arbitrary Deployment name
// (needed so the external-SpiceDB-over-TLS flip can also target the authzd
// and webd Deployments — see flipSpiceDBInsecureForExternalTLS).
func setOperatorEnv(docs []*unstructured.Unstructured, name, value string) error {
	return setDeploymentEnv(docs, operatorDeploymentName, name, value)
}

// setDeploymentEnv mutates the parsed install docs in place: it finds the
// named Deployment (matched by Kind == "Deployment" and metadata.name) and
// sets env var `name` to value on every container in its pod template
// (upserting — replacing an existing entry's value, dropping any valueFrom,
// or appending a new entry when absent). Applied to the base region BEFORE
// the apply loop so the single ap-install field manager carries the value in
// one pass.
//
// "Every container" rather than a container-name match: every Deployment
// this is called against today (operator, authzd, webd) ships exactly one
// container, so the two coincide, and this stays a 4-argument helper (no
// container-name parameter needed per caller).
//
// Returns an error only if the named Deployment is found but its container
// shape is malformed (a real packaging bug surfaces rather than silently
// producing a pod with the wrong env). A doc set without the named
// Deployment is not an error — callers pass the base region, and not every
// base-region Deployment is relevant to every mutation.
func setDeploymentEnv(docs []*unstructured.Unstructured, deploymentName, name, value string) error {
	for _, d := range docs {
		if d.GetKind() != "Deployment" || d.GetName() != deploymentName {
			continue
		}
		containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return fmt.Errorf("%s: read spec.template.spec.containers: %w", deploymentName, err)
		}
		if !found || len(containers) == 0 {
			return fmt.Errorf("%s: no spec.template.spec.containers", deploymentName)
		}
		for ci, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: container[%d] is not a map", deploymentName, ci)
			}
			env, err := upsertEnvValue(c["env"], name, value)
			if err != nil {
				return fmt.Errorf("%s: container[%d] env: %w", deploymentName, ci, err)
			}
			c["env"] = env
			containers[ci] = c
		}
		if err := unstructured.SetNestedSlice(d.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("%s: write spec.template.spec.containers: %w", deploymentName, err)
		}
		return nil
	}
	return nil
}

// flipSpiceDBInsecureForExternalTLS sets SPICEDB_INSECURE=false on the
// operator, authzd, and webd Deployments in docs when ext specifies an
// external SpiceDB reached over TLS (ext.enabled() && !ext.Insecure). It is a
// no-op otherwise, leaving the base bundle's plaintext default
// (SPICEDB_INSECURE="true") in place for the in-cluster operator-managed
// SpiceDB and for --external-spicedb-insecure.
//
// Runner pods need no separate flip here: the operator value-copies its own
// resolved SpiceDBInsecure onto every runner pod's env (see
// pkg/controllers/agentsession/podspec.go), so flipping the operator
// Deployment's env is sufficient — the runner picks it up transitively.
// channelsd is flipped separately, via its --spicedb-insecure CLI flag (see
// appendChannelsdInsecureFlag), because it has no SPICEDB_INSECURE env in the
// base bundle.
func flipSpiceDBInsecureForExternalTLS(docs []*unstructured.Unstructured, ext ExternalSpiceDB) error {
	if !ext.enabled() || ext.Insecure {
		return nil
	}
	for _, name := range []string{operatorDeploymentName, authzdDeploymentName, webdDeploymentName} {
		if err := setDeploymentEnv(docs, name, spicedb.EnvInsecure, "false"); err != nil {
			return fmt.Errorf("set %s=false on %s for external TLS SpiceDB: %w", spicedb.EnvInsecure, name, err)
		}
	}
	return nil
}

// appendChannelsdInsecureFlag finds the Deployment doc among ms (the
// channelsd manifest set returned by manifests.ChannelsD: ServiceAccount,
// ClusterRole, ClusterRoleBinding, Deployment) and idempotently appends
// channelsdInsecureFlag to its container's args, re-marshaling the mutated
// doc back into ms in place. Returns an error if no Deployment doc is found
// (a packaging-shape bug) or if the container/args shape is malformed.
func appendChannelsdInsecureFlag(ms [][]byte) error {
	for i, m := range ms {
		docs, err := manifests.Split(m)
		if err != nil {
			return fmt.Errorf("split channelsd manifest[%d]: %w", i, err)
		}
		if len(docs) != 1 || docs[0].GetKind() != "Deployment" {
			continue // ServiceAccount / ClusterRole / ClusterRoleBinding: not our target
		}
		d := docs[0]
		containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return fmt.Errorf("channelsd deployment: read spec.template.spec.containers: %w", err)
		}
		if !found || len(containers) == 0 {
			return fmt.Errorf("channelsd deployment: no spec.template.spec.containers")
		}
		c, ok := containers[0].(map[string]any)
		if !ok {
			return fmt.Errorf("channelsd deployment: container[0] is not a map")
		}
		args, err := stringSlice(c["args"])
		if err != nil {
			return fmt.Errorf("channelsd deployment: container args: %w", err)
		}
		if !containsString(args, channelsdInsecureFlag) {
			args = append(args, channelsdInsecureFlag)
			ifaceArgs := make([]any, len(args))
			for j, a := range args {
				ifaceArgs[j] = a
			}
			c["args"] = ifaceArgs
			containers[0] = c
			if err := unstructured.SetNestedSlice(d.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				return fmt.Errorf("channelsd deployment: write spec.template.spec.containers: %w", err)
			}
		}
		out, err := yaml.Marshal(d.Object)
		if err != nil {
			return fmt.Errorf("channelsd deployment: marshal: %w", err)
		}
		ms[i] = out
		return nil
	}
	return fmt.Errorf("channelsd manifest set: no Deployment doc found")
}

// upsertEnvValue returns the container env list ([]any of {name,value} maps)
// with name set to a literal value: an existing entry's value is replaced (and
// any valueFrom dropped), else a new entry is appended. It rejects a non-list
// env or a non-map element so a malformed manifest surfaces rather than
// silently dropping the override.
func upsertEnvValue(raw any, name, value string) ([]any, error) {
	var env []any
	if raw != nil {
		var ok bool
		env, ok = raw.([]any)
		if !ok {
			return nil, fmt.Errorf("expected a list, got %T", raw)
		}
	}
	for i, e := range env {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("env[%d] is %T, not a map", i, e)
		}
		if m["name"] == name {
			m["value"] = value
			delete(m, "valueFrom") // a literal value and valueFrom are mutually exclusive
			env[i] = m
			return env, nil
		}
	}
	return append(env, map[string]any{"name": name, "value": value}), nil
}
