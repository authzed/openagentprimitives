package installcmd

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// DetectedSettings is the current cluster's install settings, gathered by a
// read-only sweep of existing objects. It drives the "existing install
// detected" copy on the init wizard's Detected screen and gates
// --accept-existing's loud-failure check.
//
// TrustedHostname and SandboxHostname are read from the installed webd
// HTTPRoutes. ExternalSpiceDB is true when the cluster's shared SpiceDB endpoint
// ConfigMap points off-cluster (a prior --external-spicedb-endpoint install);
// ExternalSpiceDBEndpoint carries that stored endpoint so the wizard can name it
// when it refuses to silently flip the backend to the bundled in-cluster one.
type DetectedSettings struct {
	Existing                bool
	ACMEEmail               string
	TrustedHostname         string
	SandboxHostname         string
	WorkspaceClass          string
	IdPKind                 string
	MonitoringChannel       string
	ExternalSpiceDB         bool
	ExternalSpiceDBEndpoint string
}

// detectDeps carries the clients detectSettings needs. Typed is required
// separately from Ctrl because readWorkspaceMarker reads the workspace
// marker ConfigMap through the typed clientset, not the controller-runtime
// client.
type detectDeps struct {
	Dyn       dynamic.Interface
	Ctrl      client.Client
	Typed     kubernetes.Interface
	Namespace string
}

// detectSettings gathers the current install's settings from the live
// cluster. Each field reuses an existing read path —
// cloud.DetectLetsEncryptEmail, readWorkspaceMarker, the
// ClusterIdentityProvider Get, channelcmd.ListMonitoringChannels — no new
// detection logic is invented here. A NotFound (or a found=false result)
// leaves that field at its zero value rather than erroring, so a fresh
// cluster returns the zero DetectedSettings (Existing=false) and only an
// unexpected API error propagates.
//
// A cluster with a CRD not yet installed — every CRD detectSettings reads is
// either ap's own (ahead of `oap install`) or the Gateway API's (any --local
// install, which never installs it) — answers meta.IsNoMatchError instead of
// apierrors.IsNotFound: the controller-runtime client resolves GVK->GVR
// through its RESTMapper before the request ever reaches the apiserver, so a
// missing CRD's whole API group is unregistered there, not merely the object
// absent. Every read below tolerates both the same way.
func detectSettings(ctx context.Context, d detectDeps) (DetectedSettings, error) {
	var out DetectedSettings

	email, err := cloud.DetectLetsEncryptEmail(ctx, cloud.Clients{Dynamic: d.Dyn})
	if err != nil {
		return out, fmt.Errorf("detect ACME email: %w", err)
	}
	out.ACMEEmail = email

	cls, _, err := readWorkspaceMarker(ctx, d.Typed)
	if err != nil {
		return out, fmt.Errorf("detect workspace class: %w", err)
	}
	out.WorkspaceClass = cls

	var idp spiceboxv1alpha1.ClusterIdentityProvider
	switch err := d.Ctrl.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &idp); {
	case err == nil:
		out.IdPKind = idp.Spec.Kind
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		// None configured yet, or the ClusterIdentityProvider CRD isn't
		// installed at all (a fresh cluster, ahead of `oap install`) — either
		// way leave IdPKind at its zero value.
	default:
		return out, fmt.Errorf("detect identity provider: %w", err)
	}

	chans, err := channelcmd.ListMonitoringChannels(ctx, d.Ctrl, d.Namespace)
	switch {
	case err == nil:
		if len(chans) > 0 {
			out.MonitoringChannel = chans[0].Name
		}
	case meta.IsNoMatchError(err):
		// The Channel CRD isn't installed yet — leave MonitoringChannel at its
		// zero value, same as "no monitoring channel exists".
	default:
		return out, fmt.Errorf("detect monitoring channels: %w", err)
	}

	// The webd external-access hostnames live on the two installed HTTPRoutes.
	// A NotFound (no route yet) or a NoKindMatch (no Gateway API CRDs at all)
	// leaves the field empty; only an unexpected API error propagates, wrapped.
	trusted, err := readWebdRouteHostname(ctx, d.Ctrl, cloud.WebdServiceNamespace, webdTrustedRouteName)
	if err != nil {
		return out, fmt.Errorf("detect trusted hostname: %w", err)
	}
	out.TrustedHostname = trusted

	sandbox, err := readWebdRouteHostname(ctx, d.Ctrl, cloud.WebdServiceNamespace, webdSandboxRouteName)
	if err != nil {
		return out, fmt.Errorf("detect sandbox hostname: %w", err)
	}
	out.SandboxHostname = sandbox

	external, extEndpoint, err := detectExternalSpiceDB(ctx, d.Typed)
	if err != nil {
		return out, fmt.Errorf("detect external SpiceDB: %w", err)
	}
	out.ExternalSpiceDB = external
	out.ExternalSpiceDBEndpoint = extEndpoint

	// A trusted hostname is a durable trace of an install on its own — an
	// install whose only recorded routing is its web-UI origin is still existing.
	// So is a SpiceDB endpoint pointing at an external instance.
	out.Existing = out.ACMEEmail != "" || out.WorkspaceClass != "" || out.IdPKind != "" || out.MonitoringChannel != "" || out.TrustedHostname != "" || out.ExternalSpiceDB
	return out, nil
}

// detectExternalSpiceDB reports whether the cluster's shared SpiceDB endpoint
// ConfigMap (spicebox-spicedb-config) points at an external instance rather than
// the bundled in-cluster Service, returning the stored endpoint when it does.
// The install itself writes the evidence: the in-cluster path stamps
// inClusterSpiceDBEndpoint, an --external-spicedb-endpoint install stamps the
// caller's address. A missing ConfigMap (fresh cluster, ahead of `oap install`)
// or an endpoint equal to the in-cluster Service means "not external". Read
// through the typed clientset, mirroring readWorkspaceMarker — the ConfigMap is
// a core resource, so a missing CRD's NoKindMatchError cannot arise here.
func detectExternalSpiceDB(ctx context.Context, typed kubernetes.Interface) (bool, string, error) {
	cm, err := typed.CoreV1().ConfigMaps(cloud.WebdServiceNamespace).Get(ctx, spicedb.SharedConfigMapName, metav1.GetOptions{})
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		return false, "", nil // no SpiceDB config yet — fresh cluster
	default:
		return false, "", err
	}
	endpoint := strings.TrimSpace(cm.Data[spicedb.SharedConfigMapEndpointKey])
	if endpoint == "" || endpoint == inClusterSpiceDBEndpoint {
		return false, "", nil // bundled in-cluster SpiceDB (or unset)
	}
	return true, endpoint, nil
}
