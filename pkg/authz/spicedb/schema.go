package spicedb

import (
	"context"
	"fmt"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"

	// compiler.Compile logs a trace line per definition through zerolog's
	// process-global logger. Blank-imported (rather than left to each main) so
	// that anything linking this package is silent: binaries, `go test`
	// binaries, and the in-process e2e harness alike.
	_ "github.com/authzed/openagentprimitives/pkg/platform/deplogs"
)

// PermissionResolution describes how a name is defined within a schema definition.
type PermissionResolution int

const (
	// PermissionResolutionNotFound means the definition or name was not found.
	PermissionResolutionNotFound PermissionResolution = iota
	// PermissionResolutionPermission means the name is a permission (computed).
	PermissionResolutionPermission
	// PermissionResolutionRelation means the name is a direct relation.
	PermissionResolutionRelation
)

func (p PermissionResolution) String() string {
	switch p {
	case PermissionResolutionPermission:
		return "Permission"
	case PermissionResolutionRelation:
		return "Relation"
	default:
		return "NotFound"
	}
}

// Schema is a parsed SpiceDB schema, providing lookups over definitions,
// relations, and permissions without requiring a live SpiceDB connection.
type Schema struct {
	defs map[string]*defEntry
}

type defEntry struct {
	relations   map[string]bool
	permissions map[string]bool
}

// ParseSchema compiles src as a SpiceDB schema and returns a Schema ready for
// HasDefinition / ResolvePermission queries. Returns an error if src is not
// valid SpiceDB schema DSL.
func ParseSchema(src string) (*Schema, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("schema"),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		return nil, fmt.Errorf("spicedb: compile schema: %w", err)
	}

	s := &Schema{defs: make(map[string]*defEntry, len(compiled.ObjectDefinitions))}
	for _, ns := range compiled.ObjectDefinitions {
		entry := &defEntry{
			relations:   make(map[string]bool),
			permissions: make(map[string]bool),
		}
		for _, rel := range ns.GetRelation() {
			if rel.GetUsersetRewrite() != nil {
				// A non-nil UsersetRewrite means this entry is a permission.
				entry.permissions[rel.GetName()] = true
			} else {
				entry.relations[rel.GetName()] = true
			}
		}
		s.defs[ns.GetName()] = entry
	}
	return s, nil
}

// HasDefinition reports whether the schema contains a definition with the
// given name (e.g. "user", "github_repo").
func (s *Schema) HasDefinition(name string) bool {
	_, ok := s.defs[name]
	return ok
}

// ResolvePermission reports whether permName within defName is a permission,
// a relation, or absent. Returns PermissionResolutionNotFound if defName
// itself does not exist.
func (s *Schema) ResolvePermission(defName, permName string) PermissionResolution {
	d, ok := s.defs[defName]
	if !ok {
		return PermissionResolutionNotFound
	}
	if d.permissions[permName] {
		return PermissionResolutionPermission
	}
	if d.relations[permName] {
		return PermissionResolutionRelation
	}
	return PermissionResolutionNotFound
}

// SchemaReader is satisfied by any client that can read the schema text from a
// live SpiceDB instance. *Client satisfies this interface.
type SchemaReader interface {
	// ReadSchema returns the schema text currently installed on the live
	// instance — the composed text, not the embedded scaffold. Callers that
	// validate against it must treat an error as "cannot validate" and refuse,
	// never as "the definition is absent".
	ReadSchema(ctx context.Context, in *v1.ReadSchemaRequest) (*v1.ReadSchemaResponse, error)
}

// FetchAndParseSchema reads the current schema from r and parses it. Useful
// for AgentClass-side validation where a live SpiceDB connection is available.
func FetchAndParseSchema(ctx context.Context, r SchemaReader) (*Schema, error) {
	resp, err := r.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if err != nil {
		return nil, fmt.Errorf("spicedb: ReadSchema: %w", err)
	}
	return ParseSchema(resp.GetSchemaText())
}
