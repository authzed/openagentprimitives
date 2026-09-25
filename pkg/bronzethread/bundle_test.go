package bronzethread_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// runnableBundle is the minimum a driver will accept: a name, somewhere to read
// the fixture from, a class, one model step, and exactly one way to start the
// run. Every case below doctors ONE field of it, so a case that passes because
// the base was already broken cannot hide.
func runnableBundle() bt.Bundle {
	return bt.Bundle{
		Name:       "demo-scenario",
		AgentDir:   "testdata/demo-scenario",
		AgentClass: "demo-agent",
		UserTurns:  []bt.UserTurn{{Text: "list the widgets"}},
		LLM:        []bt.LLMStep{{Reply: []bt.ReplyPart{{Text: "none"}}}},
	}
}

// TestBundle_Validate covers every structural precondition a driver has.
//
// This method is the one definition BOTH consumers use — threadrun.Load refuses
// on it, and the steelthread capture's self-check raises a hard finding on it —
// so a case here is a case for both. The shape that motivated lifting it out of
// the driver is "both userTurns and a trigger": a webhook-opened session a
// person also replied in folds to exactly that, and the capture emitted it
// reporting zero findings while the loader refused the file.
func TestBundle_Validate(t *testing.T) {
	withTrigger := func(b bt.Bundle) bt.Bundle {
		b.Trigger = &bt.Trigger{
			Channel:    "demo-hooks",
			Payload:    "payload.json",
			Event:      "pull_request",
			ChannelKey: "pr:demo-org/demo-repo#4",
		}
		return b
	}

	cases := []struct {
		name    string
		doctor  func(b bt.Bundle) bt.Bundle
		wantErr string // empty means the bundle must validate
	}{
		{
			name:   "the minimum runnable bundle validates",
			doctor: func(b bt.Bundle) bt.Bundle { return b },
		},
		{
			name: "a trigger-started bundle with no userTurns validates",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = nil
				return withTrigger(b)
			},
		},
		{
			name:    "no name: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.Name = ""; return b },
			wantErr: "bundle needs a name",
		},
		{
			name:    "no agentDir: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.AgentDir = ""; return b },
			wantErr: "bundle needs an agentDir",
		},
		{
			name:    "no agentClass: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.AgentClass = ""; return b },
			wantErr: "bundle needs an agentClass",
		},
		{
			// A session with no assistant turn folds to an empty LLM. The
			// capture's own no-agent-reply check is only a WARNING, so without
			// this precondition such a bundle emits clean and is refused on
			// load.
			name:    "no llm steps: refused",
			doctor:  func(b bt.Bundle) bt.Bundle { b.LLM = nil; return b },
			wantErr: "at least one llm step",
		},
		{
			// The flagship shape: a webhook opened the session and a person
			// replied in the thread. Only the SYNTHESIZED index-0 turn is
			// suppressed, so UserTurns survives alongside Trigger.
			name:    "both userTurns and a trigger: refused",
			doctor:  withTrigger,
			wantErr: "userTurns OR from a trigger, not both",
		},
		{
			name: "neither userTurns nor a trigger: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = nil
				return b
			},
			wantErr: "nothing would start the run",
		},
		{
			name: "a trigger with no channel: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = nil
				b = withTrigger(b)
				b.Trigger.Channel = ""
				return b
			},
			wantErr: "trigger needs a channel",
		},
		{
			name: "a trigger with no payload: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = nil
				b = withTrigger(b)
				b.Trigger.Payload = ""
				return b
			},
			wantErr: "trigger needs a payload",
		},
		{
			name: "a trigger with no event: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = nil
				b = withTrigger(b)
				b.Trigger.Event = ""
				return b
			},
			wantErr: "trigger needs an event",
		},
		{
			name: "a trigger with no channelKey: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.UserTurns = nil
				b = withTrigger(b)
				b.Trigger.ChannelKey = ""
				return b
			},
			wantErr: "trigger needs a channelKey",
		},
		{
			// Folded in rather than left as a separate call, so both consumers
			// get it from the one method.
			name: "a tool declared in both output maps: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.ToolOutputs = map[string]json.RawMessage{"list_widgets": json.RawMessage(`{}`)}
				b.ToolOutputSequence = map[string][]json.RawMessage{"list_widgets": {json.RawMessage(`{}`)}}
				return b
			},
			wantErr: "both toolOutputs and toolOutputSequence",
		},
		{
			name: "mintedIDs naming both families in the minted shape: validates",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MintedIDs = map[string][]string{
					bt.FamilyOperation: {"op-1111111111111111", "op-2222222222222222"},
					bt.FamilyArtifact:  {"artifact-3333333333333333"},
				}
				return b
			},
		},
		{
			// A typo'd key would otherwise pin nothing: Minter reads the map by
			// family name, so "operations" silently leaves the real family
			// unpinned and the replay mints its own ids.
			name: "mintedIDs under an unknown family: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MintedIDs = map[string][]string{"operations": {"op-1111111111111111"}}
				return b
			},
			wantErr: `mintedIDs["operations"] is not a minted-id family`,
		},
		{
			name: "an id filed under the wrong family: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MintedIDs = map[string][]string{bt.FamilyOperation: {"artifact-1111111111111111"}}
				return b
			},
			wantErr: `is an "artifact" id`,
		},
		{
			// The shape check is what keeps a pinned id distinguishable from a
			// hand-authored one: the capture files ids BY shape, so a value
			// this loose could never have come from one.
			name: "an id that is not the minted shape: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MintedIDs = map[string][]string{bt.FamilyOperation: {"op-1"}}
				return b
			},
			wantErr: `does not match any family's minted shape`,
		},
		{
			// Two operations sharing an id is not a sequence, it is an alias:
			// whichever call the second one belongs to would address the first.
			name: "the same id recorded twice in one family: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.MintedIDs = map[string][]string{
					bt.FamilyOperation: {"op-1111111111111111", "op-1111111111111111"},
				}
				return b
			},
			wantErr: "recorded twice",
		},
		{
			// The positive row for the seeds: fully-specified entries of both
			// kinds ride through, so the refusal rows below fail on the blank
			// field alone, never on the seeding feature being present at all.
			name: "fully-specified seeds: validates",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedUserPreferences = []bt.SeedUserPreference{
					{Subject: "email:demo-author@example.com", Key: "notifications", Value: json.RawMessage(`false`)},
				}
				b.SeedRelationships = []bt.SeedRelationship{
					{Resource: "github_user:99", Relation: "sole_user", Subject: "user:demo-author@example.com"},
				}
				return b
			},
		},
		{
			// A blank seed field is not "unpinned" the way an absent seed is:
			// the bundle claims to set up fixture state and silently sets up
			// none (see validateSeeds).
			name: "a preference seed with no subject: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedUserPreferences = []bt.SeedUserPreference{{Key: "notifications", Value: json.RawMessage(`false`)}}
				return b
			},
			wantErr: "seedUserPreferences[0] has no subject",
		},
		{
			name: "a preference seed with no key: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedUserPreferences = []bt.SeedUserPreference{{Subject: "email:demo-author@example.com", Value: json.RawMessage(`false`)}}
				return b
			},
			wantErr: "seedUserPreferences[0] has no key",
		},
		{
			name: "a preference seed with no value: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedUserPreferences = []bt.SeedUserPreference{{Subject: "email:demo-author@example.com", Key: "notifications"}}
				return b
			},
			wantErr: "seedUserPreferences[0] has no value",
		},
		{
			name: "a relationship seed with no resource: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedRelationships = []bt.SeedRelationship{{Relation: "sole_user", Subject: "user:demo-author@example.com"}}
				return b
			},
			wantErr: "seedRelationships[0] has no resource",
		},
		{
			name: "a relationship seed with no relation: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedRelationships = []bt.SeedRelationship{{Resource: "github_user:99", Subject: "user:demo-author@example.com"}}
				return b
			},
			wantErr: "seedRelationships[0] has no relation",
		},
		{
			name: "a relationship seed with no subject: refused",
			doctor: func(b bt.Bundle) bt.Bundle {
				b.SeedRelationships = []bt.SeedRelationship{{Resource: "github_user:99", Relation: "sole_user"}}
				return b
			},
			wantErr: "seedRelationships[0] has no subject",
		},
		{
			name:    "the zero bundle: refused",
			doctor:  func(bt.Bundle) bt.Bundle { return bt.Bundle{} },
			wantErr: "bundle needs a name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doctor(runnableBundle()).Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err, "this bundle must not be runnable")
			assert.Contains(t, err.Error(), tc.wantErr,
				"the message must name the precondition, or a capture's finding says only that something is wrong")
		})
	}
}
