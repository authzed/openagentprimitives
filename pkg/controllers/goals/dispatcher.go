// Package goals activates bounded, operator-approved goal sessions.
package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Dispatcher struct {
	Service        *domain.Service
	Store          domain.OccurrenceStore
	Client         client.Client
	Reader         client.Reader
	Worker         string
	Now            func() time.Time
	DeliveryMemory memory.Memory
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (*Dispatcher) NeedLeaderElection() bool { return true }
func (d *Dispatcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := d.Tick(ctx); err != nil {
			log.FromContext(ctx).Info("goal dispatch failed", "worker", d.Worker, "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (d *Dispatcher) Tick(ctx context.Context) error {
	due, err := d.Store.Due(ctx, d.now(), 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range due {
		occurrence, err := d.Store.Claim(ctx, domain.ClaimRequest{ID: candidate.ID, Worker: d.Worker, Now: d.now(), Lease: 5 * time.Second, OwnerLimit: 1, ClassLimit: 4})
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrDenied) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		occurrence, replyErr := d.processReply(ctx, occurrence)
		if replyErr != nil {
			failures = append(failures, fmt.Errorf("occurrence %s reply: %w", occurrence.ID, replyErr))
		}
		// Cancellation and termination still run when receipt storage is down.
		if err := d.activate(ctx, occurrence); err != nil {
			failures = append(failures, fmt.Errorf("occurrence %s: %w", occurrence.ID, err))
		}
	}
	return errors.Join(failures...)
}
func ref(o domain.Occurrence) *v1.GoalExecutionReference {
	return &v1.GoalExecutionReference{GoalID: o.GoalID, OccurrenceID: o.ID, GoalRevision: o.GoalRevision, ConsentDigest: o.ConsentDigest}
}
func (d *Dispatcher) activate(ctx context.Context, o domain.Occurrence) error {
	g, err := d.Service.Store.Get(ctx, o.Domain, o.GoalID)
	if err != nil {
		return err
	}
	var sess v1.AgentSession
	key := client.ObjectKey{Namespace: o.Domain.Namespace, Name: o.SessionName}
	lookup := d.Reader.Get(ctx, key, &sess)
	if lookup != nil && !apierrors.IsNotFound(lookup) {
		return lookup
	}
	// A persisted stop observation wins after a crash, including after the
	// AgentSession has disappeared. Never reinterpret cleanup as a lost session.
	if o.Outcome != nil {
		var costErr error
		o, costErr = d.captureCost(ctx, o, &sess, lookup == nil, o.Outcome.Reason == domain.RunSessionEnded || o.Outcome.Reason == domain.RunSessionFailed)
		if o.Outcome.Reason == domain.RunSessionMissing {
			_, err := d.Store.Finish(ctx, o, domain.OccurrenceUnknown, d.now())
			return errors.Join(costErr, err)
		}
		// Keep the completed conversation inspectable for its original bounded
		// window. Its observed outcome already prevents further goal actions.
		if lookup == nil && o.Outcome.Reason == domain.RunSessionEnded && g.State == domain.Active && g.Revision == o.GoalRevision && g.Execution != nil && d.now().Before(o.ExpiresAt) && d.now().Before(sess.CreationTimestamp.Add(time.Duration(g.Execution.Terms.Bounds.DurationSeconds)*time.Second)) {
			return costErr
		}
		return errors.Join(costErr, d.stop(ctx, o, &sess, lookup == nil))
	}
	reason := domain.RunReason("")
	switch {
	case g.State == domain.Cancelled:
		reason = domain.RunCancelled
	case g.State == domain.Paused:
		reason = domain.RunPaused
	case g.Revision != o.GoalRevision || g.Execution == nil || g.Execution.Digest != o.ConsentDigest:
		reason = domain.RunSuperseded
	case !d.now().Before(o.ExpiresAt):
		reason = domain.RunConsentExpired
	}
	if reason == "" {
		if err := d.Service.Dispatchable(ctx, g); err != nil {
			if !errors.Is(err, domain.ErrDenied) {
				return err
			}
			reason = domain.RunAuthorityDenied
		}
	}
	if reason != "" {
		return d.observeAndStop(ctx, o, &sess, lookup == nil, reason)
	}
	if lookup == nil {
		if !reflect.DeepEqual(sess.Spec.GoalExecution, ref(o)) || sess.Spec.Class != g.Domain.Class || (o.SessionUID != "" && o.SessionUID != string(sess.UID)) {
			return fmt.Errorf("goal session identity mismatch")
		}
		if o.SessionUID == "" {
			o, err = d.Store.Attach(ctx, o, string(sess.UID), d.now())
			if err != nil {
				return err
			}
		}
		// Finishing a session never completes the goal. Goal completion remains a
		// separate evidence-bearing decision by the human's management session.
		if sess.Status.Phase == v1.AgentSessionPhaseSucceeded || sess.Status.Phase == v1.AgentSessionPhaseFailed {
			reason := domain.RunSessionEnded
			if sess.Status.Phase == v1.AgentSessionPhaseFailed {
				reason = domain.RunSessionFailed
			}
			return d.observeAndStop(ctx, o, &sess, true, reason)
		}
		if !sess.DeletionTimestamp.IsZero() {
			return nil
		}
		if d.now().Sub(sess.CreationTimestamp.Time) > time.Duration(g.Execution.Terms.Bounds.DurationSeconds)*time.Second {
			return d.observeAndStop(ctx, o, &sess, true, domain.RunDurationExpired)
		}
		// The runner can finish a conversation while the CR remains Idle. Read
		// the durable pod status rather than depending on a lifecycle notification.
		reason, err := d.runnerOutcome(ctx, &sess)
		if err != nil {
			return err
		}
		if reason != "" {
			return d.observeAndStop(ctx, o, &sess, true, reason)
		}
		_, err = d.captureCost(ctx, o, &sess, true, false)
		return err
	}
	// A missing known UID is an unknown result, never permission to recreate.
	if o.SessionUID != "" {
		if o.Outcome == nil {
			o, err = d.recordOutcome(ctx, o, domain.RunSessionMissing)
			if err != nil {
				return err
			}
		}
		o, err = d.captureCost(ctx, o, &sess, false, false)
		if err != nil {
			return err
		}
		_, err = d.Store.Finish(ctx, o, domain.OccurrenceUnknown, d.now())
		return err
	}
	ns, name, _ := strings.Cut(g.Execution.Session, "/")
	var source v1.AgentSession
	if err := d.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &source); err != nil {
		return err
	}
	binding := v1.OutboundBinding(&source)
	if binding == nil {
		return domain.ErrDenied
	}
	output := *binding
	output.InheritFrom = ""
	output.NATSSubjectPrefix = "ap.session." + o.Domain.Namespace + "." + o.SessionName
	owner := identity.CanonicalFromTrusted(g.Domain.Owner, "verified execution owner")
	b := g.Execution.Terms.Bounds
	duration := metav1.Duration{Duration: time.Duration(b.DurationSeconds) * time.Second}
	evidence, err := json.Marshal(g.Execution.Terms.Evidence)
	if err != nil {
		return err
	}
	sess = v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: o.Domain.Namespace, Name: o.SessionName, Labels: map[string]string{v1.LabelChannelKind: output.Kind}, Annotations: map[string]string{
		v1.AnnotationStartedByCanonicalID: owner.Subject().String(), v1.AnnotationStartedByEmail: source.Annotations[v1.AnnotationStartedByEmail], v1.AnnotationStartedByExternalID: source.Annotations[v1.AnnotationStartedByExternalID],
	}}, Spec: v1.AgentSessionSpec{Class: g.Domain.Class, GoalExecution: ref(o), InputChannel: output.DeepCopy(), OutputChannel: &output,
		OpeningSummary: openingSummary(g),
		Budget:         &v1.BudgetConfig{MaxDuration: duration, SessionExpiration: duration, MaxTurns: int32(b.Turns), MaxTokens: b.Tokens},
		Prompt:         v1.PromptSource{Inline: fmt.Sprintf("This is one bounded execution of private goal %q. Required outcome: %q. Required evidence: %s. Only respond_to_user may perform actions. Create a fresh plan and obtain approval before acting. Report a concise private result with evidence. Before agent_work_complete, call report_goal_result with a stable requestID, status reported_success, blocked, failed or unknown, a summary, and evidence references. This records your account, not verified delivery. Do not claim the durable goal is completed; session success alone is insufficient.", g.Title, g.Outcome, evidence)}}}
	latest, err := d.Store.Occurrence(ctx, o.ID)
	if err != nil {
		return err
	}
	if latest.Fence != o.Fence || latest.Worker != o.Worker || !d.now().Before(latest.LeaseUntil) {
		return domain.ErrConflict
	}
	if err := d.Service.Dispatchable(ctx, g); err != nil {
		return err
	}
	if err := d.Client.Create(ctx, &sess); err != nil {
		return err
	}
	_, err = d.Store.Attach(ctx, o, string(sess.UID), d.now())
	return err
}

