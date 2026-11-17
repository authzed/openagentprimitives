package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	agentcaps "github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Group scope: goals owned by the members of a multi-person conversation.
//
// The DOMAIN is the conversation's member subject-set, resolved here from the
// session's binding through the kind's GroupGoalHost — never from tool
// arguments. The ACTOR is a separate person: the attested author of the
// inbound that started the current turn (turnActor), checked live against
// the domain — view_memory to read, manage to change, pause to pause.

// maxGroupAudience bounds the per-person source check a group write runs
// against the conversation's audience. A larger audience is refused rather
// than partially checked: the check is only meaningful if it is complete.
const maxGroupAudience = 250

var (
	_ domain.GroupAuthorizer = (*Server)(nil)
	_ domain.Authorizer      = (*Server)(nil)
)

// groupsEnabled reports whether the class opted in to group goals
// (spec.capabilities.goals.groups).
func groupsEnabled(class *v1.AgentClass) (bool, error) {
	grant, err := agentcaps.GrantOf(class, "goals")
	if err != nil {
		return false, err
	}
	cfg, err := domain.ParseConfig(grant.Raw)
	if err != nil {
		return false, err
	}
	return cfg.Groups, nil
}

// groupQualifies is the opt-in check for one Channel: the class
// opted in, acts as the agent, and the Channel's kind hosts group goals with
// membership kept current. Each failure says which, so the tool can tell the
// person why this conversation cannot hold group goals.
func (s *Server) groupQualifies(ctx context.Context, class *v1.AgentClass, ch *v1.Channel) error {
	enabled, err := groupsEnabled(class)
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("%w: group goals are not enabled for this agent", domain.ErrDenied)
	}
	if mode := class.Spec.IdentityMode; mode != "" && mode != "agent" {
		return fmt.Errorf("%w: group goals act as the agent, but this agent's identityMode is %q; group goals need identityMode agent", domain.ErrDenied, mode)
	}
	host, ok := registry.GroupGoalHostFor(ch.Spec.Kind)
	if !ok {
		return fmt.Errorf("%w: channel kind %q cannot host group goals", domain.ErrDenied, ch.Spec.Kind)
	}
	if k, _ := registry.Get(ch.Spec.Kind); k == nil {
		return fmt.Errorf("%w: channel kind %q is not registered", domain.ErrDenied, ch.Spec.Kind)
	} else if _, ok := k.(channelkinds.AudienceResolver); !ok {
		return fmt.Errorf("%w: channel kind %q cannot list who reads its conversations, which group goals need", domain.ErrDenied, ch.Spec.Kind)
	}
	if err := host.MembershipCurrent(ctx, s.Reader, ch); err != nil {
		return fmt.Errorf("%w: this conversation's group membership is not kept current: %v", domain.ErrDenied, err)
	}
	return nil
}

// groupConversation returns the binding and Channel of the conversation the
// session is in. A session whose replies go elsewhere is refused: a group goal
// belongs to the conversation its members can see.
func (s *Server) groupConversation(ctx context.Context, sess *v1.AgentSession) (*v1.ChannelBinding, *v1.Channel, error) {
	b := sess.Spec.InputChannel
	if b == nil || b.Name == "" {
		return nil, nil, fmt.Errorf("%w: this session is not in a conversation", domain.ErrDenied)
	}
	if !b.SameChannelAs(sess.Spec.OutputChannel) {
		return nil, nil, fmt.Errorf("%w: this session replies somewhere other than its conversation, so it cannot hold group goals", domain.ErrDenied)
	}
	var ch v1.Channel
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: b.Name}, &ch); err != nil {
		return nil, nil, fmt.Errorf("%w: conversation channel lookup: %v", domain.ErrDenied, err)
	}
	if ch.Spec.Kind != b.Kind || !ch.DeletionTimestamp.IsZero() {
		return nil, nil, fmt.Errorf("%w: this conversation's channel changed", domain.ErrDenied)
	}
	return b, &ch, nil
}

