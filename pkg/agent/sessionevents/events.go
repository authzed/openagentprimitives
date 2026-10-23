// Package sessionevents owns verified observations for asynchronous sessions.
// An observation is data, not an instruction or execution authorization.
package sessionevents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var (
	ErrInvalid  = errors.New("invalid session observation")
	ErrConflict = errors.New("session observation or checkpoint conflict")
	ErrNotFound = errors.New("session observation not found")
	ErrDenied   = errors.New("session observation access denied")
)

const MaxDataBytes = 256 * 1024

// Source pins a resource incarnation. Recreating a resource starts a new stream.
// Adapters derive this from verified ingress, never agent-selected ownership.
type Source struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	UID       string `json:"uid"`
}

func (s Source) Validate() error {
	for _, value := range []string{s.Kind, s.Namespace, s.ID, s.UID} {
		if strings.TrimSpace(value) == "" || len(value) > 1024 || !utf8.ValidString(value) {
			return ErrInvalid
		}
	}
	return nil
}

func (s Source) Key() string { return mustDigest(s) }

// Dependency retains the current access checks needed before exposing data or
// acting on it. The source adapter supplies these through its authority lookup.
type Dependency struct {
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceID"`
	Permission   string `json:"permission"`
}

type Witness struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

type Observation struct {
	Witness      *Witness        `json:"witness,omitempty"`
	Source       Source          `json:"source"`
	EventID      string          `json:"eventID"`
	Kind         string          `json:"kind"`
	Subject      string          `json:"subject"`
	ObservedAt   time.Time       `json:"observedAt"`
	Data         json.RawMessage `json:"data"`
	Dependencies []Dependency    `json:"dependencies"`
}

func (o Observation) Validate() error {
	if o.Source.Validate() != nil || o.ObservedAt.IsZero() || len(o.Data) == 0 || len(o.Data) > MaxDataBytes || !json.Valid(o.Data) || len(o.Dependencies) < 1 || len(o.Dependencies) > 32 {
		return ErrInvalid
	}
	for _, value := range []string{o.EventID, o.Kind, o.Subject} {
		if strings.TrimSpace(value) == "" || len(value) > 1024 || !utf8.ValidString(value) {
			return ErrInvalid
		}
	}
	for _, d := range o.Dependencies {
		for _, value := range []string{d.ResourceType, d.ResourceID, d.Permission} {
			if strings.TrimSpace(value) == "" || len(value) > 1024 || !utf8.ValidString(value) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func (o Observation) ID() string {
	return "obs-" + mustDigest(struct {
		Source  Source
		EventID string
	}{o.Source, o.EventID})
}

func (o Observation) Digest() (string, error) {
	// A redelivery may have another signed envelope. Retain the first witness,
	// while identity and conflict checks commit to the data and restrictions.
	o.Witness = nil
	return jsonDigest(o)
}

// mustDigest is used only with structs of strings, whose JSON encoding cannot
// fail. A failure here is a programming error, rather than an empty identity.
func mustDigest(value any) string {
	result, err := jsonDigest(value)
	if err != nil {
		panic(fmt.Sprintf("encode event identity: %v", err))
	}
	return result
}

func jsonDigest(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Input carries the adapter's verified cursor separately from event identity.
// Sequence orders a publisher stream, not provider event times. It may skip
// positions occupied by records the adapter does not ingest.
type Input struct {
	Observation Observation
	Publisher   string
	Sequence    int64
}

func (i Input) Validate() error {
	if i.Observation.Validate() != nil || strings.TrimSpace(i.Publisher) == "" || len(i.Publisher) > 1024 || i.Sequence < 1 {
		return ErrInvalid
	}
	return nil
}

type Checkpoint struct {
	Source    Source
	Publisher string
	Sequence  int64
}

// Store atomically retains one immutable observation and advances its stream
// checkpoint. expected is a compare-and-swap value, read from Checkpoint. A
// replay already at/before the durable checkpoint returns the original record;
// conflicting reuse never rewrites evidence or advances the cursor.
type Store interface {
	Ingest(context.Context, Input, int64) (Observation, error)
	Get(context.Context, Source, string) (Observation, error)
	Checkpoint(context.Context, Source, string) (Checkpoint, error)
}

// Adapter verifies and normalizes transport input with its configured trust
// roots and live source lookup. No agent tool exposes this interface.
type Adapter interface {
	Kind() string
	Verify(context.Context, json.RawMessage) (Input, error)
}

type Registry struct {
	kinds *kindregistry.Registry[Adapter]
}

func NewRegistry() *Registry {
	return &Registry{kinds: kindregistry.New[Adapter]("session event sources", func(a Adapter) string { return a.Kind() })}
}

func (r *Registry) Register(a Adapter) { r.kinds.Register(a) }
func (r *Registry) Kinds() []string    { return r.kinds.Keys() }

// SourceAccess resolves a reviewed source incarnation and rechecks access. Each
// adapter owns its resource mapping; consumers never branch on a source kind.
type SourceAccess interface {
	Resolve(context.Context, string, Source) (Source, error)
	Check(context.Context, string, Source, []Dependency) error
}

func (r *Registry) Resolve(ctx context.Context, principal string, source Source) (Source, error) {
	if r == nil {
		return Source{}, ErrDenied
	}
	a, ok := r.kinds.Get(source.Kind)
	if !ok {
		return Source{}, ErrDenied
	}
	access, ok := a.(SourceAccess)
	if !ok {
		return Source{}, ErrDenied
	}
	return access.Resolve(ctx, principal, source)
}

func (r *Registry) Check(ctx context.Context, principal string, source Source, deps []Dependency) error {
	if r == nil {
		return ErrDenied
	}
	a, ok := r.kinds.Get(source.Kind)
	if !ok {
		return ErrDenied
	}
	access, ok := a.(SourceAccess)
	if !ok {
		return ErrDenied
	}
	return access.Check(ctx, principal, source, deps)
}

type Ingester struct {
	Store    Store
	Adapters *Registry
}

func (s *Ingester) Ingest(ctx context.Context, kind string, raw json.RawMessage) (Observation, error) {
	if s.Store == nil || s.Adapters == nil {
		return Observation{}, ErrDenied
	}
	if len(raw) > 2*MaxDataBytes {
		return Observation{}, ErrInvalid
	}
	adapter, ok := s.Adapters.kinds.Get(kind)
	if !ok {
		return Observation{}, fmt.Errorf("%w: unknown source adapter %q", ErrDenied, kind)
	}
	input, err := adapter.Verify(ctx, raw)
	if err != nil {
		return Observation{}, err
	}
	if err = input.Validate(); err != nil {
		return Observation{}, err
	}
	if input.Observation.Source.Kind != kind {
		return Observation{}, ErrDenied
	}
	checkpoint, err := s.Store.Checkpoint(ctx, input.Observation.Source, input.Publisher)
	if err != nil {
		return Observation{}, err
	}
	return s.Store.Ingest(ctx, input, checkpoint.Sequence)
}

// SourceDependencies exposes the live flow dependencies for discovered sources.
// Source kinds without this contract cannot advertise private feed metadata.
type SourceDependencies interface {
	Dependencies(context.Context, string, Source) ([]Dependency, error)
}

func (r *Registry) Dependencies(ctx context.Context, principal string, source Source) ([]Dependency, error) {
	if r == nil {
		return nil, ErrDenied
	}
	a, ok := r.kinds.Get(source.Kind)
	if !ok {
		return nil, ErrDenied
	}
	d, ok := a.(SourceDependencies)
	if !ok {
		return nil, ErrDenied
	}
	return d.Dependencies(ctx, principal, source)
}
