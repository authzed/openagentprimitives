// pkg/controllers/relationshipsource/doc.go
//
// Package relationshipsource reconciles RelationshipSource objects: it
// resolves spec.auth's credential, polls the upstream directory named by
// spec.kind (Slack first) through the registered relsync kind, and
// writes/prunes the membership relationships it reports in SpiceDB, one
// pass at a time. See controller.go for the Reconciler.
//
// The RBAC markers below landed ahead of the reconciler itself, in the same
// commit as the CRD. pkg/apis/v1alpha1/relationshipsource_types.go (that
// earlier task) is the only task in this series permitted to run
// `mage gen:api` and `mage manifests` — every later task, this one
// included, is forbidden from regenerating — so the ClusterRole grants this
// reconciler needs had to be emitted then, or the operator would crash-loop
// on "failed to wait for caches to sync" the moment the reconciler starts
// watching RelationshipSource with no way to regenerate its own RBAC.
//
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=relationshipsources,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=relationshipsources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
package relationshipsource
