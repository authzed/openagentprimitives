package spicedb

// Coverage for SchemaIOAdapter and the bootstrap BootstrapWriter adapters.
//
// SchemaIOAdapter.ReadSchema re-prepends the `use expiration` directive that
// SpiceDB v1.52+ strips from rendered schema text. Its own doc comment calls
// that "a deploy-time-only failure invisible in unit tests that mock SchemaIO"
// — so these tests do NOT mock SchemaIO. They stand up a real gRPC
// SchemaService and drive the adapter through it, which is the only way the
// strip/re-prepend round trip is actually exercised.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// scriptedSchemaServer serves the SchemaService with canned text/errors and
// records what WriteSchema was handed.
type scriptedSchemaServer struct {
	v1.UnimplementedSchemaServiceServer

	mu        sync.Mutex
	readText  string
	readErr   error
	writeErr  error
	writeSeen []string
}

func (s *scriptedSchemaServer) ReadSchema(context.Context, *v1.ReadSchemaRequest) (*v1.ReadSchemaResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	return &v1.ReadSchemaResponse{SchemaText: s.readText}, nil
}

func (s *scriptedSchemaServer) WriteSchema(_ context.Context, req *v1.WriteSchemaRequest) (*v1.WriteSchemaResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeSeen = append(s.writeSeen, req.GetSchema())
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	return &v1.WriteSchemaResponse{}, nil
}

func (s *scriptedSchemaServer) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.writeSeen))
	copy(out, s.writeSeen)
	return out
}

// newSchemaClient starts a local gRPC SchemaService and returns a Client dialed
// against it plus the scripted server.
func newSchemaClient(t *testing.T) (*Client, *scriptedSchemaServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")

	srv := grpc.NewServer()
	sch := &scriptedSchemaServer{}
	v1.RegisterSchemaServiceServer(srv, sch)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("schema server exited: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	c, err := NewClient(ln.Addr().String(), "test-token", true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c, sch
}

// TestSchemaIOAdapter_ReadSchema_UseExpirationDirective is the regression this
// adapter exists for: SpiceDB strips `use expiration` from rendered text, and
// the guardian composer emits relation lines whose syntax requires it. Without
// the re-prepend the composer round-trips its own output into a schema SpiceDB
// rejects on the next WriteSchema.
func TestSchemaIOAdapter_ReadSchema_UseExpirationDirective(t *testing.T) {
	cases := []struct {
		name       string
		serverText string
		wantPrefix bool // true ⇒ the directive must have been added
	}{
		{
			name:       "directive stripped by SpiceDB: re-prepended",
			serverText: "definition user {}\n",
			wantPrefix: true,
		},
		{
			name:       "directive already present: not duplicated",
			serverText: "use expiration\n\ndefinition user {}\n",
			wantPrefix: false,
		},
		{
			name:       "empty schema text: directive still added",
			serverText: "",
			wantPrefix: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, sch := newSchemaClient(t)
			sch.readText = tc.serverText

			got, err := SchemaIOFor(c).ReadSchema(context.Background())
			require.NoError(t, err, "ReadSchema")

			assert.True(t, strings.HasPrefix(got, "use expiration"),
				"the composer requires the directive to be live at compose time")
			assert.Equal(t, 1, strings.Count(got, "use expiration"),
				"the directive must appear exactly once, never doubled")
			if tc.wantPrefix {
				assert.Equal(t, "use expiration\n\n"+tc.serverText, got)
			} else {
				assert.Equal(t, tc.serverText, got, "already-compliant text must pass through byte-identical")
			}
		})
	}
}

func TestSchemaIOAdapter_ReadSchema_BackendErrorSurfaces(t *testing.T) {
	c, sch := newSchemaClient(t)
	sch.readErr = errors.New("schema unavailable")

	got, err := SchemaIOFor(c).ReadSchema(context.Background())
	require.Error(t, err, "an unreadable schema must not be reported as an empty one")
	assert.Empty(t, got, "a failed read must not hand back text the composer would treat as the live schema")
}

