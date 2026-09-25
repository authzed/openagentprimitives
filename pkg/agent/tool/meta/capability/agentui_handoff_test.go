package capability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"

	// slack registers the kind the "an agent UI on Slack" rows name. The
	// equivalence matrix in this package already imports it directly; these
	// rows only need the registration, so the import is blank here.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// handoffSession builds an AgentSession whose input/output channel bindings
// carry the given kinds. An empty kind means that binding is absent, which is
// how a session with no channel at all (kubectl-driven) is expressed.
func handoffSession(t *testing.T, inputKind, outputKind string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-session"},
	}
	if inputKind != "" {
		s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "demo-channel", Kind: inputKind}
	}
	if outputKind != "" {
		s.Spec.OutputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "demo-out-channel", Kind: outputKind}
	}
	return s
}

// TestAgentUIHandoffOffer is the injection gate's decision table. It drives
// the REAL channel-kind registry — browser, local and slack are imported so
// their init registrations are live — because a stubbed IsBrowserSurface
// would prove the table and nothing about the gate.
func TestAgentUIHandoffOffer(t *testing.T) {
	const showAgentUI = "show_agent_ui"

	cases := []struct {
		name string
		// noUIView drops RunnerEnv.UIView: the class references no AgentUI.
		noUIView bool
		// noViewerCheck drops RunnerEnv.ViewerCanInteract: the runner wiring
		// this gate depends on is incomplete.
		noViewerCheck bool
		inputKind     string
		outputKind    string
		wantTools     []string
		wantSkip      bool
	}{
		{
			name:      "no agent UI: nothing offered, and nothing logged about it",
			noUIView:  true,
			inputKind: "slack",
			wantTools: []string{},
		},
		{
			name:      "an agent UI on Slack: the tool is offered",
			inputKind: "slack",
			wantTools: []string{showAgentUI},
		},
		{
			name:      "an agent UI on a browser channel: not offered, and not a skip",
			inputKind: browser.KindName,
			wantTools: []string{},
		},
		{
			name:      "an agent UI on a terminal: offered",
			inputKind: local.KindName,
			wantTools: []string{showAgentUI},
		},
		{
			name:      "no channel at all: nothing offered",
			wantTools: []string{},
		},
		{
			// The split-channel pair is the reason the gate reads the session's
			// OUTBOUND binding rather than OfferContext.Binding (which is the
			// INPUT channel). Delete these two rows and the gate can be
			// switched to o.Binding.Kind with every other row still green.
			name:       "a split-channel session is gated on the output channel: browser in, Slack out",
			inputKind:  browser.KindName,
			outputKind: "slack",
			wantTools:  []string{showAgentUI},
		},
		{
			name:       "…and the other direction: Slack in, browser out",
			inputKind:  "slack",
			outputKind: browser.KindName,
			wantTools:  []string{},
		},
		{
			// Deliberate, and pinned so a later "fail closed on unknown kinds"
			// edit has to argue with it: the registry answers false for an
			// unregistered name because a redundant link offer is cosmetic,
			// while withholding one leaves a Slack or terminal viewer unable to
			// reach the page at all.
			name:      "an unregistered kind name is not a browser surface: offered",
			inputKind: "no-such-kind",
			wantTools: []string{showAgentUI},
		},
		{
			name:          "the viewer check is unwired: a loud skip, not a silent absence",
			noViewerCheck: true,
			inputKind:     "slack",
			wantTools:     []string{},
			wantSkip:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := RunnerEnv{
				UIView:            &uiview.Runtime{Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui"},
				NATSPublish:       func(context.Context, string, []byte) error { return nil },
				ViewerCanInteract: func(context.Context) (bool, error) { return true, nil },
			}
			if tc.noUIView {
				env.UIView = nil
			}
			if tc.noViewerCheck {
				env.ViewerCanInteract = nil
			}
			sess := handoffSession(t, tc.inputKind, tc.outputKind)

			got, skip := agentUIHandoffCapability{}.Offer(OfferContext{
				Ctx:     context.Background(),
				Session: sess,
				// Binding mirrors what internal/cmd/runner passes: the INPUT channel.
				// The gate must not read it.
				Binding: sess.Spec.InputChannel,
				Env:     env,
			})

			// Two independent facts, so both are reported: a gate that offers
			// the right tools while logging a spurious skip (or the reverse) is
			// still wrong, and one failure must not hide the other.
			assert.Equal(t, tc.wantTools, toolNames(got), "tools offered")
			if tc.wantSkip {
				if assert.NotNil(t, skip, "an incomplete runner wiring must be reported, not silently absent") {
					assert.Equal(t, "agent_ui_handoff", skip.Capability, "the skip must name the capability that declined")
					assert.NotEmpty(t, skip.Reason, "the skip must carry a reason an operator can act on")
				}
			} else {
				assert.Nil(t, skip, "a normal, common state must not put a skip line in the log")
			}
		})
	}
}

