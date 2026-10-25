package mcpfront

import (
	"context"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Deps is what internal/cmd/webd's umbrella must implement for the /mcp
// plugin to mount. ui.Routes casts the generic webui.Deps to this interface;
// a failed cast, or a nil AccessTokenAuthz(), means NO routes are returned at
// all — an unconfigured SpiceDB yields no MCP surface, fail closed by
// absence rather than by every handler call refusing one request at a time.
type Deps interface {
	K8s() client.Client
	// AccessTokenAuthz is the SpiceDB half of an authorized tool call (the
	// three-legged intersection check, the role mirror, and the scope
	// filter). nil means SpiceDB is not configured.
	AccessTokenAuthz() AccessTokenAuthz
	// AccessTokenNamespace is where AccessToken CRs live — the SAME
	// namespace pkg/web/mcpfront's Minter creates them in (webd's
	// --accesstoken-namespace), so the bearer middleware's hash lookup finds
	// every token a mint on this process could have produced.
	AccessTokenNamespace() string
	OperatorURL() string
	MemoryToken() string
	Artifacts() *artifacts.Service
	// LookupReadableSessions answers which sessions owner may read. v1
	// enumerates the interactable set — interact implies read_transcript for
	// a root session — rather than a dedicated read-only lookup; see the
	// webd implementation's own doc comment.
	LookupReadableSessions(ctx context.Context, owner identity.CanonicalUserID) (spicedb.InteractableSessions, error)
	// FetchArtifact fetches an artifact's rendered content bytes: resolve
	// artifactID to its head revision's render, then fetch that render's
	// bytes, the same two-step webd's own ContentRender/FetchRender pair does
	// for the browser live-view. Returns the bytes and the render's MIME type.
	FetchArtifact(ctx context.Context, ns, name, artifactID string) ([]byte, string, error)
	// ExternalBaseURL is the trusted-origin base URL this process is reached
	// at. Used both to build the protected-resource metadata's "resource"/
	// "authorization_servers" fields and the WWW-Authenticate challenge's
	// resource_metadata URL every 401 carries.
	ExternalBaseURL() string
	Logger() logr.Logger
}

// AccessTokenAuthz is the SpiceDB half of an authorized /mcp tool call.
// Declared here (not imported as spicedb.Client directly) so a fake is
// trivial to construct in tests — the same shape as mint.go's GrantWriter.
// Satisfied by *spicedb.Client; see the compile-time guard below.
type AccessTokenAuthz interface {
	CheckAccessTokenOp(ctx context.Context, chk spicedb.AccessTokenCheck, fullyConsistent bool) (spicedb.AccessTokenDecision, error)
	CheckAccessTokenMirror(ctx context.Context, tokenID string, owner identity.CanonicalUserID, permission string, fullyConsistent bool) (bool, error)
	FilterAccessTokenCoveredClasses(ctx context.Context, tokenID string, classIDs []string, fullyConsistent bool) (map[string]bool, error)
}

// var _ AccessTokenAuthz = (*spicedb.Client)(nil) is a compile-time proof
// that *spicedb.Client still satisfies this interface — a signature drift on
// either side fails `go build`, not a runtime deps.(AccessTokenAuthz)
// assertion deep inside webd's artifactViewDeps.AccessTokenAuthz nil-guard.
var _ AccessTokenAuthz = (*spicedb.Client)(nil)
