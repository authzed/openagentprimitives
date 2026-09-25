// Package spicedb wraps the SpiceDB Go client with the channels-specific
// operations (TouchStartedBy, CheckInteract, ...). All identities are
// expressed at the canonical user level (`user:<canonicalID>` produced by
// identity.Principal.Canonical()); platform-specific principals like Slack
// user IDs are converted to canonical IDs at the channelsd layer before
// any SpiceDB call. There is intentionally no platform-indirection
// definition in the schema — every check goes through SpiceDB at the
// user level so divergence between write-shape and check-shape can't
// hide.
package spicedb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/authzed/grpcutil"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	insecuregrpc "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
)

// Client wraps the SpiceDB Go client. Used by channelsd's inbound pipeline.
type Client struct {
	cl *authzed.Client
}

func NewClient(endpoint, token string, insecure bool) (*Client, error) {
	opts := []grpc.DialOption{}
	if insecure {
		// Bearer token over plaintext, plus the explicit insecure transport
		// credentials gRPC now requires (NewClient refuses to dial without
		// either TLS or an explicit insecure-creds DialOption).
		opts = append(opts,
			grpcutil.WithInsecureBearerToken(token),
			grpc.WithTransportCredentials(insecuregrpc.NewCredentials()),
		)
	} else {
		// Bearer token over TLS; default system roots are fine for Authzed
		// Cloud / any well-known SpiceDB endpoint.
		opts = append(opts,
			grpcutil.WithBearerToken(token),
			grpc.WithTransportCredentials(credentials.NewTLS(nil)),
		)
	}
	c, err := authzed.NewClient(endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial spicedb: %w", err)
	}
	return &Client{cl: c}, nil
}

func (c *Client) Close() error { return c.cl.Close() }

// TouchStartedBy writes agentsession:<ns>/<name>#started_by@user:<canonicalID>.
// started_by records who initiated the session. It confers exactly ONE
// permission — #interact — so a session can never lock out its own starter
// while #owner is unwritten (this write happens at session-create time; #owner
// is written later by the operator's owner resolver via TouchOwner). Every
// other gate (manage_scope/fork/approve) resolves through #owner alone, so a
// channel whose owner policy names someone else keeps the starter out of the
// approver seat. Idempotent (TOUCH).
func (c *Client) TouchStartedBy(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	sess := &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name}
	user := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}}
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{
			{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: &v1.Relationship{Resource: sess, Relation: "started_by", Subject: user}},
		},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession#started_by: %w", err)
	}
	return nil
}

// TouchOwner writes agentsession:<ns>/<name>#owner@<subjectRef>. subjectRef is
// "objType:objId" or "objType:objId#relation" (e.g. "user:abc",
// "group:eng#member", "slack_channel:C9#member"). The owner relation is the
// single authz subject every gate (interact/manage_scope/fork/approve) resolves
// through. Idempotent (TOUCH).
func (c *Client) TouchOwner(ctx context.Context, ns, name, subjectRef string) error {
	objType, objID, rel, err := parseSubjectRef(subjectRef)
	if err != nil {
		return fmt.Errorf("touch agentsession#owner: %w", err)
	}
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: objType, ObjectId: objID}}
	if rel != "" {
		subj.OptionalRelation = rel
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: "owner",
				Subject:  subj,
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession#owner@%s: %w", subjectRef, err)
	}
	return nil
}

// parseSubjectRef splits "objType:objId[#relation]" into parts. Unlike
// ParseSubject, the #relation segment is optional — this handles both plain
// user refs ("user:abc") and subject-set refs ("group:eng#member").
func parseSubjectRef(s string) (objType, objID, relation string, err error) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return "", "", "", fmt.Errorf("invalid subject ref %q (want objType:objId[#relation])", s)
	}
	objType = s[:colon]
	rest := s[colon+1:]
	if hash := strings.IndexByte(rest, '#'); hash >= 0 {
		objID, relation = rest[:hash], rest[hash+1:]
	} else {
		objID = rest
	}
	if objID == "" {
		return "", "", "", fmt.Errorf("invalid subject ref %q (empty objId)", s)
	}
	return objType, objID, relation, nil
}

// consistencyFor maps the fullyConsistent opt-in the exported Check* helpers
// carry onto a SpiceDB Consistency: MinimizeLatency by default, FullyConsistent
// when the caller knows the tuples it depends on may be a very recent write.
// Callers that must never be able to opt out (CheckAgentIdentityUpdateCredential)
// do not take the flag at all — they pass FullyConsistent here directly.
func consistencyFor(fullyConsistent bool) *v1.Consistency {
	if fullyConsistent {
		return &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}
	}
	return &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}}
}

// check issues one CheckPermission and reduces the response to a bool. It is
// the SINGLE place this package asks SpiceDB a permission question, so anything
// that must apply to every check — a freshness floor from a ZedToken cache, a
// deny log line, a retry, a metric — has exactly one site to change instead of
// nine that a compiler cannot keep in step. subject is the caller's, because
// the subject type is not always `user` (CheckUseToken asks as an
// externaltoken). errCtx names the check in the wrapped error.
func (c *Client) check(ctx context.Context, req *v1.CheckPermissionRequest, errCtx string) (bool, error) {
	resp, err := c.cl.CheckPermission(ctx, req)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", errCtx, err)
	}
	return resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
}

// checkUser answers <resType>:<resID>#<permission>@user:<canonicalID> — the
// shape every identity-level gate in this package takes.
// checkUser builds a user:<canonicalID> subject deliberately: a canonical
// identity is a USER, and forcing the "user:" type is what makes a resource
// reference smuggled in as a canonical (e.g. "agentsession:ns/name") land as
// the object id "agentsession:ns/name" — which SpiceDB rejects outright, ':'
// being illegal in an object id. That hard rejection is a fail-closed guard
// (see TestInteract_RejectsASessionReferenceSmuggledInAsAUserID); honoring the
// embedded type instead would let a smuggled agentsession:/group: subject match
// a relation it was never meant to. A service-triggered inbound never reaches a
// user-typed check anyway — channelsd dispatches it on the channel-as-boundary
// path before the check (pipeline.go inherit-fork), which is where that concern
// belongs.
func (c *Client) checkUser(ctx context.Context, resType, resID, permission string, canonicalID identity.CanonicalUserID, cons *v1.Consistency, errCtx string) (bool, error) {
	return c.check(ctx, &v1.CheckPermissionRequest{
		Resource:    &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
		Permission:  permission,
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
		Consistency: cons,
	}, errCtx)
}

// CheckInteract checks agentsession:<ns>/<name>#interact@user:<canonicalID>.
//
// All identity in the SpiceDB schema lives at the canonical user level:
// channelsd resolves the canonical via identity.Principal.Canonical() at the
// pipeline boundary and passes it here. Class-wide grants like
// `participant@group:engineering#member` continue to work because SpiceDB
// walks the group's members on the RELATION side and matches against our
// user:<canonical> subject — that walk is server-side and does not require
// any platform-specific indirection in the schema.
//
// fullyConsistent=true should be used right after a write; otherwise
// MinimizeLatency is fine. Not cached — checks must reflect current state.
func (c *Client) CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "interact", canonicalID, consistencyFor(fullyConsistent), "interact")
}

// CheckManageScope checks agentsession:<ns>/<name>#manage_scope@user:<canonicalID>.
//
// The metaagent control-plane gate (MetaagentReceived). manage_scope resolves
// via agentsession#owner (written by the operator owner resolver), so this
// answers "is the session owner?". Modeled byte-for-byte on CheckInteract but
// with Permission: "manage_scope". canonicalID is the bare canonical (no "user:"
// prefix); the caller strips the type prefix before calling.
//
// fullyConsistent should always be true here: a freshly-started session's
// owner tuple may be a recent write, so a MinimizeLatency read could miss
// it and deny the owner. Not cached — checks must reflect current state.
func (c *Client) CheckManageScope(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "manage_scope", canonicalID, consistencyFor(fullyConsistent), "manage_scope")
}

// CheckFork answers agentsession#fork for user:<canonicalID>. Mirrors
// CheckManageScope (permission "fork" = owner). Callers MUST bind
// fullyConsistent=true: a freshly-set owner tuple may be a recent write.
func (c *Client) CheckFork(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "fork", canonicalID, consistencyFor(fullyConsistent), "fork")
}

// delegationProbeSubject is the user object id CheckDelegationBlocked probes
// with. It is immaterial: delegation_revoked is the WILDCARD user:*, so the
// permission resolves the same for any concrete user, and only the wildcard can
// be written (a specific @user:x tuple is refused by the relation type). It just
// has to be a valid object id.
const delegationProbeSubject = "delegation-revocation-probe"

// CheckDelegationBlocked answers agentsession:<ns>/<name>#delegation_blocked —
// the operator-side kill switch for a session's delegation. true means an
// operator has written agentsession:<X>#delegation_revoked@user:* to halt this
// session spawning children, and the SubagentRequest controller then refuses the
// spawn. Narrower than a `hold`: the session keeps running, it just cannot
// delegate.
//
// FullyConsistent, always: a revoke written moments before a delegate must be
// seen, and a stale read would honour a delegation an operator just killed —
// the one direction a kill switch must never fail.
func (c *Client) CheckDelegationBlocked(ctx context.Context, ns, name string) (bool, error) {
	return c.check(ctx, &v1.CheckPermissionRequest{
		Resource:    &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
		Permission:  "delegation_blocked",
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: delegationProbeSubject}},
		Consistency: consistencyFor(true),
	}, "delegation_blocked")
}

