// Package grants writes, deletes, and checks the per-(session, tool, args)
// SpiceDB grant tuples behind the tool-approval flow. The tuple shape:
//
//	agentsession:<ns/name>#grant_<perm>_<resType>@<resType>:<resID>
//	  caveat: check_hash, ctx: {allowed_arguments_hash: <hmac-sha256, keyed per session>}
//	  expires_at: now + <ttl>     // always set; TTL==0 gets DefaultSessionGrantTTL
package grants

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// AgentSessionDefinition is the SpiceDB definition name of the session
// resource that owns grant relations.
const AgentSessionDefinition = "agentsession"

// CheckHashCaveat is the name of the caveat that gates a grant tuple on
// the call-time `allowed_arguments_hash`.
const CheckHashCaveat = "check_hash"

// Source identifies the runner's tool-approval grant writes — the
// per-(session, tool, args) grant tuple WriteToolGrant/DeleteToolGrant write
// on approve, keyed by schema.GrantPair.RelationName():
// "grant_<permission>_<resourceType>" on the agentsession resource.
//
// Claims deliberately EMPTY. The relation name is generated per (resourceType,
// permission) pair an AgentClass declares through AgentSessionGrants — it
// cannot be enumerated any more than a slot-grant relation
// (schema.SlotGrantRelationName) can, for the identical reason: both are
// per-tenant and dynamic. This mirrors, not contradicts, the slot-grant
// exclusion: same shape of unclaimability, different (older) relation
// family.
//
// This writer is also DORMANT in production today: the tool-approval flow
// now writes a slot grant instead (pkg/channels/channelsd/pipeline/
// tool_approval_interaction.go), and WriteToolGrant/DeleteToolGrant are
// exercised only from pkg/authz/guardian/integration_test.go. The runner
// still wires GuardianGrantWriter (a RelWriter bound to this Source) but
// only checks it for nilness before taking the slot-grant path — see
// pkg/agent/runner/host_approval.go. Registered anyway, with empty claims,
// so the shape holds if/when this path is revived.
var Source = relsource.Source{Name: "guardiangrants"}

func init() {
	relsource.Register(Source)
}

// Writer is the minimal SpiceDB surface area required to manage grant
// tuples. The concrete spicedb v1.PermissionsServiceClient satisfies it;
// tests pass a recorder double.
type Writer interface {
	// WriteRelationships issues one v1 WriteRelationships RPC carrying the grant
	// tuples an approval produces, each caveated on the call's arguments hash.
	// An error means the approval did not take effect and the retried call will
	// still be denied.
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)

	// DeleteRelationships issues one v1 DeleteRelationships RPC, retracting
	// grants. An error means the grant may still be live, so a revoke must be
	// surfaced rather than reported as done.
	DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error)
}

// Grant describes a single per-(session, tool, args) authorization to
// write. TTL=0 means "session-wide" — the writer applies a long
// backstop expiration (DefaultSessionGrantTTL) so the tuple satisfies
// the schema's `with check_hash and expiration` constraint while
// still acting as effectively-permanent for the session lifetime.
// TTL>0 stamps OptionalExpiresAt = now+TTL (used for external-effect
// tools that should auto-expire well before the session does).
type Grant struct {
	SessionRef   string
	Permission   string
	ResourceType string
	ResourceID   string
	ArgsHash     string
	TTL          time.Duration
}

// DefaultSessionGrantTTL is the backstop expiration applied to
// "session-wide" (TTL==0) grants. The schema mandates expiration on
// every grant relation (`with check_hash and expiration`), so an
// "indefinite" tuple is not actually expressible — instead we pick a
// horizon long enough to outlast any reasonable session. A grant
// re-issued after expiry just goes through the approval flow again.
const DefaultSessionGrantTTL = 7 * 24 * time.Hour

// GrantKey is the identity of a grant tuple sufficient to delete it.
// Note that the caveat / args-hash is NOT part of the key — DeleteToolGrant
// removes any matching (session, relation, subject) tuple regardless of
// caveat context.
type GrantKey struct {
	SessionRef   string
	Permission   string
	ResourceType string
	ResourceID   string
}

