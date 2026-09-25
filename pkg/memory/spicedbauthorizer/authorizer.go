package spicedbauthorizer

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/memory"
	artifactkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
)

const systemCaller = "system:channelsd"

// Source identifies this package's writes: memory_entry#session
// (AuthorizePut, below) and memory_entry#creator (also AuthorizePut) are the
// two write sites; their bulk delete on scope cleanup is CleanupScope,
// below. Verified as the sole in-tree writer of both — no other package
// constructs a memory_entry relationship write — the same unambiguous shape
// as pttagmint's or leakagegrants' claims.
//
// Claiming both makes CleanupScope's own empty-relation delete filter
// load-bearing: it passes the could-match rule's allow arm only because this
// Source owns EVERY claim on memory_entry. See
// TestCleanupScope_DeleteFilter_OwnSourceAllowed and
// TestCleanupScope_DeleteFilter_OtherSourceRefused in
// authorizer_relsource_test.go, which pin both directions so a future third
// claim on this type cannot break CleanupScope silently.
//
// Exported for consistency with every other package-owned relsource.Source
// var, even though this package is this name's only writer today.
var Source = relsource.Source{
	Name: "spicedbauthorizer",
	Claims: []string{
		"memory_entry#session",
		"memory_entry#creator",
	},
}

func init() {
	relsource.Register(Source)
}

// isSystemCaller reports whether caller is one of the privileged system
// identities (system:channelsd, system:authzd, …) that operate across all
// sessions. They skip the CALLER-SCOPED tuples and read filtering — already
// trusted by the httpsrv token gate, and their identifiers contain a ':' that is
// not a valid SpiceDB object id anyway. They do NOT skip the caller-independent
// artifact#parent write; see AuthorizePut.
func isSystemCaller(caller string) bool {
	return strings.HasPrefix(caller, "system:")
}

// Authorizer implements memory.Authorizer using SpiceDB's memory_entry
// resource type. It writes session+creator relationships on Put, post-filters
// via BulkCheckPermission on Query, and checks delete permission on Delete. A
// no-caller or system: caller (system:channelsd, system:authzd, …) bypasses
// those three — but NOT the artifact#parent write, which is caller-independent.
type Authorizer struct {
	client *spicedb.Client
	writer spicedb.RelWriter
}

var _ memory.Authorizer = (*Authorizer)(nil)

// New returns an Authorizer backed by the given SpiceDB client. A nil client
// is valid only when no-caller or system-caller paths are the only code paths
// exercised (e.g. in tests) — client.Writer on a nil *Client returns a
// genuine nil RelWriter (see AGENTS.md "Nil interfaces"), so this stays
// consistent with that.
func New(client *spicedb.Client) *Authorizer {
	return &Authorizer{client: client, writer: client.Writer(Source)}
}

// EntryResourceID returns the canonical memory_entry resource ID for an entry.
// Format: "<scope.Kind>/<scope.ID>/<entry.Kind>/<entry.ID>"
func EntryResourceID(e memory.Entry) string {
	return e.Scope.Kind + "/" + e.Scope.ID + "/" + e.Kind + "/" + e.ID
}

// EntryResourceIDFromParts builds the resource ID from individual components.
func EntryResourceIDFromParts(scope memory.Scope, kind, id string) string {
	return scope.Kind + "/" + scope.ID + "/" + kind + "/" + id
}

// SplitSessionScope splits a session scope ID ("<ns>/<name>") into its parts.
// Exported for testing.
func SplitSessionScope(scopeID string) (ns, name string) {
	ns, name, _ = strings.Cut(scopeID, "/")
	return ns, name
}

// ScopePrefix returns the prefix used for all entries under a scope, suitable
// for use with OptionalResourceIdPrefix in SpiceDB relationship filters.
func ScopePrefix(scope memory.Scope) string {
	return scope.Kind + "/" + scope.ID + "/"
}

