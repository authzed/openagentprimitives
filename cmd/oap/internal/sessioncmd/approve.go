// `oap session approve` — force-fire an approval (or denial) decision
// for a pending approval request, simulating what would happen if the
// approver clicked Approve / Deny in the channel. Useful for end-to-end
// testing the approval flows without a Slack workspace.
//
// It drives ANY pending approval on the AgentSession — tool-call,
// info-leakage, and content-inspection — by resolving the request id across
// the generic PendingInteractions list (the single home for all three approval
// families since they were flipped onto the unified Interaction model), then
// publishing the generic interaction_decision envelope on the inbound NATS
// subject for the session and optionally surfacing the channelsd-side
// Applied outcome.
//
// Wire path mirrors the generic interaction click path
// (handleInteractionDecisionClick): build an InteractionDecisionPayload tagged
// with the family's Category and the approver's canonical identity (Kind="cli"),
// publish it to NATS on the inbound subject for the session, then optionally
// subscribe to the outbound Applied subject and surface the channelsd-side
// outcome (approved / denied / not_authorized / etc.) to the operator.
//
// Authorization still runs server-side in channelsd's pipeline — the
// CLI does not bypass any check. If --approver-email isn't in the
// approver subject set in SpiceDB, the Applied envelope reports
// "not_authorized" exactly as a channel click would.
package sessioncmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clinats"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

const (
	natsServiceNamespace = "agentprimitives-system"
	natsServiceName      = "spicebox-nats"
	natsServiceSelector  = "app.kubernetes.io/name=spicebox-nats"
	natsServicePort      = uint16(4222)
)

// approvalKind names one of the three approval families this command can
// drive. The string values match the user-facing kind labels printed in
// the auto-pick / disambiguation output.
type approvalKind string

const (
	approvalKindTool              approvalKind = "tool_call"
	approvalKindLeakage           approvalKind = "leakage_share"
	approvalKindContentInspection approvalKind = "content_inspection"
)

// pendingEntry is the unified view over the generic PendingInteractions list
// on AgentSession.status. It carries just what the resolution + publish
// need: the request id, the required approver subject set (for the
// server-agreeing pre-check), and which approval family (Kind) it belongs to
// (so publish stamps the right interaction Category).
type pendingEntry struct {
	RequestID       string
	ApproverSubject string
	// ApproverSubjects, when non-empty, carries EVERY subject-set the
	// approval fanned out to; membership in ANY set authorizes. Empty ⇒
	// fall back to the singular ApproverSubject. The generic
	// PendingInteractions list carries only the singular ApproverSubject
	// today, so this stays empty — kept for the union-fanout pre-check
	// shape approverAuthorized still supports.
	ApproverSubjects []string
	Kind             approvalKind
}

// appliedOutcome is the normalized Applied-envelope result rendered to
// the operator under --wait, regardless of which kind's payload shape it
// was decoded from. tool_call carries {Decision, Approver}; leakage and
// content_inspection carry {Approved bool, ApproverID}.
type appliedOutcome struct {
	// Decision is "approve" | "deny" (normalized from Approved bool for
	// the leakage / content_inspection kinds).
	Decision string
	// Reason is "" | "timeout" | "not_authorized" (and possibly an
	// approver note on deny).
	Reason string
	// ApproverDisplay is a human-readable approver string for the output
	// line ("cli:alice" or a bare approver id).
	ApproverDisplay string
}

