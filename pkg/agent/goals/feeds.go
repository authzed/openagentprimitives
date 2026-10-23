package goals

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

// Config describes feeds installed by the agent's operator. It is discovery
// guidance only: each source still requires live access and exact watch consent.
type Config struct {
	Enabled    *bool       `json:"enabled,omitempty"`
	EventFeeds []EventFeed `json:"eventFeeds,omitempty"`
}

type EventFeed struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Source      sessionevents.Source `json:"source"`
	EventKinds  []string             `json:"eventKinds"`
	Sources     []Source             `json:"sources,omitempty"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, err
		}
	}
	if len(c.EventFeeds) > 32 {
		return c, fmt.Errorf("%w: at most 32 configured event feeds", ErrInvalid)
	}
	names := map[string]bool{}
	for i := range c.EventFeeds {
		f := &c.EventFeeds[i]
		// Dependencies are computed by the source adapter, never class-supplied.
		if len(f.Sources) > 0 {
			return c, fmt.Errorf("%w: event feed dependencies cannot be configured", ErrInvalid)
		}
		if strings.TrimSpace(f.Name) == "" || len(f.Name) > 128 || names[f.Name] || len(f.Description) > 1024 || len(f.EventKinds) < 1 || len(f.EventKinds) > 32 {
			return c, fmt.Errorf("%w: invalid configured event feed", ErrInvalid)
		}
		names[f.Name] = true
		source := f.Source
		if source.UID == "" {
			source.UID = "resolved-at-read"
		}
		if err := source.Validate(); err != nil {
			return c, fmt.Errorf("%w: invalid configured source", ErrInvalid)
		}
		for _, kind := range f.EventKinds {
			if strings.TrimSpace(kind) == "" || len(kind) > 1024 {
				return c, fmt.Errorf("%w: invalid event kind", ErrInvalid)
			}
		}
	}
	return c, nil
}