// AuthorizePut writes the entry's authorization relationships to SpiceDB.
//
// Two classes of relationship, with DIFFERENT caller semantics:
//
//   - Caller-SCOPED (memory_entry#session + #creator): who wrote the entry.
//     Written only for a real user caller — system / no-caller writes arrive
//     through the trusted httpsrv token gate and have no user creator to record.
//   - Caller-INDEPENDENT (artifact#parent): gates DOWNSTREAM readers, not the
//     writer (artifact#view includes parent->interact, so every session
//     participant can view it). MUST be written regardless of caller: the runner
//     writes artifacts as a system caller, so gating this behind the user-caller
//     check silently 403s every live-view artifact ("you do not have access")
//     with zero diagnostics. Never return without having written it.
func (a *Authorizer) AuthorizePut(ctx context.Context, e memory.Entry) error {
	caller, ok := memory.CallerFrom(ctx)

	// Caller-scoped tuples: real user callers only.
	if ok && !isSystemCaller(caller) {
		resID := EntryResourceID(e)
		sessionID := e.Scope.ID
		if _, err := a.writer.WriteRelationships(ctx,
			&v1.WriteRelationshipsRequest{
				Updates: []*v1.RelationshipUpdate{
					{
						Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
						Relationship: &v1.Relationship{
							Resource: &v1.ObjectReference{
								ObjectType: "memory_entry",
								ObjectId:   resID,
							},
							Relation: "session",
							Subject: &v1.SubjectReference{
								Object: &v1.ObjectReference{
									ObjectType: "agentsession",
									ObjectId:   sessionID,
								},
							},
						},
					},
					{
						Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
						Relationship: &v1.Relationship{
							Resource: &v1.ObjectReference{
								ObjectType: "memory_entry",
								ObjectId:   resID,
							},
							Relation: "creator",
							Subject: &v1.SubjectReference{
								Object: &v1.ObjectReference{
									ObjectType: "user",
									ObjectId:   caller,
								},
							},
						},
					},
				},
			}); err != nil {
			return fmt.Errorf("spicedbauthorizer: write relationships for %s: %w", resID, err)
		}
	}

	// Caller-independent: every logical-artifact head gets an `artifact`
	// SpiceDB resource whose `view` derives from the parent session — the gate
	// the live-view checks. Written for ALL callers (incl. the system caller
	// that the runner uses to persist artifacts).
	if e.Kind == artifactkind.KindName {
		if a.client == nil {
			// Never in production: the operator only constructs the authorizer
			// with a real client. Loud, NOT silent — a nil client here means
			// the artifact is unviewable by everyone, the exact failure mode
			// this method exists to prevent.
			err := fmt.Errorf("spicedbauthorizer: nil SpiceDB client; cannot write artifact#parent for %s — artifact would be unviewable", e.ID)
			log.FromContext(ctx).Error(err, "artifact#parent write impossible",
				"artifactID", e.ID, "scope", e.Scope.ID)
			return err
		}
		ns, name := SplitSessionScope(e.Scope.ID)
		if err := a.client.TouchArtifactParent(ctx, e.ID, ns, name); err != nil {
			return fmt.Errorf("spicedbauthorizer: write artifact parent for %s: %w", e.ID, err)
		}
	}
	return nil
}

// AuthorizeQuery post-filters entries to those the caller has read permission
// for. When no caller is present or the caller is the system caller, all
// entries are returned unfiltered. Uses BulkCheckPermission to minimize
// round-trips.
func (a *Authorizer) AuthorizeQuery(ctx context.Context, entries []memory.Entry) ([]memory.Entry, error) {
	caller, ok := memory.CallerFrom(ctx)
	if !ok || isSystemCaller(caller) {
		return entries, nil
	}
	if len(entries) == 0 {
		return entries, nil
	}

	items := make([]*v1.CheckBulkPermissionsRequestItem, len(entries))
	for i, e := range entries {
		items[i] = &v1.CheckBulkPermissionsRequestItem{
			Resource: &v1.ObjectReference{
				ObjectType: "memory_entry",
				ObjectId:   EntryResourceID(e),
			},
			Permission: "read",
			Subject: &v1.SubjectReference{
				Object: &v1.ObjectReference{
					ObjectType: "user",
					ObjectId:   caller,
				},
			},
		}
	}

	resp, err := a.writer.CheckBulkPermissions(ctx,
		&v1.CheckBulkPermissionsRequest{
			Consistency: &v1.Consistency{
				Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true},
			},
			Items: items,
		})
	if err != nil {
		return nil, fmt.Errorf("spicedbauthorizer: bulk check read: %w", err)
	}

	return filterAllowed(ctx, entries, resp.Pairs), nil
}

