package wizardrun

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

const (
	// FieldManager owns the objects a channel wizard creates: the Channel and
	// its credentials Secret. EVERY client that drives a wizard applies under
	// it, because all of them are the same actor as far as the apiserver is
	// concerned — a channel wizard landing its own output. Two managers for one
	// set of fields would make a later `oap channel create` over a Channel the
	// admin UI wired fight it for ownership.
	FieldManager = "ap-channel-create"

	// CapabilityFieldManager owns ONLY spec.capabilities on the AgentClass a
	// channel is bound to.
	//
	// Separate from FieldManager, and separate from the agent installer's
	// (install.FieldManager), because the AgentClass is not the wizard's
	// object: it was created elsewhere and is owned elsewhere, and the
	// wizard's claim on it must be exactly the capability keys the capability
	// screen asked about. Sharing a manager with the installer would make a
	// later `oap agent install` and the wizard fight over one set of fields.
	CapabilityFieldManager = "ap-channel-capabilities"
)

// Applier lands one object under a field manager. It is the ONE thing the two
// clients genuinely do differently: `oap` holds a dynamic client and resolves
// the GVR from the embedded CRDs, while a server-side caller already has a
// controller-runtime client with the scheme registered.
//
// Note that it is the MECHANISM only. Which objects go, in what order, under
// which manager, and what is refused in front of them are decisions this
// package makes for both — see Apply.
type Applier interface {
	ApplyObject(ctx context.Context, obj *unstructured.Unstructured, fieldManager string) error
}

// clientApplier is the controller-runtime SSA applier.
type clientApplier struct {
	c client.Client
	// installedBy, when non-empty, is stamped as the
	// agentprimitives.authzed.com/installed-by annotation, the marker
	// `oap clean` reads to tell ap-managed resources from pre-existing cluster
	// infrastructure. It is a STABLE constant, never an observation, so a
	// byte-identical re-apply stays a no-op.
	installedBy string
}

// ClientApplier is the Applier for a caller that already holds a
// controller-runtime client — admind, and any other in-cluster component.
//
// installedBy is the value for the installed-by provenance annotation
// (cloud.InstalledByAnnotation / kube.InstalledByValue, "ap"), or "" to stamp
// none. It is a parameter rather than a constant here because the annotation's
// meaning belongs to whoever is doing the installing, and this package is not
// that; the CLI's own applier stamps it from kube.Apply for the same reason.
func ClientApplier(c client.Client, installedBy string) Applier {
	return clientApplier{c: c, installedBy: installedBy}
}

func (a clientApplier) ApplyObject(ctx context.Context, obj *unstructured.Unstructured, fieldManager string) error {
	if a.installedBy != "" {
		anns := obj.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[InstalledByAnnotation] = a.installedBy
		obj.SetAnnotations(anns)
	}
	if err := a.c.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("ssa-apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

// InstalledByAnnotation marks a resource as one ap created, so `oap clean` can
// tell it from pre-existing cluster infrastructure.
//
// Spelled here rather than imported because the two places that already define
// it — cmd/oap/internal/kube (unreachable from pkg/) and pkg/platform/cloud
// (whose cluster-kind registry has nothing to do with a channel) — would each
// be the wrong dependency for this package to take on. It is an API-server
// string constant; a test in this package pins it against cloud's spelling so
// the three cannot drift.
const InstalledByAnnotation = "agentprimitives.authzed.com/installed-by"

// Apply lands what a channel wizard produced: the credentials Secret, the
// Channel, and the capability patch for the AgentClass the Channel binds to.
//
// c is what the refuse-to-overwrite net reads and may be nil (an offline run
// refuses nothing). ap is how each object is actually written.
//
// WHAT IS DECIDED HERE, and is the reason this is not per-client code:
//
//   - The refusal in front. The apply below is a server-side apply (upsert),
//     so a duplicate name would silently merge over a Channel the operator did
//     not mean to touch. Every client already steers away from a taken name
//     earlier; this is the last net, for an explicitly supplied collision or a
//     race. ReplaceExisting bypasses it, and only the wizard itself sets that
//     — when it matched and is intentionally updating an existing (monitoring)
//     Channel in place.
//
//   - A nil SecretManifest means the wizard intentionally left the existing
//     creds Secret untouched (re-setup where the operator left the token
//     blank); it is skipped rather than applied as an empty object.
//
//   - The capability patch goes LAST, because the failure it leaves behind is
//     the safe one. A Channel that exists with the agent's capabilities
//     unchanged is a Channel whose newly requested features are off; the
//     reverse — capabilities enabled for a Channel that failed to be created —
//     turns things on with nothing to use them.
//
//   - The patch goes DIRECTLY, without the marshal + Split round trip the
//     typed manifests take: it is already the unstructured shape an Applier
//     consumes, and a round trip could only reintroduce fields the wizard
//     deliberately left out.
func Apply(ctx context.Context, c client.Client, ap Applier, wizOut channelkinds.WizardOutput) error {
	if ap == nil {
		return fmt.Errorf("wizardrun: no applier wired, so there is nothing to land this channel with")
	}
	if ch := wizOut.ChannelManifest; ch != nil && !wizOut.ReplaceExisting {
		if err := RefuseExisting(ctx, c, ch.Namespace, ch.Name); err != nil {
			return err
		}
	}

	toApply := make([]any, 0, 2)
	if wizOut.SecretManifest != nil {
		if err := refuseForeignSecret(ctx, c, wizOut.SecretManifest.Namespace, wizOut.SecretManifest.Name); err != nil {
			return err
		}
		toApply = append(toApply, wizOut.SecretManifest)
	}
	if wizOut.ChannelManifest != nil {
		toApply = append(toApply, wizOut.ChannelManifest)
	}
	for _, m := range toApply {
		raw, err := yaml.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal manifest: %w", err)
		}
		docs, err := manifests.Split(raw)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if err := ap.ApplyObject(ctx, d, FieldManager); err != nil {
				return fmt.Errorf("apply %s/%s: %w", d.GetKind(), d.GetName(), err)
			}
		}
	}

	if p := wizOut.CapabilityPatch; p != nil {
		if err := ap.ApplyObject(ctx, p, CapabilityFieldManager); err != nil {
			return fmt.Errorf("apply capabilities to AgentClass %q in namespace %q: %w", p.GetName(), p.GetNamespace(), err)
		}
	}
	return nil
}
