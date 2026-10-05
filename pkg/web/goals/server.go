// Package goals serves authenticated goal management on the operator data plane.
package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	agentcaps "github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const ResourceType = "agent_goal_domain"

type Authority interface {
	Relations() authz.RelWriter
	GrantSlots(context.Context, string, string, []authz.SlotBinding, time.Time) error
	CheckInteract(context.Context, string, string, identity.CanonicalUserID, bool) (bool, error)
	CheckOnResource(context.Context, string, string, string, identity.CanonicalUserID, bool) (bool, error)
}
type ExecutionSessionAuthority interface {
	ValidateGoalSession(context.Context, *v1.AgentSession) error
}
type ExecutionResultRecorder interface {
	RecordGoalResult(context.Context, *v1.AgentSession, domain.RunProposal) (domain.Occurrence, error)
}

type ExecutionReplyPreparer interface {
	PrepareGoalReply(context.Context, *v1.AgentSession, channelevents.OutboundUserMessagePayload, []domain.Source) (domain.Occurrence, error)
}

type Server struct {
	ExecutionSessions ExecutionSessionAuthority
	Service           *domain.Service
	Reader            client.Reader
	Memory            memory.Memory
	Tokens            *tokens.Registry
	Keys              provenance.PublisherKeyLookup
	Auth              Authority
	// ActorWait bounds the initial Create-to-attestation race; negative disables
	// waiting in tests. Every result still requires a trusted actor record.
	ActorWait time.Duration
	// ColdRegistryUntil enables the operator's bounded startup grace for
	// unknown bearers. Registered tokens still undergo the primary-token gate.
	ColdRegistryUntil time.Time
}
type Request = domain.Request
type Response = domain.Response

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.URL.Path == "/goals/decision" {
		s.decideHTTP(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/goals/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	ns, name := parts[0], parts[1]
	bearer, has := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	// Use one registry snapshot so concurrent token restoration cannot turn
	// an unknown-token observation into a permanent refusal midway through.
	var info tokens.TokenInfo
	var known bool
	if has && bearer != "" && s.Tokens != nil {
		info, known = s.Tokens.LookupInfo(bearer)
	}
	// Extras are read-only; only the token's primary session qualifies here.
	if !known || info.Session != (memory.NamespacedName{Namespace: ns, Name: name}) {
		if has && bearer != "" && s.Tokens != nil && !known && time.Now().Before(s.ColdRegistryUntil) {
			w.Header().Set("Retry-After", "1")
			log.FromContext(r.Context()).Info("goal API: token registry warming; answering retryable 503", "session", ns+"/"+name)
			http.Error(w, "goal service warming after operator restart; retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "unauthorized", 401)
		return
	}
	if s.Auth == nil || s.Reader == nil || s.Memory == nil || s.Service == nil || s.Keys == nil {
		http.Error(w, "goal service unavailable", 503)
		return
	}
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	ctx := memory.WithCaller(memory.WithSystemApproval(r.Context(), "system:operator"), "system:operator")
	if req.Operation == "authorize_execution" || req.Operation == "report_execution_result" || req.Operation == "prepare_reply" {
		var sess v1.AgentSession
		if s.ExecutionSessions == nil {
			s.fail(w, r, domain.ErrDenied)
			return
		}
		if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
			s.fail(w, r, err)
			return
		}
		if sess.Spec.GoalExecution == nil {
			s.fail(w, r, domain.ErrDenied)
			return
		}
		if err := s.ExecutionSessions.ValidateGoalSession(ctx, &sess); err != nil {
			s.fail(w, r, err)
			return
		}
		owner := v1.StartedByCanonical(&sess)
		allowed, err := s.Auth.CheckInteract(ctx, ns, name, owner, true)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !allowed {
			s.fail(w, r, domain.ErrDenied)
			return
		}
		out := Response{ExecutionAvailable: true}
		if req.Operation == "prepare_reply" {
			preparer, ok := s.ExecutionSessions.(ExecutionReplyPreparer)
			if !ok || req.Reply == nil {
				s.fail(w, r, domain.ErrDenied)
				return
			}
			sources, err := s.Sources(ctx, domain.Actor{Session: ns + "/" + name, Domain: domain.Domain{Owner: owner.String()}})
			if err != nil {
				s.fail(w, r, err)
				return
			}
			run, err := preparer.PrepareGoalReply(ctx, &sess, *req.Reply, sources)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			out.Run = &run
		}
		if req.Operation == "report_execution_result" {
			recorder, ok := s.ExecutionSessions.(ExecutionResultRecorder)
			if !ok {
				s.fail(w, r, domain.ErrDenied)
				return
			}
			// Source dependencies come from trusted session memory, not wire fields.
			req.Proposal.Sources, err = s.Sources(ctx, domain.Actor{Session: ns + "/" + name, Domain: domain.Domain{Owner: owner.String()}})
			if err != nil {
				s.fail(w, r, err)
				return
			}
			run, err := recorder.RecordGoalResult(ctx, &sess, req.Proposal)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			out.Run = &run
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.FromContext(ctx).Info("goal authority response failed", "session", ns+"/"+name, "error", err)
		}
		return
	}
	a, err := s.resolve(ctx, ns, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resource := ResourceType + ":" + a.Domain.ID()
	if req.Operation == "create" || req.Operation == "update" || req.Operation == "request_execution" {
		if req.Resource != resource {
			s.fail(w, r, domain.ErrDenied)
			return
		}
	}
	out := Response{Resource: resource}
	if _, ok := s.Service.Store.(domain.OccurrenceStore); ok && s.Service.ExecutionAuth != nil {
		out.ExecutionAvailable = true
	}
	switch req.Operation {
	case "create":
		g, e := s.Service.Create(ctx, a, req.Create)
		err = e
		out.Goal = &g
	case "update":
		g, e := s.Service.Update(ctx, a, req.Change)
		err = e
		out.Goal = &g
	case "request_execution":
		err = s.PrepareExecution(ctx, a, &req.Execution)
		if err == nil {
			var g domain.Goal
			g, err = s.Service.RequestExecution(ctx, a, req.Execution)
			out.Goal = &g
		}
	case "runs":
		p, e := s.Service.Runs(ctx, a, req.ID, req.List)
		err = e
		out.Runs = &p
	case "get":
		g, e := s.Service.Get(ctx, a, req.ID)
		err = e
		out.Goal = &g
	case "list":
		p, e := s.Service.List(ctx, a, req.List)
		err = e
		p.ExecutionAvailable = out.ExecutionAvailable
		out.Page = &p
	default:
		err = domain.ErrInvalid
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.FromContext(ctx).Info("goal response failed", "session", a.Session, "error", err)
	}
}
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, message := 500, "goal operation failed"
	switch {
	case errors.Is(err, domain.ErrDenied), errors.Is(err, domain.ErrNotFound):
		code, message = 404, "goal unavailable"
	case errors.Is(err, domain.ErrConflict):
		code, message = 409, err.Error()
	case errors.Is(err, domain.ErrInvalid):
		code, message = 400, err.Error()
	}
	log.FromContext(r.Context()).Info("goal operation refused", "path", r.URL.Path, "status", code, "error", err)
	http.Error(w, message, code)
}
func (s *Server) resolve(ctx context.Context, ns, name string) (domain.Actor, error) {
	var sess v1.AgentSession
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return domain.Actor{}, fmt.Errorf("%w: session lookup: %v", domain.ErrDenied, err)
	}
	if sess.Spec.Parent != nil || sess.UID == "" || !sess.DeletionTimestamp.IsZero() {
		return domain.Actor{}, domain.ErrDenied
	}
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: sess.Spec.Class}, &class); err != nil {
		return domain.Actor{}, fmt.Errorf("%w: class lookup: %v", domain.ErrDenied, err)
	}
	grant, err := agentcaps.GrantOf(&class, "goals")
	if err != nil {
		return domain.Actor{}, err
	}
	if class.Spec.GetAuthz().InformationLeakage.ResolvedMode() != "enforcing" {
		return domain.Actor{}, fmt.Errorf("%w: goals require enforcing information leakage policy", domain.ErrDenied)
	}
	if !agentcaps.Active(false, grant) || class.UID == "" || !class.DeletionTimestamp.IsZero() {
		return domain.Actor{}, domain.ErrDenied
	}
	rows, err := s.actorRecords(ctx, ns, name)
	if err != nil {
		return domain.Actor{}, err
	}
	var latest, first memory.Entry
	for _, e := range rows.Entries {
		if e.Provenance == nil || e.Provenance.Publisher != "system:channelsd" {
			continue
		}
		if err := provenance.VerifyEntrySignature(s.Keys, e); err != nil {
			return domain.Actor{}, err
		}
		if first.Provenance == nil || e.Provenance.Seq < first.Provenance.Seq {
			first = e
		}
		if latest.Provenance == nil || e.Provenance.Seq > latest.Provenance.Seq {
			latest = e
		}
	}
	if latest.Provenance == nil {
		return domain.Actor{}, fmt.Errorf("%w: trusted human attestation is missing", domain.ErrDenied)
	}
	var proof goalactor.Content
	if err := json.Unmarshal(latest.Content, &proof); err != nil {
		return domain.Actor{}, err
	}
	if proof.Owner == "" || proof.SessionUID != string(sess.UID) || proof.ClassUID != string(class.UID) {
		return domain.Actor{}, domain.ErrDenied
	}
	var initial goalactor.Content
	if err := json.Unmarshal(first.Content, &initial); err != nil {
		return domain.Actor{}, err
	}
	if initial.Owner != proof.Owner || initial.SessionUID != proof.SessionUID || initial.ClassUID != proof.ClassUID {
		return domain.Actor{}, domain.ErrDenied
	}
	owner := identity.CanonicalFromTrusted(proof.Owner, "channelsd goal actor")
	ok, err := s.Auth.CheckInteract(ctx, ns, name, owner, true)
	if err != nil {
		return domain.Actor{}, err
	}
	if !ok {
		return domain.Actor{}, domain.ErrDenied
	}
	attestation, err := json.Marshal(latest)
	if err != nil {
		return domain.Actor{}, err
	}
	a := domain.Actor{Domain: domain.Domain{Namespace: ns, Owner: proof.Owner, Class: class.Name, ClassUID: string(class.UID)}, Session: ns + "/" + name, SessionUID: string(sess.UID), Proof: latest.ID, Attestation: string(attestation)}
	// The pin survives grant expiry. Another employee in this conversation must
	// start their own session, rather than move a framework-owned private slot.
	id, err := authz.NewObjectID(a.Domain.ID(), nil)
	if err != nil {
		return domain.Actor{}, err
	}
	if err := s.Auth.GrantSlots(ctx, ns, name, []authz.SlotBinding{{ResourceType: ResourceType, ResourceID: id, Permission: "write_memory", Occupancy: authz.SlotOccupancySingle, Rebind: authz.SlotRebindNever}}, authz.SlotGrantExpiry(time.Now(), 0)); err != nil {
		return domain.Actor{}, fmt.Errorf("%w: private goal scope binding failed: %v", domain.ErrDenied, err)
	}
	if err := s.Auth.Relations().WriteRelationships(ctx, []authz.Relation{{ResourceType: ResourceType, ResourceID: a.Domain.ID(), Relation: "owner", SubjectType: "user", SubjectID: proof.Owner}}); err != nil {
		return domain.Actor{}, err
	}
	return a, nil
}

