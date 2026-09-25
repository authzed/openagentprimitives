package gke

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// GKE's workspaceStorage carries the optional enable capability; cmd/oap
// type-asserts for it and runs it before Resolve. Locked at compile time.
var _ cloud.WorkspaceStorageEnabler = workspaceStorage{}

// Injection points (overridden in tests), mirroring gatewaycontroller.go.
var (
	filestoreClassPoll     = 5 * time.Second
	filestoreClassDeadline = 3 * time.Minute
	enableFilestoreCSI     = realEnableFilestoreCSI
)

// EnsureWorkspaceStorage makes a durable Filestore RWX class available on the
// current GKE cluster before Resolve runs. If a Filestore class already exists
// it is a no-op. Otherwise it derives the connected cluster's identity, offers
// to enable the Filestore CSI addon, runs `gcloud container clusters update …
// --update-addons=GcpFilestoreCsiDriver=ENABLED`, and waits until a class
// appears. A decline, a non-interactive run without --yes, or an underivable
// cluster identity returns nil (never targets a guessed cluster) and leaves
// Resolve to fall back — the bundled hostPath on Standard, Degraded on
// Autopilot. It is billable, so it is always an OFFER, never automatic.
func (workspaceStorage) EnsureWorkspaceStorage(ctx context.Context, p cloud.WorkspaceEnableParams) error {
	if has, err := hasFilestoreClass(ctx, p.Clients); err != nil {
		return err
	} else if has {
		return nil // a durable RWX class already exists
	}

	project, cluster, location, err := deriveClusterIdentity(ctx, p.Clients, p.Reporter)
	if err != nil {
		// Never target a guessed cluster — surface the manual command and skip.
		p.Reporter.Warn("no Filestore RWX StorageClass on this cluster, and oap could not derive which cluster to enable it on: %v", err)
		p.Reporter.Info("  for durable workspaces, enable it manually then re-run `oap init`:\n    gcloud container clusters update <cluster> --location=<region> --update-addons=GcpFilestoreCsiDriver=ENABLED")
		return nil
	}

	p.Reporter.Warn("no durable (Filestore) RWX StorageClass on cluster %q (%s); workspaces would fall back to node-local storage", cluster, location)
	p.Reporter.Info("  this runs: gcloud container clusters update %s --location=%s --update-addons=GcpFilestoreCsiDriver=ENABLED", cluster, location)
	if !cloud.Confirm(p.In, p.Reporter, fmt.Sprintf("Enable the Filestore CSI driver on cluster %q for durable workspaces? (billable)", cluster), p.AssumeYes) {
		p.Reporter.Warn("continuing without a durable RWX class; enable it later with:\n    gcloud container clusters update %s --location=%s --update-addons=GcpFilestoreCsiDriver=ENABLED\n  then re-run `oap init`", cluster, location)
		return nil
	}

	p.Reporter.Step("enable the Filestore CSI driver on %s (%s)", cluster, location)
	if err := enableFilestoreCSI(ctx, p.Reporter, project, cluster, location); err != nil {
		return fmt.Errorf("enable the Filestore CSI driver: %w", err)
	}
	return waitFilestoreClass(ctx, p)
}

// hasFilestoreClass reports whether ANY Filestore RWX class (multishare or
// per-PVC) is already registered — either means Resolve has a durable class to
// pick, so there is nothing to enable.
func hasFilestoreClass(ctx context.Context, cl cloud.Clients) (bool, error) {
	multishare, perPVC, err := cloud.FindPreferredRWXClass(ctx, cl.Typed, filestoreProvisioner, "multishare", "true")
	if err != nil {
		return false, fmt.Errorf("check for Filestore RWX class: %w", err)
	}
	return multishare != "" || perPVC != "", nil
}

// realEnableFilestoreCSI turns on the Filestore CSI addon for one cluster. Like
// the Gateway enable it is a multi-minute, progress-emitting operation, so it
// streams gcloud's output live rather than buffering it into a silent hang.
func realEnableFilestoreCSI(ctx context.Context, rep cloud.Reporter, project, cluster, location string) error {
	return cloud.GcloudStreaming(ctx, rep,
		"container", "clusters", "update", cluster,
		"--project="+project, "--location="+location, "--update-addons=GcpFilestoreCsiDriver=ENABLED")
}

// waitFilestoreClass polls until a Filestore RWX class appears or the deadline
// elapses. GKE creates the managed *-rwx classes shortly after the addon is on.
func waitFilestoreClass(ctx context.Context, p cloud.WorkspaceEnableParams) error {
	ctx, cancel := context.WithTimeout(ctx, filestoreClassDeadline)
	defer cancel()
	for {
		has, err := hasFilestoreClass(ctx, p.Clients)
		if err == nil && has {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no Filestore RWX StorageClass appeared within %s after enabling the addon", filestoreClassDeadline)
		case <-time.After(filestoreClassPoll):
		}
	}
}
