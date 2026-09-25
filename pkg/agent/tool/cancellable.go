package tool

import "context"

// Cancellable is an OPTIONAL capability declaring a Tool can be cancelled
// mid-call and can run teardown that cancelling the call's context does not
// achieve alone. The runner type-asserts it (like OriginTool); a tool without it
// is gracefully non-interruptible, and a batch containing one is not offered
// "Interrupt & Send Now".
//
// The runner ALWAYS cancels the per-call context on interrupt — for MCP that
// makes the go-sdk emit notifications/cancelled — and Cancel runs IN ADDITION,
// for teardown the context cannot do (deleting a sandbox ToolCall CR). Cancel
// must be safe to call from a goroutine other than Execute's and must not block
// indefinitely.
type Cancellable interface {
	Cancel(ctx context.Context) error
}
