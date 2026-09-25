package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&introspectionCapability{}) }

// introspectionCapability is infrastructural (always-on, cannot be disabled)
// and built LAST by Assemble: Env.AllToolsSoFar is set to the full merged
// tool list (every other capability's tools + NonMetaTools) just before this
// capability's Offer runs, so introspect_tool resolves over everything else
// assembled so far but never over itself.
type introspectionCapability struct{}

func (introspectionCapability) Name() string                                { return "introspection" }
func (introspectionCapability) DefaultOn() bool                             { return true }
func (introspectionCapability) Infrastructural() bool                       { return true }
func (introspectionCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (introspectionCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	// Name index captured before introspect_tool is appended, so it does not
	// resolve itself. Mirrors internal/cmd/runner/main.go:1235-1248.
	byName := make(map[string]tool.Tool, len(o.Env.AllToolsSoFar))
	for _, tt := range o.Env.AllToolsSoFar {
		byName[tt.Name()] = tt
	}
	return []tool.Tool{meta.NewIntrospect(meta.IntrospectConfig{
		Resolve: func(n string) (tool.Tool, bool) { tt, ok := byName[n]; return tt, ok },
		Names: func() []string {
			out := make([]string, 0, len(byName))
			for n := range byName {
				out = append(out, n)
			}
			return out
		},
	})}, nil
}
