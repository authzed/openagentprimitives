package sentinel_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
)

// TestTable_BothDirections_ForEveryRow is the drift guard between the two
// halves of the wire contract.
//
// The mapping used to exist twice — httpsrv.sentinelStatus chose the status,
// httpclient chose (for exactly one status) which sentinel to rebuild — and
// nothing connected them. A row present in one and absent from the other
// produced no failure anywhere: the sentinel simply stopped satisfying
// errors.Is once the call crossed a process boundary. Both sides read Table
// now, and this asserts every row is complete and usable in BOTH directions,
// so a half-added sentinel fails here rather than in production.
func TestTable_BothDirections_ForEveryRow(t *testing.T) {
	require.NotEmpty(t, sentinel.Table, "an empty table would make every assertion below vacuous")

	seenCode := map[string]bool{}
	seenMsg := map[string]bool{}

	for _, m := range sentinel.Table {
		t.Run(m.Code+": maps to a 4xx and round-trips error→code→error", func(t *testing.T) {
			require.NotNil(t, m.Err, "a row without a sentinel maps nothing")
			require.NotEmpty(t, m.Code, "a row without a code cannot be named on the wire, so the client can never rebuild it")

			// Server direction.
			status, code, ok := sentinel.StatusAndCode(m.Err)
			require.True(t, ok, "StatusAndCode must recognize a sentinel that is in the table")
			assert.Equal(t, m.Status, status, "the status must be the row's")
			assert.Equal(t, m.Code, code, "the code must be the row's")

			// Client direction.
			got, ok := sentinel.ForCode(m.Code)
			require.True(t, ok, "ForCode must resolve a code the server can stamp, or the client cannot rebuild this sentinel")
			assert.Same(t, m.Err, got, "the rebuilt sentinel must be the same value, not a look-alike")

			assert.GreaterOrEqual(t, m.Status, 400, "a sentinel is a permanent verdict")
			assert.Less(t, m.Status, 500,
				"5xx is retried eight times over ~17s by httpclient.do; a settled verdict must never be answered with one")

			assert.False(t, seenCode[m.Code], "codes are wire format and must be unique")
			seenCode[m.Code] = true
			assert.False(t, seenMsg[m.Err.Error()], "two rows for one sentinel would make the mapping order-dependent")
			seenMsg[m.Err.Error()] = true
		})
	}
}

// TestStatusAndCode_NonSentinel_IsAFault pins the default arm. Everything the
// table does not name is a fault the client should retry, and answering a
// backend blip 4xx would turn a momentary Postgres failure into a permanent
// refusal no caller retries.
func TestStatusAndCode_NonSentinel_IsAFault(t *testing.T) {
	_, _, ok := sentinel.StatusAndCode(assertAnError{})
	assert.False(t, ok, "an unrecognized error is not a sentinel")
}

type assertAnError struct{}

func (assertAnError) Error() string { return "dial tcp 10.0.0.9:5432: i/o timeout" }

// TestForCode_UnknownAndEmpty_NotResolved is the fail-closed half of the client
// direction: a response with no discriminator, or one from a newer server
// naming a code this build does not know, must NOT resolve to a sentinel. A 403
// from an intermediary proxy or from handleKG's approval door would otherwise
// be laundered into ErrMissingApproval and handed to the model as a
// platform-authored authorization verdict.
func TestForCode_UnknownAndEmpty_NotResolved(t *testing.T) {
	for _, code := range []string{"", "not_a_code", "MISSING_APPROVAL", " missing_approval"} {
		t.Run(strconv.Quote(code)+": not resolved", func(t *testing.T) {
			got, ok := sentinel.ForCode(code)
			assert.False(t, ok, "only an exact known code names a sentinel")
			assert.Nil(t, got, "a non-resolution must not hand back an error to wrap")
		})
	}
}

