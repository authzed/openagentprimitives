//go:build e2e

// Package e2e — interrupt API.
//
// The harness's interrupt API mirrors the user-facing mid-turn
// queue+confirm round-trip:
//
//   - ExpectEnqueueAck blocks until the fake queued_messages sub-channel
//     sender records a KindEnqueueAck envelope matching all predicates.
//     The envelope arrives via: a mid-turn SendUserMessage lands on a
//     Running session → channelsd pipeline's HandleInbound mints a
//     RequestID and publishes KindEnqueueAck on OUT → the outbound relay
//     → fake queued_messages sub-channel sender → Driver.enqueueAcks (see
//     pkg/channels/channelsd/pipeline/pipeline.go's "Mid-turn enqueue ack" block).
//
//   - Interrupt.Click publishes a KindInterruptRequest envelope on the
//     session's IN subject, carrying the RequestID captured off the
//     matched ack. The e2e harness's InProcessRunnerFactory subscribes to
//     that subject per session (subscribeFactoryInterruptRequest, mirroring
//     internal/cmd/runner/nats.go's subscribeInterruptRequest) and calls
//     loop.Interrupt, then publishes KindInterruptApplied on OUT.
//
//   - Interrupt.WaitInterruptApplied blocks until the fake queued_messages
//     sub-channel sender records a KindInterruptApplied envelope for the
//     same RequestID, or DefaultTimeout elapses.
//
// No real names — tests use "user@example.com" per AGENTS.md /
// memory/feedback_no_real_names_in_code.md.
package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// EnqueueAck is the harness's projection of one captured KindEnqueueAck
// envelope. RequestID is the opaque ID channelsd minted; the same ID
// correlates the interrupt button rendered alongside the ack, the
// KindInterruptRequest a click publishes, and the eventual
// KindInterruptApplied outcome.
type EnqueueAck struct {
	RequestID  string
	Requester  string
	Caption    string
	SessionRef string
}

// EnqueueAckPredicate filters EnqueueAcks. ExpectEnqueueAck accepts a
// variadic of these and requires every predicate to match.
type EnqueueAckPredicate func(EnqueueAck) bool

// ForEnqueueRequester matches when the ack's requester email equals
// email. The fake channel kind uses email-as-external-id (see
// SendUserMessage's InboundEvent construction), so this matches the
// AsUser email passed to the mid-turn SendUserMessage.
func ForEnqueueRequester(email string) EnqueueAckPredicate {
	return func(a EnqueueAck) bool { return a.Requester == email }
}

// ExpectEnqueueAck blocks until the fake queued_messages sub-channel
// sender records a KindEnqueueAck envelope matching every predicate, or
// DefaultTimeout elapses. On timeout, fatals with the count of acks seen
// — a non-zero count usually means "an ack rendered, but the predicate
// didn't match"; zero means "channelsd never published an enqueue ack"
// (most commonly: the second message didn't land while the session was
// genuinely Running, so the pipeline took the ordinary wakeup path
// instead of the mid-turn-ack path — see pipeline.go's
// AgentSessionPhaseRunning gate).
func (h *Harness) ExpectEnqueueAck(preds ...EnqueueAckPredicate) *Interrupt {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectEnqueueAck")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			acks := drv.EnqueueAcks()
			for i := seen; i < len(acks); i++ {
				a := enqueueAckFromRecord(acks[i])
				if matchesAllEnqueueAck(a, preds) {
					return &Interrupt{h: h, ack: a}
				}
			}
			seen = len(acks)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectEnqueueAck: timed out after %s (seen %d ack(s))\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return nil
}

// Interrupt is the handle returned by ExpectEnqueueAck. Tests call Click
// to publish the interrupt request correlated with the matched ack, then
// WaitInterruptApplied to observe the runner's outcome.
type Interrupt struct {
	h   *Harness
	ack EnqueueAck
}

// Ack returns a copy of the captured KindEnqueueAck projection. Useful
// for tests that want to assert on Caption after matching.
func (i *Interrupt) Ack() EnqueueAck { return i.ack }

// Click publishes a KindInterruptRequest envelope on the session's IN
// subject, carrying the RequestID captured off the matched enqueue ack.
// The harness's InProcessRunnerFactory subscribes per session
// (subscribeFactoryInterruptRequest) and calls loop.Interrupt, publishing
// the outcome as KindInterruptApplied on OUT — drained by
// WaitInterruptApplied.
func (i *Interrupt) Click(opts ...SendOption) {
	i.h.t.Helper()
	cfg := sendCfg{user: i.h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	payload := channelevents.InterruptRequestPayload{
		RequestID:  i.ack.RequestID,
		SessionRef: i.ack.SessionRef,
		Requester: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	ns, name, ok := splitSessionRef(i.ack.SessionRef)
	if !ok {
		i.h.t.Fatalf("Interrupt.Click: malformed SessionRef %q", i.ack.SessionRef)
	}
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return i.h.nc.Publish(subj, b) },
		ns, name, channelevents.KindInterruptRequest, payload,
	); err != nil {
		i.h.t.Fatalf("Interrupt.Click: publish interrupt_request: %v", err)
	}
	if err := i.h.nc.Flush(); err != nil {
		// Flush failure is rare; log so a downstream timeout has context
		// but don't fatal — the subscriber may still process the
		// in-flight publish.
		i.h.t.Logf("Interrupt.Click: nats Flush: %v", err)
	}
}

// WaitInterruptApplied blocks until the fake queued_messages sub-channel
// sender records a KindInterruptApplied envelope for this interrupt's
// RequestID, or ctx / DefaultTimeout elapses (whichever is sooner), and
// returns the matched payload. A caller-supplied ctx deadline shorter
// than DefaultTimeout is honored — mirrors Approval.WaitApplied's ctx
// handling for the "no applied envelope expected" negative-check shape.
func (i *Interrupt) WaitInterruptApplied(ctx context.Context) (channelevents.InterruptAppliedPayload, error) {
	i.h.t.Helper()
	ch := i.h.singleChannel("WaitInterruptApplied")
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	if drv == nil {
		return channelevents.InterruptAppliedPayload{}, fmt.Errorf("WaitInterruptApplied: no driver for channel %s/%s", ch.Namespace, ch.Name)
	}

	deadline := time.Now().Add(i.h.opts.DefaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	seen := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return channelevents.InterruptAppliedPayload{}, ctx.Err()
		default:
		}
		applieds := drv.InterruptApplieds()
		for j := seen; j < len(applieds); j++ {
			if applieds[j].Payload.RequestID == i.ack.RequestID {
				return applieds[j].Payload, nil
			}
		}
		seen = len(applieds)
		time.Sleep(100 * time.Millisecond)
	}
	return channelevents.InterruptAppliedPayload{}, fmt.Errorf(
		"WaitInterruptApplied: timed out after %s waiting for requestID=%s (seen %d applied envelope(s))",
		i.h.opts.DefaultTimeout, i.ack.RequestID, seen)
}

func matchesAllEnqueueAck(a EnqueueAck, preds []EnqueueAckPredicate) bool {
	for _, pred := range preds {
		if !pred(a) {
			return false
		}
	}
	return true
}

func enqueueAckFromRecord(r fakekind.EnqueueAck) EnqueueAck {
	return EnqueueAck{
		RequestID:  r.Payload.RequestID,
		Requester:  identityHandle(r.Payload.Requester),
		Caption:    r.Payload.Caption,
		SessionRef: r.SessionRef.Namespace + "/" + r.SessionRef.Name,
	}
}