// CheckApprove answers agentsession:<ns>/<name>#approve for user:<canonicalID>
// (approve = owner today). This is the approver-side gate for human approvals.
// Bind fullyConsistent=true after a recent owner write. Not cached.
func (c *Client) CheckApprove(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "approve", canonicalID, consistencyFor(fullyConsistent), "approve")
}

// CheckOwnerOnResource answers <resType>:<resID>#owner for user:<canonicalID> —
// the per-resource source-permission half of approver authorization. Bind
// fullyConsistent=true after recent owner writes. Not cached.
func (c *Client) CheckOwnerOnResource(ctx context.Context, resType, resID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.CheckOnResource(ctx, resType, resID, "owner", canonicalID, fullyConsistent)
}

// CheckOnResource answers <resType>:<resID>#<permission> for user:<canonicalID>.
//
// The general form of CheckOwnerOnResource. A slot's permission is named by the
// AgentClass rather than fixed by this package, so the instance axis cannot use
// the owner-pinned variant. Not cached.
func (c *Client) CheckOnResource(ctx context.Context, resType, resID, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, resType, resID, permission, canonicalID, consistencyFor(fullyConsistent),
		fmt.Sprintf("%s:%s#%s", resType, resID, permission))
}

// HasAnyOfType reports whether the subject holds `permission` on AT LEAST ONE
// resource of `resType`.
//
// This answers the plan gate's approver question for a SLOT. A phase requests
// slots by TYPE (`plangate.Slot{Type}`) — the instances arrive later from the
// class's fill sources — so at plan-approval time there is no instance for a
// Check to name, and standing on the type is the only question available.
//
// "At least one" is deliberate, and safe for a structural reason rather than a
// hopeful one: clearing a slot request grants no instance any access by itself.
// The instance axis is the tool's ordinary PermissionCheck at
// OrderToolCallAuthz (20), which runs AFTER the plan gate at 19 — so an
// instance this approver never held is still refused there, and escalates to
// its real owner through the JIT path (pkg/channelsd/pipeline's tool-approval
// decision, which writes the slot grant). The split exists to tell an approver
// which requests they can speak for, not to be the thing that stops them
// speaking for others.
//
// Bounded to one result: the caller only needs emptiness, and enumerating a
// large type to answer a boolean would make card rendering scale with the
// tenant's data.
func (c *Client) HasAnyOfType(
	ctx context.Context, resType, permission string,
	canonicalID identity.CanonicalUserID, fullyConsistent bool,
) (bool, error) {
	consistency := &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}}
	if fullyConsistent {
		consistency = &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}
	}
	stream, err := c.cl.LookupResources(ctx, &v1.LookupResourcesRequest{
		ResourceObjectType: resType,
		Permission:         permission,
		Subject:            &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
		Consistency:        consistency,
		OptionalLimit:      1,
	})
	if err != nil {
		return false, fmt.Errorf("lookup %s#%s for user:%s: %w", resType, permission, canonicalID, err)
	}
	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			return false, nil
		}
		if rerr != nil {
			// Never collapsed into "no standing": that would silently hold every
			// slot back and read as a deliberate policy decision rather than the
			// lookup failure it is.
			return false, fmt.Errorf("lookup %s#%s for user:%s: %w", resType, permission, canonicalID, rerr)
		}
		if resp.GetPermissionship() == v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION {
			return true, nil
		}
	}
}

// AddGroupMember writes group:<groupName>#member@user:<canonicalID>.
// Idempotent (TOUCH). Used by integration tests and by `oap session
// grant` to populate group memberships referenced by AgentClass
// class-wide grants.
func (c *Client) AddGroupMember(ctx context.Context, groupName string, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "group", ObjectId: groupName},
				Relation: "member",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("add group:%s#member user:%s: %w", groupName, canonicalID, err)
	}
	return nil
}

// DeleteGroupMember removes a single user from a group's member set.
// Idempotent — succeeds if the membership doesn't exist.
func (c *Client) DeleteGroupMember(ctx context.Context, groupName string, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "group",
			OptionalResourceId: groupName,
			OptionalRelation:   "member",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: canonicalID.String(),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("delete group:%s#member user:%s: %w", groupName, canonicalID, err)
	}
	return nil
}

// Ping is a one-shot connectivity probe. Used at startup.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cl.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	return err
}

// ReadSchema delegates to the underlying authzed client so that *Client
// satisfies the spicedb.SchemaReader interface. This allows the operator
// to pass the same *Client to the AgentClass reconciler for per-tool
// schema validation without creating a separate connection.
func (c *Client) ReadSchema(ctx context.Context, in *v1.ReadSchemaRequest) (*v1.ReadSchemaResponse, error) {
	return c.cl.ReadSchema(ctx, in)
}

// CheckPermission delegates to the underlying authzed client. This makes
// *Client satisfy the toolcheck.Client interface so the same connection can be
// used for both agentsession-level checks (CheckInteract, CheckDenied) and
// per-tool permission checks (toolcheck.Checker.CheckToolCall).
func (c *Client) CheckPermission(ctx context.Context, in *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	return c.cl.CheckPermission(ctx, in)
}

// ReadRelationships delegates to the underlying authzed client. Unguarded —
// relsource only gates writes (see pkg/authz/spicedb/relsource) — and exposed
// directly on *Client, alongside ReadSchema and CheckPermission above, so a
// caller that only needs to read never has to go through Writer for a
// relsource.Source it doesn't otherwise need.
func (c *Client) ReadRelationships(ctx context.Context, in *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error) {
	return c.cl.ReadRelationships(ctx, in)
}

// SplitObject parses "<type>:<id>" into its two parts. Returns a
// descriptive error if the separator is missing or either segment is
// empty. The single shared splitter used by both ParseSubject (for the
// type:id portion of a subject expression) and external callers that
// need to break apart a bare object reference.
func SplitObject(s string) (objType, objID string, err error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("object %q: must be of the form type:id", s)
	}
	return s[:i], s[i+1:], nil
}

// ParseSubject splits a "<type>:<id>#<relation>" expression into its
// three parts. Returns an error for malformed input. The #<relation>
// segment is required.
func ParseSubject(s string) (objType, objID, relation string, err error) {
	hashIdx := strings.Index(s, "#")
	if hashIdx <= 0 {
		return "", "", "", fmt.Errorf("subject %q: must be of the form type:id#relation", s)
	}
	objType, objID, err = SplitObject(s[:hashIdx])
	if err != nil {
		return "", "", "", fmt.Errorf("subject %q: missing type:id (colon not found before #)", s)
	}
	relation = s[hashIdx+1:]
	if relation == "" {
		return "", "", "", fmt.Errorf("subject %q: empty relation", s)
	}
	return objType, objID, relation, nil
}

// TouchInteractParticipantUser writes
//
//	agentsession:<ns>/<name>#participant@user:<canonicalID>
//
// for per-user grants from the approve flow. Distinct from
// TouchInteractParticipant, which takes a subject-set expression
// (e.g. "group:engineering#member"). Idempotent (TOUCH).
func (c *Client) TouchInteractParticipantUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: "participant",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession#participant (per-user): %w", err)
	}
	return nil
}

// DeleteAgentSessionRelationships wipes every relationship whose
// resource is agentsession:<ns>/<name>. Used by the AgentSession
// finalizer; idempotent (succeeds even if no relationships exist).
func (c *Client) DeleteAgentSessionRelationships(ctx context.Context, ns, name string) error {
	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: ns + "/" + name,
		},
	})
	if err != nil {
		return fmt.Errorf("delete agentsession relationships: %w", err)
	}
	return nil
}

// TouchInteractParticipant writes
//
//	agentsession:<ns>/<name>#participant@<subject>
//
// where subject is parsed from a "type:id#relation" expression.
// Idempotent (TOUCH).
func (c *Client) TouchInteractParticipant(ctx context.Context, ns, name, subject string) error {
	objType, objID, relation, err := ParseSubject(subject)
	if err != nil {
		return fmt.Errorf("touch participant: %w", err)
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: "participant",
				Subject: &v1.SubjectReference{
					Object:           &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
					OptionalRelation: relation,
				},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession#participant: %w", err)
	}
	return nil
}

// DeleteInteractParticipant removes a specific participant relationship from
// agentsession:<ns>/<name>#participant. subject may be either a subject-set
// ("type:id#relation") or a direct user reference ("user:<canonicalID>").
func (c *Client) DeleteInteractParticipant(ctx context.Context, ns, name, subject string) error {
	var subjectFilter *v1.SubjectFilter

	// Try subject-set form first.
	if objType, objID, relation, err := ParseSubject(subject); err == nil {
		subjectFilter = &v1.SubjectFilter{
			SubjectType:       objType,
			OptionalSubjectId: objID,
			OptionalRelation:  &v1.SubjectFilter_RelationFilter{Relation: relation},
		}
	} else if strings.HasPrefix(subject, "user:") {
		canonicalID := strings.TrimPrefix(subject, "user:")
		subjectFilter = &v1.SubjectFilter{
			SubjectType:       "user",
			OptionalSubjectId: canonicalID,
		}
	} else {
		return fmt.Errorf("subject %q: must be \"<type>:<id>#<relation>\" or \"user:<canonicalID>\"", subject)
	}

	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:          "agentsession",
			OptionalResourceId:    ns + "/" + name,
			OptionalRelation:      "participant",
			OptionalSubjectFilter: subjectFilter,
		},
	})
	if err != nil {
		return fmt.Errorf("delete agentsession#participant: %w", err)
	}
	return nil
}

