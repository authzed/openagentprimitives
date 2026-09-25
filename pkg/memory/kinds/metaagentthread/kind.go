// Package metaagentthread is the memory Kind for messages in the
// metaagent's sub-thread (user mentions + metaagent responses).
// Channelsd writes inbound; authzd writes outbound. The primary
// agent's runner does NOT read this Kind — that's the prompt-injection
// isolation contract.
package metaagentthread

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Role enumerates the message origin within the metaagent sub-thread.
const (
	RoleUserMention     = "user_mention"
	RoleMetaagentNotice = "metaagent_notice"
	RoleApprovalBlock   = "approval_block"
)

// Content is one message in the metaagent sub-thread.
type Content struct {
	// Ts is when the message was appended to the sub-thread.
	Ts time.Time `json:"ts"`
	// Role is one of the Role* constants above — what produced the message.
	Role string `json:"role"`
	Body string `json:"body"`
	// RequesterID is the user who sent a RoleUserMention; empty otherwise.
	RequesterID string `json:"requesterId,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "metaagent_thread"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "mth-" }

// WriteAuthority: authzd owns the metaagent thread (internal/cmd/authzd/metaagent.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"ts", "role"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
