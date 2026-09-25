package capability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
)

func init() { Register(&credentialUpdateCapability{}) }

// credentialUpdateMinWait floors MaxWait when it is derived from
// RunnerEnv.IdleTTL. IdleTTL sizes await_user_message's inbound wait, a far
// shorter horizon than "a human notices a credential card, opens it, and fills
// it in" — and some AgentClasses disable idle entirely (IdleTTL == 0). Unfloored,
// the blocked tool times out before anyone could see the card, silently burning
// one of the two asks in the per-credential lifetime budget.
//
// An ALIAS of credupdate.DefaultToolWait rather than a copied value: this is the
// lower term of the cross-binary ordering "tool wait <= park TTL <= link
// lifetime" that the assertion below enforces at compile time.
const credentialUpdateMinWait = credupdate.DefaultToolWait

// Compile-time ordering assertion, this site's half of the contract in
// pkg/platform/identity/credupdate/timing.go. Converting a negative constant to uint64
// is a compile error, so this statically asserts the tool gives up no later
// than the operator expires the request: raising this floor past the ask
// window would leave the tool blocking on a request that has already gone
// terminal -- caught at BUILD time, not by an agent hanging in production.
const _ = uint64(credupdate.DefaultAskWindow - credentialUpdateMinWait) // tool wait <= park TTL

// credentialUpdateCapability contributes request_credential_update.
//
// Opt-in (DefaultOn == false) on purpose: the tool can put a credential-entry
// form in front of a human, so an AgentClass has to ask for it rather than
// inherit it. The determination that actually guards that form lives
// operator-side in pkg/controllers/credentialupdaterequest; this gate is the
// coarser "should this agent be able to ask at all".
type credentialUpdateCapability struct{}

// credentialUpdateConfig is the sub-config under
// capabilities.credential_update. No capability-specific field exists yet —
// only the common {enabled} flag, which agentcaps.GrantOf already parses out
// of the SAME raw blob we're handed here (Raw is the whole value, not just
// the capability-specific remainder). Declaring Enabled here (rather than
// decoding into an entirely empty struct{}) lets DisallowUnknownFields
// distinguish "the common enabled flag" (tolerated) from "a typo'd/
// unsupported key" (rejected) — an empty struct{} would incorrectly reject a
// perfectly legal {"enabled": false}.
type credentialUpdateConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

func (credentialUpdateCapability) Name() string          { return "credential_update" }
func (credentialUpdateCapability) DefaultOn() bool       { return false }
func (credentialUpdateCapability) Infrastructural() bool { return false }

// ParseConfig rejects any key besides the common {enabled} flag. An unknown
// key under capabilities.credential_update in an AgentClass is a typo, not a
// silently-ignorable extension point (AGENTS.md: never silently drop errors).
func (credentialUpdateCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var cfg credentialUpdateConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("credential_update: parsing capability config: %w", err)
	}
	return nil, nil
}

func (credentialUpdateCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Env.CredentialUpdateClient == nil {
		return nil, &SkipReason{
			Capability: "credential_update",
			Reason:     "no cluster client wired into the runner; request_credential_update would have nothing to write to",
		}
	}
	return []tool.Tool{
		meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
			Client:       o.Env.CredentialUpdateClient,
			ToolLookup:   o.Env.ToolLookup,
			PollInterval: 2 * time.Second,
			MaxWait:      credentialUpdateMaxWait(o.Env.IdleTTL),
		}),
	}, nil
}

// credentialUpdateMaxWait derives the tool's MaxWait from the session's
// IdleTTL, floored at credentialUpdateMinWait. Zero IdleTTL (idle disabled)
// floors to the same default rather than reaching meta.NewCredentialUpdate as
// 0 — see credentialUpdateMinWait's doc for why a short/zero value is unsafe
// here specifically (a raw pass-through of IdleTTL is what the original task
// brief proposed and is wrong).
func credentialUpdateMaxWait(idleTTL time.Duration) time.Duration {
	if idleTTL < credentialUpdateMinWait {
		return credentialUpdateMinWait
	}
	return idleTTL
}
