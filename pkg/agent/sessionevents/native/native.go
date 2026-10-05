// Package native verifies platform-signed native observations. Trust roots and
// live source authority are supplied by the binary; transport data never selects
// its own verifier or current resource incarnation.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionobservation"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

const Key = "native"

// Authority checks the exact current UID and trusted scope mapping, and returns
// all additional read dependencies. A missing/deleted/recreated source denies.
// It must not infer source ownership from payload text or agent claims.
type Authority interface {
	CheckSource(context.Context, sessionevents.Source, memory.Entry) ([]sessionevents.Dependency, error)
}

type Adapter struct {
	// Memory retains the verified signed witness before intake may acknowledge it.
	Memory    memory.Memory
	Access    sessionevents.SourceAccess
	Keys      provenance.PublisherKeyLookup
	Authority Authority
	// Only explicitly configured platform publishers are accepted. Session
	// publishers remain refused even when their keys verify cryptographically.
	Publishers map[string]bool
}

func (a *Adapter) Resolve(ctx context.Context, principal string, source sessionevents.Source) (sessionevents.Source, error) {
	if a == nil || a.Access == nil {
		return sessionevents.Source{}, sessionevents.ErrDenied
	}
	return a.Access.Resolve(ctx, principal, source)
}

func (a *Adapter) Check(ctx context.Context, principal string, source sessionevents.Source, deps []sessionevents.Dependency) error {
	if a == nil || a.Access == nil {
		return sessionevents.ErrDenied
	}
	return a.Access.Check(ctx, principal, source, deps)
}

func (*Adapter) Kind() string { return Key }
func (a *Adapter) Verify(ctx context.Context, raw json.RawMessage) (sessionevents.Input, error) {
	var result sessionevents.Input
	if a == nil || a.Keys == nil || a.Authority == nil {
		return result, sessionevents.ErrDenied
	}
	if len(raw) > 2*sessionevents.MaxDataBytes {
		return result, sessionevents.ErrInvalid
	}
	var entry memory.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return result, fmt.Errorf("%w: decode native observation: %v", sessionevents.ErrInvalid, err)
	}
	if entry.Kind != sessionobservation.KindName || entry.Provenance == nil || !strings.HasPrefix(entry.Provenance.Publisher, "system:") || !a.Publishers[entry.Provenance.Publisher] {
		return result, sessionevents.ErrDenied
	}
	if err := provenance.VerifyEntrySignature(a.Keys, entry); err != nil {
		return result, errors.Join(sessionevents.ErrDenied, err)
	}
	var content sessionobservation.Content
	if err := json.Unmarshal(entry.Content, &content); err != nil {
		return result, fmt.Errorf("%w: decode native observation content: %v", sessionevents.ErrInvalid, err)
	}
	observation := content.Observation
	if err := observation.Validate(); err != nil {
		return result, err
	}
	if observation.Source.Kind != Key || entry.Scope.Kind != "session" || entry.Scope.ID != observation.Source.ID || !strings.HasPrefix(entry.Scope.ID, observation.Source.Namespace+"/") {
		return result, sessionevents.ErrDenied
	}
	deps, err := a.Authority.CheckSource(ctx, observation.Source, entry)
	if err != nil {
		return result, err
	}
	// Retain signed dependencies even when a current lookup adds more. A
	// normalization step can never erase an information-flow restriction.
	seen := map[sessionevents.Dependency]bool{{ResourceType: "agentsession", ResourceID: observation.Source.ID, Permission: "read_transcript"}: true}
	for _, dep := range append(observation.Dependencies, deps...) {
		seen[dep] = true
	}
	observation.Dependencies = nil
	for dep := range seen {
		observation.Dependencies = append(observation.Dependencies, dep)
	}
	sort.Slice(observation.Dependencies, func(i, j int) bool {
		a, b := observation.Dependencies[i], observation.Dependencies[j]
		if a.ResourceType != b.ResourceType {
			return a.ResourceType < b.ResourceType
		}
		if a.ResourceID != b.ResourceID {
			return a.ResourceID < b.ResourceID
		}
		return a.Permission < b.Permission
	})
	if err = observation.Validate(); err != nil {
		return result, err
	}
	observation.Witness = &sessionevents.Witness{Kind: "signed-memory-entry", Reference: entry.Scope.ID + "/" + entry.ID, Digest: provenance.EntryDigest(entry)}
	result = sessionevents.Input{Observation: observation, Publisher: entry.Provenance.Publisher, Sequence: int64(entry.Provenance.Seq)}
	if err = result.Validate(); err != nil {
		return sessionevents.Input{}, err
	}
	if a.Memory != nil {
		if _, err := a.Memory.Put(ctx, entry); err != nil {
			return sessionevents.Input{}, fmt.Errorf("retain native witness: %w", err)
		}
	}
	return result, nil
}

var _ sessionevents.Adapter = (*Adapter)(nil)

func (a *Adapter) Dependencies(ctx context.Context, principal string, source sessionevents.Source) ([]sessionevents.Dependency, error) {
	if a == nil || a.Access == nil {
		return nil, sessionevents.ErrDenied
	}
	d, ok := a.Access.(sessionevents.SourceDependencies)
	if !ok {
		return nil, sessionevents.ErrDenied
	}
	return d.Dependencies(ctx, principal, source)
}
