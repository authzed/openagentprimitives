package spicedb

import (
	"context"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// RelWriter is the SpiceDB surface a component gets once it is bound to a
// relsource.Source, instead of the raw *authzed.Client the package used to
// hand out directly. This is the one seam every WriteRelationships /
// DeleteRelationships call goes through, so a guard placed here catches
// every writer that constructs one — see NewWriter and (*Client).Writer.
//
// WriteRelationships and DeleteRelationships are GUARDED: every update /
// delete filter is checked with relsource.CheckWrite / CheckDeleteFilter
// before it reaches SpiceDB, so a source can never touch a relation another
// source claims. Several sources hold real claims today
// (pkg/memory/pttagmint, pkg/memory/spicedbauthorizer,
// pkg/authz/guardian/approval, pkg/channels/channelkinds/slack,
// TypedWritesSource in this package), so those checks refuse a conflicting
// write — provided the binary constructing this RelWriter blank-imports
// pkg/authz/spicedb/relsource/imports; a binary that forgets to link that
// bundle has every guarded write refused instead of silently allowing all
// of them, per relsource.MarkComplete.
//
// CheckBulkPermissions, LookupSubjects and ExpandPermissionTree are
// unguarded pass-throughs. relsource is a WRITE guard by design (see its
// package doc) — reads are never gated, whatever is claimed. They live on
// this interface anyway because a component holding a writer needs them in
// the same breath as its writes, and *Client doesn't expose them in this
// shape: (*Client).LookupSubjects is a different, typed signature
// (LookupSubjects(ctx, subjectRef string) ([]string, error)), not this RPC.
//
// WriteRelationships/DeleteRelationships are option-free — no variadic
// grpc.CallOption tail — so a RelWriter structurally satisfies every
// no-option Writer-shaped interface in the repo
// (pkg/authz/guardian/grants.Writer, pkg/authz/relwrites.SpiceDBClient,
// spicedb.BootstrapWriter, pkg/memory/pttagmint.RelationshipWriter) with no
// adapter required. CheckBulkPermissions / LookupSubjects /
// ExpandPermissionTree keep the full v1.PermissionsServiceClient signature,
// opts tail included, because steelthread.PermissionExpander requires
// ExpandPermissionTree verbatim (opts and all) and nothing needs it
// stripped.
type RelWriter interface {
	// WriteRelationships is refused by relsource.CheckWrite when any update
	// names a relation another source claims.
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)
	// DeleteRelationships is refused by relsource.CheckDeleteFilter when the
	// filter could match a relation another source claims.
	DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error)

	// CheckBulkPermissions is an unguarded pass-through; see the type doc.
	CheckBulkPermissions(ctx context.Context, req *v1.CheckBulkPermissionsRequest, opts ...grpc.CallOption) (*v1.CheckBulkPermissionsResponse, error)
	// LookupSubjects is an unguarded pass-through; see the type doc. Not to
	// be confused with (*Client).LookupSubjects, a different, typed method.
	LookupSubjects(ctx context.Context, req *v1.LookupSubjectsRequest, opts ...grpc.CallOption) (v1.PermissionsService_LookupSubjectsClient, error)
	// ExpandPermissionTree is an unguarded pass-through; see the type doc.
	ExpandPermissionTree(ctx context.Context, req *v1.ExpandPermissionTreeRequest, opts ...grpc.CallOption) (*v1.ExpandPermissionTreeResponse, error)
}

// relWriter is the concrete, guarded RelWriter implementation, bound to one
// relsource.Source at construction.
type relWriter struct {
	cl  *authzed.Client
	src relsource.Source
}

// NewWriter returns a RelWriter delegating to cl, bound to src. Returns nil
// — a genuine nil RelWriter, not a typed-nil *relWriter boxed into the
// interface — when cl is nil, so a caller assigning the result into an
// interface field gets a real nil interface rather than one that panics on
// first use (see AGENTS.md "Nil interfaces: never assign a typed-nil
// pointer").
func NewWriter(cl *authzed.Client, src relsource.Source) RelWriter {
	if cl == nil {
		return nil
	}
	return &relWriter{cl: cl, src: src}
}

// WriteRelationships checks relsource.CheckWrite before dispatching. A
// refusal is returned to the caller unaltered — never logged and swallowed;
// see AGENTS.md "Never silently drop errors".
func (w *relWriter) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	if err := relsource.CheckWrite(w.src, req.GetUpdates()); err != nil {
		return nil, err
	}
	return w.cl.WriteRelationships(ctx, req)
}

// DeleteRelationships checks relsource.CheckDeleteFilter before dispatching.
func (w *relWriter) DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	if err := relsource.CheckDeleteFilter(w.src, req.GetRelationshipFilter()); err != nil {
		return nil, err
	}
	return w.cl.DeleteRelationships(ctx, req)
}

// CheckBulkPermissions passes straight through; reads are never guarded.
func (w *relWriter) CheckBulkPermissions(ctx context.Context, req *v1.CheckBulkPermissionsRequest, opts ...grpc.CallOption) (*v1.CheckBulkPermissionsResponse, error) {
	return w.cl.CheckBulkPermissions(ctx, req, opts...)
}

// LookupSubjects passes straight through; reads are never guarded.
func (w *relWriter) LookupSubjects(ctx context.Context, req *v1.LookupSubjectsRequest, opts ...grpc.CallOption) (v1.PermissionsService_LookupSubjectsClient, error) {
	return w.cl.LookupSubjects(ctx, req, opts...)
}

// ExpandPermissionTree passes straight through; reads are never guarded.
func (w *relWriter) ExpandPermissionTree(ctx context.Context, req *v1.ExpandPermissionTreeRequest, opts ...grpc.CallOption) (*v1.ExpandPermissionTreeResponse, error) {
	return w.cl.ExpandPermissionTree(ctx, req, opts...)
}
