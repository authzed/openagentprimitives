package gke

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const managedGatewayClass = "gke-l7-global-external-managed"

// Injection points (overridden in tests), mirroring unwedge.go's deleteGCPNEG /
// gcloudOnPath pattern.
var (
	gatewayAPIServed  = cloud.GatewayAPIServed
	gatewayServedPoll = 5 * time.Second
	// gatewayServedDeadline bounds the post-enable wait for the control plane to
	// start serving the Gateway API. The blocking cluster-update usually returns
	// with it already served; this covers discovery propagation. A var (not const)
	// so tests can shorten it without sleeping the full 3 minutes.
	gatewayServedDeadline   = 3 * time.Minute
	enableGatewayAPI        = realEnableGatewayAPI
	describeClusterIdentity = realDescribeClusterIdentity
)

// EnsureGatewayController makes the managed Gateway API available on the current
// GKE cluster: if it isn't served, it derives the connected cluster's identity,
// offers to enable it, runs `gcloud container clusters update … --gateway-api=
// standard`, and waits until the API is served. Proceed=false on decline, a
// non-interactive run without --yes, or when the cluster identity cannot be
// derived (it never targets a guessed cluster).
//
// If GatewayClassOverride is set it selects the class name to bind, but the
// Gateway API served check still runs. When the override is set and the API is
// not served, oap surfaces a diagnostic and returns Proceed=false rather than
// enabling GKE's managed Gateway API on the operator's behalf (their controller
// may not be GKE's managed one).
func (Strategy) EnsureGatewayController(ctx context.Context, p cloud.GatewayControllerParams) (cloud.GatewayControllerResult, error) {
	// Determine the class to bind. The override selects the name but does NOT
	// skip the served check — an absent Gateway API crashes HTTPRoute applies
	// regardless of which class is requested.
	gwClass := managedGatewayClass
	if p.GatewayClassOverride != "" {
		gwClass = p.GatewayClassOverride
	}

	served, err := gatewayAPIServed(p.Clients.REST)
	if err != nil {
		return cloud.GatewayControllerResult{}, fmt.Errorf("check Gateway API availability: %w", err)
	}
	if served {
		return cloud.GatewayControllerResult{Proceed: true, GatewayClass: gwClass}, nil
	}

	// Gateway API is not served. If the operator supplied their own class, oap
	// must not enable GKE's managed Gateway API on their behalf — their
	// controller may be entirely different. Surface the gap and skip.
	if p.GatewayClassOverride != "" {
		p.Reporter.Warn("--gateway-class %q was set, but the Gateway API (gateway.networking.k8s.io) isn't served on this cluster; oap won't enable GKE's managed Gateway API when a custom class is requested.", p.GatewayClassOverride)
		p.Reporter.Info("  install the Gateway controller that serves %q (or drop --gateway-class to use GKE's managed Gateway API), then re-run `oap init`.", p.GatewayClassOverride)
		return cloud.GatewayControllerResult{Proceed: false}, nil
	}

	project, cluster, location, err := deriveClusterIdentity(ctx, p.Clients, p.Reporter)
	if err != nil {
		// Can't safely target a cluster — surface the placeholder command and skip.
		p.Reporter.Warn("the Gateway API (gateway.networking.k8s.io) is not enabled on this GKE cluster, and oap could not derive which cluster to enable it on: %v", err)
		p.Reporter.Info("  enable it manually, then re-run `oap init`:\n    gcloud container clusters update <cluster> --location=<region> --gateway-api=standard")
		return cloud.GatewayControllerResult{Proceed: false}, nil
	}

	p.Reporter.Warn("the Gateway API (gateway.networking.k8s.io) is not enabled on cluster %q (%s) — webd external access needs it", cluster, location)
	p.Reporter.Info("  this runs: gcloud container clusters update %s --location=%s --gateway-api=standard", cluster, location)
	if !cloud.Confirm(p.In, p.Reporter, fmt.Sprintf("Enable the GKE Gateway API on cluster %q now?", cluster), p.AssumeYes) {
		p.Reporter.Warn("skipping webd external access; enable it with:\n    gcloud container clusters update %s --location=%s --gateway-api=standard\n  then re-run `oap init`", cluster, location)
		return cloud.GatewayControllerResult{Proceed: false}, nil
	}

	p.Reporter.Step("enable the GKE Gateway API on %s (%s)", cluster, location)
	if err := enableGatewayAPI(ctx, p.Reporter, project, cluster, location); err != nil {
		return cloud.GatewayControllerResult{}, fmt.Errorf("enable the GKE Gateway API: %w", err)
	}
	if err := waitGatewayServed(ctx, p); err != nil {
		return cloud.GatewayControllerResult{}, err
	}
	return cloud.GatewayControllerResult{Proceed: true, GatewayClass: gwClass}, nil
}