// kindDispatch is the per-kind table that keeps the decision-build and
// the Applied-decode in ONE place each, rather than a switch repeated at
// every call site. The pending-resolution switch (collectPendings) and
// this dispatch are the only two places that enumerate the kinds.
type kindDispatch struct {
	// decisionKind is the channelevents.Kind published inbound.
	decisionKind channelevents.Kind
	// appliedKind is the channelevents.Kind the channelsd outcome is
	// published outbound on (used to build the --wait subscription).
	appliedKind channelevents.Kind
	// buildDecisionPayload constructs the kind's *DecisionPayload from the
	// resolved request id, approver email, and decision string. All three
	// payload types share the shape {RequestID, Approver, Decision}; this
	// returns the concrete typed value for marshaling.
	buildDecisionPayload func(requestID, approverEmail, decision string) any
	// decodeApplied unmarshals the kind's Applied payload from the
	// envelope body and normalizes it to an appliedOutcome. It returns
	// (_, false) when the requestID does not match (so the subscriber can
	// ignore unrelated Applied envelopes).
	decodeApplied func(body []byte, requestID string) (appliedOutcome, bool)
}

// cliApprover builds the canonical "cli" approver identity carried on
// every decision payload. identity.EmailReference keys off Email when
// non-empty (the same canonical the SpiceDB write uses), so channelsd's
// LookupSubjectIncludes resolves correctly when approverEmail is in the
// approver subject set.
func cliApprover(approverEmail string) channelevents.ExternalIdentity {
	return channelevents.ExternalIdentity{
		Kind:  "cli",
		Email: identity.Email(approverEmail),
	}
}

// dispatchFor returns the kind-dispatch table for a pending entry's kind.
// Since Slice C2 all three approval families flow through the SAME generic
// interaction envelopes (interaction_decision / interaction_applied) — the
// typed Kind*ToolApproval* / Kind*InfoLeakageApproval* decision pairs are no
// longer produced (the runner parks every family as a generic interaction_request
// — see pkg/agent/runner/host_approval.go). Each kind therefore differs ONLY in
// the interaction Category it carries, so all three delegate to
// genericInteractionDispatch. Unknown kinds return ok=false so the caller fails
// loudly rather than publishing a no-op.
func dispatchFor(k approvalKind) (kindDispatch, bool) {
	switch k {
	case approvalKindTool:
		return genericInteractionDispatch(string(categories.ToolApproval)), true
	case approvalKindLeakage:
		return genericInteractionDispatch(string(categories.InfoLeakage)), true
	case approvalKindContentInspection:
		return genericInteractionDispatch(string(categories.ContentInspection)), true
	default:
		return kindDispatch{}, false
	}
}

// genericInteractionDispatch builds the decision-publish + applied-decode pair
// for a generic-interaction approval category. The decision leg publishes a
// KindInteractionDecision(category) and the wait leg decodes the matching
// KindInteractionApplied — identical wire shape for tool_approval, info_leakage,
// and content_inspection, differing only in the Category stamped on the payload.
func genericInteractionDispatch(category string) kindDispatch {
	return kindDispatch{
		decisionKind: channelevents.KindInteractionDecision,
		appliedKind:  channelevents.KindInteractionApplied,
		buildDecisionPayload: func(requestID, approverEmail, decision string) any {
			// InteractionDecisionPayload.Validate requires a non-empty
			// Decider.ExternalID; the cli approver keys on Email for
			// canonicalization, so mirror the email into ExternalID (the
			// server canonicalizes on Email regardless).
			dec := cliApprover(approverEmail)
			dec.ExternalID = identity.RawExternalID(approverEmail)
			return channelevents.InteractionDecisionPayload{
				Category:   category,
				RequestRef: requestID,
				ActionID:   decision, // "approve" | "deny" — the interaction action ids
				Decider:    dec,
			}
		},
		decodeApplied: func(body []byte, requestID string) (appliedOutcome, bool) {
			var pl channelevents.InteractionAppliedPayload
			if err := json.Unmarshal(body, &pl); err != nil || pl.RequestRef != requestID {
				return appliedOutcome{}, false
			}
			display := ""
			if pl.DecidedBy != nil {
				display = pl.DecidedBy.Kind.String() + ":" + pl.DecidedBy.ExternalID.String()
			}
			return appliedOutcome{
				Decision:        boolDecision(pl.Outcome == channelevents.OutcomeApproved),
				Reason:          pl.Reason,
				ApproverDisplay: display,
			}, true
		},
	}
}

