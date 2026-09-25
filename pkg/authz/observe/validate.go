// Package observe evaluates the `observes` blocks declared on a tool, turning
// one tool result into co-derived subjects and facts.
//
// It is a sibling of pkg/authz/relwrites and shares its CEL bindings (`args`,
// `result`, `item`, `session`) and its "runs only after a SUCCESSFUL call"
// contract. The difference is what it produces: relwrites emits SpiceDB tuples,
// this emits memory facts.
package observe

import (
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
)

// SubjectExpr is one subject as a pair of CEL string expressions.
type SubjectExpr struct {
	ResourceType string
	ResourceID   string
}

// Block is one `observes` declaration, decoupled from the CRD type so tests
// need no v1alpha1 import — the same shape relwrites.Block uses.
type Block struct {
	When     string
	ForEach  string
	Subjects []SubjectExpr
	// Facts maps a fact name to a CEL expression yielding its value.
	Facts map[string]string
}

// ValidateBlock compiles every expression and enforces the structural rules, so
// a malformed declaration fails at ADMISSION rather than at dispatch.
//
// The subject rule is the security one and it is not a style check. A block
// with facts and no subject would record a value bound to nothing — a
// session-scoped boolean, which is precisely the shape a precondition must
// never be able to read, because it answers for every instance at once.
// Refusing it here makes that shape unrepresentable instead of merely
// discouraged.
func ValidateBlock(b Block) error {
	var errs []error

	if len(b.Subjects) == 0 {
		errs = append(errs, errors.New("observes: a block must name at least one subject; a fact with no subject is a session-scoped boolean and would answer for every instance"))
	}
	if len(b.Facts) == 0 {
		errs = append(errs, errors.New("observes: a block must record at least one fact"))
	}
	if b.When != "" {
		if _, err := relwrites.CompileBoolExpr(b.When); err != nil {
			errs = append(errs, fmt.Errorf("observes: when: %w", err))
		}
	}
	if b.ForEach != "" {
		if _, err := relwrites.CompileAnyExpr(b.ForEach); err != nil {
			errs = append(errs, fmt.Errorf("observes: forEach: %w", err))
		}
	}
	for i, s := range b.Subjects {
		if _, err := relwrites.CompileStringExpr(s.ResourceType); err != nil {
			errs = append(errs, fmt.Errorf("observes: subject %d resourceType: %w", i, err))
		}
		if _, err := relwrites.CompileStringExpr(s.ResourceID); err != nil {
			errs = append(errs, fmt.Errorf("observes: subject %d resourceID: %w", i, err))
		}
	}
	for name, expr := range b.Facts {
		if name == "" {
			errs = append(errs, errors.New("observes: empty fact name"))
			continue
		}
		if _, err := relwrites.CompileAnyExpr(expr); err != nil {
			errs = append(errs, fmt.Errorf("observes: fact %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
