package judge

// registry.go is ConsequenceJudge's global lookup. Backends self-register via
// an init() (see deterministic.go's deterministicJudge); consumers resolve by
// name (registry-over-branching). Follows the same shape as
// pkg/agent/harness/registry/registry.go — kindregistry.New + thin
// Register/Get/All/Reset wrappers — but folded into this same package rather
// than a separate "judge/registry" subpackage, so this task's interface,
// registry, and built-in backend all live together in
// pkg/agent/tool/browser/judge. (The type arg to kindregistry.New is inferred
// from the ConsequenceJudge.Name method expression here, where the harness
// sibling spells it explicitly; a behavioral mirror, not a syntactic one.)
import "github.com/authzed/openagentprimitives/pkg/x/kindregistry"

var reg = kindregistry.New("consequence-judge", ConsequenceJudge.Name)

// Register adds a ConsequenceJudge backend. Panics on an empty name or a
// duplicate — both are programmer errors caught at process start, not
// runtime conditions.
func Register(j ConsequenceJudge) { reg.Register(j) }

// Get looks up a ConsequenceJudge by its Name().
func Get(name string) (ConsequenceJudge, bool) { return reg.Get(name) }

// All returns every registered ConsequenceJudge, sorted by name.
func All() []ConsequenceJudge { return reg.All() }

// Reset clears the registry (test-only).
func Reset() { reg.Reset() }
