package runner

import (
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// PublishTimeoutApplied routes a runner's timeout through the decision pipeline.
// Publishing OUT here would bypass the retained winner if approval races expiry.
// sign may be nil in an in-process fixture; production signs the IN subject.
func PublishTimeoutApplied(publish func(string, []byte) error, sign func(string, *channelevents.Envelope) error, namespace, session string, envelope channelevents.Envelope) error {
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(namespace, session), envelope.Kind)
	if sign != nil {
		if err := sign(subject, &envelope); err != nil {
			return fmt.Errorf("sign timeout outcome: %w", err)
		}
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal timeout outcome: %w", err)
	}
	if err := publish(subject, raw); err != nil {
		return fmt.Errorf("publish timeout outcome on IN: %w", err)
	}
	return nil
}