// TestAgentUIHandoffIsInfrastructural pins the activation shape the design
// chose: the gate is the UI's existence, so the capability must run Offer with
// no spec.capabilities entry. A capability that quietly became opt-in would
// make show_agent_ui unreachable for every existing AgentClass.
//
// The Offer table above cannot see this: it calls Offer directly, so it stays
// green whatever the activation shape says. The behavioural half is the
// disabled-grant case in TestAgentUIHandoffIsOfferedThroughAssemble; this test
// is the declaration, stated separately so the failure names the property.
func TestAgentUIHandoffIsInfrastructural(t *testing.T) {
	c, ok := Lookup("agent_ui_handoff")
	require.True(t, ok, "agent_ui_handoff must be registered by init")
	assert.True(t, c.Infrastructural(), "must bypass the grant gate")
	assert.True(t, c.DefaultOn(), "must not disagree with Infrastructural")
}

// TestAgentUIHandoffIsOfferedThroughAssemble spans the gate to the seam both
// binaries actually drive, and pins the consequence of the activation shape
// rather than the shape itself. Offer being correct is not the same claim as
// the tool reaching the merged tool list: only Assemble runs the grant gate an
// infrastructural capability bypasses.
//
// The disabled-grant case is the one that bites. Being default-on is not
// enough on its own — a default-on capability that a class lists with
// {"enabled": false} is switched off, and this one has no off switch by
// design: an agent that has a UI can point at it. Without that case, being
// infrastructural is unobservable here, because default-on alone reproduces
// every other row.
func TestAgentUIHandoffIsOfferedThroughAssemble(t *testing.T) {
	sess := handoffSession(t, "slack", "")
	deps := AssembleDeps{
		// A class granting NOTHING: the tool must still arrive.
		Class:   classGranting(t),
		Session: sess,
		Binding: sess.Spec.InputChannel,
		Env: RunnerEnv{
			UIView:            &uiview.Runtime{Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui"},
			NATSPublish:       func(context.Context, string, []byte) error { return nil },
			ViewerCanInteract: func(context.Context) (bool, error) { return true, nil },
			// A non-nil ResolveErr is how a channel-attached fixture opts the
			// two resolve-dependent capabilities (mention_lookup,
			// channel_history) out cleanly — the same escape the equivalence
			// matrix uses, since a nil ResolvedChannel with a nil ResolveErr is
			// a combination production never produces and neither guards.
			ResolveErr: errors.New("this fixture resolves no channel"),
		},
		Logger: testLogger(t),
	}
	assert.Contains(t, toolNames(Assemble(context.Background(), deps)), "show_agent_ui",
		"an AgentClass granting no capabilities at all must still receive show_agent_ui when it has a UI on a non-browser channel")

	// The accepted consequence of the activation shape, asserted rather than
	// only written down: there is no off switch. A class that tries to turn the
	// capability off still gets the tool.
	deps.Class = &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{
		Capabilities: map[string]apiextensionsv1.JSON{"agent_ui_handoff": {Raw: []byte(`{"enabled":false}`)}},
	}}
	assert.Contains(t, toolNames(Assemble(context.Background(), deps)), "show_agent_ui",
		`spec.capabilities {"enabled": false} must not remove show_agent_ui — offering the page is inherent to having one`)

	// The negative control in the same shape, so the two assertions above are
	// not satisfied by a tool list that would contain show_agent_ui regardless.
	deps.Env.UIView = nil
	assert.NotContains(t, toolNames(Assemble(context.Background(), deps)), "show_agent_ui",
		"a class with no agent UI must not receive show_agent_ui")
}

