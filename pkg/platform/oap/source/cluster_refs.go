package source

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// refDescriptor describes one AgentClass ref field that names dependent CRs.
// cluster.go's walk is data-driven off refDescriptors — adding a new ref type
// (a future AgentClass field pointing at a new CRD) is one row here, not a
// change to the walk logic.
type refDescriptor struct {
	// Kind is the dependent CR's Kind: used to stamp the fetched object's GVK
	// before sanitizing/marshaling it, and as the CR-set/error identifier.
	Kind string

	// Names extracts the dependent CR names the AgentClass references for this
	// ref type. Returns nil/empty when nothing of this kind is referenced.
	Names func(ac *v1alpha1.AgentClass) []string

	// NewObj returns a fresh, empty client.Object of the dependent type — the
	// Get target.
	NewObj func() client.Object

	// Required marks a missing referenced CR as a hard, fail-closed export
	// error. Optional rows are logged and skipped instead.
	Required bool

	// ClusterScoped marks a dependent kind that is cluster-scoped (Get by name
	// alone, no namespace) rather than namespaced-alongside-the-AgentClass.
	ClusterScoped bool
}

// refDescriptors is the ref-graph walk table, one row per owning ref field on
// AgentClass.spec that names dependent CRs by name. Second-level refs (Toolspec
// -> Toolkit) are discovered separately in cluster.go — they resolve by
// List+filter on a spec field, not a Get-by-name, so they don't fit the
// Names/NewObj shape.
var refDescriptors = []refDescriptor{
	{
		// AgentIdentity refs: the class-default spec.agentIdentity PLUS every
		// per-bundle spec.toolBundles[].agentIdentity override. All are required
		// — a class that names an identity it can't resolve cannot run. dedup +
		// empty-drop happens in the walk (dedupNonEmpty), and each resolved
		// identity's credential secret refs are collected there too, so
		// Requires.Secrets stays complete across bundle overrides.
		Kind: "AgentIdentity",
		Names: func(ac *v1alpha1.AgentClass) []string {
			var names []string
			if ac.Spec.AgentIdentity != "" {
				names = append(names, ac.Spec.AgentIdentity)
			}
			for _, tb := range ac.Spec.ToolBundles {
				if tb.AgentIdentity != "" {
					names = append(names, tb.AgentIdentity)
				}
			}
			return names
		},
		NewObj:   func() client.Object { return &v1alpha1.AgentIdentity{} },
		Required: true,
	},
	{
		// spec.agentUI.ref: the AgentUI CR this class serves, a plain CR-name
		// reference like spec.agentIdentity and the .ref rows around it — NOT the
		// canonical-name + List+scan shape spec.skills uses. AgentUI is +optional
		// as a whole (Names returns nil when unset, so no Get happens), but once
		// spec.agentUI.ref names something, that something must resolve: Required
		// is true so a dangling ref fails the export loudly instead of shipping a
		// bundle whose AgentClass points at an AgentUI nobody packaged, where the
		// grant would resolve to nothing on install with no signal.
		Kind: "AgentUI",
		Names: func(ac *v1alpha1.AgentClass) []string {
			if ac.Spec.AgentUI == nil || ac.Spec.AgentUI.Ref == "" {
				return nil
			}
			return []string{ac.Spec.AgentUI.Ref}
		},
		NewObj:   func() client.Object { return &v1alpha1.AgentUI{} },
		Required: true,
	},
	{
		// spec.mcpServers[].ref: the MCPServer CR each ref names (not .Name,
		// which is the LLM-facing prefix).
		Kind: "MCPServer",
		Names: func(ac *v1alpha1.AgentClass) []string {
			names := make([]string, 0, len(ac.Spec.MCPServers))
			for _, ref := range ac.Spec.MCPServers {
				names = append(names, ref.Ref)
			}
			return names
		},
		NewObj:   func() client.Object { return &v1alpha1.MCPServer{} },
		Required: true,
	},
	{
		// spec.sidecarToolboxes[].ref: same shape as MCPServers above.
		Kind: "SidecarToolbox",
		Names: func(ac *v1alpha1.AgentClass) []string {
			names := make([]string, 0, len(ac.Spec.SidecarToolboxes))
			for _, ref := range ac.Spec.SidecarToolboxes {
				names = append(names, ref.Ref)
			}
			return names
		},
		NewObj:   func() client.Object { return &v1alpha1.SidecarToolbox{} },
		Required: true,
	},
	{
		// spec.toolBundles[].class: a cluster-scoped SpiceboxClass name.
		Kind: "SpiceboxClass",
		Names: func(ac *v1alpha1.AgentClass) []string {
			var names []string
			for _, tb := range ac.Spec.ToolBundles {
				names = append(names, tb.Class)
			}
			return names
		},
		NewObj:        func() client.Object { return &v1alpha1.SpiceboxClass{} },
		Required:      true,
		ClusterScoped: true,
	},
	{
		// spec.toolBundles[].toolspecs[]: cluster-scoped SpiceboxToolspec names.
		Kind: "SpiceboxToolspec",
		Names: func(ac *v1alpha1.AgentClass) []string {
			var names []string
			for _, tb := range ac.Spec.ToolBundles {
				names = append(names, tb.Toolspecs...)
			}
			return names
		},
		NewObj:        func() client.Object { return &v1alpha1.SpiceboxToolspec{} },
		Required:      true,
		ClusterScoped: true,
	},
}
