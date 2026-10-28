package livemirror

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
)

// replayInteractions folds display events only. Historical approval cards are
// observations; rendering one never creates standing or bypasses a decision.
func replayInteractions(entries []memory.Entry) ([]TimelineEntry, error) {
	records := []interactionhistory.Content{}
	for _, e := range entries {
		if e.Kind != interactionhistory.KindName {
			continue
		}
		if e.Provenance == nil || e.Provenance.Publisher != "system:channelsd" {
			return nil, fmt.Errorf("interaction history requires channelsd provenance")
		}
		var c interactionhistory.Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			return nil, err
		}
		if c.Source.Namespace == "" || c.Source.Name == "" || c.SourceUID == "" || c.At.IsZero() || (c.Request == nil) == (c.Applied == nil) {
			return nil, fmt.Errorf("invalid interaction history record")
		}
		if c.Request != nil {
			if err := c.Request.Validate(); err != nil {
				return nil, err
			}
			if c.Request.AgentSessionRef != c.Source {
				return nil, fmt.Errorf("interaction history source mismatch")
			}
		} else {
			if err := c.Applied.Validate(); err != nil {
				return nil, err
			}
			if c.Applied.AgentSessionRef != c.Source || c.Applied.MintedURL != "" || c.Applied.ResponseRef != "" {
				return nil, fmt.Errorf("invalid interaction resolution history")
			}
		}
		records = append(records, c)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].At.Before(records[j].At) })
	out := []TimelineEntry{}
	positions := map[string]int{}
	// Correlate resolutions independently of storage order; a resolution must
	// survive arriving before its request during replay or websocket bootstrap.
	resolved := map[string]interactionhistory.Content{}
	key := func(c interactionhistory.Content, category, ref string) string {
		return c.Source.Namespace + "/" + c.Source.Name + "/" + c.SourceUID + "/" + category + "/" + ref
	}
	for _, c := range records {
		if c.Applied != nil {
			resolved[key(c, c.Applied.Category, c.Applied.RequestRef)] = c
		}
	}
	for _, c := range records {
		if c.Request == nil {
			continue
		}
		cat, ok := channelinteractions.Get(c.Request.Category)
		if !ok || cat.Resurface != channelinteractions.ResurfaceCached {
			continue
		}
		k := key(c, c.Request.Category, c.Request.RequestRef)
		if idx, ok := positions[k]; ok {
			out[idx].InteractionRequest = c.Request
			continue
		}
		entry := TimelineEntry{Kind: "interaction", InteractionRequest: c.Request, CreatedAt: c.At}
		if resolution, ok := resolved[k]; ok {
			entry.InteractionApplied = resolution.Applied
		}
		positions[k] = len(out)
		out = append(out, entry)
	}
	return out, nil
}