// groupDomain resolves the goal domain owned by the members of the session's
// conversation.
func (s *Server) groupDomain(ctx context.Context, sess *v1.AgentSession, class *v1.AgentClass) (domain.Domain, error) {
	b, ch, err := s.groupConversation(ctx, sess)
	if err != nil {
		return domain.Domain{}, err
	}
	if err := s.groupQualifies(ctx, class, ch); err != nil {
		return domain.Domain{}, err
	}
	host, _ := registry.GroupGoalHostFor(ch.Spec.Kind) // groupQualifies established it
	ref, ok := host.ConversationMemberRef(b)
	if !ok {
		return domain.Domain{}, fmt.Errorf("%w: this conversation is not a group; group goals live in a shared conversation", domain.ErrDenied)
	}
	return domain.Domain{Namespace: sess.Namespace, Owner: ref, Class: class.Name, ClassUID: string(class.UID)}, nil
}

// isGroupRequest decides the scope. An explicit scope wins. With none, a
// request naming a resource addresses whichever domain that resource is, so
// a write the model sends without repeating the scope still reaches the
// group goal it listed.
func (s *Server) isGroupRequest(ctx context.Context, ns, name string, req Request) (bool, error) {
	switch req.Scope {
	case "group":
		return true, nil
	case "private":
		return false, nil
	case "":
	default:
		return false, fmt.Errorf("%w: scope must be private or group", domain.ErrInvalid)
	}
	if req.Resource == "" {
		return false, nil
	}
	// Each failure below means "not addressable as a group goal", so the
	// request takes the private path, which re-derives and reports anything
	// that is really wrong. Logged so the fallback is never silent.
	logger := log.FromContext(ctx).WithValues("session", ns+"/"+name, "resource", req.Resource)
	var sess v1.AgentSession
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		logger.Info("goal scope: session lookup failed; treating the request as private", "error", err)
		return false, nil
	}
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: sess.Spec.Class}, &class); err != nil {
		logger.Info("goal scope: class lookup failed; treating the request as private", "class", sess.Spec.Class, "error", err)
		return false, nil
	}
	enabled, err := groupsEnabled(&class)
	if err != nil {
		logger.Info("goal scope: goals capability config unreadable; treating the request as private", "class", class.Name, "error", err)
		return false, nil
	}
	if !enabled {
		return false, nil
	}
	d, err := s.groupDomain(ctx, &sess, &class)
	if err != nil {
		logger.Info("goal scope: this conversation hosts no group goals; treating the request as private", "error", err)
		return false, nil
	}
	return req.Resource == ResourceType+":"+d.ID(), nil
}

// groupActor derives the group-scope actor: the turn's attested author as
// User, acting on the conversation's group domain. It writes nothing.
func (s *Server) groupActor(ctx context.Context, ns, name, turnRef string) (domain.Actor, error) {
	a, sess, class, err := s.turnActor(ctx, ns, name, turnRef)
	if err != nil {
		return domain.Actor{}, err
	}
	d, err := s.groupDomain(ctx, sess, class)
	if err != nil {
		return domain.Actor{}, err
	}
	a.User, a.TurnRef, a.Domain = a.Domain.Owner, turnRef, d
	return a, nil
}

// resolveGroup is resolve for group scope. It binds the session to the group
// domain with a multi-occupancy slot, so it never contends with the single,
// never-rebind slot private scope pins on the same resource type — one
// session can hold its human's private goals and its group's. The group
// domain is fixed by the session's binding, so a pin would add nothing.
//
// It writes member_group and class (idempotent touches, true of the domain
// whoever asks) so a member can list an empty group's goals before the first
// create; it never writes #owner. Managers are written by create and by
// manager changes, from Goal.Managers.
func (s *Server) resolveGroup(ctx context.Context, ns, name, turnRef string) (domain.Actor, error) {
	a, err := s.groupActor(ctx, ns, name, turnRef)
	if err != nil {
		return domain.Actor{}, err
	}
	id, err := authz.NewObjectID(a.Domain.ID(), nil)
	if err != nil {
		return domain.Actor{}, err
	}
	if err := s.Auth.GrantSlots(ctx, ns, name, []authz.SlotBinding{{ResourceType: ResourceType, ResourceID: id, Permission: "write_memory", Occupancy: authz.SlotOccupancyMulti}}, authz.SlotGrantExpiry(time.Now(), 0)); err != nil {
		return domain.Actor{}, fmt.Errorf("%w: group goal scope binding failed: %v", domain.ErrDenied, err)
	}
	rels, err := groupDomainRelations(a.Domain)
	if err != nil {
		return domain.Actor{}, err
	}
	if err := s.Auth.Relations().WriteRelationships(ctx, rels); err != nil {
		return domain.Actor{}, err
	}
	return a, nil
}