// boolDecision maps the leakage / content_inspection Applied payloads'
// Approved bool onto the "approve" | "deny" string the tool_call kind
// uses, so the --wait output line is uniform across kinds.
func boolDecision(approved bool) string {
	if approved {
		return "approve"
	}
	return "deny"
}

// collectPendings flattens the AgentSession status's generic PendingInteractions
// list into one ordered view, mapping each entry's Category onto its CLI
// approvalKind. Since Slice C2 all three approval families — tool_approval,
// info_leakage, and content_inspection — park on the single PendingInteractions
// list (the typed pendingToolGrants/pendingLeakageApprovals lists were deleted),
// so this is the single place that enumerates them. Entries whose category has
// no CLI approvalKind (identity_choice, credential_link, …) are skipped — this
// command only drives the three approve/deny families.
func collectPendings(sess *spiceboxv1alpha1.AgentSession) []pendingEntry {
	out := make([]pendingEntry, 0, len(sess.Status.PendingInteractions))
	for _, e := range sess.Status.PendingInteractions {
		k, ok := approvalKindForCategory(e.Category)
		if !ok {
			continue
		}
		out = append(out, pendingEntry{RequestID: e.RequestID, ApproverSubject: e.ApproverSubject, Kind: k})
	}
	return out
}

// approvalKindForCategory maps a generic interaction Category onto the CLI's
// approvalKind, or ok=false for categories this command does not drive.
func approvalKindForCategory(category string) (approvalKind, bool) {
	switch category {
	case string(categories.ToolApproval):
		return approvalKindTool, true
	case string(categories.InfoLeakage):
		return approvalKindLeakage, true
	case string(categories.ContentInspection):
		return approvalKindContentInspection, true
	default:
		return "", false
	}
}

// pendingListing renders the unified pending view as "id (kind), id
// (kind), …" for the auto-pick disambiguation + no-match errors.
func pendingListing(pendings []pendingEntry) string {
	parts := make([]string, 0, len(pendings))
	for _, p := range pendings {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.RequestID, p.Kind))
	}
	return strings.Join(parts, ", ")
}

// resolvePending matches requestID against the unified pending view. With
// requestID empty it auto-picks the only pending entry (erroring on
// none/multiple); with requestID given it matches the union (erroring with
// the full listing on no match). The returned entry carries the kind +
// approver subject the rest of the command needs.
func resolvePending(pendings []pendingEntry, requestID, ns, sessionName string) (pendingEntry, error) {
	if requestID == "" {
		switch len(pendings) {
		case 0:
			return pendingEntry{}, fmt.Errorf("no pending approvals on AgentSession %s/%s — nothing to approve", ns, sessionName)
		case 1:
			return pendings[0], nil
		default:
			return pendingEntry{}, fmt.Errorf("multiple pending approvals — pass <requestID> explicitly. pending: %s",
				pendingListing(pendings))
		}
	}
	for _, p := range pendings {
		if p.RequestID == requestID {
			return p, nil
		}
	}
	return pendingEntry{}, fmt.Errorf("requestID %q is not pending for %s/%s. pending: [%s]",
		requestID, ns, sessionName, pendingListing(pendings))
}