// ---------------------------------------------------------------------------
// The wiring tripwire.
//
// Adding a field to RunnerEnv breaks nothing: every construction site is a
// struct literal, so a site that never assigns the new field still compiles,
// still vets — under every build tag — and still passes every suite, while the
// capability that needs it declines on every session. The e2e in-process
// factory is where that hides best, because it is behind //go:build e2e and
// stands in for the runner in every scenario.
//
// PinAttachment is the standing proof this is not hypothetical: internal/cmd/runner
// assigns it, test/e2e does not, and show_attachment is therefore absent from
// every e2e scenario with nothing saying so.
// ---------------------------------------------------------------------------

// A RunnerEnv site falls into one of two categories, and which one it is
// decides what may be required of it.
//
// runnerEnvRunnerSites RUN the tools they assemble: they drive a turn loop,
// dispatch calls, and publish results. Every runtime field is meaningful for
// them, so the wiring guards below hold them to it, and both must agree — the
// e2e factory's whole purpose is that it and the runner cannot drift.
//
// runnerEnvAssemblyOnlySites build a RunnerEnv for ONE question — "which tools
// would this class be offered?" — and never dispatch a single one. `oap session
// capture` is the first: a steelthread capture has to know which names in a
// recorded transcript are meta tools that run for real at replay, and
// capability.Assemble is the only thing that can answer, because the answer
// depends on the class's own grants (see steelthread.SelfCheckInput.MetaTools).
// For such a site NATSPublish and LeakageGate are not merely unset — they are
// MEANINGLESS: there is no NATS connection to publish a result to and no reply
// whose audience could leak, because nothing is executed. Requiring them would
// mean inventing wiring the site does not use, and a stubbed closure is exactly
// what classifyRunnerEnvValue refuses to call "wired" everywhere else here.
//
// The distinction is DISPATCH, not package or binary. If a third caller
// appears, ask one question: does it ever call a tool it assembled? Yes => a
// runner site, and wire every field. No => assembly-only, and say here what it
// assembles for.
//
// Both categories are checked by TestRunnerEnvProductionSitesIsComplete, so a
// site in neither list still fails.
var (
	runnerEnvRunnerSites       = []string{"internal/cmd/runner", "test/e2e"}
	runnerEnvAssemblyOnlySites = []string{"cmd/oap/internal/sessioncmd"}
)

// runnerEnvProductionSites is every site in either category — the set the tree
// scan below is compared against.
var runnerEnvProductionSites = append(
	append([]string{}, runnerEnvRunnerSites...), runnerEnvAssemblyOnlySites...)

// viewerCanInteractWiring is a LEDGER of which production sites assign
// RunnerEnv.ViewerCanInteract, not a preference. The test below fails when
// reality diverges from it in EITHER direction, so the change that wires the
// field has to come here and write down what it did — which is the moment
// somebody notices whether the other site was wired too.
//
// If you are here because this test failed after wiring one binary: wire the
// other one as well, then update this map. A site left false ships a runner
// that offers show_agent_ui and a harness that silently never does.
//
// An assembly-only site answers false, and that answer has a real consequence
// rather than being a formality: `oap session capture` never learns that
// show_agent_ui was offered, so a captured session that called it is refused
// with a sandbox-tool-call finding naming the tool. Note WHERE that decline
// happens, because it is not this field: Offer returns early and silently on
// Env.UIView being nil, which an assembly-only site also does not populate, so
// the tool is never reached rather than skipped-with-a-reason. Wiring either
// field with a stub would make the capture claim a tool was offered on evidence
// it does not have; the honest fix, if this gap ever bites, is for the capture
// to resolve the class's AgentUI and populate UIView for real.
var viewerCanInteractWiring = map[string]bool{
	"internal/cmd/runner":         true,
	"test/e2e":                    true,
	"cmd/oap/internal/sessioncmd": false,
}

