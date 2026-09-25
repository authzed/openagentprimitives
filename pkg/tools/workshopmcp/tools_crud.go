// tools_crud.go implements `get`, `list` and `delete` (announced with the
// workshop toolbox's `workshop_` prefix as `workshop_get`, `workshop_list`,
// `workshop_delete`): read/write access to the CR kinds a builder authors in
// the workshop namespace W (design spec §2.4). All three operate ONLY
// within s.Identity.Namespace — none accepts a namespace argument, so a CR
// outside W is simply not nameable through this surface; the apiserver's own
// RBAC (bound to the workshop SA token, see server.go's header) is the
// second, structural boundary behind it.
//
// get/list are read-only/auto; delete is approval-gated — both policies are
// DECLARED on this sidecar's SidecarToolbox entry (Task 8), not enforced by
// this file.
package workshopmcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Tool names announced on the MCP surface (`workshop_get`, `workshop_list`,
// `workshop_delete` once the runner prefixes them with this toolbox's handle
// — Task 3).
const (
	toolWorkshopGet    = "get"
	toolWorkshopList   = "list"
	toolWorkshopDelete = "delete"
)

// workshopKindEntry is one workshopKindTable row: constructors for a fresh
// typed object/list of one CR kind.
type workshopKindEntry struct {
	newObject func() client.Object
	newList   func() client.ObjectList
}

// workshopKindTable maps the CR kind names a builder reads/writes in W to
// constructors for that kind. Kept local to this package rather than routed
// through pkg/tools/kinds/registry: that registry is scoped to the
// tool-authoring kinds the unified `oap tools` CLI presents
// (SpiceboxToolspec/MCPServer/SidecarToolbox) and deliberately excludes
// AgentClass — which a builder DOES author in W (design spec §3.3's `agent`
// loop). A data table, not an `if kind == "..."` chain: adding a kind is one
// new entry here, read uniformly by every handler below.
var workshopKindTable = map[string]workshopKindEntry{
	"AgentClass": {
		newObject: func() client.Object { return &spiceboxv1alpha1.AgentClass{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.AgentClassList{} },
	},
	"MCPServer": {
		newObject: func() client.Object { return &spiceboxv1alpha1.MCPServer{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.MCPServerList{} },
	},
	"SidecarToolbox": {
		newObject: func() client.Object { return &spiceboxv1alpha1.SidecarToolbox{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.SidecarToolboxList{} },
	},
	"SpiceboxToolspec": {
		newObject: func() client.Object { return &spiceboxv1alpha1.SpiceboxToolspec{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.SpiceboxToolspecList{} },
	},
	// The rest of the design's closed namespaced kind set (§1.2), which the
	// Workshop controller's Role in W already grants: the identity a
	// credential is connected to (request_credential requires one authored
	// here), and the agent's own skills and starter view (the `agent` phase).
	// Missing from this table they were unauthorable — apply refused the
	// kind by name — while every skill assumed they could be written.
	"AgentIdentity": {
		newObject: func() client.Object { return &spiceboxv1alpha1.AgentIdentity{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.AgentIdentityList{} },
	},
	"Skill": {
		newObject: func() client.Object { return &spiceboxv1alpha1.Skill{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.SkillList{} },
	},
	"AgentUI": {
		newObject: func() client.Object { return &spiceboxv1alpha1.AgentUI{} },
		newList:   func() client.ObjectList { return &spiceboxv1alpha1.AgentUIList{} },
	},
}

// workshopKindNames lists every kind workshopKindTable knows, sorted — used
// to build a helpful error message for an unknown kind.
func workshopKindNames() []string {
	names := make([]string, 0, len(workshopKindTable))
	for k := range workshopKindTable {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// newObjectForKind returns a fresh empty object for kind, or an error naming
// every kind this sidecar knows how to read/write.
func newObjectForKind(kind string) (client.Object, error) {
	e, ok := workshopKindTable[kind]
	if !ok {
		return nil, fmt.Errorf("unknown or unsupported kind %q (known: %v)", kind, workshopKindNames())
	}
	return e.newObject(), nil
}

// newListForKind returns a fresh empty list for kind, or an error naming
// every kind this sidecar knows how to read/write.
func newListForKind(kind string) (client.ObjectList, error) {
	e, ok := workshopKindTable[kind]
	if !ok {
		return nil, fmt.Errorf("unknown or unsupported kind %q (known: %v)", kind, workshopKindNames())
	}
	return e.newList(), nil
}

// registerCRUD wires `get`, `list` and `delete` onto mcpSrv.
func (s *Server) registerCRUD(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolWorkshopGet,
		Description: "Read one custom resource, by kind and name, from the workshop. For kind " +
			"AgentClass, the result is wrapped as {resource, standin}: standin is true when this " +
			"AgentClass is a credential-free rehearsal double (project_agent) rather than something " +
			"you authored yourself — the resource itself has the same shape either way.",
		InputSchema: kindNameSchema("the resource to read"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleGet)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolWorkshopList,
		Description: "List every custom resource of one kind in the workshop. Call this first on " +
			"every restart — the workshop's CRs are the draft. For kind AgentClass, each entry is " +
			"wrapped as {resource, standin} — see the get tool's own description for what standin means.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind": map[string]any{"type": "string", "description": "the resource kind to list"},
			},
			"required": []any{"kind"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleList)

	mcpSrv.AddTool(&mcp.Tool{
		Name:        toolWorkshopDelete,
		Description: "Delete one custom resource, by kind and name, from the workshop.",
		InputSchema: kindNameSchema("the resource to delete"),
	}, s.handleDelete)
}

// kindNameSchema builds the shared {kind, name} InputSchema get and delete
// both use.
func kindNameSchema(verbDescription string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "description": "kind of " + verbDescription},
			"name": map[string]any{"type": "string", "description": "name of " + verbDescription},
		},
		"required": []any{"kind", "name"},
	}
}

// kindNameArgs is get's and delete's shared argument shape.
type kindNameArgs struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// standinTaggedResource wraps one AgentClass with a STRUCTURAL Standin
// marker: Workshop.status.standins is the authoritative registry, and
// `get`/`list` surface it directly on an
// AgentClass result rather than making the model infer a rehearsal double
// from the Description's stand-in-first sentence. Sourced from
// Server.standinNames (tools_export.go) — never
// spiceboxv1alpha1.AnnotationStandinSource, for the same forgeability reason
// soleAuthoredClassName no longer trusts it.
type standinTaggedResource struct {
	Resource any  `json:"resource"`
	Standin  bool `json:"standin"`
}

// handleGet answers the `get` tool call.
func (s *Server) handleGet(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a kindNameArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("get: decode arguments: %v", err), nil
	}
	if a.Kind == "" || a.Name == "" {
		return s.toolErr("get: kind and name are both required"), nil
	}
	obj, err := newObjectForKind(a.Kind)
	if err != nil {
		return s.toolErr("get: %v", err), nil
	}
	if err := s.K8s.Get(ctx, client.ObjectKey{Namespace: s.Identity.Namespace, Name: a.Name}, obj); err != nil {
		return s.crudReadErr("get", a.Kind, a.Name, err)
	}
	if a.Kind == "AgentClass" {
		standins, err := s.standinNames(ctx)
		if err != nil {
			return s.toolErr("get: %v", err), nil
		}
		return s.jsonResult(standinTaggedResource{Resource: obj, Standin: standins[a.Name]})
	}
	return s.jsonResult(obj)
}

// listArgs is list's own argument shape.
type listArgs struct {
	Kind string `json:"kind"`
}

// handleList answers the `list` tool call.
func (s *Server) handleList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a listArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("list: decode arguments: %v", err), nil
	}
	if a.Kind == "" {
		return s.toolErr("list: kind is required"), nil
	}
	list, err := newListForKind(a.Kind)
	if err != nil {
		return s.toolErr("list: %v", err), nil
	}
	if err := s.K8s.List(ctx, list, client.InNamespace(s.Identity.Namespace)); err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("list: %v", err), nil
	}
	if a.Kind == "AgentClass" {
		acList, ok := list.(*spiceboxv1alpha1.AgentClassList)
		if !ok {
			// Unreachable in practice: workshopKindTable's own "AgentClass" entry
			// constructs exactly this type. Surfaced rather than panicking, per
			// CLAUDE.md's never-silently-drop-an-error rule.
			return s.toolErr("list: internal error: AgentClass list has unexpected type %T", list), nil
		}
		standins, err := s.standinNames(ctx)
		if err != nil {
			return s.toolErr("list: %v", err), nil
		}
		tagged := make([]standinTaggedResource, 0, len(acList.Items))
		for i := range acList.Items {
			tagged = append(tagged, standinTaggedResource{Resource: &acList.Items[i], Standin: standins[acList.Items[i].Name]})
		}
		return s.jsonResult(tagged)
	}
	return s.jsonResult(list)
}

// handleDelete answers the `delete` tool call.
func (s *Server) handleDelete(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a kindNameArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("delete: decode arguments: %v", err), nil
	}
	if a.Kind == "" || a.Name == "" {
		return s.toolErr("delete: kind and name are both required"), nil
	}
	obj, err := newObjectForKind(a.Kind)
	if err != nil {
		return s.toolErr("delete: %v", err), nil
	}
	obj.SetName(a.Name)
	obj.SetNamespace(s.Identity.Namespace)
	if err := s.K8s.Delete(ctx, obj); err != nil {
		return s.crudReadErr("delete", a.Kind, a.Name, err)
	}
	return s.jsonResult(map[string]any{"deleted": true, "kind": a.Kind, "name": a.Name})
}

// crudReadErr distinguishes an apiserver/webhook DENIAL (surfaced verbatim
// via Server.deniedResult — never reworded) from a genuine NotFound (a clear
// tool error naming what was looked for) and from any other failure. Shared
// by get and delete, whose failure shapes are identical.
func (s *Server) crudReadErr(op, kind, name string, err error) (*mcp.CallToolResult, error) {
	if isDeniedErr(err) {
		return s.deniedResult(err), nil
	}
	if apierrors.IsNotFound(err) {
		return s.toolErr("%s: no %s named %q was found in the workshop", op, kind, name), nil
	}
	return s.toolErr("%s: %v", op, err), nil
}
