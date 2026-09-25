package install

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// ChannelCredentialPreflight is the read-only graph ownership decision for a
// wizard-created credential Secret. Guard must travel to the eventual tracked
// apply so a replacement between planning and execution is not reclassified.
type ChannelCredentialPreflight struct {
	guard    wizardrun.ObjectGuard
	conflict *Conflict
}

// PreflightGraphChannelCredential observes a graph-planned credential Secret.
// Project provenance is necessary for wizard replacement, but it is not graph
// ownership: only the root install label authorizes a reinstall. A foreign or
// ownerless Secret is reported through the graph's normal conflict aggregation
// and is categorically non-adoptable because adopting it transfers credentials
// and uninstall eligibility together.
func PreflightGraphChannelCredential(ctx context.Context, c client.Client, rootInstallName, namespace, name string) (ChannelCredentialPreflight, error) {
	desired := &unstructured.Unstructured{}
	desired.SetAPIVersion("v1")
	desired.SetKind("Secret")
	desired.SetNamespace(namespace)
	desired.SetName(name)
	observed, err := wizardrun.ObserveObject(ctx, c, desired)
	if err != nil {
		return ChannelCredentialPreflight{}, fmt.Errorf("install: preflight channel credential Secret/%s: %w", name, err)
	}
	preflight := ChannelCredentialPreflight{guard: observed.Guard}
	if observed.Exists && observed.Labels[instance.LabelInstall] != rootInstallName {
		preflight.conflict = &Conflict{
			Kind: "Secret", Namespace: namespace, Name: name, Secret: true,
			adoptionRefused: true,
		}
	}
	return preflight, nil
}

// ApplyGuard returns the exact plan-time identity for the tracked write.
func (p ChannelCredentialPreflight) ApplyGuard() wizardrun.ObjectGuard { return p.guard }

// WithChannelCredentialPreflights attaches ownership decisions to a node's
// declarative plan so Prepare can aggregate them with bundle conflicts before
// the graph performs any write.
func (p PlannedChannels) WithChannelCredentialPreflights(preflights ...ChannelCredentialPreflight) PlannedChannels {
	for _, preflight := range preflights {
		if preflight.conflict != nil {
			p.conflicts = append(p.conflicts, *preflight.conflict)
		}
	}
	return p
}
