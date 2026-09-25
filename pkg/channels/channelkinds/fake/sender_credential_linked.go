package fake

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fakeCredLinkedSender is the credential_linked sub-channel sender for the fake
// kind. It decodes incoming CredentialLinked envelopes and appends them to the
// per-channel Driver's credentialLinkeds queue, so an E2E can assert the OOB
// confirmation watcher published the right fields.
type fakeCredLinkedSender struct {
	drv *Driver
}

func newCredentialLinkedSender(drv *Driver) *fakeCredLinkedSender {
	return &fakeCredLinkedSender{drv: drv}
}

func (s *fakeCredLinkedSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if env.Kind != channelevents.KindCredentialLinked {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake credential_linked sender: unexpected envelope kind %q", env.Kind)
	}
	var p channelevents.CredentialLinkedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake credential_linked sender: parse payload: %w", err)
	}
	s.drv.mu.Lock()
	defer s.drv.mu.Unlock()
	s.drv.credentialLinkeds = append(s.drv.credentialLinkeds, recordedCredentialLinked{
		Payload:    p,
		SessionRef: sess,
	})
	return channelkinds.SubChannelSendResult{}, nil
}

// Verify the interface assertion at compile time.
var _ channelkinds.Sender = (*fakeCredLinkedSender)(nil)