// deriveClusterIdentity finds the cluster oap is connected to by reading the
// cluster-name/cluster-location GCE metadata off one of its nodes. It never
// returns another cluster's identity: the node belongs to the current cluster.
// The project is taken from the node's gce:// providerID — the cluster lives in
// the same project as its nodes — so callers can target `gcloud` explicitly
// rather than relying on the operator's (possibly unset) default project config.
func deriveClusterIdentity(ctx context.Context, cl cloud.Clients, rep cloud.Reporter) (project, cluster, location string, err error) {
	nodes, err := cl.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 5})
	if err != nil {
		return "", "", "", fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		project, zone, instance, ok := parseGCEProviderIDParts(nodes.Items[i].Spec.ProviderID)
		if !ok {
			continue
		}
		cluster, location, err := describeClusterIdentity(ctx, rep, project, zone, instance)
		if err != nil {
			return "", "", "", err
		}
		return project, cluster, location, nil
	}
	return "", "", "", fmt.Errorf("no node with a gce:// providerID found")
}

// realDescribeClusterIdentity reads cluster-name + cluster-location from a node
// instance's GCE metadata via gcloud.
func realDescribeClusterIdentity(ctx context.Context, rep cloud.Reporter, project, zone, instance string) (cluster, location string, err error) {
	out, err := cloud.Gcloud(ctx, rep,
		"compute", "instances", "describe", instance,
		"--zone="+zone, "--project="+project, "--format=json(metadata.items)")
	if err != nil {
		return "", "", fmt.Errorf("describe node instance %q: %w", instance, err)
	}
	var parsed struct {
		Metadata struct {
			Items []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"items"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return "", "", fmt.Errorf("parse instance metadata: %w", err)
	}
	for _, it := range parsed.Metadata.Items {
		switch it.Key {
		case "cluster-name":
			cluster = it.Value
		case "cluster-location":
			location = it.Value
		}
	}
	if cluster == "" || location == "" {
		return "", "", fmt.Errorf("node instance %q is missing cluster-name/cluster-location metadata", instance)
	}
	return cluster, location, nil
}

// realEnableGatewayAPI turns on the managed Gateway API for one cluster. This is
// a multi-minute, progress-emitting operation, so it streams gcloud's output live
// (via GcloudStreaming) rather than buffering it — otherwise the operator sees a
// silent hang while the cluster updates. gcloud blocks until the operation
// completes.
func realEnableGatewayAPI(ctx context.Context, rep cloud.Reporter, project, cluster, location string) error {
	return cloud.GcloudStreaming(ctx, rep,
		"container", "clusters", "update", cluster,
		"--project="+project, "--location="+location, "--gateway-api=standard")
}

// waitGatewayServed polls discovery until the Gateway API is served or the
// deadline elapses. The last non-nil error from the discovery check is folded
// into the timeout message so the operator knows why the poll kept failing.
func waitGatewayServed(ctx context.Context, p cloud.GatewayControllerParams) error {
	ctx, cancel := context.WithTimeout(ctx, gatewayServedDeadline)
	defer cancel()
	var lastErr error
	for {
		served, err := gatewayAPIServed(p.Clients.REST)
		if err == nil && served {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("Gateway API did not become served within %s after enabling it (last check: %w)", gatewayServedDeadline, lastErr)
			}
			return fmt.Errorf("Gateway API did not become served within %s after enabling it", gatewayServedDeadline)
		case <-time.After(gatewayServedPoll):
		}
	}
}
