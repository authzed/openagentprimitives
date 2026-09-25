package capability

import (
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { Register(&triggerStatusCapability{}) }

// triggerStatusCapability offers the two tools that let an agent answer the
// EVENT that started its session, on that event's own status surface — a
// GitHub check run today.
//
// Gated on the INPUT channel kind, resolved through the one shared lookup
// (chregistry.TriggerStatusReporterFor). A kind whose trigger has no status
// surface — every conversational kind — contributes nothing and is untouched;
// nothing here names a kind, and a second kind that grows a surface is offered
// these tools without this file changing.
//
// # Why default-on
//
// Unlike artifacts, which needs an operator to choose renderers, there is no
// choice to make here: a kind either has a surface for its trigger or it does
// not, and an event left permanently unanswered is never what an operator
// wanted. Making it opt-in would reproduce, at configuration level, exactly the
// failure this seam exists to end — a review that finished with the pull
// request still showing "in progress", because the step that answers it was
// missing.
//
// # Why the input binding and not the resolved channel
//
// RunnerEnv.Resolved* is the OUTBOUND channel (resolve.ForSession prefers
// spec.outputChannel), which for a review agent is the Slack thread the reply
// goes to — not the pull request that started the session. The trigger is the
// INPUT binding's, always, which is what OfferContext.Binding already carries.
type triggerStatusCapability struct{}

func (triggerStatusCapability) Name() string          { return "trigger_status" }
func (triggerStatusCapability) DefaultOn() bool       { return true }
func (triggerStatusCapability) Infrastructural() bool { return false }

// triggerStatusConfig is the parsed per-capability config.
//
// PublishedText decides who AUTHORS the text this capability's tools publish
// on the trigger surface. "model" (and absent — the default) is the original
// behavior: the model supplies conclude_trigger_status's summary and may
// supply its own details link. "composed" removes every model-authored byte
// from the surface: the tool takes only the outcome enum, the summary is a
// fixed text per outcome, and the details link is always the framework's
// own. A class whose surface is readable by more people than its channel —
// a GitHub check run, readable by everyone who can read the repository —
// opts into "composed" so a security finding delivered through the channel
// cannot be re-described in public by the status that announces it.
type triggerStatusConfig struct {
	PublishedText string `json:"publishedText"`
}

const (
	publishedTextModel    = "model"
	publishedTextComposed = "composed"
)

func (triggerStatusCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return triggerStatusConfig{}, nil
	}
	var cfg triggerStatusConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	switch cfg.PublishedText {
	case "", publishedTextModel, publishedTextComposed:
		return cfg, nil
	default:
		// Refused rather than defaulted: falling back to model text would
		// silently undo the exact guarantee the operator asked for.
		return nil, fmt.Errorf("publishedText must be %q or %q, got %q",
			publishedTextModel, publishedTextComposed, cfg.PublishedText)
	}
}

// ActsOnProviderSurface declares ProviderSurface: both tools below write to the
// system that raised the trigger — a check run on the pull request itself — and
// nothing local can stand in for that write. See ProviderSurface.
func (triggerStatusCapability) ActsOnProviderSurface() {}

func (triggerStatusCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → no trigger to report on, not a skip
	}
	// THE shared lookup, the same one both tools resolve through at call time.
	// A kind that does not report is a normal inactive state and not a skip:
	// every conversational kind lands here, and reporting each of them would
	// bury the skips that mean something.
	reporter, ok := chregistry.TriggerStatusReporterFor(o.Binding.Kind)
	if !ok {
		return nil, nil
	}

	// ComposedTextOnly defaults to false — model-authored text, the original
	// behavior — for a nil or absent config; only an explicit
	// {publishedText: composed} switches it. The e2e composed-mode scenario
	// is what pins this wire end to end.
	composedOnly := false
	if tc, ok := o.Config.(triggerStatusConfig); ok {
		composedOnly = tc.PublishedText == publishedTextComposed
	}

	cfg := meta.TriggerStatusConfig{
		KindName: o.Binding.Kind,
		// The kind's own words for what it reports on, so the prompt text
		// lands with the kind rather than being restated here — the same rule
		// artifact renderers' Instructions() follow.
		SurfaceKind:        reporter.TriggerSurfaceKind(),
		ChannelName:        o.Binding.Name,
		Binding:            o.Binding,
		ProviderAPIBaseURL: o.Env.TriggerStatusAPIBaseURL,
		// So conclude_trigger_status can name the result this session
		// delivered rather than asking the model for a link. Nil here is a
		// normal state (no webd on this cluster), and the tool degrades to a
		// conclusion with no details link.
		WebdBaseURL:      o.Env.WebdBaseURL,
		ComposedTextOnly: composedOnly,
	}
	return []tool.Tool{
		meta.NewClaimTriggerStatus(cfg),
		meta.NewConcludeTriggerStatus(cfg),
	}, nil
}