// TouchDeniedUser writes
//
//	agentsession:<ns>/<name>#denied@user:<canonicalID>
//
// Used when the original requester clicks Deny on a permission_request DM.
// Subsequent inbounds from the canonical user are checked against #is_denied
// and silently dropped (no follow-up DM). Idempotent (TOUCH).
// TouchDenied writes agentsession#denied for an arbitrary SUBJECT, including a
// subject-set reference such as "group:eng#member".
//
// The user-only variant below cannot rescind a group grant: with
// slack_channel#member admissible on owner and participant, one tuple can mean
// an entire channel, and denying it member-by-member never finishes for a
// channel that keeps growing. Denying the same subject set the grant used is
// the only revocation that actually terminates.
func (c *Client) TouchDenied(ctx context.Context, ns, name, subject string) error {
	objType, objID, relation, err := ParseSubject(subject)
	if err != nil {
		return fmt.Errorf("touch denied: %w", err)
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: "denied",
				Subject: &v1.SubjectReference{
					Object:           &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
					OptionalRelation: relation,
				},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession#denied: %w", err)
	}
	return nil
}

// DeleteDenied lifts a Deny written for an arbitrary subject, including a
// subject-set reference. The counterpart to TouchDenied — a Deny that could be
// written but not lifted would make the group blocklist a one-way door in the
// opposite direction.
func (c *Client) DeleteDenied(ctx context.Context, ns, name, subject string) error {
	objType, objID, relation, err := ParseSubject(subject)
	if err != nil {
		return fmt.Errorf("delete denied: %w", err)
	}
	filter := &v1.SubjectFilter{SubjectType: objType, OptionalSubjectId: objID}
	if relation != "" {
		filter.OptionalRelation = &v1.SubjectFilter_RelationFilter{Relation: relation}
	}
	_, err = c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:          "agentsession",
			OptionalResourceId:    ns + "/" + name,
			OptionalRelation:      "denied",
			OptionalSubjectFilter: filter,
		},
	})
	if err != nil {
		return fmt.Errorf("delete agentsession#denied: %w", err)
	}
	return nil
}

func (c *Client) TouchDeniedUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: "denied",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession#denied: %w", err)
	}
	return nil
}

// DeleteDeniedUser removes
//
//	agentsession:<ns>/<name>#denied@user:<canonicalID>
//
// i.e. it lifts a Deny. Without this, a Deny is permanent for the life of the
// session and a misclick has no remedy — the blocklist had two writers and no
// delete. Idempotent: deleting an absent tuple is not an error.
func (c *Client) DeleteDeniedUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: ns + "/" + name,
			OptionalRelation:   "denied",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: canonicalID.String(),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("delete agentsession#denied: %w", err)
	}
	return nil
}

// ListDeniedUsers returns the canonical IDs of every user directly on
// agentsession:<ns>/<name>#denied, fully-consistent. The returned IDs are
// raw canonicals (no "user:" prefix) so they feed straight into
// TouchDeniedUser. Used by the fork reconciler to carry a parent's denied
// blocklist onto its child — without it, a parent-denied user would regain
// interact on the child (whose interact derives view over the inherited
// transcript). Non-user subjects on the relation (should not occur) are
// skipped.
func (c *Client) ListDeniedUsers(ctx context.Context, ns, name string) ([]string, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: ns + "/" + name,
			OptionalRelation:   "denied",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("read agentsession#denied: %w", err)
	}
	var out []string
	for {
		resp, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read agentsession#denied recv: %w", rerr)
		}
		sub := resp.GetRelationship().GetSubject().GetObject()
		if sub.GetObjectType() != "user" {
			continue
		}
		out = append(out, sub.GetObjectId())
	}
	return out, nil
}

// CheckDenied checks agentsession:<ns>/<name>#is_denied@user:<canonicalID>.
// Pipeline resolves canonical from Facts before calling; same shape
// contract as CheckInteract.
func (c *Client) CheckDenied(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "is_denied", canonicalID, consistencyFor(fullyConsistent), "is_denied")
}

// TouchArtifactParent writes, in a single TOUCH WriteRelationships request, BOTH
//   - artifact:<artifactID>#parent@agentsession:<ns>/<name>, and
//   - artifact:<artifactID>#platform@platform:platform.
//
// Idempotent (TOUCH). The artifact's `view` permission is
// `parent->interact + parent->artifact_org_view + platform->view_audit`: the
// #parent tuple grants view to the session's started_by + participants (minus
// denied) — and, when the session carries the artifact_org_viewer opt-in
// (SyncArtifactOrgViewer), to any user subject; the #platform tuple
// ties the artifact to the singleton platform object so a platform admin
// (platform:platform#admin, whose view_audit aliases can_admin) may view ANY
// artifact. Writing both in one request keeps every artifact funnelled through
// a single call site — no artifact can be created with #parent but without
// #platform, which would silently exclude it from the admin live-view.
func (c *Client) TouchArtifactParent(ctx context.Context, artifactID, ns, name string) error {
	artifactRef := &v1.ObjectReference{ObjectType: "artifact", ObjectId: artifactID}
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{
			{
				Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &v1.Relationship{
					Resource: artifactRef,
					Relation: "parent",
					Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name}},
				},
			},
			{
				Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &v1.Relationship{
					Resource: artifactRef,
					Relation: "platform",
					Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "platform", ObjectId: platformObjectID}},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("touch artifact#parent + artifact#platform: %w", err)
	}
	return nil
}

// CheckArtifactView checks artifact:<artifactID>#view@user:<canonicalID>.
// fullyConsistent=true right after a write; otherwise MinimizeLatency.
func (c *Client) CheckArtifactView(ctx context.Context, artifactID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "artifact", artifactID, "view", canonicalID, consistencyFor(fullyConsistent), "artifact view")
}

// SyncArtifactOrgViewer levels agentsession:<ns>/<name>#artifact_org_viewer@user:*
// to enabled — the opt-in that widens every artifact of the session to any
// user subject via artifact#view = parent->artifact_org_view (webd's
// IdP-gated login is what scopes "any user" to the corp directory; interact,
// and with it the conversation mirrors, stays closed).
//
// Level-triggered and idempotent in both directions: TOUCH when enabled,
// filtered delete when not — deleting an absent tuple succeeds, which is the
// hot path, since every session whose class never opted in re-levels to
// "absent" on each reconcile. The subject is the wildcard because the widening
// is session-wide, not per-user; the relation admits only user:*.
func (c *Client) SyncArtifactOrgViewer(ctx context.Context, ns, name string, enabled bool) error {
	if !enabled {
		_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
			RelationshipFilter: &v1.RelationshipFilter{
				ResourceType:       "agentsession",
				OptionalResourceId: ns + "/" + name,
				OptionalRelation:   "artifact_org_viewer",
			},
		})
		if err != nil && !isSchemaDefinitionAbsent(err) {
			return fmt.Errorf("delete agentsession#artifact_org_viewer: %w", err)
		}
		return nil
	}
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: "artifact_org_viewer",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "*"}},
			},
		}},
	})
	if err != nil && !isSchemaDefinitionAbsent(err) {
		return fmt.Errorf("touch agentsession#artifact_org_viewer: %w", err)
	}
	return nil
}

// isSchemaDefinitionAbsent reports whether err is SpiceDB's "the schema has no
// such object definition" precondition. The agentsession definition is written
// to SpiceDB only when the guardian composes the schema (which a class becoming
// Valid drives), so a session whose class was never valid reconciles against an
// instance that has no schema at all. Because the org-viewer sync runs on every
// reconcile — above the class-valid gate, so terminal sessions level too — it
// meets that pre-first-compose state, and it is a genuine no-op there rather
// than an error: no schema means no session has run, so there are no artifacts
// to widen or revoke, and the AgentClass watch re-levels the tuple for real
// once the schema lands. Every other SpiceDB error (network, auth, a real
// FailedPrecondition on a populated schema) still fails the caller.
func isSchemaDefinitionAbsent(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return false
	}
	// SpiceDB phrases it "object definition `<type>` not found"; match on the
	// stable "definition" + "not found" pair rather than the exact backtick
	// framing so a message reword does not silently turn this back into a hard
	// error.
	msg := st.Message()
	return strings.Contains(msg, "definition") && strings.Contains(msg, "not found")
}

// LookupInteractSubjects returns the list of user IDs that have interact
// permission on agentsession:<ns>/<name> via SpiceDB's LookupSubjects API.
func (c *Client) LookupInteractSubjects(ctx context.Context, ns, name string) ([]string, error) {
	stream, err := c.cl.LookupSubjects(ctx, &v1.LookupSubjectsRequest{
		Resource:          &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
		Permission:        "interact",
		SubjectObjectType: "user",
	})
	if err != nil {
		return nil, fmt.Errorf("lookup interact subjects: %w", err)
	}
	var out []string
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("lookup interact subjects recv: %w", err)
		}
		out = append(out, "user:"+r.Subject.SubjectObjectId)
	}
	return out, nil
}

// directUserSubject reports whether s is a "user:<canonicalID>" direct
// subject (with optional empty "#" suffix). Direct subjects bypass the
// SpiceDB LookupSubjects round-trip because the canonical ID IS the
// answer — there's no subject-set to expand.
func directUserSubject(s string) (canonical string, ok bool) {
	if i := strings.Index(s, "#"); i >= 0 {
		if i != len(s)-1 {
			return "", false // non-empty relation → subject-set
		}
		s = s[:i]
	}
	const prefix = "user:"
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	canonical = s[len(prefix):]
	if canonical == "" {
		return "", false
	}
	return canonical, true
}