func newSessionApproveCmd(g *apcmd.Globals) *cobra.Command {
	var approverEmail string
	var deny bool
	var requestID string
	var wait bool
	var force bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "approve <session> [<requestID>]",
		Short: "Force-fire an approval (or --deny) on any pending approval, simulating a channel click",
		Long: `Publish an approval decision envelope to NATS as if the named
approver had clicked Approve (or --deny) in the channel. Drives any
pending approval on the session — tool-call, info-leakage, or
content-inspection. Useful for end-to-end testing without a Slack
workspace.

The decision still flows through channelsd's normal pipeline:
  - approver canonical-id is derived from --approver-email
  - SpiceDB LookupSubjectIncludes validates the approver against
    the request's ApproverSubject set
  - on approve: SpiceDB grant tuple is written; runner re-Checks
    against the grant and proceeds
  - on deny / not_authorized: runner sees the denied decision

requestID may be omitted when the session has exactly one pending
approval (of any kind); the command picks that one and prints its
kind. If multiple approvals are pending, requestID is required (the
show output above lists each one with its kind).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionName := args[0]
			if len(args) == 2 {
				requestID = args[1]
			}
			return runSessionApprove(cmd.Context(), cmd.OutOrStdout(),
				g, sessionName, requestID, identity.Email(approverEmail), !deny, wait, force, timeout)
		},
	}
	cmd.Flags().StringVar(&approverEmail, "approver-email", "",
		"Email of the approver — must match the SpiceDB approver subject. "+
			"When omitted, the CLI's cached/logged-in identity is used. "+
			"Pass an explicit email as an unverified override for scripting (use `oap identity canonical-id <email>` to preview the canonical form).")
	cmd.Flags().BoolVar(&deny, "deny", false,
		"Publish a deny decision instead of approve")
	cmd.Flags().BoolVar(&wait, "wait", true,
		"Subscribe to the outbound Applied envelope and surface the channelsd-side outcome before returning")
	cmd.Flags().BoolVar(&force, "force", false,
		"Publish even if --approver-email is not in the request's required approver set (skips the client-side pre-check; channelsd's server-side check still applies)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second,
		"How long to wait for the Applied envelope when --wait is true")
	return cmd
}

// subjectLookuper enumerates the canonical user IDs contained in a SpiceDB
// subject-set ref (e.g. "crm_company:54835545860#owner", or a direct
// "user:<canonical>"). *spicedb.Client satisfies it via LookupSubjects.
type subjectLookuper interface {
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)
}

// approverAuthorized reports whether canonicalApprover is a member of ANY of
// the approverSubjects subject-sets (union semantics — one approval may fan
// out to several owner-sets and any member of any set may approve), returning
// the full deduped authorized list (for display) regardless of membership. A
// lookup error is propagated so the caller can treat the advisory pre-check
// as unavailable and proceed.
func approverAuthorized(ctx context.Context, sdb subjectLookuper, approverSubjects []string, canonicalApprover identity.CanonicalUserID) (ok bool, authorized []string, err error) {
	seen := make(map[string]struct{})
	for _, subject := range approverSubjects {
		members, lerr := sdb.LookupSubjects(ctx, subject)
		if lerr != nil {
			return false, nil, lerr
		}
		for _, s := range members {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			authorized = append(authorized, s)
			if s == canonicalApprover.String() {
				ok = true
			}
		}
	}
	return ok, authorized, nil
}

// runSessionApprove is the body of the cobra command, factored out so
// it's reachable from tests / future automation without going through
// cobra arg parsing.
func runSessionApprove(
	ctx context.Context,
	out interface{ Write([]byte) (int, error) },
	g *apcmd.Globals,
	sessionName, requestID string,
	approverEmail identity.Email,
	approve bool, wait bool, force bool, timeout time.Duration,
) error {
	b, err := g.Bundle()
	if err != nil {
		return err
	}
	// 1. Resolve the AgentSession + match against the unified pending
	// view across all three kinds. With requestID empty the command
	// auto-picks when exactly one approval is pending; with multiple it
	// errors listing each id + kind.
	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &sess); err != nil {
		return fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}
	pendings := collectPendings(&sess)
	autoPicked := requestID == ""
	matched, err := resolvePending(pendings, requestID, b.Namespace, sessionName)
	if err != nil {
		return err
	}
	requestID = matched.RequestID
	// The matched entry's required approver subject-sets: the multi-set list
	// when present (leakage fan-out), else the singular subject.
	matchedSubjects := matched.ApproverSubjects
	if len(matchedSubjects) == 0 && matched.ApproverSubject != "" {
		matchedSubjects = []string{matched.ApproverSubject}
	}
	matchedSubject := strings.Join(matchedSubjects, ", ") // display form
	// Announce the resolved request + its kind so the operator knows which
	// approval family is being driven (and, on auto-pick, that the only
	// pending one was selected).
	if autoPicked {
		fmt.Fprintf(out, "auto-selected the only pending request: %s (%s)\n", requestID, matched.Kind)
	} else {
		fmt.Fprintf(out, "resolved request %s (%s)\n", requestID, matched.Kind)
	}

	dispatch, ok := dispatchFor(matched.Kind)
	if !ok {
		return fmt.Errorf("unsupported approval kind %q for request %s", matched.Kind, requestID)
	}

	// 1b. Derive the approver canonical and display email. When
	// --approver-email is set it is treated as a configuration/reference
	// value (EmailReference, EmailVerified=false) — useful for scripting
	// where the caller knows the email without having to log in. When the
	// flag is omitted, the CLI's cached or interactively-acquired identity
	// is used: clilogin.EnsureIdentity returns VerifiedEmail (or the legacy local
	// fallback), and p.Canonical() is the canonical for both cases.
	var canonicalApprover identity.CanonicalUserID
	if approverEmail != "" {
		// Explicit flag: canonicalize identically to channelsd's generic
		// interaction-decision pipe (EmailReference — see
		// pkg/channels/channelsd/pipeline/interaction_decision.go) so the pre-check
		// agrees with the server.
		c, cerr := identity.EmailReference(approverEmail).Canonical()
		if cerr != nil {
			return fmt.Errorf("canonicalize --approver-email %q: %w", approverEmail, cerr)
		}
		canonicalApprover = c
	} else {
		p, iderr := clilogin.EnsureIdentity(ctx, g)
		if iderr != nil {
			return fmt.Errorf("resolve approver identity: %w", iderr)
		}
		// The CLI identity may be the local synthetic user (no verified email)
		// on a local install; it is keyed by the synthetic subject as the
		// owner, so opt in. channelsd re-checks the approver server-side.
		c, cerr := p.AllowSynthetic().Canonical()
		if cerr != nil {
			return fmt.Errorf("canonicalize approver identity: %w", cerr)
		}
		canonicalApprover = c
		// Use the email for display and the NATS envelope when available.
		approverEmail = p.Email()
	}
	if cl, cerr := apspicedb.NewClientFromEnv(); cerr != nil {
		fmt.Fprintf(out, "warning: could not verify approver authorization (SpiceDB client: %v); proceeding — channelsd enforces it server-side\n", cerr)
	} else {
		ok, authorized, lerr := approverAuthorized(ctx, cl, matchedSubjects, canonicalApprover)
		_ = cl.Close()
		switch {
		case lerr != nil:
			fmt.Fprintf(out, "warning: could not verify approver authorization (%v); proceeding — channelsd enforces it server-side\n", lerr)
		case !ok:
			authzList := "(none)"
			if len(authorized) > 0 {
				authzList = "user:" + strings.Join(authorized, ", user:")
			}
			msg := fmt.Sprintf("approver %q (user:%s) is not in the required approver set %q for request %s\n  authorized: %s",
				approverEmail, canonicalApprover, matchedSubject, requestID, authzList)
			if !force {
				return fmt.Errorf("%s\nre-run with a correct --approver-email, or pass --force to publish anyway (e.g. to exercise the server-side not_authorized path)", msg)
			}
			fmt.Fprintf(out, "warning: %s\n--force set: publishing the decision anyway\n", msg)
		default:
			fmt.Fprintf(out, "approver %q is authorized for %s ✓\n", approverEmail, matchedSubject)
		}
	}

	// 2. Port-forward to NATS.
	pf, err := portforward.New(b.REST, natsServiceNamespace, natsServiceName, natsServiceSelector, natsServicePort, 0 /* OS-assigned */)
	if err != nil {
		return fmt.Errorf("portforward.New: %w", err)
	}
	pfCtx, cancelPF := context.WithCancel(ctx)
	defer cancelPF()
	if err := pf.Start(pfCtx, out); err != nil {
		return fmt.Errorf("port-forward to NATS: %w", err)
	}
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", pf.LocalPort())

	// Load the CLI's per-client NATS creds + CA from the cluster and
	// connect with authenticated TLS. apnats.Connect's reconnect policy
	// (RetryOnFailedConnect + indefinite reconnect) covers transient
	// dial failures; the prior synchronous 5s connect timeout is
	// subsumed by it.
	natsOpts, natsCleanup, err := clinats.LoadCreds(ctx, b)
	if err != nil {
		return err
	}
	defer natsCleanup()
	natsOpts.URL = natsURL
	nc, err := apnats.Connect(natsOpts)
	if err != nil {
		return fmt.Errorf("nats connect %s: %w", natsURL, err)
	}
	defer nc.Drain() //nolint:errcheck

	// 3. (Optional) Subscribe to the outbound Applied subject BEFORE
	// publishing so we can't miss the response under the wire-time
	// race. NATS subscriptions are ordered; once Subscribe returns
	// the consumer is bound. The Applied payload shape is kind-specific;
	// dispatch.decodeApplied normalizes it to an appliedOutcome.
	var appliedCh chan appliedOutcome
	if wait {
		appliedCh = make(chan appliedOutcome, 1)
		appliedSubject := channelevents.SubjectOut(
			channelevents.SubjectPrefix(b.Namespace, sessionName),
			dispatch.appliedKind,
		)
		sub, err := nc.Subscribe(appliedSubject, func(m *nats.Msg) {
			var env channelevents.Envelope
			if err := json.Unmarshal(m.Data, &env); err != nil {
				return
			}
			outcome, ok := dispatch.decodeApplied(env.Payload, requestID)
			if !ok {
				return
			}
			select {
			case appliedCh <- outcome:
			default:
			}
		})
		if err != nil {
			return fmt.Errorf("subscribe to %s: %w", appliedSubject, err)
		}
		defer sub.Unsubscribe()
	}

	// 4. Build + publish the kind-appropriate decision envelope. The
	// --deny flag flips Decision to "deny" for every kind identically;
	// dispatch.buildDecisionPayload selects the concrete payload type.
	decision := "approve"
	if !approve {
		decision = "deny"
	}
	pl := dispatch.buildDecisionPayload(requestID, approverEmail.String(), decision)
	publishFn := channelevents.PublishFunc(func(subject string, body []byte) error {
		return nc.Publish(subject, body)
	})
	if err := channelevents.PublishIn(publishFn, b.Namespace, sessionName,
		dispatch.decisionKind, pl); err != nil {
		return fmt.Errorf("publish decision envelope: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return fmt.Errorf("nats flush: %w", err)
	}
	fmt.Fprintf(out, "published %s decision for request %s (%s) (approver: %s)\n",
		decision, requestID, matched.Kind, approverEmail)

	if !wait {
		return nil
	}

	// 5. Wait for the Applied envelope (channelsd-side outcome).
	select {
	case applied := <-appliedCh:
		fmt.Fprintf(out, "channelsd applied: decision=%s reason=%q approver=%s\n",
			applied.Decision, applied.Reason, applied.ApproverDisplay)
		switch applied.Reason {
		case "not_authorized":
			return fmt.Errorf("approver %q is not authorized for this request — check the AgentClass approver subject + SpiceDB tuples",
				approverEmail)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s waiting for the Applied envelope (channelsd may have rejected the click; check `kubectl logs deploy/spicebox-channelsd`)",
			timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}