func TestViewerCanInteractWiringMatchesTheLedger(t *testing.T) {
	for _, site := range runnerEnvProductionSites {
		want, listed := viewerCanInteractWiring[site]
		// A missing row must FAIL rather than read as "not required": the map's
		// zero value is false, so a new site added to runnerEnvProductionSites
		// without a row would otherwise be green while shipping the very gap
		// this ledger exists to catch.
		if !assert.True(t, listed,
			"%s builds a RunnerEnv but has no viewerCanInteractWiring row; answer for it "+
				"explicitly rather than letting the map's zero value answer for you", site) {
			continue
		}
		got := runnerEnvAssignedFields(t, site)["ViewerCanInteract"]

		assert.Equal(t, want, got == assignFuncLit,
			"%s: RunnerEnv.ViewerCanInteract is %s, but the ledger in this file says wired=%v. "+
				"Wire it at BOTH %v (the agent_ui_handoff capability declines, with a logged skip, "+
				"wherever it is missing) and then update viewerCanInteractWiring.",
			site, got, want, runnerEnvProductionSites)
		// Independent of the ledger's answer, and asserted separately because
		// it fails for a different reason: a name bound elsewhere satisfies
		// "not the nil literal" while being nil in production. This walk
		// cannot follow it, so it refuses to guess.
		assert.NotEqual(t, assignName, got,
			"%s assigns RunnerEnv.ViewerCanInteract a name bound elsewhere. This pin cannot see "+
				"whether that name is nil, and a nil there is indistinguishable — at runtime — from "+
				"never assigning the field: show_agent_ui is silently never offered. Write the "+
				"closure at the literal, or replace this pin with a test that calls the field.", site)
	}
}

// TestRunnerEnvWiringDetectorFires guards the guard. A detector that finds
// nothing reads as "everything is wired" and proves nothing, so it is checked
// against fields that are known-assigned today, one per syntactic form it has
// to understand.
func TestRunnerEnvWiringDetectorFires(t *testing.T) {
	for _, site := range runnerEnvRunnerSites {
		fields := runnerEnvAssignedFields(t, site)
		assert.Equal(t, assignName, fields["NATSPublish"],
			"%s assigns RunnerEnv.NATSPublish a name inside the composite literal; a detector "+
				"that cannot see it cannot see a missing field either", site)
		assert.Equal(t, assignFuncLit, fields["LeakageGate"],
			"%s assigns RunnerEnv.LeakageGate a closure written at the literal — the one form "+
				"this walk can prove non-nil, and the form the ledger requires", site)
		assert.Equal(t, assignAbsent, fields["NoSuchRunnerEnvField"],
			"%s: the detector must not report a field nobody assigns", site)
	}
	// The negative half of the same claim, and the reason the category exists.
	// An assembly-only site must NOT carry the dispatch wiring: a stub assigned
	// only to satisfy the loop above would read as wired here while being a
	// no-op in the one place it could matter.
	for _, site := range runnerEnvAssemblyOnlySites {
		fields := runnerEnvAssignedFields(t, site)
		assert.Equal(t, assignAbsent, fields["NATSPublish"],
			"%s is listed as assembly-only, so it must not publish: it never dispatches a tool, "+
				"and a NATSPublish here would be wiring invented to satisfy a test. If it now "+
				"runs tools, move it to runnerEnvRunnerSites and wire every field", site)
		assert.Equal(t, assignAbsent, fields["LeakageGate"],
			"%s is listed as assembly-only, so it produces no reply whose audience could leak. "+
				"A LeakageGate here would gate nothing", site)
	}
	// The e2e factory backfills two fields AFTER the literal (env.Artifacts =
	// …), a form the detector has to handle separately from literal keys.
	assert.Equal(t, assignConstructed, runnerEnvAssignedFields(t, "test/e2e")["Artifacts"],
		"the detector must see fields assigned after the composite literal, not only literal keys")
}

