# `config/workspace-provisioner/` — optional workspace storage tier

An agent session's runner, sandbox, and bundle sidecars all need to see the same
`/workspace`. On a cluster with no shared-filesystem StorageClass, this
directory installs a rancher `local-path-provisioner` configured to serve that
need, plus the `ap-workspace-rwx` StorageClass session PVCs request.

**This is an optional tier.** Every resource here carries
`agentprimitives.authzed.com/install-tier: workspace-provisioner-rwx`, which
`manifests.FilterByInstallTier` uses to split it out of the base region so
`oap install` applies it only when asked.

## Files

- `namespace.yaml` — the `ap-workspace-storage` namespace.
- `rbac.yaml` — ServiceAccount, ClusterRole, and binding. The provisioner needs
  `create`/`delete` on pods (not just read) because it provisions each volume by
  spawning a short-lived busybox helper pod.
- `config.yaml` — the provisioner ConfigMap: the `nodePathMap` config and the
  `setup` script.
- `deployment.yaml` — `rancher/local-path-provisioner:v0.0.32` with its flags.
- `storageclass.yaml` — `ap-workspace-rwx`, `WaitForFirstConsumer`,
  `reclaimPolicy: Delete`.

## Caveat: this is a shim, not real RWX

The backing path is a per-node `hostPath` directory, not a shared filesystem.
`config.yaml` uses `nodePathMap` rather than `sharedFileSystemPath` on purpose:
`sharedFileSystemPath` emits RWX PVs with no `nodeAffinity`, so on a multi-node
cluster a session's pods land on different nodes and see divergent (often empty)
`/workspace`. `nodePathMap` pins each PV to the node its first consumer
scheduled on. It provisions RWO only — which is fine, because RWO is
single-_node_, so the co-located session pods still share the volume.

Genuine shared RWX (Filestore / NFS / EFS) is the durable fix.

Nothing here is generated.

[← config/](../README.md)
