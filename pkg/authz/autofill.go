package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// SlotLister enumerates the instances bound into a session's slots.
//
// Autofill reads the SAME store that authorizes the call — the slot grants in
// SpiceDB — rather than the Layer-2 scope document it used to read. Two stores
// meant autofill could name an instance the Check would then refuse, or omit
// one it would have allowed; the value the agent sees and the value the gate
// admits now come from one place.
type SlotLister interface {
	ListSlotGrants(ctx context.Context, ns, name string) ([]SlotBinding, error)
}

// FillToolArgs auto-fills tool args from the session's slot grants, per
// entities[i].AutoFillArgs. An empty/missing arg is filled when EXACTLY one
// instance of the target type is bound; zero or several leave it alone, since
// picking among several would be the runtime choosing the agent's target.
//
// Returns the (possibly mutated) args as a new json.RawMessage. The caller
// writes this back into uses[i].Input before the per-tool Check.
//
// A lookup failure is returned, not swallowed: autofill is advisory and the
// caller falls back to the agent's raw args, but an operator debugging "why
// wasn't my repo filled in" needs the reason to reach a log.
func FillToolArgs(
	ctx context.Context,
	lister SlotLister,
	sess SessionRef,
	entities []BoundEntitySpec,
	toolName string,
	input json.RawMessage,
) (json.RawMessage, error) {
	if lister == nil || len(entities) == 0 {
		return input, nil
	}
	granted, err := lister.ListSlotGrants(ctx, sess.Namespace, sess.Name)
	if err != nil {
		return input, fmt.Errorf("FillToolArgs: list slot grants for %s: %w", sess, err)
	}
	if len(granted) == 0 {
		return input, nil
	}
	return fillFromGrants(entities, granted, toolName, input)
}

// fillFromGrants fills args from the session's bound instances. Only fills
// when exactly one instance of the matching resource type is bound.
func fillFromGrants(entities []BoundEntitySpec, granted []SlotBinding, toolName string, input json.RawMessage) (json.RawMessage, error) {
	var args map[string]any
	if len(input) == 0 {
		args = map[string]any{}
	} else if err := json.Unmarshal(input, &args); err != nil {
		// Args aren't a JSON object; leave alone (matches old autofill.Fill).
		return input, nil
	}
	changed := false
	for _, et := range entities {
		ids := grantedIDsOfType(granted, et.ResourceType)
		if len(ids) != 1 {
			continue // 0 → nothing to fill; >1 → ambiguous, leave unset
		}
		for _, af := range et.AutoFillArgs {
			if !autofillToolNameMatches(toolName, af.ToolNamePattern) {
				continue
			}
			existing, ok := args[af.ArgName]
			if ok && existing != nil && existing != "" {
				continue // agent already set it
			}
			args[af.ArgName] = ids[0]
			changed = true
		}
	}
	if !changed {
		return input, nil
	}
	out, err := json.Marshal(args)
	if err != nil {
		return input, nil
	}
	return out, nil
}

// grantedIDsOfType returns the bound instance IDs of one resource type.
func grantedIDsOfType(granted []SlotBinding, resourceType string) []string {
	var out []string
	for _, b := range granted {
		if b.ResourceType == resourceType {
			out = append(out, b.ResourceID.String())
		}
	}
	return out
}

// autofillToolNameMatches honors filepath.Match's glob syntax against
// the ToolNamePattern. Empty pattern matches everything (per spec §3.2).
func autofillToolNameMatches(toolName, pattern string) bool {
	if pattern == "" {
		return true
	}
	ok, err := filepath.Match(pattern, toolName)
	return err == nil && ok
}
