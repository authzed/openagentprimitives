package oap

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Capabilities is what installing a bundle graph would hand its agents: the
// tools they can call, the network they can reach, the credentials they hold,
// and the other agents that come with them. It is derived from the bundled
// CRs and manifests of the root and every dependency, for a consent prompt to
// show before the install, so it names things rather than judging them.
// Every entry is bundle-supplied text: a renderer must sanitize it.
type Capabilities struct {
	// MCPServers are bundled MCP servers, as "name (url)".
	MCPServers []string
	// SidecarToolboxes are bundled sidecar MCP servers, as "name (image)".
	SidecarToolboxes []string
	// Toolspecs are bundled sandbox tool specs, by name.
	Toolspecs []string
	// Network is every bundled egress grant: sandbox classes that allow
	// egress, sidecar toolbox host allowlists, and credential host scopes.
	Network []string
	// Credentials are bundled AgentIdentity credentials, as "identity/name (type)".
	Credentials []string
	// Secrets are the Secrets the install will ask the user to provide.
	Secrets []string
	// ClusterDeps are shared cluster-scoped resources the agent uses, as "Kind/name".
	ClusterDeps []string
	// Agents are the bundled dependency agents, as "path vVersion".
	Agents []string
}

// Empty reports whether the graph grants nothing worth listing.
func (c Capabilities) Empty() bool {
	return len(c.MCPServers)+len(c.SidecarToolboxes)+len(c.Toolspecs)+len(c.Network)+
		len(c.Credentials)+len(c.Secrets)+len(c.ClusterDeps)+len(c.Agents) == 0
}

// SummarizeCapabilities walks root and its dependencies (parents first, so the
// root's own grants lead) and collects what each would install. A CR that
// does not decode as its declared kind is an error: a prompt that silently
// skipped it would understate what the user is approving.
func SummarizeCapabilities(root *Bundle) (Capabilities, error) {
	var c Capabilities
	err := WalkDependencies(root, ParentsFirst, func(path DependencyPath, b *Bundle) error {
		where := graphDisplayPath(root, path)
		if b.Manifest == nil {
			return fmt.Errorf("summarize %s: bundle has no manifest", where)
		}
		if len(path) > 0 {
			c.Agents = append(c.Agents, fmt.Sprintf("%s v%s", path, b.Manifest.Agent.Version))
		}
		for _, s := range b.Manifest.Requires.Secrets {
			c.Secrets = append(c.Secrets, s.Name)
		}
		for _, d := range b.Manifest.Requires.ClusterDeps {
			c.ClusterDeps = append(c.ClusterDeps, d.Kind+"/"+d.Name)
		}
		crs, err := b.CRs()
		if err != nil {
			return fmt.Errorf("summarize %s: %w", where, err)
		}
		for _, cr := range crs {
			if err := c.addCR(cr); err != nil {
				return fmt.Errorf("summarize %s: %s %q: %w", where, cr.GetKind(), cr.GetName(), err)
			}
		}
		return nil
	})
	return c, err
}

// addCR records cr's grants. Kinds that grant nothing on their own (the
// AgentClass, skills, UI, ConfigMaps) are skipped: what an AgentClass can use
// is the bundled kinds below plus the cluster deps already listed.
func (c *Capabilities) addCR(cr *unstructured.Unstructured) error {
	if cr.GroupVersionKind().Group != spiceboxv1alpha1.GroupName {
		return nil
	}
	name := cr.GetName()
	switch cr.GetKind() {
	case "MCPServer":
		var o spiceboxv1alpha1.MCPServer
		if err := fromUnstructured(cr, &o); err != nil {
			return err
		}
		c.MCPServers = append(c.MCPServers, fmt.Sprintf("%s (%s)", name, o.Spec.Server.URL))
	case "SidecarToolbox":
		var o spiceboxv1alpha1.SidecarToolbox
		if err := fromUnstructured(cr, &o); err != nil {
			return err
		}
		image := o.Spec.Source.Image
		if image == "" {
			image = "built from an inline script"
		}
		c.SidecarToolboxes = append(c.SidecarToolboxes, fmt.Sprintf("%s (%s)", name, image))
		if hosts := o.Spec.Sandbox.Network.AllowedHosts; len(hosts) > 0 {
			c.Network = append(c.Network, fmt.Sprintf("toolbox %s: %s", name, strings.Join(hosts, ", ")))
		}
	case "SpiceboxToolspec":
		c.Toolspecs = append(c.Toolspecs, name)
	case "SpiceboxClass":
		var o spiceboxv1alpha1.SpiceboxClass
		if err := fromUnstructured(cr, &o); err != nil {
			return err
		}
		if mode := o.Spec.Network.Mode; mode != "" && mode != spiceboxv1alpha1.NetworkModeNone {
			grant := fmt.Sprintf("sandbox %s: %s", name, mode)
			if hosts := o.Spec.Network.AllowedHosts; len(hosts) > 0 {
				grant += " (" + strings.Join(hosts, ", ") + ")"
			}
			c.Network = append(c.Network, grant)
		}
	case "AgentIdentity":
		var o spiceboxv1alpha1.AgentIdentity
		if err := fromUnstructured(cr, &o); err != nil {
			return err
		}
		for _, cred := range o.Spec.Credentials {
			c.Credentials = append(c.Credentials, fmt.Sprintf("%s/%s (%s)", name, cred.Name, cred.Type))
			if len(cred.AllowedHosts) > 0 {
				c.Network = append(c.Network, fmt.Sprintf("credential %s/%s: %s", name, cred.Name, strings.Join(cred.AllowedHosts, ", ")))
			}
		}
	}
	return nil
}

func fromUnstructured(cr *unstructured.Unstructured, into any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, into)
}
