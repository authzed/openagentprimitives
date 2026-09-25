// Package chatembed serves the ONE-session browser chat as an embeddable
// page: GET /chat-embed/{ns}/{name} mounts the transcript data plane's chat
// view and the shell's startup line for that session, with no tabs, no header
// and no session list, so another same-origin page (the agent-builder's Test
// panel, through ap:chat) can frame it. Authorization is the viewer's own
// agentsession#interact, checked in the page build — the framed page is the
// enforcement; the framing page adds nothing.
package chatembed

import (
	"context"

	"github.com/go-logr/logr"
)

// Deps is what this page needs from webd. CheckInteract is the ONLY gate —
// the same check the chat data plane and the session view use; an error is a
// fail-closed 500, never a denial.
type Deps interface {
	CheckInteract(ctx context.Context, ns, name, subject string) (bool, error)
	Logger() logr.Logger
}