// WriteToolGrant TOUCHes the grant tuple corresponding to g into SpiceDB.
// The caveat is always `check_hash` with `allowed_arguments_hash` set to
// g.ArgsHash; the relation is `grant_<perm>_<resType>` and the subject
// is `<resType>:<resID>`.
func WriteToolGrant(ctx context.Context, w Writer, g Grant) error {
	if g.ArgsHash == "" {
		return fmt.Errorf("grants.WriteToolGrant: ArgsHash is empty (programming error)")
	}
	pair := schema.GrantPair{ResourceType: g.ResourceType, Permission: g.Permission}
	cavCtx, err := structpb.NewStruct(map[string]any{
		"allowed_arguments_hash": g.ArgsHash,
	})
	if err != nil {
		return fmt.Errorf("build caveat ctx: %w", err)
	}
	rel := &v1.Relationship{
		Resource: &v1.ObjectReference{
			ObjectType: AgentSessionDefinition,
			ObjectId:   g.SessionRef,
		},
		Relation: pair.RelationName(),
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{
				ObjectType: g.ResourceType,
				ObjectId:   g.ResourceID,
			},
		},
		OptionalCaveat: &v1.ContextualizedCaveat{
			CaveatName: CheckHashCaveat,
			Context:    cavCtx,
		},
	}
	// SpiceDB schema requires every grant tuple to carry an
	// expiration (the relation declares `with check_hash and
	// expiration`). For TTL==0 ("session-wide") we apply a long
	// backstop horizon; for TTL>0 (external-effect tools) we use the
	// short caller-supplied window. Either way OptionalExpiresAt is
	// never left unset.
	expiry := g.TTL
	if expiry <= 0 {
		expiry = DefaultSessionGrantTTL
	}
	rel.OptionalExpiresAt = timestamppb.New(time.Now().Add(expiry))
	_, err = w.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: rel,
		}},
	})
	return err
}

// DeleteToolGrant removes the grant tuple identified by k. The delete is
// caveat-agnostic — any (session, relation, subject) tuple is removed.
func DeleteToolGrant(ctx context.Context, w Writer, k GrantKey) error {
	pair := schema.GrantPair{ResourceType: k.ResourceType, Permission: k.Permission}
	_, err := w.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       AgentSessionDefinition,
			OptionalResourceId: k.SessionRef,
			OptionalRelation:   pair.RelationName(),
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       k.ResourceType,
				OptionalSubjectId: k.ResourceID,
			},
		},
	})
	return err
}

// ArgsHash returns the canonical-JSON HMAC-SHA256 of the supplied
// tool-call arguments under the per-session key. Map key order is
// canonicalized recursively (emitted as a flat [k0,v0,k1,v1,...]
// sequence to remain stable across Go versions where map iteration
// order is randomized). Slice order is preserved — argument lists
// carry positional meaning.
//
// The key is the session's args-hash key (32 random bytes minted by
// the AgentSession reconciler into the per-session Secret). Keying
// makes the grant binding unforgeable: a component that knows the
// arguments but not the key — the LLM, sandbox/sidecar code, or a
// confused deputy with SpiceDB write access — cannot produce the hash
// the runner will compute at check time. Callers MUST supply the
// session key; the runner refuses to start without one (internal/cmd/runner).
//
// Equivalent to ArgsHashFiltered(key, args, nil): every key
// participates in the hash.
func ArgsHash(key []byte, args map[string]any) string {
	return ArgsHashFiltered(key, args, nil)
}

// ArgsHashFiltered is ArgsHash with an optional allow-list of arg keys.
// When keys is non-nil, only the top-level entries whose key appears in
// the list contribute to the hash; all other entries are dropped before
// normalization. The keys list does NOT recurse — nested maps still hash
// in full per ArgsHash's canonicalization rules.
//
// Use this to support Permission.Check.GrantBindsArgs: a readwrite tool
// can be approved once for a resource (e.g. `repo`) and have the grant
// continue to satisfy subsequent calls that differ only in non-bound
// args (e.g. `pr` number). Passing a nil keys allowlist reproduces ArgsHash exactly.
//
// keys is treated as a set, not a sequence; order has no effect on the
// result.
func ArgsHashFiltered(key []byte, args map[string]any, keys []string) string {
	body, _ := json.Marshal(normalize(filterArgs(args, keys)))
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// filterArgs returns a copy of args restricted to the top-level keys in
// the allow-list. Returns args unchanged when keys is nil (back-compat
// path). Returns an empty map when keys is non-nil but empty — the
// resulting hash is the hash of {} which is intentional: an explicitly-
// empty key set binds the grant to "every call, regardless of args".
func filterArgs(args map[string]any, keys []string) map[string]any {
	if keys == nil {
		return args
	}
	allow := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		allow[k] = struct{}{}
	}
	out := make(map[string]any, len(keys))
	for k, v := range args {
		if _, ok := allow[k]; ok {
			out[k] = v
		}
	}
	return out
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys)*2)
		for _, k := range keys {
			out = append(out, k, normalize(x[k]))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	default:
		return v
	}
}
