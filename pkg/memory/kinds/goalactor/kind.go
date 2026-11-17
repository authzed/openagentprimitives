// Package goalactor records channelsd's attestation of a goal session's human.
package goalactor

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const KindName = "goal_actor"

// InitialPromptRef names the opening prompt's inbound. That message is the
// runner's turn 0, placed from spec.prompt rather than drained from an inbox
// item, so it has no minted ref for the runner to carry.
const InitialPromptRef = "initial-prompt"

type Content struct {
	Owner      string `json:"owner"`
	SessionUID string `json:"sessionUID"`
	ClassUID   string `json:"classUID"`
	// InboundRef names the inbound this attestation was recorded for; the
	// same value rides on the inbox item the runner drains. It is a selector
	// only: the owner named here is what the server trusts, never the ref.
	InboundRef string `json:"inboundRef,omitempty"`
}

type Kind struct{}

func (Kind) Name() string                          { return KindName }
func (Kind) IDPrefix() string                      { return "goalactor-" }
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) SessionReadable() bool                 { return false }
func (Kind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true, EssentialWhileLive: true, NeverForkCopy: true}
}

func (Kind) ContentSchema() reflect.Type                  { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                      { return nil }
func (Kind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(context.Context, memory.Signal) error { return nil }
func init()                                                 { memory.RegisterKind(Kind{}) }

// Record is called only after channelsd has accepted the human's input. The
// supplied memory must sign as channelsd; unsigned component writes are refused.
func Record(ctx context.Context, signed memory.Memory, scope memory.Scope, content Content) error {
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:channelsd"), "system:channelsd")
	b, err := json.Marshal(content)
	if err != nil {
		return err
	}
	_, err = signed.Put(ctx, memory.Entry{Scope: scope, Kind: KindName, ID: memory.NewID(Kind{}), CreatedAt: time.Now().UTC(), Content: b})
	return err
}