// LookupSubjectIncludes reports whether the user identified by canonicalID
// is in the resolved subject-set of `subjectRef` (e.g. "group:eng#member").
// Used by the tool-approval flow to verify a clicker is authorized to
// approve/deny under the request's ApproverSubject expression.
//
// For direct "user:<canonicalID>" subjects (no relation), this short-circuits
// without a SpiceDB round-trip: the canonical ID IS the answer.
// subjectRef with a non-empty relation must be of the form
// "<objType>:<objID>#<relation>"; the SpiceDB API is invoked with
// Resource=<objType>:<objID>, Permission=<relation>, SubjectObjectType="user".
func (c *Client) LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error) {
	if direct, ok := directUserSubject(subjectRef); ok {
		return direct == canonicalID.String(), nil
	}
	objType, objID, relation, err := ParseSubject(subjectRef)
	if err != nil {
		return false, fmt.Errorf("parse approver subject: %w", err)
	}
	stream, err := c.cl.LookupSubjects(ctx, &v1.LookupSubjectsRequest{
		Resource:          &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
		Permission:        relation,
		SubjectObjectType: "user",
		// FullyConsistent: this is an approval-gate decision read. A
		// just-written approver/participant tuple must be visible or a
		// valid approval is silently rejected — same stale-read failure
		// mode documented on the pipeline's post-write CheckInteract.
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	if err != nil {
		return false, fmt.Errorf("lookup subjects: %w", err)
	}
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("lookup subjects recv: %w", err)
		}
		if r.Subject != nil && r.Subject.SubjectObjectId == canonicalID.String() {
			return true, nil
		}
	}
}

// LookupSubjects expands the resolved subject-set referenced by subjectRef
// (form "<objType>:<objID>#<relation>") into a list of canonical user
// IDs. Used by approver fan-out (expanding the approver subject set into
// individual canonical IDs for per-approver ephemeral messages),
// info-leakage audience resolution (determining which subjects may receive
// tool-output data), and Slack audience resolution (resolving channel
// participants for delivery). The SpiceDB API is invoked with
// Resource=<objType>:<objID>, Permission=<relation>,
// SubjectObjectType="user"; the returned IDs are the raw
// SubjectObjectIds (already canonical "<canonicalID>" form, NOT
// "user:<canonicalID>" — callers that need the typed form re-prefix).
//
// For direct "user:<canonicalID>" subjects (no relation), this short-circuits
// without a SpiceDB round-trip: the single canonical ID is returned directly.
func (c *Client) LookupSubjects(ctx context.Context, subjectRef string) ([]string, error) {
	if direct, ok := directUserSubject(subjectRef); ok {
		return []string{direct}, nil
	}
	objType, objID, relation, err := ParseSubject(subjectRef)
	if err != nil {
		return nil, fmt.Errorf("parse approver subject: %w", err)
	}
	stream, err := c.cl.LookupSubjects(ctx, &v1.LookupSubjectsRequest{
		Resource:          &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
		Permission:        relation,
		SubjectObjectType: "user",
		// FullyConsistent: all current callers are security-gate reads —
		// approver fan-out and info-leakage audience resolution must see
		// relationships written moments earlier (a just-granted approver or
		// audience member must be immediately visible). Any future caller
		// that does not need a security gate and is willing to accept
		// potential staleness should not reuse this method as-is; add a
		// consistency parameter or a separate path instead, since
		// FullyConsistent bypasses SpiceDB's quantization-window caching
		// and resolves at head revision on every call.
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	if err != nil {
		return nil, fmt.Errorf("lookup subjects: %w", err)
	}
	var out []string
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("lookup subjects recv: %w", err)
		}
		if r.Subject != nil && r.Subject.SubjectObjectId != "" {
			out = append(out, r.Subject.SubjectObjectId)
		}
	}
}

// Writer returns a RelWriter bound to src: WriteRelationships and
// DeleteRelationships go through relsource.CheckWrite / CheckDeleteFilter
// before reaching SpiceDB, refusing a write or delete filter that touches a
// relation another Source claims. CheckBulkPermissions, LookupSubjects and
// ExpandPermissionTree pass straight through — see RelWriter's doc comment.
//
// Returns nil (a genuine nil RelWriter, not a typed-nil pointer boxed into
// the interface — see AGENTS.md "Nil interfaces: never assign a typed-nil
// pointer") for a nil *Client, so a caller assigning the result into an
// interface field gets a real nil rather than one that panics on first use.
func (c *Client) Writer(src relsource.Source) RelWriter {
	if c == nil {
		return nil
	}
	return NewWriter(c.cl, src)
}

// SchemaIO wraps *Client to satisfy the guardian schema package's
// SchemaIO interface (text-based read/write of the live SpiceDB
// schema). A separate adapter type is required because *Client already
// has a ReadSchema(ctx, *v1.ReadSchemaRequest) method shape (used by
// the AgentClass schema validator); adding the simpler text-typed
// method to *Client would collide on name. The constructor function
// SchemaIOFor takes a *Client and returns a value satisfying
// pkg/authz/guardian/schema.SchemaIO.
type SchemaIOAdapter struct{ c *Client }

// SchemaIOFor returns a SchemaIO adapter for the given client. Returns
// the zero value if c is nil — callers MUST guard with a nil check
// against the underlying *Client before wrapping (or call
// IsNil on the returned adapter), otherwise the resulting interface
// will be non-nil and panic on first call (see AGENTS.md "Nil
// interfaces: never assign a typed-nil pointer").
func SchemaIOFor(c *Client) SchemaIOAdapter { return SchemaIOAdapter{c: c} }

// IsNil reports whether this adapter wraps a nil *Client. Callers
// that constructed the adapter via SchemaIOFor(nil) MUST check this
// before passing the adapter to anything that holds the
// guardian-schema.SchemaIO interface — otherwise the typed-nil
// pointer becomes a non-nil interface and the first method call
// dereferences the nil *Client and panics.
//
// This method is the defensive guard for the AGENTS.md "Nil interfaces"
// pattern. The matching guard in internal/cmd/operator/main.go is the
// `spiceDBClient != nil` check BEFORE calling SchemaIOFor; IsNil is
// the belt-and-suspenders alternative for call sites that lose track
// of the original *Client.
func (a SchemaIOAdapter) IsNil() bool { return a.c == nil }

// ReadSchema returns the live SpiceDB schema text. Implements
// pkg/authz/guardian/schema.SchemaIO.
//
// SpiceDB v1.52+ strips the `use expiration` directive from the
// rendered text on ReadSchema, even when WriteSchema declared it.
// guardian/schema.Compose emits relation lines with
// `with check_hash and expiration` syntax that requires the directive
// to be live at compose time, so we re-prepend it when missing.
// Without this, the composer round-trips its own output into a schema
// SpiceDB then rejects on WriteSchema — a deploy-time-only failure
// invisible in unit tests that mock SchemaIO. The integration tests
// in pkg/authz/guardian wrap an equivalent adapter (`useExpirationTolerant`)
// for parity; this method is the production seam.
//
// Emits start/end log lines at INFO so the guardian-reconciler
// timing breadcrumb has SpiceDB-side latency to compare against.
// Without these, a slow / blocking ReadSchema looks identical to a
// fast one in the reconcile-exit summary and a hang in the schema
// composer is the silent-crash failure mode AGENTS.md warns about.
func (a SchemaIOAdapter) ReadSchema(ctx context.Context) (string, error) {
	// Use log.FromContext rather than ctrl.Log directly: the latter
	// goes through the controller-runtime delegating root logger,
	// which can be hijacked by other packages' init() functions
	// (notably spicedb's own internal/logging) and silently
	// discarded. The reconciler-supplied ctx carries the manager's
	// logger via controller-runtime's LogConstructor.
	logger := log.FromContext(ctx).WithName("spicedb.schemaio")
	started := time.Now()
	logger.Info("ReadSchema start")
	resp, err := a.c.cl.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if err != nil {
		logger.Info("ReadSchema error",
			"duration", time.Since(started).String(),
			"err", err.Error())
		return "", err
	}
	text := resp.GetSchemaText()
	if !strings.Contains(text, "use expiration") {
		text = "use expiration\n\n" + text
	}
	logger.Info("ReadSchema ok",
		"duration", time.Since(started).String(),
		"bytes", len(text))
	return text, nil
}

// WriteSchema writes text as the live SpiceDB schema. Implements
// pkg/authz/guardian/schema.SchemaIO. Logged at INFO with the byte count
// of the request body so an out-of-band WriteSchema spike (e.g. a
// runaway composer rewriting the same schema each reconcile) is
// visible in operator logs.
func (a SchemaIOAdapter) WriteSchema(ctx context.Context, text string) error {
	logger := log.FromContext(ctx).WithName("spicedb.schemaio")
	started := time.Now()
	logger.Info("WriteSchema start", "bytes", len(text))
	_, err := a.c.cl.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: text})
	if err != nil {
		logger.Info("WriteSchema error",
			"duration", time.Since(started).String(),
			"err", err.Error())
		return err
	}
	logger.Info("WriteSchema ok", "duration", time.Since(started).String())
	return nil
}

// platformObjectID is the singleton platform object's id. The admin UI's
// per-area permissions live on platform:platform.
const platformObjectID = "platform"

// CheckPlatformPermission checks platform:platform#<permission>@user:<canonicalID>.
// permission is one of the per-area names (view_sessions / view_audit /
// kill_session) — callers must NOT check can_admin directly (the per-area
// indirection is what lets the schema grow real per-area relations later).
// fullyConsistent=true right after a grant write; otherwise MinimizeLatency.
func (c *Client) CheckPlatformPermission(ctx context.Context, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "platform", platformObjectID, permission, canonicalID, consistencyFor(fullyConsistent),
		"platform "+permission)
}

