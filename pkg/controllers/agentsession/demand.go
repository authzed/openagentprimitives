package agentsession

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	harnessregistry "github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/resolve"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
)

// demandPlaceholderImage stands in for an image a pod's demand cannot depend
// on. The SpiceboxSession reconciler fills an unpinned class image from the
// operator's configured sandbox image, which this package-level function does
// not have; requests come from the class's resources alone, so any image
// prices the same.
const demandPlaceholderImage = "demand-estimate"

// DemandFor returns the resource requests of every pod sess will create, so
// the whole set can be admitted before any of it exists. It reads only; it
// creates nothing.
//
// Each pod is rendered by the same builder the reconciler uses to create it,
// then priced by schedfit.EffectiveRequests:
//
//   - the runner (BuildRunnerPod, as PodRunnerFactory calls it), with the
//     in-pod sidecars it carries;
//   - one sandbox per ac.Spec.ToolBundles entry (BuildBundleSession +
//     podspec.Build, against the bundle's SpiceboxClass and its toolchains
//     resolved as the SpiceboxSession reconciler resolves them at bind);
//   - one pod per content-guard detector (cosidecar.BuildPod);
//   - one pod per separate-pod SidecarToolbox (BuildSidecarPod).
//
// Inputs that only exist once provisioning runs — the workspace claim, the
// secret-output token hash, a Workshop identity, the operator's URLs and pull
// secret — are left empty: none of them reaches a container's resources.
//
// A referenced object that is missing (a SpiceboxClass, a SidecarToolbox, a
// toolchain) is an error naming it, never a zero demand, and so is a session
// whose settings are not yet resolved.
func DemandFor(ctx context.Context, c client.Reader, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass) (admission.Demand, error) {
	eff := sess.Status.EffectiveSettings
	if eff == nil {
		return admission.Demand{}, fmt.Errorf("session %s/%s has no effectiveSettings yet: its detectors and sandbox backends derive from them",
			sess.Namespace, sess.Name)
	}

	var pods []*corev1.Pod

	sidecars, err := demandSidecars(ctx, c, sess, ac)
	if err != nil {
		return admission.Demand{}, err
	}
	dets, err := resolveContentGuardDetectors(eff)
	if err != nil {
		return admission.Demand{}, err
	}
	sidecarRefs := make([]string, 0, len(sidecars))
	for _, rt := range sidecars {
		sidecarRefs = append(sidecarRefs, rt.Ref)
	}
	placeContentGuardDetectors(sess, dets, sidecarRefs)

	h, err := harnessregistry.Resolve(ac.Spec.Harness)
	if err != nil {
		return admission.Demand{}, fmt.Errorf("resolve harness for %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	runner, err := BuildRunnerPod(PodSpecOpts{
		Session:                       sess,
		Class:                         ac,
		Image:                         demandPlaceholderImage,
		ServiceAcct:                   sess.Name + "-runner-sa",
		MemoryToken:                   MemoryTokenSecretName(sess),
		ResolvedSidecars:              sidecars,
		SidecarSecretName:             func(ref string) string { return cosidecar.CredentialSecretName(sess.Name, ref) },
		ResolvedContentGuardDetectors: dets,
		Harness:                       h,
	})
	if err != nil {
		return admission.Demand{}, fmt.Errorf("build runner pod: %w", err)
	}
	pods = append(pods, runner)

	for _, b := range ac.Spec.ToolBundles {
		pod, err := demandBundlePod(ctx, c, sess, ac, b)
		if err != nil {
			return admission.Demand{}, err
		}
		pods = append(pods, pod)
	}

	for _, det := range dets {
		pod, _, err := cosidecar.BuildPod(sess, detectorPodSpec(sess, det, ""))
		if err != nil {
			return admission.Demand{}, fmt.Errorf("build detector pod %q: %w", det.Inspector, err)
		}
		pods = append(pods, pod)
	}

	soSecretName := secretoutsrv.SecretOutputSecretName(sess.Name)
	for _, rt := range sidecars {
		if rt.RunMode != RunModeSeparatePod {
			continue
		}
		pod, err := BuildSidecarPod(sess, rt, soSecretName, cosidecar.CredentialSecretName(sess.Name, rt.Ref), nil, "", "", "")
		if err != nil {
			return admission.Demand{}, fmt.Errorf("build sidecar pod %q: %w", rt.Ref, err)
		}
		pods = append(pods, pod)
	}

	d := admission.Demand{Pods: make([]admission.PodDemand, 0, len(pods))}
	for _, p := range pods {
		cpu, mem, eph := schedfit.EffectiveRequests(&p.Spec)
		d.Pods = append(d.Pods, admission.PodDemand{Name: p.Name, CPUMilli: cpu, MemBytes: mem, EphemeralBytes: eph})
	}
	return d, nil
}