// notOnTheWire records the memory sentinels that deliberately have NO row in
// Table, with the reason. Its purpose is to make adding a sentinel a decision
// rather than an omission: TestEverySentinel_IsWiredOrExplicitlyExempt fails on
// any sentinel that is in neither place, so "I forgot the client half" — the
// exact defect this package exists to prevent — cannot happen quietly again.
//
// Every entry here is route-specific: httpsrv's putStatus and deleteStatus map
// them for the one route where they are meaningful, and no caller tests for
// them with errors.Is across the wire. Promote one to Table the day a caller
// needs to.
var notOnTheWire = map[string]string{
	"ErrInvalidEntry":       "Put-only; putStatus answers 400. No cross-wire caller tests for it.",
	"ErrAppendOnlyConflict": "Put-only; putStatus answers 409, a status no other route produces.",
	"ErrAppendOnlyKind":     "Delete-only; deleteStatus answers 403.",
	"ErrProvenanceRequired": "Put-only; putStatus answers 403 for an unsigned append-only write.",
	"ErrBadProvenance":      "Put-only; putStatus answers 403 for a forged one.",
	"ErrIndexConflict":      "Never crosses the wire: turn.Appender decides it client-side from a Query result.",
}

// TestEverySentinel_IsWiredOrExplicitlyExempt scans pkg/memory's own source for
// package-level Err… sentinels and requires each to be either in Table or in
// notOnTheWire.
//
// Matching is by message text, not by name: the table holds error VALUES, and
// comparing what the source declares against what the table actually maps is
// what makes this a drift test rather than a second transcription of the same
// list.
func TestEverySentinel_IsWiredOrExplicitlyExempt(t *testing.T) {
	declared := scanSentinels(t)
	require.NotEmpty(t, declared,
		"the scan found no sentinels at all — the parse is broken and every assertion below would be vacuous")

	wired := map[string]string{} // message → code
	for _, m := range sentinel.Table {
		wired[m.Err.Error()] = m.Code
	}

	for name, msg := range declared {
		t.Run(name+": has a wire mapping or a recorded reason for having none", func(t *testing.T) {
			if _, ok := wired[msg]; ok {
				assert.NotContains(t, notOnTheWire, name,
					"a sentinel cannot be both mapped and exempt")
				return
			}
			assert.Contains(t, notOnTheWire, name,
				"a new memory sentinel needs a row in sentinel.Table — giving it a status server-side "+
					"without a code leaves httpclient unable to rebuild it, so errors.Is silently stops "+
					"matching for every caller whose memory is an HTTP client (the runner's is). If it "+
					"genuinely does not belong on the wire, add it to notOnTheWire with the reason.")
		})
	}

	// The reverse direction: a table row whose sentinel is no longer declared
	// where this test looks means the scan has gone stale and stopped guarding.
	byMsg := map[string]bool{}
	for _, msg := range declared {
		byMsg[msg] = true
	}
	for _, m := range sentinel.Table {
		assert.True(t, byMsg[m.Err.Error()],
			"table row %q maps an error the scan did not find in pkg/memory; widen scanSentinels or the guard is blind",
			m.Code)
	}
	for name := range notOnTheWire {
		assert.Contains(t, declared, name,
			"notOnTheWire names a sentinel that no longer exists; drop the stale exemption")
	}
}

// scanSentinels parses pkg/memory's non-test sources and returns every
// package-level `Err… = errors.New("…")` as name → message.
//
// Only the package's own directory is scanned: sentinels a caller tests for
// across the wire are declared there, and a subpackage's error is that
// package's concern.
func scanSentinels(t *testing.T) map[string]string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join("..", "*.go"))
	require.NoError(t, err, "glob pkg/memory sources")

	out := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err, "parse %s", f)

		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Err") || i >= len(vs.Values) {
						continue
					}
					msg, ok := errorsNewLiteral(vs.Values[i])
					require.True(t, ok,
						"%s declares %s in a shape this drift test cannot read (expected errors.New(\"…\")); "+
							"teach scanSentinels about it rather than letting a sentinel slip past the guard",
						f, name.Name)
					out[name.Name] = msg
				}
			}
		}
	}
	return out
}

// errorsNewLiteral extracts the message from an `errors.New("literal")` call.
func errorsNewLiteral(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "New" {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "errors" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	msg, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return msg, true
}

// TestHeader_IsACanonicalHTTPHeaderName keeps the constant usable with
// Header.Get/Set, which canonicalize: a lowercase or underscored spelling would
// still work through those two calls and then quietly fail for anything reading
// the raw map.
func TestHeader_IsACanonicalHTTPHeaderName(t *testing.T) {
	assert.Equal(t, sentinel.Header, http.CanonicalHeaderKey(sentinel.Header),
		"Header must already be in canonical form")
}
