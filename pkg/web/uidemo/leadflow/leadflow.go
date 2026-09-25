// Package leadflow is a fabricated CRM MCP server used to demonstrate the
// agent-defined-UI leads console. It carries no build tag, because both the
// e2e-tagged scenarios and the plain `go test` suite import it. It is not
// platform code — nothing in pkg/, cmd/oap, or any service depends on it — so it
// lives under the obviously-demo pkg/web/uidemo/ prefix, and outside examples/
// so tests may import it.
//
// Deployed shape: pkg/x/safehttp's SSRF guard refuses loopback/link-local/
// RFC1918-private destinations, so an MCPServer CR's spec.server.url cannot
// point at a cluster-internal Service. internal/cmd/leadflowfake is meant to run
// somewhere the cluster can reach by a public address; this package's own tests
// reach it over loopback through the plain-client seam (mcpdispatch's
// WithHTTPClient / mcpprobe.Client.HTTP), never through the guarded client.
package leadflow

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Lead is one row of the fabricated pipeline. The value/label pair is
// deliberately part of the ROW rather than a separate lookup: an ap:table
// binds rows and picks display columns by key, while an ap:select binds
// options and reads value/label, so one tool result serves both controls
// and the console needs no second "list the ids" tool.
type Lead struct {
	Value   string `json:"value"`   // stable id, also an ap:select option value
	Label   string `json:"label"`   // display text for both the table and the select
	Stage   string `json:"stage"`   // one of Stages
	Owner   string `json:"owner"`   // sales rep the lead is assigned to
	Updated string `json:"updated"` // RFC3339 date-time
}

// Stages is the closed pipeline vocabulary, in order. AdvanceStage moves a
// lead to the next entry and refuses to move past the last one.
var Stages = []string{"new", "qualified", "proposal", "won"}

// Book is the in-memory lead store. Safe for concurrent use: the console
// resolves several bindings concurrently on one page load.
type Book struct {
	mu    sync.RWMutex
	leads []Lead
}

// NewBook returns the deterministic seeded book (seed.go). Deterministic
// because a test asserting a stage count cannot depend on a clock.
func NewBook() *Book {
	return &Book{leads: seedLeads()}
}

// Filter returns a fresh copy of the leads whose Updated falls within [from, to]
// and whose Stage equals stage. An empty bound is unbounded on that side; an
// empty stage matches every stage. The RFC3339 bounds are compared
// LEXICOGRAPHICALLY, valid only because seed.go's timestamps share one timezone
// and precision, so string order and chronological order agree. Filter never
// errors — a malformed from/to is rejected by validateRange before reaching it.
//
// The returned slice is an independent copy: Lead is a plain value type, so a
// later AdvanceStage never retroactively changes a slice a caller holds.
func (b *Book) Filter(from, to, stage string) []Lead {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Lead, 0, len(b.leads))
	for _, l := range b.leads {
		if from != "" && l.Updated < from {
			continue
		}
		if to != "" && l.Updated > to {
			continue
		}
		if stage != "" && l.Stage != stage {
			continue
		}
		out = append(out, l)
	}
	return out
}

// AdvanceStage moves one lead one stage forward. An unknown id, or a lead
// already at the last stage, is an error — never a silent no-op, because
// the console surfaces the outcome on the control that fired it. The book
// is left unchanged on every error path.
func (b *Book) AdvanceStage(id string) (Lead, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.leads {
		if b.leads[i].Value != id {
			continue
		}
		idx := stageIndex(b.leads[i].Stage)
		if idx < 0 || idx == len(Stages)-1 {
			return b.leads[i], fmt.Errorf("lead %q is already at the last stage (%q)", id, b.leads[i].Stage)
		}
		b.leads[i].Stage = Stages[idx+1]
		return b.leads[i], nil
	}
	return Lead{}, fmt.Errorf("no lead with id %q", id)
}

func stageIndex(stage string) int {
	for i, s := range Stages {
		if s == stage {
			return i
		}
	}
	return -1
}

// Options configures a server instance.
type Options struct {
	Book *Book // nil ⇒ NewBook()
	// MaxRows caps how many leads list_leads returns. A SERVER-side constant,
	// not a tool argument: a binding parameter can only produce a JSON string
	// (uibindings.SubstituteParams) and the app-tool path has no InputSchema
	// validation to catch a string where a number is declared, so a row cap
	// expressed as an argument would be a trap rather than a demonstration.
	MaxRows int // 0 ⇒ defaultMaxRows
	// Logger receives a line for every tool call plus the bound address at
	// startup (internal/cmd/leadflowfake). nil ⇒ slog.Default().
	Logger *slog.Logger
}

// Call is one recorded tool invocation, exposed via Server.Calls so a test
// can prove the CRM observed specific arguments rather than inferring it
// from the response shape alone — a stub that ignores its arguments but
// returns a plausible narrower result would otherwise pass a
// rows-shrink-under-a-narrower-window assertion for the wrong reason.
type Call struct {
	Tool string
	Args map[string]any
}

// Server is the fake CRM. Handler serves MCP Streamable HTTP; mount it at
// the path the MCPServer CR's url names.
type Server struct {
	book    *Book
	maxRows int
	logger  *slog.Logger
	handler http.Handler

	mu    sync.Mutex
	calls []Call
}

// New builds a Server and registers its tools once against a fresh go-sdk
// mcp.Server.
func New(o Options) *Server {
	book := o.Book
	if book == nil {
		book = NewBook()
	}
	maxRows := o.MaxRows
	if maxRows == 0 {
		maxRows = defaultMaxRows
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{book: book, maxRows: maxRows, logger: logger}

	impl := &mcp.Implementation{Name: "leadflow", Version: "1.0.0"}
	srv := mcp.NewServer(impl, nil)
	registerTools(srv, s)
	s.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return s
}

// Handler returns the MCP Streamable HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Book returns the server's lead store, e.g. for a test to inspect state a
// tool call mutated.
func (s *Server) Book() *Book { return s.book }

// recordCall appends one observed invocation. Called by registerTools'
// shared wrapper for every tool, before the tool's own handler runs, so a
// call is recorded even if the handler goes on to return an error result.
func (s *Server) recordCall(tool string, args map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Tool: tool, Args: args})
}

// Calls returns a copy of every tool invocation this server has observed,
// in call order. Safe for concurrent use with in-flight tool calls (a real
// UI resolves several bindings concurrently on one page load).
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	copy(out, s.calls)
	return out
}