// demandBundlePod renders the sandbox pod one tool bundle will run in. It
// takes the reconciler's own desiredBundleSession (identity and tier-resolved
// backend) and mirrors the SpiceboxSession reconciler's bind (class
// snapshot, image default, toolchain resolution), then calls podspec.Build —
// the builder both registered sandbox backends render through.
func demandBundlePod(ctx context.Context, c client.Reader, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass, b spiceboxv1alpha1.ToolBundle) (*corev1.Pod, error) {
	// No claim name: whether the workspace is shared depends on the operator's
	// configured StorageClass, and only changes the pod's volumes.
	bs := desiredBundleSession(sess, ac, b, "")

	var cls spiceboxv1alpha1.SpiceboxClass
	if err := c.Get(ctx, types.NamespacedName{Name: b.Class}, &cls); err != nil {
		return nil, fmt.Errorf("get SpiceboxClass %q for bundle %q: %w", b.Class, b.Name, err)
	}
	classSpec := *cls.Spec.DeepCopy()
	if classSpec.Image == "" {
		classSpec.Image = demandPlaceholderImage
	}
	mounts, _, err := resolve.Resolve(ctx, c, classSpec.Toolchains)
	if err != nil {
		return nil, fmt.Errorf("resolve toolchains of SpiceboxClass %q for bundle %q: %w", b.Class, b.Name, err)
	}
	bs.Status.ResolvedToolchains = mounts

	pod, err := podspec.Build(bs, classSpec)
	if err != nil {
		return nil, fmt.Errorf("build sandbox pod for bundle %q (SpiceboxClass %q): %w", b.Name, b.Class, err)
	}
	return pod, nil
}

// demandSidecars resolves ac's SidecarToolbox refs to the fields a pod build
// reads: spec, run mode, and port. The reconciler's by-digest image rewrite
// and network merge are skipped — neither reaches a container's resources.
func demandSidecars(ctx context.Context, c client.Reader, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass) ([]spiceboxv1alpha1.ResolvedSidecarToolbox, error) {
	if len(ac.Spec.SidecarToolboxes) == 0 {
		return nil, nil
	}
	out := make([]spiceboxv1alpha1.ResolvedSidecarToolbox, 0, len(ac.Spec.SidecarToolboxes))
	refs := make([]string, 0, len(ac.Spec.SidecarToolboxes))
	for _, ref := range ac.Spec.SidecarToolboxes {
		var tb spiceboxv1alpha1.SidecarToolbox
		if err := c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Ref}, &tb); err != nil {
			return nil, fmt.Errorf("get SidecarToolbox %q in namespace %q: %w", ref.Ref, sess.Namespace, err)
		}
		spec := *tb.Spec.DeepCopy()
		out = append(out, spiceboxv1alpha1.ResolvedSidecarToolbox{
			Name:    ref.Name,
			Ref:     ref.Ref,
			Spec:    spec,
			RunMode: RunModeFor(spec),
		})
		refs = append(refs, ref.Ref)
	}
	ports := AllocatePorts(refs)
	for i := range out {
		out[i].Port = ports[out[i].Ref]
	}
	return out, nil
}