// TouchPlatformAdmin writes platform:platform#admin@user:<canonicalID> (TOUCH —
// idempotent). Written by `oap platform grant-admin`.
func (c *Client) TouchPlatformAdmin(ctx context.Context, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "platform", ObjectId: platformObjectID},
				Relation: "admin",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch platform admin: %w", err)
	}
	return nil
}

// EnsureAgentIdentityPlatform writes
// agentidentity:<ns>/<name>#platform@platform:platform (TOUCH — idempotent, so
// it is safe on every reconcile; CREATE would error on the second pass).
//
// This ONE tuple is what makes agentidentity#update_credential — the gate on
// replacing an agent's own dead shared credential — satisfiable at all. The
// permission is `editor + platform->can_admin`; `editor` ships deliberately
// unpopulated, so platform->can_admin is the only live arm, and it resolves
// only for an agentidentity carrying this link to the singleton platform
// object. Omit the write and every check fails closed SILENTLY: the schema
// still compiles, the credential-update card still publishes, the button still
// renders, and every click is refused. Called by the AgentIdentity reconciler.
//
// CONSISTENCY — read this before writing a Check helper for update_credential.
// Every check of agentidentity#update_credential MUST be FullyConsistent. Do
// NOT copy the `fullyConsistent bool` opt-in shape of the neighbouring helpers
// (CheckDenied, CheckPlatformPermission): those default to MinimizeLatency,
// and a caller passing false here gets a SILENT refusal of a legitimate admin.
// The reason is the write ordering — the link is written by the very reconcile
// that surfaces the dead credential, so the admin's click can land inside
// SpiceDB's quantization window and read a snapshot from before this tuple
// existed. A stale read is indistinguishable from "not permitted", so the
// refusal looks exactly like the failure mode this tuple exists to prevent.
func (c *Client) EnsureAgentIdentityPlatform(ctx context.Context, ns, name string) error {
	// A name SpiceDB cannot express is a PERMANENT failure, not a blip: the
	// gRPC InvalidArgument it would otherwise return is indistinguishable from
	// a transient write error, so a requeueing caller retries it forever. See
	// AgentIdentityObjectID.
	objectID, err := AgentIdentityObjectID(ns, name)
	if err != nil {
		return err
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentidentity", ObjectId: objectID},
				Relation: "platform",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "platform", ObjectId: platformObjectID}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentidentity#platform: %w", err)
	}
	return nil
}

// CheckAgentIdentityUpdateCredential answers
// agentidentity:<ns>/<name>#update_credential for user:<canonicalID> — may this
// human replace the agent's OWN shared credential? The object id is
// "<namespace>/<name>", matching the tuple EnsureAgentIdentityPlatform writes.
//
// It is ALWAYS FullyConsistent, and it deliberately does NOT take the
// `fullyConsistent bool` opt-in every neighbouring Check helper carries
// (CheckDenied, CheckPlatformPermission, CheckApprove, CheckOwnerOnResource).
// Those default to MinimizeLatency, which is right for a permission whose
// tuples were written long before the check; it is wrong here. See
// EnsureAgentIdentityPlatform's CONSISTENCY note for the write ordering: the
// #platform link and the dead-credential determination come from the SAME
// reconcile, so a check can land inside SpiceDB's quantization window and read
// a snapshot predating the tuple. A stale read returns NO_PERMISSION, which is
// indistinguishable from "this person is not an admin" — a legitimate admin
// would be silently refused the one action the tuple exists to permit. Making
// the consistency a parameter would mean any future caller could reintroduce
// that bug by passing false; making it unconditional means they cannot.
func (c *Client) CheckAgentIdentityUpdateCredential(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) (bool, error) {
	// Same composition as the write, through the same function, so the object
	// this asks about and the object the reconciler linked cannot drift — and
	// so an unrepresentable name refuses with a NAMED cause rather than an
	// opaque InvalidArgument the caller reads as an authorization-service fault.
	objectID, err := AgentIdentityObjectID(ns, name)
	if err != nil {
		return false, err
	}
	// consistencyFor(true), not a caller-supplied flag: the guard above is that
	// no caller can reach this line with anything else.
	return c.checkUser(ctx, "agentidentity", objectID, "update_credential", canonicalID, consistencyFor(true),
		fmt.Sprintf("agentidentity:%s/%s#update_credential", ns, name))
}

// EnsureAgentClassPlatform writes
// agentclass:<ns>/<name>#platform@platform:platform (TOUCH — idempotent, so it
// is safe on every reconcile; CREATE would error on the second pass).
//
// This ONE tuple is what makes agentclass#start_session — the gate on starting
// a session from a browser — satisfiable at all. The permission is
// `starter + platform->start_session`; `starter` ships deliberately
// unpopulated, so platform->start_session is the only live arm, and it
// resolves only for an agentclass carrying this link to the singleton platform
// object. Omit the write and every check fails closed SILENTLY: the schema
// still compiles, the dashboard still renders, and the agent picker is empty
// with nothing saying why. Called by the AgentClass reconciler.
//
// Written on every reconcile and BEFORE any validation gate, deliberately: an
// AgentClass that fails validation is exactly the one an admin may need to
// start a session of in order to diagnose it, and a link withheld until the
// class is healthy would make the picker's contents depend on a fact that has
// nothing to do with authorization.
func (c *Client) EnsureAgentClassPlatform(ctx context.Context, ns, name string) error {
	// A name SpiceDB cannot express is a PERMANENT failure, not a blip: the
	// gRPC InvalidArgument it would otherwise return is indistinguishable from
	// a transient write error, so a requeueing caller retries it forever. See
	// AgentClassObjectID.
	objectID, err := AgentClassObjectID(ns, name)
	if err != nil {
		return err
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: objectID},
				Relation: "platform",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "platform", ObjectId: platformObjectID}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentclass#platform: %w", err)
	}
	return nil
}

// DeletePlatformAdmin removes platform:platform#admin@user:<canonicalID>.
func (c *Client) DeletePlatformAdmin(ctx context.Context, canonicalID identity.CanonicalUserID) error {
	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "platform",
			OptionalResourceId: platformObjectID,
			OptionalRelation:   "admin",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: canonicalID.String(),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("delete platform admin: %w", err)
	}
	return nil
}

// ListPlatformAdmins returns every subject on platform:platform#admin as
// SpiceDB subject strings ("user:<canonical>" or "group:<id>#member"),
// sorted. Fully-consistent — admin listings must reflect current state.
func (c *Client) ListPlatformAdmins(ctx context.Context) ([]string, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "platform",
			OptionalResourceId: platformObjectID,
			OptionalRelation:   "admin",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("read platform admins: %w", err)
	}
	var out []string
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read platform admins: %w", err)
		}
		sub := resp.GetRelationship().GetSubject()
		s := sub.GetObject().GetObjectType() + ":" + sub.GetObject().GetObjectId()
		if rel := sub.GetOptionalRelation(); rel != "" {
			s += "#" + rel
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// TouchAgentClassStarter writes agentclass:<ns>/<name>#starter@user:<canonicalID>
// (TOUCH — idempotent). The standing override for the session-start gate:
// `start_session = starter + platform->start_session`, so a starter grant
// lets an org non-member (a channel guest) start sessions of this class
// without a per-session platform-admin approval. Written by
// `oap class grant-start`.
func (c *Client) TouchAgentClassStarter(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	objectID, err := AgentClassObjectID(ns, name)
	if err != nil {
		return err
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: objectID},
				Relation: "starter",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentclass starter: %w", err)
	}
	return nil
}

// DeleteAgentClassStarter removes agentclass:<ns>/<name>#starter@user:<canonicalID>.
func (c *Client) DeleteAgentClassStarter(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	objectID, err := AgentClassObjectID(ns, name)
	if err != nil {
		return err
	}
	_, err = c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentclass",
			OptionalResourceId: objectID,
			OptionalRelation:   "starter",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: canonicalID.String(),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("delete agentclass starter: %w", err)
	}
	return nil
}

// TouchInteractor writes agentclass:<classNS>/<className>#interactor@<subjectRef>
// (TOUCH — idempotent). interactor records that subjectRef has interacted
// with a session of this class — the enumeration source for the App Home
// preferences pane (`can_personalize = interactor`). subjectRef is
// "objType:objId" or "objType:objId#relation", the same convention TouchOwner
// uses. Composes the object id through AgentClassObjectID, the single site
// that validates a class name is representable in SpiceDB — the same gate
// TouchAgentClassStarter and EnsureAgentClassPlatform apply to every other
// agentclass write, so an unrepresentable class name fails closed here too
// rather than producing a permanent, retried-forever gRPC InvalidArgument.
func (c *Client) TouchInteractor(ctx context.Context, classNS, className, subjectRef string) error {
	objectID, err := AgentClassObjectID(classNS, className)
	if err != nil {
		return err
	}
	objType, objID, rel, err := parseSubjectRef(subjectRef)
	if err != nil {
		return fmt.Errorf("touch agentclass#interactor: %w", err)
	}
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: objType, ObjectId: objID}}
	if rel != "" {
		subj.OptionalRelation = rel
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: objectID},
				Relation: "interactor",
				Subject:  subj,
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch agentclass#interactor@%s: %w", subjectRef, err)
	}
	return nil
}

