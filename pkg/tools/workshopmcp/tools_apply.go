// tools_apply.go implements `apply` (announced with the workshop toolbox's
// `workshop_` prefix as `workshop_apply`): server-side-apply a candidate
// CR into the workshop namespace W under this sidecar's own field manager,
// then poll for the object's own controller to stamp its Valid/
// SchemaValidated conditions before answering — design spec §2.4's "the
// approval card, rendered by the summarizer over render_summary, is the 'a
// human sees every CR before it lands' step".
//
// stateImpact:external / approval-required is a DECLARATION on this tool's
// SidecarToolbox entry (Task 8), not something this handler enforces itself:
// by the time a call reaches handleApply, the runner's approval flow has
// already gated it. This file only applies.
package workshopmcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// toolWorkshopApply is the name `apply` announces on the MCP surface —
// `workshop_apply` once the runner prefixes it with this toolbox's handle
// (Task 3).
const toolWorkshopApply = "apply"

// applyPollInterval / applyPollTimeout govern applyCR's re-Get cadence and
// overall wait for the object's controller to stamp a Valid/SchemaValidated
// condition onto the just-applied object. Package vars, not consts, so a
// test can shrink them rather than actually waiting out a controller that,
// in a fake-client test, will never come.
var (
	applyPollInterval = 250 * time.Millisecond
	applyPollTimeout  = 30 * time.Second
)

// conditionTypeValid / conditionTypeSchemaValidated are the condition TYPE
// STRINGS every candidate kind this sidecar reads/writes (the kinds in
// workshopKindTable — see pkg/apis/v1alpha1/conditions.go) uses for its own
// controller's readiness signal. applyCR checks the string value directly
// rather than importing each kind's own Go constant, which keeps it one loop
// across every kind in the table rather than a switch per kind.
const (
	conditionTypeValid           = "Valid"
	conditionTypeSchemaValidated = "SchemaValidated"
)

// registerApply wires `apply` onto mcpSrv.
func (s *Server) registerApply(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolWorkshopApply,
		Description: "Server-side-apply a candidate custom resource into the workshop, under this " +
			"sidecar's own field manager. Waits up to 30 seconds for the object's own controller to " +
			"stamp Valid/SchemaValidated conditions, then returns them so the caller can tell whether " +
			"the candidate actually took effect. A webhook or permission denial is returned verbatim " +
			"— never claimed as a partial success.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"manifest": map[string]any{
					"type": "object",
					"description": fmt.Sprintf("The candidate resource: kind (one of %s), metadata (name; "+
						"namespace may be omitted — it always applies into the workshop's own "+
						"namespace), and spec — the same spec content validate_spec checks. "+
						"apiVersion is optional and is filled in from the kind; never guess one.",
						strings.Join(workshopKindNames(), ", ")),
				},
			},
			"required": []any{"manifest"},
		},
	}, s.handleApply)
}

// applyArgs is apply's own argument shape.
type applyArgs struct {
	Manifest map[string]any `json:"manifest"`
}