// Authorize runs at every operation, including deduplication retries, and
// rechecks the latest platform actor record so a stale turn cannot retain access.
func (s *Server) Authorize(ctx context.Context, a domain.Actor, write bool) error {
	ns, name, ok := strings.Cut(a.Session, "/")
	if !ok {
		return domain.ErrDenied
	}
	current, err := s.resolve(ctx, ns, name)
	if err != nil {
		return err
	}
	if current != a {
		return domain.ErrDenied
	}
	permission := "view_memory"
	if write {
		permission = "manage"
	}
	owner := identity.CanonicalFromTrusted(a.Domain.Owner, "channelsd goal actor")
	ok, err = s.Auth.CheckOnResource(ctx, ResourceType, a.Domain.ID(), permission, owner, true)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrDenied
	}
	if write {
		_, err := s.Sources(ctx, a)
		return err
	}
	return nil
}

// Sources carries every resource dependency forward, including reads from
// earlier turns. Goals cannot launder a restricted read into future sessions.
func (s *Server) Sources(ctx context.Context, a domain.Actor) ([]domain.Source, error) {
	rows, err := s.Memory.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: a.Session}, Kinds: []string{infoleakagetaint.KindName}})
	if err != nil {
		return nil, err
	}
	sources := make([]domain.Source, 0, len(rows.Entries))
	for _, e := range rows.Entries {
		var t infoleakagetaint.TaintRecord
		if err := json.Unmarshal(e.Content, &t); err != nil {
			return nil, err
		}
		sources = append(sources, domain.Source{ResourceType: t.ResourceType, ResourceID: t.ResourceID, Permission: t.Permission})
	}
	if err := s.ReadGoal(ctx, a, domain.Goal{Sources: sources}); err != nil {
		return nil, err
	}
	return sources, nil
}
func (s *Server) ReadGoal(ctx context.Context, a domain.Actor, g domain.Goal) error {
	owner := identity.CanonicalFromTrusted(a.Domain.Owner, "channelsd goal actor")
	for _, src := range g.Sources {
		ok, err := s.Auth.CheckOnResource(ctx, src.ResourceType, src.ResourceID, src.Permission, owner, true)
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrDenied
		}
	}
	return nil
}

func (s *Server) actorRecords(ctx context.Context, ns, name string) (memory.QueryResult, error) {
	wait := s.ActorWait
	if wait == 0 {
		wait = 5 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		rows, err := s.Memory.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: ns + "/" + name}, Kinds: []string{goalactor.KindName}})
		if err != nil || len(rows.Entries) > 0 || !time.Now().Before(deadline) {
			return rows, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return memory.QueryResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}
