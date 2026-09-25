package component

import (
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
)

// reflector inlines every definition — an agent-facing tool schema should be
// self-contained rather than $ref-chasing — and omits the $id the model does
// not need. Anonymous suppresses $id but NOT $schema, which invopop sets
// unconditionally; that is what keeps each document self-describing without
// the $id this reflector omits.
var reflector = &jsonschema.Reflector{
	DoNotReference: true,
	Anonymous:      true,
	ExpandedStruct: true,
}

// Schema emits the JSON Schema for this component's props. It is derived from
// the SAME struct the validator decodes into (see validateProps in
// pkg/web/uicomponents/validate.go), so the set of legal TOP-LEVEL PROP NAMES
// cannot drift: a top-level prop the agent is told about here is exactly a prop
// validateProps accepts, and one it is not told about is exactly a prop
// validateProps rejects.
//
// That guarantee rests on validateProps' explicit exact-name check, NOT on the
// decoder: encoding/json matches struct fields case-insensitively, so a decode
// alone would accept "Body"/"BODY" for a schema publishing only "body", and
// the renderer — which reads the exact key — would paint an empty block for a
// declaration the platform called valid. Do not remove that check believing
// DisallowUnknownFields covers it; it does not.
//
// SCOPE: top-level names only. Fields nested inside a props struct still go
// through the decoder's case-insensitive matching.
//
// ENUM VALUES are published here but NOT enforced by validateProps — see the
// jsonschema struct tags in pkg/web/uicomponents/components.go.
//
// REQUIRED-ness is ADVISORY ONLY. The reflector marks a field required
// whenever its json tag lacks omitempty (TableColumn.Key, ChartSeries.Key,
// SelectOption.Value, FormField.Name, TabProps.Value — all nested slice
// elements, not top-level props), and validateProps enforces none of it: an
// absent required field decodes to its Go zero value with no error. So a
// declaration omitting a table column's "key" is ACCEPTED and renders a blank
// column, even though the schema told the agent it was required. The
// instruction to the agent is right; the enforcement is missing. Closing it
// needs a RECURSIVE required-check (into TableProps.Columns[i].Key), not a
// top-level pass, and belongs with the renderer work where the degradation is
// visible.
func (c Component) Schema() (json.RawMessage, error) {
	if c.Props == nil {
		return nil, fmt.Errorf("uicomponents: component %q has nil Props", c.Type)
	}
	s := reflector.Reflect(c.Props)
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("uicomponents: marshal schema for %q: %w", c.Type, err)
	}
	return raw, nil
}
