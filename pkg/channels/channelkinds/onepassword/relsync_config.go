package onepassword

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

var _ relsync.Configurable = (*SyncKind)(nil)

// ConfigScreens returns no screens: the SCIM Bridge's managed-group set is
// entirely upstream state (SyncKind's own doc's "Every group ... is one
// this deployment's admin opted into"), so beyond the endpoint this kind
// needs nothing else answered.
func (k *SyncKind) ConfigScreens(prior relsync.ExistingConfig) []relsync.ConfigScreen {
	return nil
}

// BuildConfig always returns nil, nil: there are no answers to turn into
// spec.config because ConfigScreens never asks any.
func (k *SyncKind) BuildConfig(answers map[string]string) (json.RawMessage, error) {
	return nil, nil
}

// NeedsEndpoint reports true: the SCIM Bridge is customer-hosted and has no
// default (errEndpointUnset already fails closed on an empty one at sync
// time — this keeps the CLI wizard consistent with that).
func (k *SyncKind) NeedsEndpoint() bool {
	return true
}

var _ relsync.CredentialSetup = (*SyncKind)(nil)

const (
	// onepasswordSetupFlowName is the builtins.Flow that walks an operator to
	// the SCIM bridge's bearer token.
	onepasswordSetupFlowName = "onepassword-scim"

	// onepasswordSetupIntent is the prose that flow reads. The bearer token
	// is minted whole at bridge setup and carries no per-use scope to narrow,
	// so this only names what the token is wanted for.
	onepasswordSetupIntent = "read-only access to managed groups and their members for directory sync"
)

// SetupFlow names the flow an operator with no SCIM bridge token yet is
// walked through. The bridge's own address is NOT part of it: that is
// spec.baseURL, which the CLI wizard's endpoint screen already collects (see
// NeedsEndpoint above), and asking for it twice is how the two come to
// disagree.
func (k *SyncKind) SetupFlow() (flow, intent string) {
	return onepasswordSetupFlowName, onepasswordSetupIntent
}

// SetupScopes returns nil: a SCIM bridge bearer token carries no per-use
// scope to narrow. It is minted whole at bridge setup and grants whatever the
// bridge grants, so there is nothing for an operator to tick.
func (k *SyncKind) SetupScopes() []string { return nil }