// groupDomainRelations are the domain's member_group and class tuples.
func groupDomainRelations(d domain.Domain) ([]authz.Relation, error) {
	typ, id, rel, ok := d.OwnerSubjectSet()
	if !ok {
		return nil, fmt.Errorf("%w: malformed group owner", domain.ErrInvalid)
	}
	return []authz.Relation{
		{ResourceType: ResourceType, ResourceID: d.ID(), Relation: "member_group", SubjectType: typ, SubjectID: id, SubjectRel: rel},
		{ResourceType: ResourceType, ResourceID: d.ID(), Relation: "class", SubjectType: "agentclass", SubjectID: d.Namespace + "/" + d.Class},
	}, nil
}

// syncGroupTuples reconciles the tuples of g's domain and of g itself with
// the goal's CURRENT record, after every group write: it touches the domain's
// member_group and class and agent_goal:<id>#domain, writes a manager tuple on
// the goal for each listed manager that lacks one, and deletes every manager
// tuple on the goal whose user is no longer listed. Managers are per goal
// (agent_goal), so a manager of one goal holds nothing on another.
//
// It re-reads the goal rather than trusting g, because a retry replays the
// receipt's snapshot. A failed delete, a replayed old request, or two changes
// racing therefore converge at the next write, and AuthorizeGoalManager's
// list check means a lingering tuple never grants a write meanwhile.
func (s *Server) syncGroupTuples(ctx context.Context, g domain.Goal) error {
	current, err := s.Service.Store.Get(ctx, g.Domain, g.ID)
	if err != nil {
		return fmt.Errorf("goal %s saved, but re-reading it to record its managers failed; retry the same request: %w", g.ID, err)
	}
	have, err := s.Auth.ListUserSubjects(ctx, GoalResourceType, current.ID, "manager")
	if err != nil {
		return fmt.Errorf("goal %s saved, but reading its manager tuples failed; retry the same request: %w", g.ID, err)
	}
	manager := func(user string) authz.Relation {
		return authz.Relation{ResourceType: GoalResourceType, ResourceID: current.ID, Relation: "manager", SubjectType: "user", SubjectID: user}
	}
	rels, err := groupDomainRelations(current.Domain)
	if err != nil {
		return err
	}
	rels = append(rels, authz.Relation{ResourceType: GoalResourceType, ResourceID: current.ID, Relation: "domain", SubjectType: ResourceType, SubjectID: current.Domain.ID()})
	for _, m := range current.Managers {
		if !slices.Contains(have, m) {
			rels = append(rels, manager(m))
		}
	}
	if err := s.Auth.Relations().WriteRelationships(ctx, rels); err != nil {
		return fmt.Errorf("goal %s saved, but recording its managers failed; retry the same request: %w", g.ID, err)
	}
	var stale []authz.Relation
	for _, m := range have {
		if !slices.Contains(current.Managers, m) {
			stale = append(stale, manager(m))
		}
	}
	if len(stale) > 0 {
		if err := s.Auth.Relations().DeleteRelationships(ctx, stale); err != nil {
			return fmt.Errorf("goal %s saved, but removing a former manager's access failed; retry the same request: %w", g.ID, err)
		}
	}
	return nil
}

