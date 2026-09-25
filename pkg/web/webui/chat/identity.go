package chat

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
)

// deriveIdentity converts the authenticated webui subject (the canonical
// "user:<base64...>" form the framework's auth middleware injects via
// webui.SubjectFromContext) into the ExternalIdentity the browser listener
// stamps on inbound events, and the Principal used for started_by writes and
// live-view link minting.
//
// Delegates to browsersession.DeriveIdentity, the one derivation the built-in
// chat and the agent-UI start path share (see its doc for the encoding).
// rehydrate calls this once to seed the Host it rebuilds; Registry's Submit*
// methods call it AGAIN per submit (via callerIdentity), deliberately not
// reusing that first result — a live entry outlives and is shared beyond the
// subject that built it (see sessionListener in session.go).
func deriveIdentity(subject string) (channelkinds.ExternalIdentity, identity.Principal, error) {
	ext, principal, err := browsersession.DeriveIdentity(identity.Subject(subject))
	if err != nil {
		return channelkinds.ExternalIdentity{}, identity.Principal{}, fmt.Errorf("derive chat identity: %w", err)
	}
	return ext, principal, nil
}