func TestSchemaIOAdapter_WriteSchema(t *testing.T) {
	t.Run("text is forwarded to SpiceDB verbatim", func(t *testing.T) {
		c, sch := newSchemaClient(t)
		const text = "use expiration\n\ndefinition user {}\n"

		require.NoError(t, SchemaIOFor(c).WriteSchema(context.Background(), text))
		assert.Equal(t, []string{text}, sch.written(),
			"the composer's exact output must reach SpiceDB unmodified")
	})
	t.Run("backend error surfaces so the caller cannot assume the schema landed", func(t *testing.T) {
		c, sch := newSchemaClient(t)
		sch.writeErr = errors.New("write rejected")

		require.Error(t, SchemaIOFor(c).WriteSchema(context.Background(), "definition user {}"))
	})
}

// TestFetchAndParseSchema_ThroughLiveReader covers the AgentClass-side
// validation path end to end: read the live text, parse it, and answer
// definition/permission queries against it.
func TestFetchAndParseSchema_ThroughLiveReader(t *testing.T) {
	c, sch := newSchemaClient(t)
	sch.readText = "definition user {}\n\ndefinition doc {\n\trelation reader: user\n\tpermission view = reader\n}\n"

	got, err := FetchAndParseSchema(context.Background(), c)
	require.NoError(t, err, "FetchAndParseSchema")
	require.NotNil(t, got)

	assert.True(t, got.HasDefinition("doc"))
	assert.False(t, got.HasDefinition("absent"))
	assert.Equal(t, PermissionResolutionPermission, got.ResolvePermission("doc", "view"))
	assert.Equal(t, PermissionResolutionRelation, got.ResolvePermission("doc", "reader"))
	assert.Equal(t, PermissionResolutionNotFound, got.ResolvePermission("doc", "absent"))
}

// TestFetchAndParseSchema_UnparseableSchemaIsAnError guards the "cannot
// validate" reading: a schema that will not compile must refuse, never be
// treated as an empty schema in which every definition is absent.
func TestFetchAndParseSchema_UnparseableSchemaIsAnError(t *testing.T) {
	c, sch := newSchemaClient(t)
	sch.readText = "this is not a schema {{{"

	got, err := FetchAndParseSchema(context.Background(), c)
	require.Error(t, err, "garbage must not parse into an empty-but-usable Schema")
	assert.Nil(t, got)
}

func TestPermissionResolution_String(t *testing.T) {
	cases := []struct {
		name string
		in   PermissionResolution
		want string
	}{
		{name: "permission resolves to \"Permission\"", in: PermissionResolutionPermission, want: "Permission"},
		{name: "relation resolves to \"Relation\"", in: PermissionResolutionRelation, want: "Relation"},
		{name: "not-found resolves to \"NotFound\"", in: PermissionResolutionNotFound, want: "NotFound"},
		{name: "an out-of-range value degrades to \"NotFound\" rather than a bare number", in: PermissionResolution(99), want: "NotFound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.in.String())
		})
	}
}

// TestNewGrantWriter_NilClientYieldsNilPointer guards the AGENTS.md typed-nil
// rule at its documented call site: NewGrantWriter(nil) must return a nil
// *GrantWriter so a caller assigning it into an interface field gets a genuine
// nil interface rather than a non-nil one that panics on first use.
func TestNewGrantWriter_NilClientYieldsNilPointer(t *testing.T) {
	assert.Nil(t, NewGrantWriter(nil),
		"a nil inner writer must yield a nil *GrantWriter, not a non-nil wrapper around nil")

	c := erroringClient(t)
	w := c.Writer(relsource.Source{Name: "spicedbtest"})
	require.NotNil(t, w, "a dialed client's Writer must be non-nil")
	assert.NotNil(t, NewGrantWriter(w), "a real writer must yield a usable adapter")
}
