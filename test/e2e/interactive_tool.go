//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
)

// ScriptedInteractiveTool is the e2e fake for an interactive tool (e.g. a
// stand-in "codey" for Claude Code). Tests register an initial emission +
// a FIFO list of OnStdin rules; Install wires the tool to a fake.Binder
// for a given pod key so the controller's StreamExec invokes our driver.
//
// Register all EmitInitial and OnStdin rules BEFORE calling Install.
// OnStdin rules can be appended later (the driver re-reads the rule list
// under lock per stdin chunk), but `initial` is read once at driver-start
// time — late EmitInitial calls won't fire.
type ScriptedInteractiveTool struct {
	t  TB
	mu sync.Mutex

	initial func(*Emitter)
	rules   []scriptedRule
	fired   int  // count of rules consumed
	started bool // set true the first time the Install-supplied driver runs
}

type scriptedRule struct {
	match func([]byte) bool
	emit  func(*Emitter)
}

// Emitter is passed to rule callbacks. Methods are concurrency-safe.
type Emitter struct {
	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer
	exitCh chan int32
}

// Stdout writes a chunk to the tool's stdout pipe.
func (e *Emitter) Stdout(b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.stdout.Write(b)
}

// Stderr writes a chunk to the tool's stderr pipe.
func (e *Emitter) Stderr(b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.stderr.Write(b)
}

// Exit ends the session with the given exit code; the driver returns and
// the bridge sees the gateway server's Exit message. Panics on double-call;
// test code should be loud about programming mistakes.
func (e *Emitter) Exit(code int32) {
	select {
	case e.exitCh <- code:
	default:
		panic("Emitter.Exit called more than once")
	}
}

// NewScriptedInteractiveTool builds an empty scripted tool bound to t.
func NewScriptedInteractiveTool(t TB) *ScriptedInteractiveTool {
	return &ScriptedInteractiveTool{t: t}
}

// EmitInitial schedules output before any stdin is read.
func (s *ScriptedInteractiveTool) EmitInitial(emit func(*Emitter)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initial = emit
}

// OnStdin registers a rule that fires on the next stdin chunk whose bytes
// match. FIFO.
func (s *ScriptedInteractiveTool) OnStdin(match func([]byte) bool, emit func(*Emitter)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, scriptedRule{match: match, emit: emit})
}

// AssertAllRulesConsumed fails the test if any OnStdin rule never fired.
// If the Install-supplied driver never ran, fails with a dedicated message
// pointing the test author at the most common cause (Install wired to the
// wrong podKey).
func (s *ScriptedInteractiveTool) AssertAllRulesConsumed() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rules) > 0 && !s.started {
		s.t.Fatalf("ScriptedInteractiveTool: driver never ran (rules registered but Install wired to wrong podKey?)")
		return
	}
	for i := s.fired; i < len(s.rules); i++ {
		s.t.Fatalf("ScriptedInteractiveTool: rule #%d (registered %d-th) was never matched", i, i)
		return
	}
}

// Install wires the scripted driver into fakeExec for podKey. After Install,
// any StreamExec for that key runs the scripted logic.
func (s *ScriptedInteractiveTool) Install(fakeExec *fake.Binder, podKey string) {
	fakeExec.ProgramStreamFunc(podKey, func(stdin io.Reader, stdout, stderr io.Writer) int32 {
		s.mu.Lock()
		s.started = true
		init := s.initial
		s.mu.Unlock()

		exitCh := make(chan int32, 1)
		em := &Emitter{stdout: stdout, stderr: stderr, exitCh: exitCh}

		// Initial emission.
		if init != nil {
			init(em)
		}

		// Stdin chunks: read line-by-line for predictable rule matching.
		// (Production tools that need binary stdin should use the lower-level
		// ProgramStreamFunc directly.)
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			chunk := append([]byte(nil), sc.Bytes()...)
			s.mu.Lock()
			var rule scriptedRule
			matched := false
			ruleIdx := -1
			for i := range s.rules {
				if i < s.fired {
					continue
				}
				if s.rules[i].match(chunk) {
					rule = s.rules[i]
					matched = true
					ruleIdx = i
					break
				}
			}
			s.mu.Unlock()
			if !matched {
				// No matching rule: drop the chunk silently. Use
				// AssertAllRulesConsumed to detect missing-rule bugs at end.
				continue
			}
			rule.emit(em)
			s.mu.Lock()
			if ruleIdx == s.fired {
				s.fired++
			}
			s.mu.Unlock()
			// If the rule called Exit, drain and return.
			select {
			case code := <-exitCh:
				return code
			default:
			}
		}
		// Stdin closed without an explicit exit.
		select {
		case code := <-exitCh:
			return code
		default:
			return 0
		}
	})
}

// StdinContains matches stdin chunks containing s (as substring).
func StdinContains(s string) func([]byte) bool {
	needle := []byte(s)
	return func(chunk []byte) bool { return bytes.Contains(chunk, needle) }
}

// StdinEqualsLine matches stdin chunks whose trimmed text equals s.
func StdinEqualsLine(s string) func([]byte) bool {
	return func(chunk []byte) bool { return strings.TrimSpace(string(chunk)) == s }
}
