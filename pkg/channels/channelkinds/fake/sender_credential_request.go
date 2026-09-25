package fake

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fakeCredReqSender is the credential_request sub-channel sender for the fake
// kind. It decodes incoming CredentialRequest envelopes and appends them to the
// per-channel Driver's credentialRequests queue, so an E2E can assert
// channelsd's watcher published the right fields.
type fakeCredReqSender struct {
	drv *Driver
}

func newCredentialRequestSender(drv *Driver) *fakeCredReqSender {
	return &fakeCredReqSender{drv: drv}
}

func (s *fakeCredReqSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if env.Kind != channelevents.KindCredentialRequest {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake credential_request sender: unexpected envelope kind %q", env.Kind)
	}
	var p channelevents.CredentialRequestPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake credential_request sender: parse payload: %w", err)
	}
	s.drv.mu.Lock()
	defer s.drv.mu.Unlock()
	s.drv.credentialRequests = append(s.drv.credentialRequests, recordedCredentialRequest{
		Payload:    p,
		SessionRef: sess,
	})
	return channelkinds.SubChannelSendResult{}, nil
}

// Verify the interface assertion at compile time.
var _ channelkinds.Sender = (*fakeCredReqSender)(nil)
