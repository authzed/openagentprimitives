package sessioncmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	apiv1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
)

// History locates non-parking requests; it never grants permission. The normal
// component decision ingress revalidates the decider and exact consent witness.
func collectHistoryPendings(sess *apiv1.AgentSession, history memory.QueryResult, now time.Time) ([]pendingEntry, error) {
	if history.Partial || history.Truncated {
		return nil, fmt.Errorf("cannot resolve approvals from incomplete interaction history")
	}
	source := channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
	requests := map[string]channelevents.InteractionRequestPayload{}
	resolved := map[string]bool{}
	for _, entry := range history.Entries {
		if entry.Kind != interactionhistory.KindName || entry.Provenance == nil || entry.Provenance.Publisher != "system:channelsd" {
			return nil, fmt.Errorf("interaction history %s requires channelsd provenance", entry.ID)
		}
		var record interactionhistory.Content
		if err := json.Unmarshal(entry.Content, &record); err != nil {
			return nil, fmt.Errorf("decode interaction history %s: %w", entry.ID, err)
		}
		if record.Source.Namespace == "" || record.Source.Name == "" || record.SourceUID == "" || record.At.IsZero() || (record.Request == nil) == (record.Applied == nil) {
			return nil, fmt.Errorf("invalid interaction history %s", entry.ID)
		}
		// Ancestor history may include delegated sessions. Never publish their
		// decisions onto this session, or accept records from a recreated UID.
		if record.Source != source || record.SourceUID != string(sess.UID) {
			continue
		}
		if record.Applied != nil {
			p := record.Applied
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("invalid resolution %s: %w", entry.ID, err)
			}
			if p.AgentSessionRef != source || p.MintedURL != "" || p.ResponseRef != "" {
				return nil, fmt.Errorf("invalid resolution source or display payload %s", entry.ID)
			}
			resolved[p.Category+"/"+p.RequestRef] = true
			continue
		}
		p := *record.Request
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("invalid request %s: %w", entry.ID, err)
		}
		if p.AgentSessionRef != source {
			return nil, fmt.Errorf("interaction request source mismatch %s", entry.ID)
		}
		cat, ok := channelinteractions.Get(p.Category)
		if !ok || cat.Park != "" {
			continue
		}
		if _, ok := approvalKindForCategory(p.Category); !ok {
			continue
		}
		approve, deny := false, false
		for _, action := range p.Actions {
			if action.Kind == channelevents.ActionKindDecision {
				approve = approve || action.ID == "approve"
				deny = deny || action.ID == "deny"
			}
		}
		if !approve || !deny {
			continue
		}
		key := p.Category + "/" + p.RequestRef
		if previous, ok := requests[key]; ok {
			a, err := json.Marshal(previous)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(p)
			if err != nil {
				return nil, err
			}
			if string(a) != string(b) {
				return nil, fmt.Errorf("conflicting interaction request %s", p.RequestRef)
			}
		}
		requests[key] = p
	}
	out := collectPendings(sess)
	ids := map[string]bool{}
	for _, p := range out {
		ids[p.RequestID] = true
	}
	keys := make([]string, 0, len(requests))
	for key := range requests {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := requests[key]
		if resolved[key] || (p.ExpiresAt != nil && !now.Before(*p.ExpiresAt)) {
			continue
		}
		if ids[p.RequestRef] {
			return nil, fmt.Errorf("ambiguous interaction request ID %s", p.RequestRef)
		}
		kind, _ := approvalKindForCategory(p.Category)
		out = append(out, pendingEntry{RequestID: p.RequestRef, Kind: kind})
		ids[p.RequestRef] = true
	}
	return out, nil
}
