// Package labelextract evaluates the per-MCPServer-tool `labels`
// blocks: CEL expressions that pull (resourceType, id, label)
// tuples out of a tool's response so the runner can later substitute
// human-readable names into approval prompts.
//
// Decoupled from the v1alpha1 CRD types so test fixtures don't need the full API
// import — the MCP dispatcher adapts CRD blocks to labelextract.Block at call
// time. Mirrors pkg/authz/relwrites.
//
// Threat model: extracted labels are attacker-controllable, since they originate
// in MCP tool responses. This package is pure and never emits a label to any
// LLM; callers stamp results into the LabelStore, which only deterministic
// channel renderers consult.
package labelextract

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"

	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
	"github.com/authzed/openagentprimitives/pkg/x/celconv"
)

// MaxNameChars caps Extracted.Name at extraction time. Matches the
// summarizer's MaxStringArgChars constant — keeping the cap aligned
// keeps the threat surface uniform across the two distinct text
// surfaces that ingest arbitrary tool-output strings.
const MaxNameChars = 200

// Block mirrors v1alpha1.MCPServerLabelExtract. CRD-decoupled so
// tests don't need to import the API package.
type Block struct {
	When    string // CEL bool; "" → always taken
	ForEach string // CEL list; "" → exactly one tuple emitted (item=nil)
	Tuple   Tuple
}

// Tuple is the unresolved CEL-expression form of one extracted label.
type Tuple struct {
	ResourceType string // CEL string
	ID           string // CEL string
	Name         string // CEL string
}

// Extracted is one label ready to store.
type Extracted struct {
	ResourceType string
	ID           string
	Name         string
}

// Evaluate runs one Block against (args, result) and returns the
// resolved Extracted entries it yields. Empty slice when When
// evaluates false or ForEach yields no items.
//
// Per-item errors abort that item (logged by the caller) but do not
// halt the rest of the iteration. A compile-time CEL error returns
// nil + error; per-eval errors return whatever was already collected
// plus the first error encountered. Callers are expected to log +
// continue on any error (labels are non-blocking).
func Evaluate(block Block, args, result map[string]any) ([]Extracted, error) {
	if block.When != "" {
		whenPrg, err := compileBool(block.When)
		if err != nil {
			return nil, fmt.Errorf("when compile: %w", err)
		}
		ok, err := evalBool(whenPrg, args, result, nil)
		if err != nil {
			return nil, fmt.Errorf("when eval: %w", err)
		}
		if !ok {
			return nil, nil
		}
	}
	rtPrg, err := compileString(block.Tuple.ResourceType)
	if err != nil {
		return nil, fmt.Errorf("tuple.resourceType compile: %w", err)
	}
	idPrg, err := compileString(block.Tuple.ID)
	if err != nil {
		return nil, fmt.Errorf("tuple.id compile: %w", err)
	}
	namePrg, err := compileString(block.Tuple.Name)
	if err != nil {
		return nil, fmt.Errorf("tuple.name compile: %w", err)
	}

	emit := func(item any) (Extracted, bool, error) {
		rt, err := evalStringSoft(rtPrg, args, result, item)
		if err != nil {
			return Extracted{}, false, fmt.Errorf("resourceType: %w", err)
		}
		id, err := evalStringSoft(idPrg, args, result, item)
		if err != nil {
			return Extracted{}, false, fmt.Errorf("id: %w", err)
		}
		name, err := evalStringSoft(namePrg, args, result, item)
		if err != nil {
			return Extracted{}, false, fmt.Errorf("name: %w", err)
		}
		// Defensive sanitization at extraction time, before any
		// caller-visible surface sees the value.
		if rt == "" || id == "" || name == "" {
			return Extracted{}, false, nil
		}
		name = sanitizeName(name)
		return Extracted{ResourceType: rt, ID: id, Name: name}, true, nil
	}

	if block.ForEach == "" {
		ex, ok, err := emit(nil)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return []Extracted{ex}, nil
	}

	fePrg, err := compileDyn(block.ForEach)
	if err != nil {
		return nil, fmt.Errorf("forEach compile: %w", err)
	}
	v, _, err := fePrg.Eval(map[string]any{"args": args, "result": result, "item": nil})
	if err != nil {
		return nil, fmt.Errorf("forEach eval: %w", err)
	}
	// Not a raw v.Value().([]any) assertion: a filter/map macro yields a list
	// whose Value() is []ref.Val, which that assertion rejects. celconv.List is
	// cel-go's own conversion and accepts every representation — see its doc.
	list, err := celconv.List(v)
	if err != nil {
		return nil, fmt.Errorf("forEach: %w", err)
	}
	out := make([]Extracted, 0, len(list))
	for _, item := range list {
		ex, ok, err := emit(item)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, ex)
		}
	}
	return out, nil
}

// sanitizeName collapses newlines to a single space and truncates at
// MaxNameChars. Length cap is applied AFTER newline collapse so a
// long single-line name and a long multi-line name truncate to the
// same character count.
func sanitizeName(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > MaxNameChars {
		// Use rune-safe truncation so multi-byte names don't get cut mid-codepoint.
		runes := []rune(s)
		if len(runes) > MaxNameChars {
			s = string(runes[:MaxNameChars-1]) + "…"
		}
	}
	return s
}

// --- CEL plumbing (mirrors relwrites) ---

func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("args", cel.DynType),
		cel.Variable("result", cel.DynType),
		cel.Variable("item", cel.DynType),
	)
}

func compileBool(expr string) (cel.Program, error) { return compileTyped(expr, cel.BoolType) }
func compileString(expr string) (cel.Program, error) {
	return compileTyped(expr, cel.StringType)
}
func compileDyn(expr string) (cel.Program, error) { return compileTyped(expr, cel.DynType) }

func compileTyped(expr string, want *cel.Type) (cel.Program, error) {
	env, err := celEnv()
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	// Allow `dyn` outputs regardless of `want` — field-lookup
	// expressions like `args.id` resolve as dyn at compile time; type
	// coercion happens at eval. The strict check below still catches
	// genuinely-mismatched types (e.g. a bool literal in a string slot).
	if want != cel.DynType && ast.OutputType() != want && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("expected %s, got %s", want, ast.OutputType())
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, err
	}
	return prg, nil
}

func evalBool(prg cel.Program, args, result map[string]any, item any) (bool, error) {
	v, _, err := prg.Eval(map[string]any{"args": args, "result": result, "item": item})
	if err != nil {
		return false, err
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expected bool, got %T", v.Value())
	}
	return b, nil
}

// evalStringSoft returns ("", nil) when the CEL eval returns a nil /
// missing value or encounters a "no such key" error — at extraction
// time we want missing fields to skip the tuple silently, not error out.
//
// Missing-key detection is by substring match on the error text
// ("no such key") because cel-go (v0.26.x) formats that condition
// inline in its evaluator and exposes no error-type or predicate to
// distinguish it. If cel-go ever changes the wording, the dedicated
// TestEvalStringSoft_MissingKeySoftMiss test (in labelextract_test.go)
// will fail loudly — that's the canary. Keep the test and this
// behavior in lockstep.
func evalStringSoft(prg cel.Program, args, result map[string]any, item any) (string, error) {
	v, _, err := prg.Eval(map[string]any{"args": args, "result": result, "item": item})
	if err != nil {
		if strings.Contains(err.Error(), "no such key") {
			return "", nil
		}
		return "", err
	}
	if v.Value() == nil {
		return "", nil
	}
	s, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("expected string, got %T", v.Value())
	}
	return s, nil
}
