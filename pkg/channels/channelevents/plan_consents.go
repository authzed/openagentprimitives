package channelevents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// PlanConsentFields exposes every child's reviewed terms on the existing field
// surface, so browser, Slack, and text transports display the same authority.
func PlanConsentFields(consents []InteractionRequestPayload) []InteractionField {
	var fields []InteractionField
	for i, child := range consents {
		prefix := fmt.Sprintf("Reminder %d", i+1)
		fields = append(fields, InteractionField{Label: prefix, Value: child.Lead})
		if child.Body != "" {
			fields = append(fields, InteractionField{Label: prefix + " · Execution", Value: child.Body})
		}
		for _, field := range child.Fields {
			field.Label = prefix + " · " + field.Label
			fields = append(fields, field)
		}
	}
	return fields
}

func PlanConsentExcerpt(consents []InteractionRequestPayload) *InteractionExcerpt {
	var parts []string
	for i, child := range consents {
		part := fmt.Sprintf("Reminder %d", i+1)
		if child.Excerpt != nil {
			part += " · " + child.Excerpt.Label + "\n" + child.Excerpt.Content
		}
		parts = append(parts, part)
	}
	return &InteractionExcerpt{Label: "Reminders included in this approval", Content: strings.Join(parts, "\n\n")}
}

// ValidatePlanConsents refuses hidden, rewritten, nested, or cross-session terms.
// The category's validator additionally compares each card with its signed
// original before any composed decision can commit.
func (p InteractionRequestPayload) ValidatePlanConsents() error {
	if len(p.Consents) == 0 {
		return nil
	}
	if len(p.Consents) > 8 {
		return fmt.Errorf("plan has too many consent requests")
	}
	fields := PlanConsentFields(p.Consents)
	if len(p.Fields) < len(fields) || !reflect.DeepEqual(p.Fields[len(p.Fields)-len(fields):], fields) || !reflect.DeepEqual(p.Excerpt, PlanConsentExcerpt(p.Consents)) {
		return fmt.Errorf("plan consent terms are not displayed exactly")
	}
	var details []InteractionRequestPayload
	if err := json.Unmarshal(p.Details, &details); err != nil {
		return fmt.Errorf("plan consent exact details differ from reviewed requests")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, p.Details, "", "  "); err != nil {
		return err
	}
	// Keep the complete disclosure inside the supported surfaces' limits;
	// refusing preparation is safer than silently truncating reviewed terms.
	if pretty.Len() > 128*1024 {
		return fmt.Errorf("plan consent exact details exceed 128 KiB; include fewer requests")
	}
	// Compare their wire representation: a time's location and monotonic clock
	// are not authority and do not survive JSON replay.
	reviewed, err := json.Marshal(p.Consents)
	if err != nil {
		return err
	}
	exact, err := json.Marshal(details)
	if err != nil || !bytes.Equal(exact, reviewed) {
		return fmt.Errorf("plan consent exact details differ from reviewed requests")
	}
	seen := map[string]bool{}
	for _, child := range p.Consents {
		if len(child.Consents) > 0 || child.AgentSessionRef != p.AgentSessionRef || child.RequestRef == p.RequestRef || seen[child.RequestRef] {
			return fmt.Errorf("invalid or duplicate plan consent")
		}
		seen[child.RequestRef] = true
		if child.ExpiresAt == nil || p.ExpiresAt == nil || p.ExpiresAt.After(*child.ExpiresAt) {
			return fmt.Errorf("plan approval outlives its consent request")
		}
		if err := child.Validate(); err != nil {
			return err
		}
	}
	return nil
}
