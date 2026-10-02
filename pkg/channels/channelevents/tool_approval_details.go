package channelevents

// ToolApprovalDetails is the tool_approval interaction's Details payload
// (InteractionRequestPayload.Details, json.RawMessage). It carries the
// non-display inputs the channelsd-side grant-write handler feeds to
// grants.WriteToolGrant, plus the Show-Details modal's render fields.
//
// It rides in Details — and is persisted on the durable memapproval record —
// rather than widening the minimal PendingInteraction, so a channelsd restart
// can still recover the grant fields from the durable record.
type ToolApprovalDetails struct {
	// Permission, ResourceType and ResourceID are the SpiceDB triple the
	// approval grants on; ArgsHash pins the grant to these exact arguments.
	Permission   string `json:"permission,omitempty"`
	ResourceType string `json:"resourceType,omitempty"`
	ResourceID   string `json:"resourceID,omitempty"`
	ArgsHash     string `json:"argsHash,omitempty"`
	// StateImpact is the tool's declared effect: "external" narrows the grant
	// to a 30s TTL (re-approve per call); anything else grants session-wide.
	StateImpact string `json:"stateImpact,omitempty"`
	// NoSlotGrant marks a ResourceType the AgentClass does NOT declare in
	// spec.authz.slots. Only a declared slot has a slot_grant relation in the
	// composed schema, so only a declared slot can receive a grant.
	//
	// The approval handler must not attempt a grant when this is set: the write
	// fails with FailedPrecondition ("relation slot_grant_<p> not found"), the
	// error propagates out of BindApproved, and the decision is never recorded —
	// leaving the tool awaiting an approval a human already gave, until it times
	// out. An external tool on an undeclared type was PERMANENTLY unapprovable
	// that way.
	//
	// Stated negatively so the zero value attempts the grant. This payload is
	// persisted on the durable memapproval record, so an interaction raised
	// before this field existed decodes it as false and behaves exactly as it
	// did then; the positive spelling would have turned every one of those into
	// a silent no-grant approval on the next channelsd restart.
	//
	// It is set by the runner, which reads the class's slot list at raise time;
	// channelsd has no other route to the answer at decision time.
	NoSlotGrant bool `json:"noSlotGrant,omitempty"`
	// Occupancy and Rebind are the slot's single-vs-multi commitment and rebind
	// policy, resolved from the class slot at REQUEST-RECORD time (the runner
	// holds the class; the channelsd decision handler does not) and read back
	// when the approval binds, so the grant GrantSlots writes is pinned exactly
	// as a non-approval bind of the same slot would be.
	//
	// Empty Occupancy reads as single downstream, so a record raised before
	// these fields existed binds gated rather than un-gated — the same
	// fail-closed default the SlotBinding fields carry. Persisted on the durable
	// memapproval record alongside the rest of this payload.
	Occupancy string `json:"occupancy,omitempty"`
	Rebind    string `json:"rebind,omitempty"`
	// ToolName is the tool's wire name. Machine-readable identity: the visible
	// "Tool" field carries the tool's DESCRIPTION, because a wire name has no
	// business on a surface a person decides from — so anything matching a
	// prompt to a tool reads this, never the field.
	//
	// It was absent, and the e2e harness matched on the visible field instead.
	// Rewording that field for humans broke six approval scenarios at once,
	// which is what a machine reading human copy always eventually does.
	ToolName string `json:"toolName,omitempty"`

	// ArgsJSON, ToolDescription and Justification are render-only copy for the
	// Show-Details modal; nothing authorizes on them.
	ArgsJSON        string `json:"argsJSON,omitempty"`
	ToolDescription string `json:"toolDescription,omitempty"`
	Justification   string `json:"justification,omitempty"`
}