// SubjectIdentity is one upstream object a directory sync linked to a platform
// user — a GitHub account, an org membership, a 1Password group.
type SubjectIdentity struct {
	// Definition is the SpiceDB definition ("github_user", "github_org", …).
	Definition string
	// Relation links the object to the user ("user", "member", …).
	Relation string
	// ObjectID is the upstream-keyed object id.
	ObjectID string
	// Source is the human-facing name (relsource.Source.Display()) of the
	// source that declared this definition#relation as an identity link, so the
	// console can say WHICH one asserted it.
	Source string
}

// SubjectIdentities is ListSubjectIdentities' answer: the links it could read,
// and the probes it could not.
//
// The two travel together because a partial read is neither of the answers a
// single slice could give. Returning only Identities would present a short list
// as a complete one; returning only an error would blank a panel over one
// absent definition. A caller MUST render both — see Unavailable.
type SubjectIdentities struct {
	// Identities are the links that were read successfully, sorted
	// source-first. May be empty while Unavailable is not.
	Identities []SubjectIdentity
	// Unavailable are the probes that failed. Non-empty means Identities is
	// INCOMPLETE, and a consumer that renders it as a finished list is making a
	// claim this read did not support.
	Unavailable []UnavailableProbe
}

// UnavailableProbe is one (definition, relation) read that failed, and why.
//
// The expected cause is a definition that is not in the live schema yet:
// github_org, github_team and github_repo are declared only in the gh toolkit
// fragment, composed into the schema by the guardian reconciler, so on a fresh
// cluster a read against them errors while every other probe succeeds.
type UnavailableProbe struct {
	// Source is the human-facing name of the source that declared the probe.
	Source string
	// Definition and Relation name the probe that failed.
	Definition, Relation string
	// Err is the read failure, already formatted. A string rather than an
	// error: nothing downstream branches on the type, every consumer renders
	// it, and a plain value keeps this struct comparable in tests.
	Err string
}

// ListSubjectIdentities returns the external identity objects linked to
// canonicalID, plus the probes that could not be read.
//
// # What this reader can and cannot see
//
// It reads tuples whose SUBJECT is the platform user directly —
// <definition>:<id>#<relation>@user:<canonicalID>. That is a deliberate limit,
// not an oversight, and it makes the answer narrower than "everything a
// directory sync knows about this person":
//
//   - VISIBLE: a bare user-subject link. github_user:<id>#user and #sole_user
//     (the useridentity attestation), slack_user:<uid>#user, and
//     onepassword_group:<gid>#member.
//   - INVISIBLE: any tuple whose subject is a USERSET.
//     github_org:<login>#member@github_user:<id>#sole_user, every github_repo
//     role, and slack_channel/slack_workspace#member@slack_user:<uid>#user are
//     all of this shape. A user-subject filter cannot match them, so an org or
//     channel membership does NOT appear here even though a sync wrote it.
//     Reaching those would mean walking one hop out from each visible link, a
//     different (and much more expensive) read than this one.
//
// Which pairs are probed is DERIVED from the relsource claim table, never
// transcribed: each registered Source declares its own SubjectIdentityClaims —
// the subset of its claims with the first shape — so registering a new sync
// kind surfaces its identities here with no list in this package to update, and
// a source cannot accidentally contribute a probe that can never match (see
// subjectProbes, and relsource.Source.SubjectIdentityClaims).
//
// # Partial failure
//
// A probe against a definition the live schema does not declare fails, and on a
// fresh cluster that is ordinary: github_org / github_team / github_repo exist
// only in the gh toolkit fragment the guardian reconciler composes in. One such
// failure must not blank the panel for every user, so failures are collected
// per probe into Unavailable and the rows that DID read are still returned.
// Fail-closed remains: the caller is told, explicitly, that the list is short.
// A returned error (as opposed to a non-empty Unavailable) means the read never
// started — an unwired claim table, or no subject to read for.
//
// Fully consistent: an admin reading a person's linked identities right after
// a sync must see the result of that sync, not a stale snapshot.
func (c *Client) ListSubjectIdentities(ctx context.Context, canonicalID identity.CanonicalUserID) (SubjectIdentities, error) {
	if err := subjectIdentitiesGuard(relsource.IsComplete()); err != nil {
		return SubjectIdentities{}, err
	}
	if canonicalID.IsZero() {
		return SubjectIdentities{}, fmt.Errorf("list subject identities: empty canonical user id")
	}

	probes := subjectProbes()

	type probeResult struct {
		found []SubjectIdentity
		err   error
	}
	results := make([]probeResult, len(probes))
	var g errgroup.Group
	// Bounds concurrent ReadRelationships RPCs against SpiceDB: one probe per
	// declared identity claim could otherwise mean dozens of simultaneous
	// streams for a single console page load.
	g.SetLimit(8)
	for i, p := range probes {
		g.Go(func() error {
			// Each probe records its own outcome and returns nil, so one
			// failure neither cancels its siblings nor aborts the whole read.
			// errgroup.WithContext is deliberately NOT used for the same
			// reason: its context cancels on first error, which is exactly the
			// all-or-nothing behavior this collects errors to avoid.
			found, err := c.readSubjectLinks(ctx, p.definition, p.relation, canonicalID.String())
			if err != nil {
				results[i] = probeResult{err: fmt.Errorf("read %s#%s for subject: %w", p.definition, p.relation, err)}
				return nil
			}
			for j := range found {
				found[j].Source = p.source
			}
			results[i] = probeResult{found: found}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// Unreachable: every goroutine above returns nil. Kept, rather than
		// discarded, so an edit that starts returning an error from one cannot
		// make it vanish silently.
		return SubjectIdentities{}, err
	}

	var out []SubjectIdentity
	var unavailable []UnavailableProbe
	for i, r := range results {
		if r.err != nil {
			unavailable = append(unavailable, UnavailableProbe{
				Source:     probes[i].source,
				Definition: probes[i].definition,
				Relation:   probes[i].relation,
				Err:        r.err.Error(),
			})
			continue
		}
		out = append(out, r.found...)
	}
	// Stable order for the same reason the identities are sorted: a console
	// re-render must not reshuffle the "some sources are unavailable" list.
	sort.Slice(unavailable, func(i, j int) bool {
		if unavailable[i].Source != unavailable[j].Source {
			return unavailable[i].Source < unavailable[j].Source
		}
		if unavailable[i].Definition != unavailable[j].Definition {
			return unavailable[i].Definition < unavailable[j].Definition
		}
		return unavailable[i].Relation < unavailable[j].Relation
	})
	// Source first: the console groups these by the sync that asserted them,
	// so an admin reads "what GitHub told us" as one block rather than
	// interleaved with 1Password. Definition/ObjectID break ties so the order
	// is total and the rendering is stable between loads.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Definition != out[j].Definition {
			return out[i].Definition < out[j].Definition
		}
		if out[i].Relation != out[j].Relation {
			return out[i].Relation < out[j].Relation
		}
		return out[i].ObjectID < out[j].ObjectID
	})
	return SubjectIdentities{Identities: out, Unavailable: unavailable}, nil
}

// subjectIdentitiesGuard refuses ListSubjectIdentities when this binary's
// relsource claim table isn't marked complete: complete reflects
// relsource.IsComplete(), the caller's actual wiring state.
//
// An unmarked table makes relsource.All() come back empty, and
// subjectProbes then finds nothing to read — indistinguishable from a
// person who genuinely has no directory links. That is a confident wrong
// answer to exactly the question the console asks, and it is the same
// silent-allow hazard relsource/complete.go already fails closed for on the
// write side (requireComplete); this is that same gate for a read.
//
// Taking the completeness value as a parameter, rather than calling
// relsource.IsComplete() itself, is what makes the false branch testable:
// relsource.complete is a process-global latch with no reset once
// MarkComplete has run (by design — see MarkComplete's own doc), and
// writer_test.go's init() already marks it complete for every test in this
// package's binary. A test exercises this function directly with false
// instead of trying to unmark that latch.
func subjectIdentitiesGuard(complete bool) error {
	if !complete {
		return fmt.Errorf("list subject identities: %w — this binary must blank-import github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports before this read can be trusted", relsource.ErrClaimTableIncomplete)
	}
	return nil
}

// subjectProbe is one (definition, relation) pair to read, and the
// human-facing name of the source that declared it.
type subjectProbe struct{ definition, relation, source string }

// subjectProbes builds the read set from the relsource claim table — from each
// registered source's SubjectIdentityClaims, NOT its Claims.
//
// Claims is the ownership table: it answers "who may write this relation", and
// walking it here produced two defects at once. It included every non-directory
// claim owner, so an operator's table contributed memory_entry#creator — TOUCHed
// on every memory Put by every user — and the panel grew one row per memory
// entry a person had ever created, attributed to the memory authorizer. And it
// included membership relations whose subject is a userset, which
// readSubjectLinks' user-subject filter can never match, so half the probes
// looked like coverage and returned nothing.
//
// SubjectIdentityClaims is still a REGISTRY derivation, not a list in this
// file: registering a new sync kind that declares one surfaces its identities
// here with nothing to update. What changed is which declaration is consulted —
// a source now states its own tuple shape, because only it knows it.
func subjectProbes() []subjectProbe {
	var probes []subjectProbe
	for _, src := range relsource.All() {
		for _, claim := range src.SubjectIdentityClaims {
			def, rel, ok := relsource.SplitClaim(claim)
			if !ok {
				// Unreachable: relsource.Register refuses a
				// SubjectIdentityClaim outside Claims, and buildIndex panics on
				// a malformed Claims entry, so anything registered here has
				// already been parsed once. Skipped rather than propagated
				// because there is no caller-visible failure to report — the
				// registration that would have produced one already panicked.
				continue
			}
			probes = append(probes, subjectProbe{def, rel, src.Display()})
		}
	}
	return probes
}

