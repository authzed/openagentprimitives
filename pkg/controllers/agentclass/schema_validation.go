package agentclass

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// validateAgainstSchema fetches the SpiceDB schema and verifies every
// Permission.Check on every referenced MCPServer tool resolves to a known
// resource type and permission. PermissionVariants[].Check.Check is validated
// too: a variant naming a typo'd pair would otherwise pass structural validation
// and surface only at runtime as a SpiceDB error.
//
// The non-nil error return is reserved for TRANSIENT infra failures — SpiceDB
// unreachable, fetch failed, schema text unparseable. Those are NOT a property
// of the spec, which may be perfectly valid. The caller MUST propagate the error
// so controller-runtime retries with backoff, rather than parking the class at
// Valid=False, which no SpiceDB-recovery watch would ever lift.
//
// A genuine schema MISMATCH — a pair the live schema does not declare — IS a
// property of the spec: it returns a non-empty (reason, message) with a nil
// error, and the caller parks the class at Valid=False without requeuing, since
// the MCPServer watch re-enqueues if the referenced server changes.
//
// An empty reason with a nil error means valid. Only stateImpact readonly or
// readwrite tools carry a Check block, so nil checks are skipped.
func validateAgainstSchema(ctx context.Context, schemaR spicedb.SchemaReader, mcpServers []spiceboxv1alpha1.MCPServer) (reason, message string, err error) {
	schema, err := spicedb.FetchAndParseSchema(ctx, schemaR)
	if err != nil {
		// Transient: surface as an error so controller-runtime retries.
		// Do NOT collapse into a Valid=False reason — that would park the
		// class until an unrelated event re-enqueues it.
		return "", "", fmt.Errorf("%s: %w", spiceboxv1alpha1.ReasonSpiceDBUnreachable, err)
	}
	for _, srv := range mcpServers {
		for _, tool := range srv.Spec.Tools {
			if tool.Permission != nil && tool.Permission.Check != nil {
				c := tool.Permission.Check
				res := schema.ResolvePermission(c.ResourceType, c.Permission)
				if res == spicedb.PermissionResolutionNotFound {
					return spiceboxv1alpha1.ReasonPermissionSchemaMismatch,
						fmt.Sprintf("MCPServer/%s tool/%s: SpiceDB schema has no %s on %s",
							srv.Name, tool.Name, c.Permission, c.ResourceType), nil
				}
			}
			for vi, v := range tool.PermissionVariants {
				if v.Check.Check == nil {
					continue // stateless/passthrough variant carries no Check
				}
				c := v.Check.Check
				res := schema.ResolvePermission(c.ResourceType, c.Permission)
				if res == spicedb.PermissionResolutionNotFound {
					return spiceboxv1alpha1.ReasonPermissionSchemaMismatch,
						fmt.Sprintf("MCPServer/%s tool/%s permissionVariants[%d]: SpiceDB schema has no %s on %s",
							srv.Name, tool.Name, vi, c.Permission, c.ResourceType), nil
				}
			}
		}
	}
	return "", "", nil
}
