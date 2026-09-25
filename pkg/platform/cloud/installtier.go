package cloud

// InstallTierLabelKey marks a resource with the install tier that created it.
// It is the ONLY handle `oap clean` has for telling apart something oap installed
// from something that was already on the cluster, and the handle
// `manifests.FilterByInstallTier` uses to apply a subset of the bundle
// conditionally — so a resource that carries it is one oap may delete and a
// resource that does not is one it must leave alone.
//
// Writer and reader live in different packages — the GKE stateful strategy
// stamps a cluster-scoped StorageClass, `oap clean` finds it again by label
// selector — so both resolve these constants: a literal typo on either side
// would orphan the class with no compile error.
const InstallTierLabelKey = "agentprimitives.authzed.com/install-tier"

// Install tier values. Each names the install step that owns the resource.
const (
	// InstallTierWorkspaceProbe marks the throwaway PVC + consumer Pod a
	// provisioning probe creates. Always cleaned up by the probe itself; the
	// label exists so a probe abandoned by a killed install is still findable.
	InstallTierWorkspaceProbe = "workspace-probe"

	// InstallTierStatefulStorage marks the RWO StorageClass oap creates for the
	// bundled stateful PVCs when the cluster's default class will not do.
	InstallTierStatefulStorage = "stateful-storage"

	// InstallTierWorkspaceProvisioner marks the bundled local-path RWX
	// provisioner's manifests inside the install bundle, so oap install can hold
	// them back on a cluster that cannot run hostPath.
	InstallTierWorkspaceProvisioner = "workspace-provisioner-rwx"
)