// readSubjectLinks streams every <definition>:<id>#<relation>@user:<subjectID>
// relationship. Mirrors ListAgentClassStarters' shape, filtered by SUBJECT
// rather than by resource id.
func (c *Client) readSubjectLinks(ctx context.Context, definition, relation, subjectID string) ([]SubjectIdentity, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:     definition,
			OptionalRelation: relation,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: subjectID,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	var out []SubjectIdentity
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		rel := resp.GetRelationship()
		out = append(out, SubjectIdentity{
			Definition: definition,
			Relation:   relation,
			ObjectID:   rel.GetResource().GetObjectId(),
		})
	}
	return out, nil
}

// SourceScope is the set of scope ids one scope-bearing definition
// ("github_org", "slack_channel", "onepassword_group", …) had at read time —
// one id per scope a directory sync's last pass actually wrote into SpiceDB,
// read from the #relhash sentinel pkg/platform/relsync/sync.go's Pass TOUCHes
// exactly once per scope it processes (see relsync.Scope,
// relsource.SentinelRelation).
//
// This is SCOPE count, never membership count: an org with 400 repos is 400
// relhash tuples on github_repo, one per repo scope, regardless of how many
// people are members of any one of them.
type SourceScope struct {
	// Definition is the SpiceDB definition this scope-bearing relation lives on.
	Definition string
	// ScopeIDs are the synced scope ids the read RETAINED — the first
	// capPerDefinition ids the stream yielded, in ARRIVAL order, sorted only
	// among themselves afterward. This is NOT the capPerDefinition
	// lowest/first ids overall: which N of the total got kept (and therefore
	// which N appear, alphabetized) depends on SpiceDB's own stream order and
	// can differ between reads of the same data — a page refresh may show a
	// different N when Total exceeds the cap. May be shorter than Total.
	ScopeIDs []string
	// Total is the number of scopes actually observed for this definition. Can
	// exceed len(ScopeIDs) when the cap truncated what was kept — a caller
	// renders that as "showing len(ScopeIDs) of Total".
	Total int
	// Source is the human-facing name (relsource.Source.Display()) of the
	// source these scopes belong to, so a console row can name what wrote
	// them without repeating Definition a third time (Title already carries
	// it) — mirrors SubjectIdentity.Source's role on the subject side.
	Source string
	// Labels maps a scope id in ScopeIDs to the human name resolved for it
	// through one of the source's declared ScopeLabelBridges — "demo-org/
	// widgets" for github_repo:1005857813.
	//
	// SPARSE, and a miss is the signal to render the raw id: nil for a source
	// declaring no bridge at all (Slack, 1Password — nothing stores their
	// names), and missing an entry for a scope no bridge could name or whose
	// bridge id did not decode. It is never a reason to hide a row.
	//
	// A bridge read that FAILED is a different answer again, and it is not
	// expressible here — an unlabelled row looks identical whether nothing
	// was stored or the read fell over. SourceScopes.LabelsUnavailable is
	// what tells those apart, and a caller must render it.
	Labels map[string]ScopeLabel
}

// SourceScopes is ListSourceScopes' answer: the scope-bearing definitions it
// could read, and the probes it could not.
//
// Same shape as SubjectIdentities, and for the same reason: a definition src
// claims that the live schema does not yet declare (github_org before the gh
// toolkit fragment is composed in) must shorten this list, not blank the
// whole panel. A caller MUST render both halves — see Unavailable.
type SourceScopes struct {
	// Scopes are the scope-bearing definitions that read successfully AND had
	// at least one synced scope. A definition with zero scopes contributes
	// nothing here — that is a genuinely empty answer, not a partial one, and
	// rendering an empty group would be a claim this read did not make.
	Scopes []SourceScope
	// Unavailable are the probes that failed. Non-empty means Scopes is
	// INCOMPLETE.
	Unavailable []UnavailableProbe
	// LabelsUnavailable are the label-BRIDGE reads that failed. Non-empty
	// means the rows are all there and some of them are named by raw id when
	// a name exists in SpiceDB.
	//
	// Separate from Unavailable because the two say different things and a
	// caller renders them differently: Unavailable means "rows are missing
	// from this list", LabelsUnavailable means "every row is here, some are
	// unnamed". Folding them would make a cosmetic degradation read as a
	// truncated list, and a truncated list read as a cosmetic one.
	//
	// It must be rendered. A silently unlabelled list is indistinguishable
	// from a directory that has no labels — the same reason an empty read and
	// a failed read are not allowed to look alike anywhere else on this page.
	LabelsUnavailable []UnavailableProbe
}

// ListSourceScopes returns what src's last sync pass actually wrote into
// SpiceDB — one SourceScope per scope-bearing definition src claims — read
// from the RESOURCE side, complementing ListSubjectIdentities' subject-side
// read (which structurally cannot see a membership tuple whose subject is a
// userset, e.g. github_org:acme#member@github_user:123#sole_user; this read
// answers the same underlying "what did this sync pull in" question from the
// side that CAN see it).
//
// Which definitions are scope-bearing is DERIVED from src.Claims, never
// hand-listed (see scopeBearingDefinitions): a Claims entry whose relation is
// the #relhash sentinel (relsource.IsSentinelRelation) names one. That is
// exactly what relsync.Pass writes exactly once per scope it processes (see
// pkg/platform/relsync/sync.go's readSentinel/diffScope), so reading it with
// NO resource-id filter returns one row per synced scope of that definition —
// bounded by SCOPE count, never membership count.
//
// src is a resolved relsource.Source (e.g. relsync.Get(spec.Kind).Source()),
// not a name looked up here: this package cannot import pkg/platform/relsync
// (relsync imports this package), so the kind → Source resolution is the
// caller's job — admind.New's adapter is where it lives for the console, the
// same split SubjectIdentityReaderFunc's own doc describes for the subject
// side.
//
// capPerDefinition bounds how many scope ids are RETURNED per definition —
// ResourceDetail has no pagination, and a large org's repo list is otherwise
// unbounded. The full count actually observed still lands on
// SourceScope.Total, so a caller can render "showing N of M". Must be
// positive.
//
// Fully consistent: an admin reading this page right after a sync must see
// that pass's own writes, not a stale snapshot.
//
// bridges name the already-written relations whose resource ids carry a human
// name for a scope id — see ScopeLabelBridge. Nil is the ordinary case for a
// source that stores no names anywhere (Slack, 1Password), and every scope
// then renders by its raw id exactly as it did before this parameter existed.
// The bridges are read AFTER the scope ids are known, and only for
// definitions that actually synced something, so a source that declares one
// costs one extra read per bridge — never one per scope.
//
// Partial failure mirrors ListSubjectIdentities exactly: one probe's failure
// (an absent definition, most commonly — github_org/github_team/github_repo
// exist only in the gh toolkit fragment the guardian reconciler composes in)
// is collected into Unavailable rather than aborting every other probe. A
// failed BRIDGE read is collected separately, into LabelsUnavailable, because
// it degrades the rows' presentation and not the list's completeness. A
// returned error means the read never started at all.
func (c *Client) ListSourceScopes(ctx context.Context, src relsource.Source, bridges []ScopeLabelBridge, capPerDefinition int) (SourceScopes, error) {
	if capPerDefinition <= 0 {
		return SourceScopes{}, fmt.Errorf("list source scopes: capPerDefinition must be positive")
	}
	defs := scopeBearingDefinitions(src)

	type probeResult struct {
		scope SourceScope
		err   error
	}
	results := make([]probeResult, len(defs))
	var g errgroup.Group
	// Same bound as ListSubjectIdentities, for the same reason: one probe per
	// scope-bearing definition could otherwise mean several simultaneous
	// ReadRelationships streams for a single console page load.
	g.SetLimit(8)
	for i, def := range defs {
		g.Go(func() error {
			// Each probe records its own outcome and returns nil, so one
			// failure neither cancels its siblings nor aborts the whole read —
			// see ListSubjectIdentities' identical note on why
			// errgroup.WithContext is deliberately NOT used here.
			ids, total, err := c.readScopeSentinels(ctx, def, capPerDefinition)
			if err != nil {
				results[i] = probeResult{err: fmt.Errorf("read %s#%s: %w", def, relsource.SentinelRelation, err)}
				return nil
			}
			results[i] = probeResult{scope: SourceScope{Definition: def, ScopeIDs: ids, Total: total, Source: src.Display()}}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// Unreachable: every goroutine above returns nil. Kept, rather than
		// discarded, so an edit that starts returning an error from one cannot
		// make it vanish silently.
		return SourceScopes{}, err
	}

	var out []SourceScope
	var unavailable []UnavailableProbe
	for i, r := range results {
		if r.err != nil {
			unavailable = append(unavailable, UnavailableProbe{
				Source:     src.Display(),
				Definition: defs[i],
				Relation:   relsource.SentinelRelation,
				Err:        r.err.Error(),
			})
			continue
		}
		if r.scope.Total == 0 {
			// Genuinely nothing synced yet for this definition — not a failure,
			// so it contributes no row rather than an empty one.
			continue
		}
		out = append(out, r.scope)
	}
	// Stable order for the same reason ListSubjectIdentities sorts: a console
	// re-render must not reshuffle either list.
	sort.Slice(unavailable, func(i, j int) bool { return unavailable[i].Definition < unavailable[j].Definition })
	sort.Slice(out, func(i, j int) bool { return out[i].Definition < out[j].Definition })

	// Labels last, and only over the ids that survived the cap: the bridge
	// read joins on the scope ids we are actually going to render, so it can
	// neither be issued before they are known nor cost anything for a row that
	// was truncated away.
	labels, labelsUnavailable := c.resolveScopeLabels(ctx, out, bridges, src.Display())
	for i := range out {
		if byID := labels[out[i].Definition]; len(byID) > 0 {
			out[i].Labels = byID
		}
	}
	return SourceScopes{Scopes: out, Unavailable: unavailable, LabelsUnavailable: labelsUnavailable}, nil
}

// scopeBearingDefinitions returns the definitions src writes a #relhash
// sentinel on, derived from src.Claims rather than hand-listed: a source
// that starts claiming a new scope-bearing definition surfaces it here with
// nothing in this file to update. Sorted and deduplicated (a source's Claims
// name each definition#relation once, so duplicates are not expected, but a
// map keeps this correct even if that ever changes) for a stable probe order.
func scopeBearingDefinitions(src relsource.Source) []string {
	seen := make(map[string]bool, len(src.Claims))
	var defs []string
	for _, claim := range src.Claims {
		def, rel, ok := relsource.SplitClaim(claim)
		if !ok {
			// Unreachable in practice: relsource.buildIndex panics on a
			// malformed Claims entry (no "#" separator) the first time
			// anything calls CheckWrite/CheckDeleteFilter, so a src reaching
			// here already has a well-formed Claims list — same reasoning as
			// subjectProbes' identical skip. Not propagated because there is
			// no caller-visible failure to report: the registration that
			// would have produced one already panicked.
			continue
		}
		if !relsource.IsSentinelRelation(rel) {
			continue
		}
		if !seen[def] {
			seen[def] = true
			defs = append(defs, def)
		}
	}
	sort.Strings(defs)
	return defs
}

// readScopeSentinels streams every <definition>:<id>#relhash tuple — NO
// resource-id filter, so this reads every scope of the definition a source
// has ever synced, not one scope's content. Mirrors readSubjectLinks' shape,
// filtered by RELATION with no subject filter rather than by subject.
//
// This ALWAYS drains the whole stream, with no ReadRelationshipsRequest
// OptionalLimit and no cache: a 40k-repo org streams 40k fully-consistent
// rows on every tab open. That is the accepted cost, not an oversight — the
// true Total and an early stop are mutually exclusive here, because SpiceDB
// exposes no separate count API and a stream stopped at capN would report
// "capN of capN" forever, never the real size. A future change that adds an
// OptionalLimit to make this cheaper would have to give up an accurate Total
// (or fetch it a second, different way) to do it.
func (c *Client) readScopeSentinels(ctx context.Context, definition string, capN int) (ids []string, total int, err error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:     definition,
			OptionalRelation: relsource.SentinelRelation,
		},
	})
	if err != nil {
		return nil, 0, err
	}
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, 0, recvErr
		}
		total++
		if len(ids) < capN {
			ids = append(ids, resp.GetRelationship().GetResource().GetObjectId())
		}
	}
	sort.Strings(ids)
	return ids, total, nil
}