// handleApply answers the `apply` tool call.
func (s *Server) handleApply(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a applyArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("apply: decode arguments: %v", err), nil
	}
	if len(a.Manifest) == 0 {
		return s.toolErr("apply: manifest is required"), nil
	}

	obj := &unstructured.Unstructured{Object: a.Manifest}
	kind := obj.GetKind()
	if kind == "" {
		return s.toolErr("apply: manifest must set kind (one of %v)", workshopKindNames()), nil
	}
	if _, known := workshopKindTable[kind]; !known {
		// Refused by name, with the kinds this workshop can author — the same
		// answer list/get/delete give. Letting an unknown kind through to the
		// apiserver came back as "no matches for kind", which told the builder
		// nothing about what it could have asked for.
		return s.toolErr("apply: unknown or unsupported kind %q (known: %v)", kind, workshopKindNames()), nil
	}
	// The kind is the builder's whole contract; the API group and version are
	// platform knowledge it has no way to learn, so they are stamped here from
	// the kind rather than read from the manifest. A manifest that carries an
	// apiVersion of its own is overwritten, never trusted and never a reason to
	// refuse: observed live, a builder guessed "oap.dev/v1", then "v1", and
	// every guess cost the person an approval card before the apiserver said no.
	obj.SetGroupVersionKind(spiceboxv1alpha1.SchemeGroupVersion.WithKind(kind))
	if obj.GetName() == "" {
		return s.toolErr("apply: manifest must set metadata.name"), nil
	}
	switch ns := obj.GetNamespace(); ns {
	case "":
		obj.SetNamespace(s.Identity.Namespace)
	case s.Identity.Namespace:
		// Already the workshop's own namespace — nothing to do.
	default:
		return s.toolErr(
			"apply: manifest namespace %q must be empty or the workshop's own namespace %q",
			ns, s.Identity.Namespace), nil
	}

	applied, err := s.applyCR(ctx, obj)
	if err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("apply: %v", err), nil
	}

	return s.jsonResult(map[string]any{
		"kind":       applied.GetKind(),
		"name":       applied.GetName(),
		"namespace":  applied.GetNamespace(),
		"conditions": objectConditions(applied),
	})
}

// applyCR server-side-applies obj under s.FieldOwner, then polls (Get) until
// the object's own controller has stamped a Valid or SchemaValidated
// condition, or applyPollTimeout elapses — whichever comes first. On
// timeout it returns the object as it stands (conditions empty or partial)
// rather than turning a successful apply into a reported failure: the SSA
// already landed, the caller just hasn't seen a readiness signal yet.
//
// A denial from the apiserver or an admission webhook is returned UNCHANGED
// from the Patch call, before anything is polled — so handleApply can tell
// it apart from every other failure via isDeniedErr and surface it verbatim
// through Server.deniedResult, never as a partial success.
func (s *Server) applyCR(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := s.K8s.Patch(ctx, obj, client.Apply, client.FieldOwner(s.FieldOwner), client.ForceOwnership); err != nil {
		return nil, err
	}

	gvk := obj.GroupVersionKind()
	key := client.ObjectKeyFromObject(obj)
	pollCtx, cancel := context.WithTimeout(ctx, applyPollTimeout)
	defer cancel()

	for {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(gvk)
		if err := s.K8s.Get(ctx, key, current); err != nil {
			return nil, fmt.Errorf("re-reading applied object: %w", err)
		}
		if conditionsStamped(objectConditions(current)) {
			return current, nil
		}
		select {
		case <-pollCtx.Done():
			return current, nil
		case <-time.After(applyPollInterval):
		}
	}
}

// conditionsStamped reports whether conds already carries a Valid or
// SchemaValidated entry — applyCR's signal to stop polling.
func conditionsStamped(conds []map[string]any) bool {
	for _, c := range conds {
		t, _ := c["type"].(string)
		if t == conditionTypeValid || t == conditionTypeSchemaValidated {
			return true
		}
	}
	return false
}

// objectConditions extracts status.conditions from obj as a slice of plain
// maps, independent of which CR kind obj is — apply's own answer shape (get/
// list instead marshal the whole typed object, conditions included
// natively). A missing or malformed status.conditions
// reports an empty, non-nil slice rather than nil (which would marshal as
// JSON null) or an error.
func objectConditions(obj *unstructured.Unstructured) []map[string]any {
	raw, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// isDeniedErr reports whether err is an apiserver/admission-webhook DENIAL —
// RBAC Forbidden, or Invalid (a validating webhook's rejection) — as opposed
// to any other failure (network, NotFound, etc.). Mirrors
// pkg/controllers/agentsession/restart.go's isPermanentSnapshotError, which
// draws the same line for the same reason: these are the apiserver's own
// refusal, not this sidecar's opinion, so Server.deniedResult surfaces them
// verbatim rather than rewording them.
func isDeniedErr(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsInvalid(err)
}