// TestRunnerEnvProductionSitesIsComplete closes the ledger's other end. Every
// guard above iterates runnerEnvProductionSites, so a THIRD binary that builds
// a RunnerEnv is invisible to all of them — its author has no reason to learn
// this file exists, and the suite stays green while the new binary never
// offers show_agent_ui. That is the same silent-absence failure one level up.
//
// So the list is checked against the tree rather than trusted: any package
// outside it that constructs a capability.RunnerEnv fails here, with the
// instruction to add it and answer for it in the ledger.
func TestRunnerEnvProductionSitesIsComplete(t *testing.T) {
	assert.ElementsMatch(t, runnerEnvProductionSites, runnerEnvSitesInTree(t),
		"every non-test package constructing a capability.RunnerEnv must be listed in "+
			"runnerEnvProductionSites and answered for in viewerCanInteractWiring; a site "+
			"missing from the list is checked by nothing")
}

// runnerEnvSitesInTree returns the module-relative directories of every
// non-test Go file that constructs a capability.RunnerEnv.
//
// Byte-scanned before parsing: the module is a few thousand files and only a
// handful mention the type, so parsing all of them would make this guard slow
// enough that somebody eventually skips it.
func runnerEnvSitesInTree(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	seen := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skipped by name: none of these can hold a Go production site,
			// and node_modules alone is large enough to dominate the walk.
			//
			// .claude holds git worktrees — whole checkouts of this same
			// module nested under the root. Their production sites are real
			// files, so the walk reports them as extra elements and the exact
			// -set assertion below fails, once per worktree, for a tree whose
			// own sources are correct. Only THIS module's sites are the
			// subject; a sibling checkout is somebody else's tree.
			switch d.Name() {
			case ".git", ".claude", "node_modules", "testdata", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, []byte("RunnerEnv")) {
			return nil
		}
		// Parsed with the file's own source: go/parser ignores build
		// constraints, so test/e2e's //go:build e2e factory is seen here
		// exactly as it is by runnerEnvAssignedFields.
		f, perr := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		alias := capabilityImportAlias(f)
		if alias == "" || !buildsRunnerEnv(f, alias) {
			return nil
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		seen[rel] = true
		return nil
	})
	require.NoError(t, err, "walking the module for RunnerEnv construction sites must succeed")

	sites := make([]string, 0, len(seen))
	for dir := range seen {
		sites = append(sites, dir)
	}
	sort.Strings(sites)
	return sites
}

// buildsRunnerEnv reports whether f contains a composite literal of the
// capability package's RunnerEnv, under the alias f binds it to.
func buildsRunnerEnv(f *ast.File, alias string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isRunnerEnvType(lit.Type, alias) {
			return true
		}
		found = true
		return false
	})
	return found
}

// runnerEnvAssignment classifies HOW a production site assigns a RunnerEnv
// field. Two states are not enough. A bare `nil` is not the only way to leave
// a field nil: `ViewerCanInteract: viewerCheckFn`, where the helper binding
// that name returns nil when some precondition is unmet, reads as wiring and
// is nil in production — the capability then declines on every session, and a
// walk that only rejected the `nil` token would call that wired.
type runnerEnvAssignment int

const (
	// assignAbsent: the field appears nowhere at this site.
	assignAbsent runnerEnvAssignment = iota
	// assignNilLiteral: the bare `nil`, which the capability treats exactly
	// like never assigning the field at all.
	assignNilLiteral
	// assignName: an identifier or selector — a value bound elsewhere. It may
	// be a live closure or it may be nil; this walk is syntactic and cannot
	// tell, so it reports the ambiguity rather than guessing either way.
	assignName
	// assignConstructed: the value is built AT the assignment (a call, a
	// composite literal, an address-of). Not a proof of non-nil — a
	// constructor can return nil — but its origin is visible at the site.
	assignConstructed
	// assignFuncLit: a function literal written at the assignment. The only
	// form this walk can PROVE non-nil, which is why the ViewerCanInteract
	// ledger requires it: a func literal is never a nil func.
	assignFuncLit
)

