// Package registry resolves toolkit references for the operator. It loads
// built-in toolkits (compile-time embedded) at construction and falls through
// to a controller-runtime client for SpiceboxToolkit CR lookups. Built-ins win
// on collision — the SpiceboxToolkit controller rejects collisions in the
// first place, so collision should never occur in steady state.
package registry

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// ErrNotFound is returned when a (name, revision) does not resolve to any
// known toolkit (built-in or CR).
var ErrNotFound = errors.New("toolkit not found")

// Registry resolves toolkit references.
type Registry struct {
	builtins map[key]*toolkit.Toolkit
	cli      client.Client
}

type key struct{ Name, Revision string }

// NewWithBuiltins constructs a registry seeded with compile-time built-ins.
// cli may be nil for unit tests that exercise built-ins only; production
// callers must supply a client.
func NewWithBuiltins(cli client.Client) (*Registry, error) {
	r := &Registry{
		builtins: map[key]*toolkit.Toolkit{},
		cli:      cli,
	}
	bi := toolkits.All()
	for i := range bi {
		tk := bi[i]
		r.builtins[key{Name: tk.Name, Revision: tk.ToolkitRevision}] = &tk
	}
	return r, nil
}

// Resolve looks up a toolkit by (name, revision). Built-ins are checked first;
// if not found and a client is configured, SpiceboxToolkit CRs are listed as
// a fallback.
func (r *Registry) Resolve(ctx context.Context, name, revision string) (*toolkit.Toolkit, error) {
	if tk, ok := r.builtins[key{Name: name, Revision: revision}]; ok {
		return tk, nil
	}
	if r.cli == nil {
		return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, name, revision)
	}

	var list spiceboxv1alpha1.SpiceboxToolkitList
	if err := r.cli.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list SpiceboxToolkits: %w", err)
	}
	for i := range list.Items {
		s := &list.Items[i].Spec
		if s.Name == name && s.ToolkitRevision == revision {
			tk, err := s.ToToolkit()
			if err != nil {
				return nil, fmt.Errorf("convert SpiceboxToolkit %q: %w", list.Items[i].Name, err)
			}
			return tk, nil
		}
	}
	return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, name, revision)
}

// Builtins returns the loaded compile-time toolkits, for diagnostics + the
// admission collision check in the SpiceboxToolkit controller.
func (r *Registry) Builtins() []toolkit.Toolkit {
	return toolkits.All()
}
