// Package imageload classifies how a locally-built Docker image reaches the
// node backing a given kube-context.
//
// The classification is TOTAL: every context maps to exactly one Disposition.
// An earlier version returned a nil argv for both "no load needed"
// (docker-desktop) and "no load possible" (any remote context), and all three
// call sites read that nil as "carry on" — which let `oap agent install` report
// success for an image the cluster could never pull.
package imageload

import "strings"

// APContextName is the oap-desktop VM's kube-context name. It lives in this
// dependency-free package so the classification can stay total;
// desktop.APContextName aliases it.
const APContextName = "oap-desktop"

// LegacyAPContextName is the pre-rename context name; the kubeconfig merge
// removes entries under it and IsVMContextCurrent still honors it.
const LegacyAPContextName = "ap-desktop"

// Disposition names how (or whether) an image can reach the context's node.
type Disposition int

const (
	// NotNeeded: the node shares the laptop's Docker daemon, so a local build
	// is already visible to the cluster.
	NotNeeded Disposition = iota
	// LocalLoad: the node's image store is reachable from the laptop via a
	// cluster-runtime CLI. Plan.Argv holds the command.
	LocalLoad
	// VMLoad: the oap-desktop VM, loaded over SSH (docker save | k3s ctr images
	// import). The mechanism needs an SSH runner and image saver, so it lives in
	// cmd/oap — this package only classifies.
	VMLoad
	// NeedsRegistry: the laptop cannot reach the node's container runtime at
	// all. The only delivery channel is a registry the node can pull from.
	NeedsRegistry
)

func (d Disposition) String() string {
	switch d {
	case NotNeeded:
		return "not-needed"
	case LocalLoad:
		return "local-load"
	case VMLoad:
		return "vm-load"
	case NeedsRegistry:
		return "needs-registry"
	default:
		return "unknown"
	}
}

// Plan is the classification of a context plus, for LocalLoad only, the argv
// that performs the load.
type Plan struct {
	Disposition Disposition
	Argv        []string // non-nil iff Disposition == LocalLoad
}

// For classifies kctx and, for LocalLoad contexts, returns the argv that loads
// tag into that cluster's nodes.
func For(kctx, tag string) Plan {
	switch {
	case strings.HasPrefix(kctx, "kind-"):
		return Plan{
			Disposition: LocalLoad,
			Argv:        []string{"kind", "load", "docker-image", tag, "--name", strings.TrimPrefix(kctx, "kind-")},
		}
	case strings.HasPrefix(kctx, "k3d-"):
		return Plan{
			Disposition: LocalLoad,
			Argv:        []string{"k3d", "image", "import", tag, "-c", strings.TrimPrefix(kctx, "k3d-")},
		}
	case kctx == "minikube":
		return Plan{Disposition: LocalLoad, Argv: []string{"minikube", "image", "load", tag}}
	// LegacyAPContextName still denotes the same VM during the migration
	// window: the kubeconfig merge renames it on the next context toggle /
	// desktop bring-up, but until then an upgraded user's current context
	// keeps the old name and must not degrade from VMLoad to NeedsRegistry.
	case kctx == APContextName, kctx == LegacyAPContextName:
		return Plan{Disposition: VMLoad}
	case kctx == "docker-desktop":
		return Plan{Disposition: NotNeeded}
	default:
		return Plan{Disposition: NeedsRegistry}
	}
}