// reauthorizeGroup re-derives the actor for the same turn and requires it to
// be unchanged, as Authorize does for private scope.
func (s *Server) reauthorizeGroup(ctx context.Context, a domain.Actor) error {
	ns, name, ok := strings.Cut(a.Session, "/")
	if !ok || a.User == "" {
		return domain.ErrDenied
	}
	current, err := s.groupActor(ctx, ns, name, a.TurnRef)
	if err != nil {
		return err
	}
	if current != a {
		return domain.ErrDenied
	}
	return nil
}

func actingUser(a domain.Actor) identity.CanonicalUserID {
	return identity.CanonicalFromTrusted(a.User, "channelsd goal actor for this turn")
}

// checkDomain checks permission on the group domain for the acting member.
func (s *Server) checkDomain(ctx context.Context, a domain.Actor, permission string) (bool, error) {
	return s.Auth.CheckOnResource(ctx, ResourceType, a.Domain.ID(), permission, actingUser(a), true)
}

// isMember checks user against the domain's owning member subject-set
// directly — not through the domain, whose tuples may not exist yet.
func (s *Server) isMember(ctx context.Context, d domain.Domain, user string) (bool, error) {
	typ, id, rel, ok := d.OwnerSubjectSet()
	if !ok {
		return false, fmt.Errorf("%w: malformed group owner", domain.ErrInvalid)
	}
	return s.Auth.CheckOnResource(ctx, typ, id, rel, identity.CanonicalFromTrusted(user, "goal manager"), true)
}

// notMember says who is not a member without naming them: who is "you" or
// "the new manager".
func notMember(who string) error {
	return fmt.Errorf("%w: %s not a member of this conversation's group (membership is as of its last sync)", domain.ErrDenied, who)
}

// authorizeGroupWrite is Authorize for a group domain: view_memory to read,
// manage to write, and for writes the same source checks a private write runs.
func (s *Server) authorizeGroupWrite(ctx context.Context, a domain.Actor, write bool) error {
	if err := s.reauthorizeGroup(ctx, a); err != nil {
		return err
	}
	// The domain answers "is this person in the group (or an admin)". Who
	// manages is per goal: the Service runs AuthorizeGoalManager against the
	// goal itself for every write on an existing goal.
	ok, err := s.checkDomain(ctx, a, "view_memory")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: you cannot see this group's goals", domain.ErrDenied)
	}
	if write {
		_, err := s.Sources(ctx, a)
		return err
	}
	return nil
}

// AuthorizeGroup implements domain.GroupAuthorizer for the checks a group
// domain has beyond read and manage.
func (s *Server) AuthorizeGroup(ctx context.Context, a domain.Actor, access domain.GroupAccess) error {
	if err := s.reauthorizeGroup(ctx, a); err != nil {
		return err
	}
	switch access {
	case domain.GroupMember:
		ok, err := s.isMember(ctx, a.Domain, a.User)
		if err != nil {
			return err
		}
		if !ok {
			return notMember("you are")
		}
		return nil
	case domain.GroupRelinquish:
		// Membership here; the goal's own manage is AuthorizeGoalManager's.
		ok, err := s.checkDomain(ctx, a, "view_memory")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: you cannot see this group's goals", domain.ErrDenied)
		}
		return nil
	case domain.GroupPause:
		ok, err := s.checkDomain(ctx, a, "pause")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: only a member of this conversation's group can pause its goals", domain.ErrDenied)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown group access %q", domain.ErrDenied, access)
}

// AuthorizeGoalManager implements domain.GroupAuthorizer: the actor must hold
// manage on agent_goal:<id> — a listed manager still in the group, or a
// platform admin through the domain's class — AND, unless an admin, be listed
// in g.Managers. The list check keeps a manager tuple that outlived its
// removal (a failed delete, a race) from granting anything on its own.
func (s *Server) AuthorizeGoalManager(ctx context.Context, a domain.Actor, g domain.Goal) error {
	refused := fmt.Errorf("%w: only a manager of this group goal can do that (any member may pause it)", domain.ErrDenied)
	ok, err := s.Auth.CheckOnResource(ctx, GoalResourceType, g.ID, "manage", actingUser(a), true)
	if err != nil {
		return err
	}
	if !ok {
		return refused
	}
	if a.User != "" && slices.Contains(g.Managers, a.User) {
		return nil
	}
	admin, err := s.Auth.CheckOnResource(ctx, "agentclass", g.Domain.Namespace+"/"+g.Domain.Class, "administer", actingUser(a), true)
	if err != nil {
		return err
	}
	if !admin {
		return refused
	}
	return nil
}

