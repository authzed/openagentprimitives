package agentsession

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	harnessregistry "github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

// PodRunnerFactory satisfies RunnerFactory by creating a Pod per
// session. Production wiring; internal/cmd/operator/main.go injects an
// instance into AgentSession.Reconciler.RunnerFactory.
type PodRunnerFactory struct {
	Client      client.Client
	RunnerImage string
	OperatorURL string
	NATSURL     string
	// SpiceDBEndpoint / SpiceDBInsecure are value-copied onto each runner
	// pod's env. The preshared token is NOT here — it rides in the
	// per-session Secret written by the AgentSession reconciler and is
	// mounted as a file (see PodSpecOpts / BuildRunnerRBAC).
	SpiceDBEndpoint string
	SpiceDBInsecure bool
	// ImagePullSecret is an optional pull Secret name added to each runner
	// pod's imagePullSecrets. Empty = unchanged behavior (no imagePullSecrets).
	ImagePullSecret string
	// HarnessImages maps a harness name to its container image. A name absent
	// from the map falls back to RunnerImage, which is the oap-native case.
	HarnessImages map[string]string

	// SecretReader is the guarded Secret reader. When set, the factory reads
	// the per-session memory-token Secret via the label-filtered cache rather
	// than the unguarded client. Nil falls back to the unguarded client
	// (tests that do not inject adoptguard).
	SecretReader *adoptguard.SecretReader

	// WebdBaseURL returns webd's externally reachable base URL, read fresh per
	// pod so a session started after the address landed gets it without an
	// operator restart. A getter rather than a string because the value
	// genuinely arrives late: `oap install` seeds the ConfigMap empty while it
	// arranges external access, and a tunnel can rotate it later.
	//
	// Nil when the operator could not build a provider (see internal/cmd/operator);
	// runners then spawn with no webd address, which costs a details link on a
	// trigger's status surface and nothing else.
	WebdBaseURL func() string
}

// webdBaseURL reads the getter if one is wired, so the nil case is answered in
// exactly one place rather than at the call site.
func (f *PodRunnerFactory) webdBaseURL() string {
	if f.WebdBaseURL == nil {
		return ""
	}
	return f.WebdBaseURL()
}

// MemoryTokenSecretName returns the per-session Secret name holding
// the memory bearer token. Exposed because the AgentSession
// controller writes the Secret BEFORE calling Start (the Secret
// is mounted into the pod, so it must exist at Pod-create time).
// Both implementations of RunnerFactory may rely on this naming
// convention. The suffix (spiceboxv1alpha1.MemoryTokenSecretSuffix)
// matches BuildRunnerRBAC's secret name and is also the suffix
// ToolCall.ValidateCredentialSourceOwnership binds to the owning
// session; changing it would break both the volume mount and that
// ownership check.
func MemoryTokenSecretName(sess *spiceboxv1alpha1.AgentSession) string {
	return sess.Name + spiceboxv1alpha1.MemoryTokenSecretSuffix
}

// getSecret returns the Secret identified by nn via the guarded reader when
// SecretReader is set, else falls back to the unguarded client. This is the
// factory's equivalent of Reconciler.getSecret.
func (f *PodRunnerFactory) getSecret(ctx context.Context, nn types.NamespacedName, out *corev1.Secret) error {
	if f.SecretReader != nil {
		s, err := f.SecretReader.Get(ctx, nn)
		if err != nil {
			return err
		}
		*out = *s
		return nil
	}
	return f.Client.Get(ctx, nn, out)
}

// Start creates the runner Pod if it doesn't exist. Idempotent via
// AlreadyExists tolerance.
func (f *PodRunnerFactory) Start(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass, opts StartOpts) error {
	// Ground truth for the NATS creds mounts: read the per-session
	// Secret the controller created (in step 3 of Reconcile, before
	// it calls Start) and check whether the nats.creds key is present.
	// The pod must declare the nats-creds/nats-ca SubPath mounts iff
	// the Secret carries those keys — a SubPath mount of an absent key
	// hangs the pod in ContainerCreating. Deriving this from "the
	// session is channel-attached" would be wrong on the back-fill
	// edge (Secret minted while the operator lacked a NATS identity).
	var sec corev1.Secret
	if err := f.getSecret(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: MemoryTokenSecretName(sess),
	}, &sec); err != nil {
		return fmt.Errorf("get per-session secret %s for runner pod build: %w",
			MemoryTokenSecretName(sess), err)
	}
	natsCredsMounted := len(sec.Data["nats.creds"]) > 0

	// EffectiveSettings must be stamped by the reconciler before pod creation.
	// A nil here means a logic error (e.g. the reconciler short-circuited without
	// refusing the session); surface it as an error rather than nil-derefing
	// inside BuildRunnerPod.
	if sess.Status.EffectiveSettings == nil {
		return fmt.Errorf("build runner pod: session %s/%s has no effectiveSettings stamped — reconciler must resolve settings before starting the runner", sess.Namespace, sess.Name)
	}

	h, herr := harnessregistry.Resolve(class.Spec.Harness)
	if herr != nil {
		return fmt.Errorf("resolve harness for %s/%s: %w", sess.Namespace, sess.Name, herr)
	}

	// A non-default harness with no configured image would otherwise launch
	// the oap-native runner image. Fail closed: a harness that cannot be given
	// its own image is a misconfiguration, not a fallback.
	if h.Name() != harness.DefaultName && f.HarnessImages[h.Name()] == "" {
		return fmt.Errorf("harness %q has no configured image for session %s/%s",
			h.Name(), sess.Namespace, sess.Name)
	}

	pod, err := BuildRunnerPod(PodSpecOpts{
		Session:                       sess,
		Class:                         class,
		Image:                         f.RunnerImage,
		ServiceAcct:                   sess.Name + "-runner-sa",
		MemoryToken:                   MemoryTokenSecretName(sess),
		OperatorURL:                   f.OperatorURL,
		NATSURL:                       f.NATSURL,
		WebdBaseURL:                   f.webdBaseURL(),
		NATSCredsMounted:              natsCredsMounted,
		SpiceDBEndpoint:               f.SpiceDBEndpoint,
		SpiceDBInsecure:               f.SpiceDBInsecure,
		ResolvedSidecars:              opts.ResolvedSidecars,
		SidecarSecretName:             opts.SidecarSecretName,
		ResolvedContentGuardDetectors: opts.ResolvedContentGuardDetectors,
		ImagePullSecret:               f.ImagePullSecret,
		Harness:                       h,
		HarnessImage:                  f.HarnessImages[h.Name()],
	})
	if err != nil {
		return fmt.Errorf("build runner pod: %w", err)
	}
	if err := f.Client.Create(ctx, pod); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create runner pod: %w", err)
	}
	return nil
}

// Stop deletes the runner Pod. Best-effort; returns nil on NotFound.
func (f *PodRunnerFactory) Stop(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	var pod corev1.Pod
	if err := f.Client.Get(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: RunnerPodName(sess),
	}, &pod); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get runner pod for stop: %w", err)
	}
	if err := f.Client.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete runner pod: %w", err)
	}
	return nil
}

// ObservedName returns the runner Pod's name. The reconciler records it on
// AgentSession.status.runnerPodName once Start has been called.
func (f *PodRunnerFactory) ObservedName(sess *spiceboxv1alpha1.AgentSession) string {
	return RunnerPodName(sess)
}