func (a runnerEnvAssignment) String() string {
	switch a {
	case assignNilLiteral:
		return "assigned the bare nil literal"
	case assignName:
		return "assigned a name bound elsewhere"
	case assignConstructed:
		return "assigned a value constructed at the site"
	case assignFuncLit:
		return "assigned a closure written at the site"
	default:
		return "never assigned"
	}
}

// classifyRunnerEnvValue maps an assigned expression onto the states above.
func classifyRunnerEnvValue(val ast.Expr) runnerEnvAssignment {
	switch v := val.(type) {
	case *ast.FuncLit:
		return assignFuncLit
	case *ast.Ident:
		if v.Name == "nil" {
			return assignNilLiteral
		}
		return assignName
	case *ast.SelectorExpr:
		return assignName
	default:
		return assignConstructed
	}
}

// runnerEnvAssignedFields reports, for every capability.RunnerEnv field
// assigned anywhere in the package rooted at the module-relative dir, the
// strongest form it is assigned in.
//
// Deliberately syntactic: go/parser ignores build constraints, so test/e2e's
// //go:build e2e factory — the file a type-checking walk would need extra tags
// to see at all — is scanned like any other.
func runnerEnvAssignedFields(t *testing.T, dir string) map[string]runnerEnvAssignment {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join(repoRoot(t), dir), func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err, "parsing %s must succeed", dir)
	require.NotEmpty(t, pkgs, "%s must contain non-test Go sources", dir)

	assigned := map[string]runnerEnvAssignment{}
	record := func(field string, val ast.Expr) {
		// Strongest form wins: a field assigned twice — once in the literal,
		// once backfilled after it — is as wired as its best assignment.
		if got := classifyRunnerEnvValue(val); got > assigned[field] {
			assigned[field] = got
		}
	}

	for _, p := range pkgs {
		for _, f := range p.Files {
			// alias is the identifier this file binds the capability package
			// to; without it no RunnerEnv literal can appear here.
			alias := capabilityImportAlias(f)
			if alias == "" {
				continue
			}
			isRunnerEnv := func(e ast.Expr) bool { return isRunnerEnvType(e, alias) }

			// Pass 1: literal keys, plus the identifiers holding a RunnerEnv.
			holders := map[string]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.CompositeLit:
					if !isRunnerEnv(s.Type) {
						return true
					}
					for _, el := range s.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok {
							record(k.Name, kv.Value)
						}
					}
				case *ast.AssignStmt:
					for i, rhs := range s.Rhs {
						lit, ok := rhs.(*ast.CompositeLit)
						if !ok || !isRunnerEnv(lit.Type) || i >= len(s.Lhs) {
							continue
						}
						if id, ok := s.Lhs[i].(*ast.Ident); ok {
							holders[id.Name] = true
						}
					}
				case *ast.ValueSpec:
					if s.Type != nil && isRunnerEnv(s.Type) {
						for _, id := range s.Names {
							holders[id.Name] = true
						}
					}
				}
				return true
			})

			// Pass 2: fields assigned on a holder after the literal.
			ast.Inspect(f, func(n ast.Node) bool {
				s, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range s.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || i >= len(s.Rhs) {
						continue
					}
					if base, ok := sel.X.(*ast.Ident); ok && holders[base.Name] {
						record(sel.Sel.Name, s.Rhs[i])
					}
				}
				return true
			})
		}
	}
	return assigned
}

// capabilityImportAlias returns the identifier f binds the capability package
// to, or "" when f does not import it — in which case no RunnerEnv composite
// literal can appear in it.
func capabilityImportAlias(f *ast.File) string {
	const capPath = `"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"`
	for _, imp := range f.Imports {
		if imp.Path.Value != capPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "capability"
	}
	return ""
}

// isRunnerEnvType reports whether e names the capability package's RunnerEnv
// under the given import alias.
func isRunnerEnvType(e ast.Expr, alias string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "RunnerEnv" {
		return false
	}
	base, ok := sel.X.(*ast.Ident)
	return ok && base.Name == alias
}

// repoRoot resolves the module root so the walk above does not depend on this
// test file's depth in the tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	require.NoError(t, err, "go list -m must resolve the module root")
	return strings.TrimSpace(string(out))
}