// CheckManagers implements domain.GroupAuthorizer. Manager status lives in
// Goal.Managers and is only effective while the manager is still in the group
// (the schema intersects manager with member_group), so both rules are live
// membership checks: an added manager must be a member now, and a removal
// must leave someone listed who still is. Platform admins hold manage too, but
// they are not the group's managers and do not count.
func (s *Server) CheckManagers(ctx context.Context, a domain.Actor, g domain.Goal, c domain.Change) error {
	if c.Manager == nil {
		return fmt.Errorf("%w: manager change names no manager", domain.ErrInvalid)
	}
	switch c.Action {
	case "add_manager":
		ok, err := s.isMember(ctx, g.Domain, *c.Manager)
		if err != nil {
			return err
		}
		if !ok {
			return notMember("the new manager is")
		}
		return nil
	case "remove_manager":
		for _, m := range g.Managers {
			ok, err := s.isMember(ctx, g.Domain, m)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		return fmt.Errorf("%w: %s", domain.ErrInvalid, domain.LastManagerRefusal)
	}
	return fmt.Errorf("%w: not a manager change", domain.ErrInvalid)
}

// readGroupGoal is ReadGoal for a group domain: the acting member must be able
// to read every source. The domain owner is a subject-set, never a user, so it
// is never checked as one.
func (s *Server) readGroupGoal(ctx context.Context, a domain.Actor, g domain.Goal) error {
	if a.User == "" {
		return fmt.Errorf("%w: reading a group goal needs an acting member", domain.ErrDenied)
	}
	for _, src := range g.Sources {
		ok, err := s.Auth.CheckOnResource(ctx, src.ResourceType, src.ResourceID, src.Permission, actingUser(a), true)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: you cannot read a %s this goal depends on", domain.ErrDenied, src.ResourceType)
		}
	}
	return nil
}

// audienceCanRead requires every person who can read the conversation to be
// able to read every source a group write would carry, using the kind's own
// AudienceResolver. An audience that cannot be fully enumerated is refused:
// a partial check would pass exactly the case it exists to catch.
func (s *Server) audienceCanRead(ctx context.Context, a domain.Actor, sources []domain.Source) error {
	if len(sources) == 0 {
		return nil
	}
	ns, name, ok := strings.Cut(a.Session, "/")
	if !ok {
		return domain.ErrDenied
	}
	var sess v1.AgentSession
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return fmt.Errorf("%w: session lookup: %v", domain.ErrDenied, err)
	}
	b, ch, err := s.groupConversation(ctx, &sess)
	if err != nil {
		return err
	}
	kind, _ := registry.Get(ch.Spec.Kind)
	resolver, ok := kind.(channelkinds.AudienceResolver)
	if !ok {
		return fmt.Errorf("%w: channel kind %q cannot list who reads its conversations, which group goals need", domain.ErrDenied, ch.Spec.Kind)
	}
	unresolved := func(why string) error {
		return fmt.Errorf("%w: this conversation's audience %s, so the sources this goal would carry cannot be checked against it", domain.ErrDenied, why)
	}
	audience, err := resolver.ResolveAudience(ctx, channelkinds.SessionInfo{Namespace: ns, Name: name, SessionInitiator: actingUser(a), Channel: b, Annotations: sess.Annotations})
	if err != nil {
		log.FromContext(ctx).Info("group goal audience resolution failed", "session", a.Session, "kind", ch.Spec.Kind, "error", err)
		return unresolved("could not be resolved")
	}
	if len(audience) == 0 {
		return unresolved("could not be resolved")
	}
	if len(audience) > maxGroupAudience {
		return unresolved(fmt.Sprintf("is too large (%d people, at most %d)", len(audience), maxGroupAudience))
	}
	for _, subject := range audience {
		user := strings.TrimPrefix(subject, "user:")
		if user == "" || strings.ContainsAny(user, ":#*") {
			return unresolved("is not a list of individual people")
		}
		for _, src := range sources {
			ok, err := s.Auth.CheckOnResource(ctx, src.ResourceType, src.ResourceID, src.Permission, identity.CanonicalFromTrusted(user, "conversation audience"), true)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: someone who can read this conversation cannot read a %s this session has used, so it cannot become part of a group goal; keep this goal private, or use a fresh session", domain.ErrDenied, src.ResourceType)
			}
		}
	}
	return nil
}