// TouchAuthorizedToken writes/updates a value-bound authorized_token grant for a
// session, keyed by credID, carrying the token_value_matches caveat. Idempotent.
func (c *Client) TouchAuthorizedToken(ctx context.Context, ns, name, credID, authorizedValueHash string) error {
	cav, err := structpb.NewStruct(map[string]any{externaltoken.CaveatArgAuthorizedHash: authorizedValueHash})
	if err != nil {
		return fmt.Errorf("build token caveat context for %s/%s cred=%s: %w", ns, name, credID, err)
	}
	_, err = c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource:       &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation:       externaltoken.RelationAuthorizedToken,
				Subject:        &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: externaltoken.ObjectType, ObjectId: credID}},
				OptionalCaveat: &v1.ContextualizedCaveat{CaveatName: externaltoken.CaveatTokenValueMatches, Context: cav},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch authorized_token %s/%s cred=%s: %w", ns, name, credID, err)
	}
	return nil
}

// TouchAuthorizedTokenIdentity writes an identity-only (uncaveated) grant for a
// federated credential whose per-mint value can't be bound. Idempotent.
func (c *Client) TouchAuthorizedTokenIdentity(ctx context.Context, ns, name, credID string) error {
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
				Relation: externaltoken.RelationAuthorizedToken,
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: externaltoken.ObjectType, ObjectId: credID}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch identity authorized_token %s/%s cred=%s: %w", ns, name, credID, err)
	}
	return nil
}

// DeleteAuthorizedToken removes a session's grant for one credID (caveat-agnostic),
// mirroring DeleteInteractParticipant's OptionalSubjectFilter shape.
func (c *Client) DeleteAuthorizedToken(ctx context.Context, ns, name, credID string) error {
	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: ns + "/" + name,
			OptionalRelation:   externaltoken.RelationAuthorizedToken,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       externaltoken.ObjectType,
				OptionalSubjectId: credID,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("delete authorized_token %s/%s cred=%s: %w", ns, name, credID, err)
	}
	return nil
}

// ListAuthorizedTokens reads back the current authorized_token grants for a
// session (fully consistent — this drives a diff that mutates state).
func (c *Client) ListAuthorizedTokens(ctx context.Context, ns, name string) ([]externaltoken.AuthorizedTokenGrant, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: ns + "/" + name,
			OptionalRelation:   externaltoken.RelationAuthorizedToken,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("read authorized_token %s/%s: %w", ns, name, err)
	}
	var out []externaltoken.AuthorizedTokenGrant
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read authorized_token stream %s/%s: %w", ns, name, rerr)
		}
		rel := msg.GetRelationship()
		g := externaltoken.AuthorizedTokenGrant{CredID: rel.GetSubject().GetObject().GetObjectId()}
		if cav := rel.GetOptionalCaveat(); cav != nil {
			if v := cav.GetContext().GetFields()[externaltoken.CaveatArgAuthorizedHash]; v != nil {
				g.AuthorizedValueHash = v.GetStringValue()
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// CheckUseToken checks whether a session may use the token identified by credID,
// presenting the per-session HMAC of the value about to be used. For a caveated
// (value-bound) grant the caveat requires presented == authorized; for an
// identity-only (federated) grant the context is ignored. Returns (false, nil)
// for a definitive deny and a non-nil error only when SpiceDB is unreachable
// (indeterminate → callers fail the session).
func (c *Client) CheckUseToken(ctx context.Context, ns, name, credID, presentedValueHash string, fullyConsistent bool) (bool, error) {
	cav, err := structpb.NewStruct(map[string]any{externaltoken.CaveatArgPresentedHash: presentedValueHash})
	if err != nil {
		return false, fmt.Errorf("build use_token caveat context: %w", err)
	}
	// Not checkUser: the subject here is the token grant, not a human.
	return c.check(ctx, &v1.CheckPermissionRequest{
		Resource:    &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
		Permission:  externaltoken.PermissionUseToken,
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: externaltoken.ObjectType, ObjectId: credID}},
		Context:     cav,
		Consistency: consistencyFor(fullyConsistent),
	}, fmt.Sprintf("use_token %s/%s cred=%s", ns, name, credID))
}

// CheckSessionPermission answers agentsession:<ns>/<name>#<permission> for
// user:<canonicalID>.
//
// The general form CheckInteract / CheckManageScope / CheckFork / CheckApprove
// all delegate to. It exists because the metaagent's capability registry
// declares its gate as a STRING (Capability.Permission()) — a method per
// permission cannot serve a registry, and adding one per capability would put
// the `if kind == "x"` this repo avoids straight into the client.
//
// Four hand-written copies of this body preceded it, differing only in the
// permission name and the error string. Each was a place the consistency
// handling could drift, and a fifth would have joined them.
//
// fullyConsistent=true after a recent write (a freshly-set owner tuple); the
// caller owns that choice because only it knows what it just wrote. Not cached
// — checks must reflect current state.
func (c *Client) CheckSessionPermission(
	ctx context.Context, ns, name, permission string,
	canonicalID identity.CanonicalUserID, fullyConsistent bool,
) (bool, error) {
	consistency := &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}}
	if fullyConsistent {
		consistency = &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}
	}
	resp, err := c.cl.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:    &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
		Permission:  permission,
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()}},
		Consistency: consistency,
	})
	if err != nil {
		return false, fmt.Errorf("check agentsession#%s: %w", permission, err)
	}
	return resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
}

// SessionHasOnResource checks <resType>:<resID>#<permission>@agentsession:<ns>/<name>.
//
// A SESSION subject, where CheckOnResource takes a user one. Data-slot
// attenuation needs it: pt_tag's `access = session + session->ancestor +
// granted_to` is a fact about the session a tag was minted in, not about
// whoever started that session, so no user-subject check can answer it.
//
// Fully consistent, deliberately. The caller is deciding what a parent may
// hand to a child, and a tag minted moments ago on this very turn is the
// common case — MinimizeLatency here would refuse a delegation for data the
// parent demonstrably holds, which reads as a bug rather than as staleness.
func (c *Client) SessionHasOnResource(
	ctx context.Context, resType, resID, permission string, session authz.SessionRef,
) (bool, error) {
	subjectID := session.Namespace + "/" + session.Name
	return c.check(ctx, &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
		Permission: permission,
		Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
			ObjectType: "agentsession", ObjectId: subjectID,
		}},
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	}, fmt.Sprintf("%s:%s#%s@agentsession:%s", resType, resID, permission, subjectID))
}

// Compile-time proof that the client satisfies the interface data-slot
// attenuation depends on. Without it, a signature drift here would surface
// only where the two are wired together — far from the change that caused it.
var _ authz.SessionPermissionChecker = (*Client)(nil)
