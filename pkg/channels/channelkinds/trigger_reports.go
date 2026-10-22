package channelkinds

import (
	"encoding/json"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TriggerReportPublisher identifies an input that publishes its final report on
// its verified provider trigger. Such an input can start without a separate
// conversational output Channel. Publication success remains a completion
// requirement; this capability does not record delivery by itself.
type TriggerReportPublisher interface {
	PublishesTriggerReports(*api.Channel) bool
}

// TriggerReportConfigPublisher answers the same question for a portable bundle
// declaration before install-time answers and a Channel exist. The kind owns
// argument decoding and rejects unsupported configurations.
type TriggerReportConfigPublisher interface {
	ConfigPublishesTriggerReports(json.RawMessage) bool
}
