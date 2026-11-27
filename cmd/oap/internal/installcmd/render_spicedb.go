package installcmd

import (
	"bytes"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// SpiceDB provisioning for the GitOps path.
//
// oap install creates these imperatively (buildCoreComponents): a Job that
// creates the 'spicedb' Postgres database, a postgres-mode SpiceDBCluster the
// bundled spicedb-operator serves, and the spicebox-spicedb-config ConfigMap
// every client reads its endpoint from. The overlay ships the same three,
// built by the same functions. With --external-spicedb-endpoint it ships only
// the ConfigMap, pointing at the user's instance, plus a patch that flips every
// client to TLS unless --external-spicedb-insecure.
const (
	spicedbResourceFile = "spicedb.yaml"
	spicedbTLSPatchFile = "patches/spicedb-tls.yaml"
)

// renderSpiceDBResources returns the spicedb.yaml overlay resource for cfg.
func renderSpiceDBResources(cfg RenderConfig) ([]byte, error) {
	endpoint := inClusterSpiceDBEndpoint
	if cfg.externalSpiceDB() {
		endpoint = cfg.ExternalSpiceDBEndpoint
	}
	cm, err := marshalClean(&corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: spicedb.SharedConfigMapName, Namespace: cloud.WebdServiceNamespace},
		Data:       map[string]string{spicedb.SharedConfigMapEndpointKey: endpoint},
	})
	if err != nil {
		return nil, err
	}
	if cfg.externalSpiceDB() {
		return cm, nil
	}

	// A persistent datastore only: render refuses memory-datastore kinds
	// (validateRenderFlags), and buildSpiceDBClusterDoc refuses to emit
	// datastoreBootstrapFiles for this mode, so the overlay cannot carry the
	// first-boot-only seed that is fatal against a non-empty datastore.
	// No activeDeadlineSeconds: this Job lands in the same apply as Postgres
	// (see buildSpiceDBDatabaseJob).
	_, db, err := buildSpiceDBDatabaseJob(0, nil)
	if err != nil {
		return nil, fmt.Errorf("build spicedb database job: %w", err)
	}
	cluster, err := buildSpiceDBClusterDoc(SpiceDBPostgres)
	if err != nil {
		return nil, fmt.Errorf("build SpiceDBCluster: %w", err)
	}
	// The SpiceDBCluster CRD is applied in the same sync as this CR, so Argo's
	// dry-run would otherwise reject the CR for an unknown kind.
	cluster, err = annotateDoc(cluster, map[string]string{argoSyncOptionsAnnotation: "SkipDryRunOnMissingResource=true"})
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{cm, db, cluster}, []byte("---\n")), nil
}

// spicedbTLSPatch flips the SpiceDB clients to TLS for an external instance —
// the overlay twin of oap install's flipSpiceDBInsecureForExternalTLS (the
// operator, authzd and webd env) and appendChannelsdInsecureFlag (channelsd's
// arg). emitted is false when no flip is needed: the bundled SpiceDB and
// --external-spicedb-insecure both keep the base's plaintext default.
func spicedbTLSPatch(cfg RenderConfig) (patch []byte, emitted bool) {
	if !cfg.externalSpiceDB() || cfg.ExternalSpiceDBInsecure {
		return nil, false
	}
	envDoc := func(deployment, container string) string {
		return "apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n" +
			"  name: " + deployment + "\n" +
			"  namespace: " + cloud.WebdServiceNamespace + "\n" +
			"spec:\n" +
			"  template:\n" +
			"    spec:\n" +
			"      containers:\n" +
			"        - name: " + container + "\n" +
			"          env:\n" +
			"            - name: " + spicedb.EnvInsecure + "\n" +
			"              value: \"false\"\n"
	}
	return []byte(envDoc(operatorDeploymentName, "operator") +
		"---\n" + envDoc(authzdDeploymentName, "authzd") +
		"---\n" + envDoc(webdDeploymentName, "webd")), true
}

// spicedbTLSChannelsdPatch appends channelsd's --spicedb-insecure=false arg. It
// is a JSON6902 patch because a strategic merge would replace the whole args
// list rather than append to it.
func spicedbTLSChannelsdPatch() string {
	return "  - target:\n" +
		"      kind: Deployment\n" +
		"      name: spicebox-channelsd\n" +
		"    patch: |-\n" +
		"      - op: add\n" +
		"        path: /spec/template/spec/containers/0/args/-\n" +
		"        value: " + channelsdInsecureFlag + "\n"
}