func openingSummary(g domain.Goal) string {
	text := []rune(fmt.Sprintf("Session created to meet goal %s: %s", strings.Join(strings.Fields(g.Title), " "), strings.Join(strings.Fields(g.Outcome), " ")))
	if len(text) > 2000 {
		return string(text[:1999]) + "…"
	}
	return string(text)
}
func (d *Dispatcher) stop(ctx context.Context, o domain.Occurrence, sess *v1.AgentSession, exists bool) error {
	if exists {
		if !reflect.DeepEqual(sess.Spec.GoalExecution, ref(o)) || (o.SessionUID != "" && o.SessionUID != string(sess.UID)) {
			return fmt.Errorf("refusing to stop unrelated session")
		}
		if o.SessionUID == "" {
			var err error
			o, err = d.Store.Attach(ctx, o, string(sess.UID), d.now())
			if err != nil {
				return err
			}
		}
		if sess.DeletionTimestamp.IsZero() {
			uid := sess.UID
			if err := d.Client.Delete(ctx, sess, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
	// A capacity reservation is released only after the session and its runner
	// pods have disappeared. GC may lag the AgentSession deletion.
	var pods corev1.PodList
	if err := d.Reader.List(ctx, &pods, client.InNamespace(o.Domain.Namespace)); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.UID == types.UID(o.SessionUID) && o.SessionUID != "" {
				return nil
			}
		}
	}
	state := domain.OccurrenceCancelled
	if o.Outcome != nil {
		switch o.Outcome.Reason {
		case domain.RunSessionEnded:
			state = domain.OccurrenceFinished
		case domain.RunSessionFailed, domain.RunInfrastructureFailed, domain.RunDurationExpired:
			state = domain.OccurrenceFailed
		}
	}
	if o.Reply != nil && o.Reply.State == "attempted" {
		state = domain.OccurrenceUnknown
	}
	_, err := d.Store.Finish(ctx, o, state, d.now())
	return err
}

// ValidateGoalSession is called before the reconciler reads any credentials.
func (d *Dispatcher) ValidateGoalSession(ctx context.Context, sess *v1.AgentSession) error {
	reference := sess.Spec.GoalExecution
	if reference == nil {
		return nil
	}
	o, err := d.Store.Occurrence(ctx, reference.OccurrenceID)
	if err != nil {
		return err
	}
	if o.SessionUID == "" || o.SessionUID != string(sess.UID) || o.SessionName != sess.Name || o.Domain.Namespace != sess.Namespace || !reflect.DeepEqual(reference, ref(o)) || o.State != domain.OccurrenceRunning || o.Outcome != nil || sess.Spec.Parent != nil || sess.Spec.ForkedFrom != "" || sess.Spec.AgentIdentity != "" {
		return domain.ErrDenied
	}
	g, err := d.Service.Store.Get(ctx, o.Domain, o.GoalID)
	if err != nil {
		return err
	}
	if g.Revision != o.GoalRevision || g.Execution == nil || v1.StartedByCanonical(sess).String() != o.Domain.Owner {
		return domain.ErrDenied
	}
	if d.now().Sub(sess.CreationTimestamp.Time) >= time.Duration(g.Execution.Terms.Bounds.DurationSeconds)*time.Second {
		return domain.ErrDenied
	}
	sourceNS, sourceName, _ := strings.Cut(g.Execution.Session, "/")
	var source v1.AgentSession
	if err := d.Reader.Get(ctx, client.ObjectKey{Namespace: sourceNS, Name: sourceName}, &source); err != nil {
		return err
	}
	sourceBinding := v1.OutboundBinding(&source)
	output := v1.OutboundBinding(sess)
	if sourceBinding == nil || output == nil {
		return domain.ErrDenied
	}
	if !reflect.DeepEqual(sess.Spec.InputChannel, output) {
		return domain.ErrDenied
	}
	expected := *sourceBinding
	actual := *output
	expected.NATSSubjectPrefix = ""
	actual.NATSSubjectPrefix = ""
	expected.InheritFrom = ""
	if !reflect.DeepEqual(expected, actual) {
		return domain.ErrDenied
	}
	if sess.Spec.Budget == nil || sess.Spec.Budget.MaxTurns < 1 || sess.Spec.Budget.MaxTokens < 1 || sess.Spec.Budget.MaxDuration.Duration < time.Second || sess.Spec.Budget.SessionExpiration.Duration < time.Second {
		return domain.ErrDenied
	}
	if sess.Spec.Budget.MaxDuration.Duration > time.Duration(g.Execution.Terms.Bounds.DurationSeconds)*time.Second || int64(sess.Spec.Budget.MaxTurns) > g.Execution.Terms.Bounds.Turns || sess.Spec.Budget.MaxTokens > g.Execution.Terms.Bounds.Tokens {
		return domain.ErrDenied
	}
	return d.Service.Dispatchable(ctx, g)
}

// ConstrainGoalSettings narrows resolved tier policy to the reviewed ceilings.
func (d *Dispatcher) ConstrainGoalSettings(ctx context.Context, sess *v1.AgentSession, eff *v1.EffectiveSettings) error {
	if err := d.ValidateGoalSession(ctx, sess); err != nil {
		return err
	}
	o, err := d.Store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
	if err != nil {
		return err
	}
	g, err := d.Service.Store.Get(ctx, o.Domain, o.GoalID)
	if err != nil {
		return err
	}
	b := g.Execution.Terms.Bounds
	eff.Authz.PlanGate = &v1.PlanGateConfig{Mode: "enforcing", RequirePlan: ptr.To(true), Rendering: &v1.PlanRendering{MaxAutoApproveHandles: 0}}
	deadline := time.Duration(b.ApprovalSeconds) * time.Second
	if eff.Authz.ApprovalTimeout.Duration <= 0 || eff.Authz.ApprovalTimeout.Duration > deadline {
		eff.Authz.ApprovalTimeout = metav1.Duration{Duration: deadline}
	}
	return nil
}

func (d *Dispatcher) recordOutcome(ctx context.Context, o domain.Occurrence, reason domain.RunReason) (domain.Occurrence, error) {
	store, ok := d.Store.(domain.RunStore)
	if !ok {
		return o, fmt.Errorf("goal dispatch requires durable run outcomes")
	}
	return store.RecordOutcome(ctx, o, reason, d.now())
}
func (d *Dispatcher) observeAndStop(ctx context.Context, o domain.Occurrence, sess *v1.AgentSession, exists bool, reason domain.RunReason) error {
	if exists && (!reflect.DeepEqual(sess.Spec.GoalExecution, ref(o)) || (o.SessionUID != "" && o.SessionUID != string(sess.UID))) {
		return fmt.Errorf("refusing to observe unrelated session")
	}
	if exists && o.SessionUID == "" {
		var err error
		o, err = d.Store.Attach(ctx, o, string(sess.UID), d.now())
		if err != nil {
			return err
		}
	}
	o, costErr := d.captureCost(ctx, o, sess, exists, reason == domain.RunSessionEnded || reason == domain.RunSessionFailed)
	o, err := d.recordOutcome(ctx, o, reason)
	if err != nil {
		return errors.Join(costErr, err)
	}
	if reason == domain.RunSessionEnded && exists {
		return costErr
	}
	// Accounting outages must not prevent cancellation or bounded termination.
	return errors.Join(costErr, d.stop(ctx, o, sess, exists))
}
func (d *Dispatcher) runnerOutcome(ctx context.Context, sess *v1.AgentSession) (domain.RunReason, error) {
	var pods corev1.PodList
	if err := d.Reader.List(ctx, &pods, client.InNamespace(sess.Namespace)); err != nil {
		return "", err
	}
	found := false
	pending := false
	for _, pod := range pods.Items {
		owned := false
		for _, owner := range pod.OwnerReferences {
			if owner.UID == sess.UID && sess.UID != "" {
				owned = true
			}
		}
		if !owned {
			continue
		}
		if pod.Status.Phase == corev1.PodFailed {
			return domain.RunInfrastructureFailed, nil
		}
		if pod.Status.Phase != corev1.PodSucceeded {
			pending = true
			continue
		}
		found = true
	}
	if found && !pending {
		return domain.RunSessionEnded, nil
	}
	return "", nil
}

// RecordGoalResult binds the proposal to a verified root, never caller IDs.
func (d *Dispatcher) RecordGoalResult(ctx context.Context, sess *v1.AgentSession, p domain.RunProposal) (domain.Occurrence, error) {
	if err := d.ValidateGoalSession(ctx, sess); err != nil {
		return domain.Occurrence{}, err
	}
	o, err := d.Store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
	if err != nil {
		return o, err
	}
	store, ok := d.Store.(domain.RunStore)
	if !ok {
		return o, domain.ErrDenied
	}
	return store.ProposeResult(ctx, o, p, d.now())
}