// serveGroup handles a group-scope request. Execution and discovery for group
// goals are not served yet; reads, create, update and manager changes are.
func (s *Server) serveGroup(ctx context.Context, w http.ResponseWriter, r *http.Request, ns, name string, req Request) {
	a, err := s.resolveGroup(ctx, ns, name, req.TurnRef)
	if err != nil {
		s.failGroup(w, r, err)
		return
	}
	resource := ResourceType + ":" + a.Domain.ID()
	switch req.Operation {
	case "create", "update", "request_execution":
		if req.Resource != resource {
			s.failGroup(w, r, fmt.Errorf("%w: resource is not this conversation's group goals; call list_goals with scope group for it", domain.ErrDenied))
			return
		}
	}
	out := Response{Resource: resource}
	switch req.Operation {
	case "create":
		var g domain.Goal
		g, err = s.Service.Create(ctx, a, req.Create)
		if err == nil {
			err = s.syncGroupTuples(ctx, g)
		}
		out.Goal = &g
	case "update":
		var g domain.Goal
		g, err = s.Service.Update(ctx, a, req.Change)
		if err == nil {
			err = s.syncGroupTuples(ctx, g)
		}
		out.Goal = &g
	case "get":
		var g domain.Goal
		g, err = s.Service.Get(ctx, a, req.ID)
		out.Goal = &g
	case "list":
		var p domain.Page
		p, err = s.Service.List(ctx, a, req.List)
		out.Page = &p
	case "runs":
		var p domain.RunPage
		p, err = s.Service.Runs(ctx, a, req.ID, req.List)
		out.Runs = &p
	case "request_execution":
		// The source-vs-audience check runs now, so a request that could
		// never be approved says why; running group goals is a later step.
		if err = s.Authorize(ctx, a, true); err == nil {
			err = fmt.Errorf("%w: running group goals is not available yet", domain.ErrDenied)
		}
	case "request_discovery", "stop_discovery", "get_discovery_policy", "get_discovery_proposal":
		err = fmt.Errorf("%w: goal discovery is for private goals only; omit scope group", domain.ErrInvalid)
	default:
		err = domain.ErrInvalid
	}
	if err != nil {
		s.failGroup(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.FromContext(ctx).Info("group goal response failed", "session", a.Session, "error", err)
	}
}

// failGroup surfaces a group refusal's reason to the caller. Private scope
// keeps fail's uniform 404, which avoids confirming whose goals exist; in a
// group the reasons (not a member, not a manager, someone else spoke this
// turn, a source the room cannot read) are what the person needs to act on,
// and none names anyone else.
func (s *Server) failGroup(w http.ResponseWriter, r *http.Request, err error) {
	code := 0
	switch {
	case errors.Is(err, domain.ErrDenied), errors.Is(err, sessionevents.ErrDenied):
		code = http.StatusForbidden
	case errors.Is(err, domain.ErrNotFound):
		code = http.StatusNotFound
	}
	if code == 0 {
		s.fail(w, r, err)
		return
	}
	log.FromContext(r.Context()).Info("group goal operation refused", "path", r.URL.Path, "status", code, "error", err)
	http.Error(w, err.Error(), code)
}