// filterAllowed keeps the entries whose bulk-check pair reports HAS_PERMISSION.
// Pairs are matched by the resource ID echoed on pair.Request, never by slice
// position: nothing in the API contract promises the response is
// request-ordered or the same length, so indexing entries[i] off the pair index
// panics on a longer Pairs and hands the WRONG entry to an unpermitted caller
// on a shorter or reordered one. Anything not matched to a HAS_PERMISSION pair
// — a pair-level error, a missing pair, an unrecognized resource ID — is
// dropped, so a mangled response denies rather than leaks.
func filterAllowed(ctx context.Context, entries []memory.Entry, pairs []*v1.CheckBulkPermissionsPair) []memory.Entry {
	granted := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		resID := pair.GetRequest().GetResource().GetObjectId()
		if perr := pair.GetError(); perr != nil {
			log.FromContext(ctx).Info("spicedbauthorizer: bulk check pair errored; entry denied",
				"resource", resID, "code", perr.GetCode(), "err", perr.GetMessage())
			continue
		}
		item := pair.GetItem()
		if item == nil || item.Permissionship != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
			continue
		}
		if resID == "" {
			log.FromContext(ctx).Info("spicedbauthorizer: bulk check pair carries no resource id; entry denied")
			continue
		}
		granted[resID] = struct{}{}
	}

	allowed := make([]memory.Entry, 0, len(entries))
	for _, e := range entries {
		if _, ok := granted[EntryResourceID(e)]; ok {
			allowed = append(allowed, e)
		}
	}
	return allowed
}

// AuthorizeDelete checks whether the caller has delete permission for the
// specified entry. When no caller is present, the deletion is allowed
// unconditionally.
func (a *Authorizer) AuthorizeDelete(ctx context.Context, scope memory.Scope, kind, id string) error {
	caller, ok := memory.CallerFrom(ctx)
	if !ok || isSystemCaller(caller) {
		return nil
	}

	resID := EntryResourceIDFromParts(scope, kind, id)
	resp, err := a.client.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: "memory_entry", ObjectId: resID},
		Permission: "delete",
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{ObjectType: "user", ObjectId: caller},
		},
		Consistency: &v1.Consistency{
			Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true},
		},
	})
	if err != nil {
		return fmt.Errorf("spicedbauthorizer: check delete %s: %w", resID, err)
	}
	if resp.GetPermissionship() != v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION {
		return fmt.Errorf("spicedbauthorizer: permission denied: delete %s", resID)
	}
	return nil
}

// CleanupScope bulk-deletes all memory_entry relationships whose resource ID
// starts with the scope prefix. Called by Local.DeleteScope. Idempotent.
func (a *Authorizer) CleanupScope(ctx context.Context, scope memory.Scope) error {
	prefix := ScopePrefix(scope)
	_, err := a.writer.DeleteRelationships(ctx,
		&v1.DeleteRelationshipsRequest{
			RelationshipFilter: &v1.RelationshipFilter{
				ResourceType:             "memory_entry",
				OptionalResourceIdPrefix: prefix,
			},
		})
	if err != nil {
		return fmt.Errorf("spicedbauthorizer: cleanup scope %s/%s: %w",
			scope.Kind, scope.ID, err)
	}
	return nil
}
