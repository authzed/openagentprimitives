package runner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
)

// ResolveDataTagFor builds the lookup `delegate`'s `inputs` uses to turn a
// tool_use_id into the pt-tag minted for that call.
//
// Scoped to ONE session's own tag records, and that scoping is the security
// property rather than an implementation detail. A parent can only hand over
// data it produced here, because a tool_use_id from anywhere else resolves to
// nothing in this scope — so the model never needs, and never gets, the
// vocabulary to name a tag directly.
//
// It answers only "this session minted that tag". Whether the parent may
// DELEGATE it is `pt_tag:<T>#access@agentsession:<parent>`, checked by the
// SubagentRequest controller. This runs in the runner, the party the
// delegation constrains, so it can never be the gate.
//
// An unminted call resolves to the empty string with NO error. A tool that
// declared no read has no provenance to hand over, which is an answer rather
// than a failure — the caller turns it into a refusal the model can act on.
//
// A call that minted SEVERAL tags is an ERROR, and that is the third answer
// this function has rather than a failure mode it stumbled into. One tool_use
// stopped meaning one tag when the memory search began minting one tag per
// resource POOL it read, each governing only that pool's slice of the result.
// Picking the first match would bind the slot to one arbitrary pool and hand
// the child that pool's entries while dropping the rest — no disclosure (tag
// and content are minted together, so the audience always matches the bytes
// the child gets) but silent data loss, in the one hand-off whose entire job
// is to be explicit about what is being handed over.
func ResolveDataTagFor(mem memory.Memory, scope memory.Scope) func(context.Context, string) (string, error) {
	return func(ctx context.Context, toolUseID string) (string, error) {
		if toolUseID == "" {
			return "", nil
		}
		// System-approved, like the plan gate's own read of its audit log.
		// The capability door exists to stop a MODEL steering a read at a
		// scope it was not granted; this is the runner reading the session's
		// own provenance records, and the scope is fixed at construction from
		// the session key rather than taken from the call. The model's only
		// influence is which tool_use_id it names, and an id it did not
		// produce simply matches nothing.
		tags, err := pttag.List(memory.WithSystemApproval(ctx, "subagent_data_slots"), mem, scope)
		if err != nil {
			// Never "" here. An unreadable tag store is UNKNOWN, not empty,
			// and reporting empty would render as "that call produced no
			// data" — a refusal blaming the model for a store outage, and one
			// that would send it off to paste the data into the task text
			// instead, which is the laundering path slots exist to prevent.
			return "", fmt.Errorf("reading this session's provenance records: %w", err)
		}
		var matches []pttag.TagRecord
		for _, t := range tags {
			if t.ToolUseID == toolUseID {
				matches = append(matches, t)
			}
		}
		switch len(matches) {
		case 0:
			return "", nil
		case 1:
			return matches[0].ID, nil
		}
		return "", fmt.Errorf(
			"the call %q read %d resources (%s) and each one's data is tracked separately, "+
				"so there is no single result to hand over — name a call that read one resource, "+
				"or put what the other agent needs into the task text",
			toolUseID, len(matches), describeTagSources(matches))
	}
}

// describeTagSources renders what a tool_use's tags came from, for a refusal a
// model reads.
//
// Sorted and deduplicated, so the same ambiguity always produces the same
// sentence: the records arrive in createdAt order and two tags minted by one
// call can share a timestamp, which would otherwise make the message — and any
// test over it — depend on map or storage order.
//
// A tag with no sources still has to be counted: a derived tag carries its
// derivation rather than a source list, and rendering an empty parenthesis
// would tell the model nothing about why its call was refused. Such a tag
// contributes its own id, which is at least a thing an operator can grep.
func describeTagSources(matches []pttag.TagRecord) string {
	seen := map[string]struct{}{}
	for _, m := range matches {
		if len(m.Sources) == 0 {
			seen[m.ID] = struct{}{}
			continue
		}
		for _, s := range m.Sources {
			seen[s] = struct{}{}
		}
	}
	return strings.Join(slices.Sorted(maps.Keys(seen)), ", ")
}
