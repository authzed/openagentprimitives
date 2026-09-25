// Package uiview turns an AgentUI CR plus the agent's stored Tier-1
// fragments into the one merged declaration everything downstream reads: the
// browser bootstrap, a binding-path lookup, an action-name lookup, the live
// push, and the runner's own write-time validation.
package uiview

import (
	"encoding/json"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// wireSlot/wireAction/wireDeclaration mirror uicomponents.Slot/Action/
// Declaration's wire shape field-for-field, holding a slot's Default (and an
// action's Args) as raw JSON from the CRD's opaque apiextensionsv1.JSON — so
// DeclarationFromSpec's strict parse depends only on bytes, not on the CRD
// package's json tags still matching uicomponents' wire vocabulary.
type wireSlot struct {
	Name string `json:"name"`

	// AgentWritable permits the agent to overwrite this slot; default false.
	AgentWritable bool `json:"agentWritable,omitempty"`

	// Default is the author-declared Tier-0 content; absent means an empty slot.
	Default json.RawMessage `json:"default,omitempty"`
}

type wireAction struct {
	Name string `json:"name"`

	// Tool is the app tool this action invokes; empty when Prompt is set.
	Tool string `json:"tool,omitempty"`

	// Prompt asks the agent instead of calling a tool; exactly one of Tool and
	// Prompt is set. Dropping it here would leave an action declaring neither,
	// which Validate rejects — a UI that reconciles clean and fails on read-back.
	Prompt string `json:"prompt,omitempty"`

	// Args is the tool-argument template, carrying {"$param":"<name>"} holes.
	Args json.RawMessage `json:"args,omitempty"`

	// Inputs allowlists the value names a control may supply at invoke time.
	Inputs []string `json:"inputs,omitempty"`
}

type wireDeclaration struct {
	Actions []wireAction `json:"actions,omitempty"`

	// View is the CR's page tree, carried through as opaque bytes for the
	// same reason a slot's Default is: DeclarationFromSpec's strict parse
	// must depend only on bytes, not on this package's json tags still
	// matching apiextensionsv1.JSON's.
	View json.RawMessage `json:"view,omitempty"`

	// Slots is the legacy wire shape. omitempty because View and Slots are
	// mutually exclusive on the wire — ParseDeclaration's Normalize rejects a
	// document carrying both — and a spec.view page must marshal with no
	// "slots" key at all, not an empty list, or a decode strict enough to
	// notice the difference would see the wrong branch.
	Slots []wireSlot `json:"slots,omitempty"`
}

// DeclarationFromSpec converts an AgentUI's bundle-authored spec into a
// uicomponents.Declaration.
//
// It marshals the CR's view/slots and actions to their wire shape and runs
// the result back through uicomponents.ParseDeclaration — the strict parse,
// which also runs Normalize — rather than field-copying: the CR holds
// AgentUISpec.View, AgentUISlot.Default and AgentUIAction.Args as opaque
// apiextensionsv1.JSON, and ParseDeclaration is what rejects a wire field
// this build does not know, at any depth. A hand-rolled field copy would
// accept it and drop it.
//
// Exactly one of ui.Spec.View and ui.Spec.Slots is set — the CRD's own
// comment says so — but this function does not enforce that itself: it
// carries whichever fields are present onto the wire struct verbatim, and
// Normalize is the one place "both" is rejected, so there is a single
// answer to that question rather than one enforced here and a second
// enforced downstream.
func DeclarationFromSpec(ui *spiceboxv1alpha1.AgentUI) (uicomponents.Declaration, error) {
	var wd wireDeclaration
	if ui.Spec.View != nil {
		wd.View = json.RawMessage(ui.Spec.View.Raw)
	}
	if len(ui.Spec.Slots) > 0 {
		wd.Slots = make([]wireSlot, len(ui.Spec.Slots))
		for i, s := range ui.Spec.Slots {
			ws := wireSlot{Name: s.Name, AgentWritable: s.AgentWritable}
			if s.Default != nil {
				ws.Default = json.RawMessage(s.Default.Raw)
			}
			wd.Slots[i] = ws
		}
	}
	if len(ui.Spec.Actions) > 0 {
		wd.Actions = make([]wireAction, len(ui.Spec.Actions))
		for i, a := range ui.Spec.Actions {
			wa := wireAction{Name: a.Name, Tool: a.Tool, Prompt: a.Prompt, Inputs: a.Inputs}
			if a.Args != nil {
				wa.Args = json.RawMessage(a.Args.Raw)
			}
			wd.Actions[i] = wa
		}
	}

	raw, err := json.Marshal(wd)
	if err != nil {
		return uicomponents.Declaration{}, fmt.Errorf("uiview: marshal wire declaration for %s: %w", ui.Name, err)
	}
	decl, err := uicomponents.ParseDeclaration(raw)
	if err != nil {
		return uicomponents.Declaration{}, fmt.Errorf("uiview: parse declaration for %s: %w", ui.Name, err)
	}
	return decl, nil
}
