package channelevents

// OperationActivityCompactLine derives the single-line "op ‣ reason" summary for
// surfaces that can only show one line (Slack setStatus, CLI TUI). It picks the
// deepest ACTIVE operation and, within it, the newest active call's _reason.
// Untruncated — per-surface length limits are the channel kind's concern.
func OperationActivityCompactLine(ops []OperationActivityNode) string {
	var op *OperationActivityNode
	for i := range ops {
		if ops[i].Active {
			op = &ops[i] // last active op wins (deepest/newest appended last)
		}
	}
	if op == nil {
		return ""
	}
	reason := ""
	for i := range op.Calls {
		if op.Calls[i].Active {
			reason = op.Calls[i].Reason
		}
	}
	if reason == "" {
		return op.Description
	}
	return op.Description + " ‣ " + reason
}
