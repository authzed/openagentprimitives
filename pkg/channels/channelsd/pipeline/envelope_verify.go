package pipeline

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// envelopeFreshnessWindow bounds both clock skew and the replay exposure
// after a channelsd restart (in-memory high-water marks are lost; anything
// older than the window is refused regardless).
const envelopeFreshnessWindow = 5 * time.Minute

type hwmKey struct{ publisher, sessionUID, epoch, subject string }

type hwmEntry struct {
	seq  uint64
	seen time.Time
}

// envelopeVerifier enforces the inter-agent envelope contract: a valid
// session signature over the subject-bound digest, session-window binding,
// freshness, and per-(publisher, sessionUID, epoch, subject) monotonic seq.
// The subject is part of the HWM key because NATS orders per-publisher
// within a subject only; one signer's counter observed across two subjects
// can legally interleave out of order.
//
// SINGLE-REPLICA ASSUMPTION: hwm is in-process memory, which is sound only
// because channelsd runs as a single replica (Deployment replicas:1,
// strategy Recreate). Scaling channelsd out would split each subject's
// high-water mark across replicas depending on which one a given message
// happened to land on, and the anti-replay guarantee — and the monotonic
// check itself — is per-PROCESS, not per-cluster: two replicas could each
// accept the same or an out-of-order envelope and double-deliver.
type envelopeVerifier struct {
	k8s client.Client
	now func() time.Time

	mu  sync.Mutex
	hwm map[hwmKey]hwmEntry
}

func newEnvelopeVerifier(k8s client.Client) *envelopeVerifier {
	return &envelopeVerifier{k8s: k8s, now: time.Now, hwm: map[hwmKey]hwmEntry{}}
}

// verifyAgentMessage enforces the agent_message_send envelope contract
// against the sending session's own K8s-witnessed audit key — read fresh on
// every call, never cached, so a session recreated under the same name never
// gets verified against a stale predecessor's key.
func (v *envelopeVerifier) verifyAgentMessage(ctx context.Context, env channelevents.Envelope) error {
	// The subject is derived, not taken from the wire: by the time a pipeline
	// handler runs, envelopeHandler (or the harness stand-in) has already
	// authorized env.Session against the real NATS subject, so this
	// reconstruction names the only subject the envelope can have arrived on.
	subject := channelevents.SubjectIn(
		channelevents.SubjectPrefix(env.Session.Namespace, env.Session.Name), env.Kind)

	wantPublisher := "session:" + env.Session.Namespace + "/" + env.Session.Name
	if env.Publisher == "" && env.Sig == "" {
		return fmt.Errorf("agent message from %s: %w", wantPublisher, channelevents.ErrEnvelopeUnsigned)
	}
	if env.Publisher != wantPublisher {
		return fmt.Errorf("agent message publisher %q is not the sending session %q", env.Publisher, wantPublisher)
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := v.k8s.Get(ctx, client.ObjectKey{Namespace: env.Session.Namespace, Name: env.Session.Name}, &sess); err != nil {
		return fmt.Errorf("agent message from %s: load sending session: %w", wantPublisher, err)
	}
	if sess.Status.AuditPublicKey == "" || sess.Status.AuditKeyID == "" {
		return fmt.Errorf("agent message from %s: session has no anchored audit key", wantPublisher)
	}
	if env.SigKeyID != sess.Status.AuditKeyID {
		return fmt.Errorf("agent message from %s: sigKeyId %q does not match anchored keyID %q", wantPublisher, env.SigKeyID, sess.Status.AuditKeyID)
	}
	pub, err := base64.StdEncoding.DecodeString(sess.Status.AuditPublicKey)
	if err != nil {
		return fmt.Errorf("agent message from %s: decode anchored pubkey: %w", wantPublisher, err)
	}
	if err := channelevents.VerifyEnvelopeSig(subject, env, ed25519.PublicKey(pub)); err != nil {
		return fmt.Errorf("agent message from %s: %w", wantPublisher, err)
	}

	// Session-window binding: a replay from a prior incarnation of this
	// session name dies here no matter what the HWM cache remembers.
	if env.SessionUID != string(sess.UID) {
		return fmt.Errorf("agent message from %s: sessionUID %q is not the live session's UID %q", wantPublisher, env.SessionUID, sess.UID)
	}

	now := v.now()
	if d := now.Sub(env.PublishedAt); d > envelopeFreshnessWindow || d < -envelopeFreshnessWindow {
		return fmt.Errorf("agent message from %s: publishedAt outside freshness window (delta %s)", wantPublisher, d.Round(time.Second))
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	// GC: an entry is retired only once NO fresh envelope could still need
	// it, and that takes TWO windows from the accept, not one. The freshness
	// check above admits a PublishedAt up to one window in the FUTURE of
	// this process's clock (sender clock skew is exactly what the ± window
	// tolerates), and such an envelope stays fresh until PublishedAt +
	// window — up to 2*window after `seen`. Sweeping at one window opened a
	// replay seam over that gap: entry gone, envelope still fresh. It bit
	// the LAST message a publisher sent on a subject, the one nothing after
	// it would ever bump the high-water mark for.
	for k, e := range v.hwm {
		if now.Sub(e.seen) > 2*envelopeFreshnessWindow {
			delete(v.hwm, k)
		}
	}
	k := hwmKey{publisher: env.Publisher, sessionUID: env.SessionUID, epoch: env.SigEpoch, subject: subject}
	if e, ok := v.hwm[k]; ok && env.SigSeq <= e.seq {
		return fmt.Errorf("agent message from %s: replay refused (sigSeq %d <= high-water %d)", wantPublisher, env.SigSeq, e.seq)
	}
	v.hwm[k] = hwmEntry{seq: env.SigSeq, seen: now}
	return nil
}
