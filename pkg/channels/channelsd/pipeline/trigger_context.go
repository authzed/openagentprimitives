package pipeline

import "github.com/authzed/openagentprimitives/pkg/channels/channelkinds"

func triggerEvent(ev channelkinds.InboundEvent) string { event, _ := ev.TriggerPayload(); return event }
func triggerBody(ev channelkinds.InboundEvent) []byte  { _, body := ev.TriggerPayload(); return body }
