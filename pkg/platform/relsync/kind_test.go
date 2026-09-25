package relsync_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// fakeKind is a minimal relsync.Kind fixture: a struct with a name field and
// stub methods returning zero values, used to exercise the registry without
// a real upstream.
type fakeKind struct {
	name string
}

func (f fakeKind) Name() string { return f.name }

func (f fakeKind) ListScopes(ctx context.Context, params relsync.SourceParams, after relsync.Cursor) (relsync.ScopePage, error) {
	return relsync.ScopePage{}, nil
}

func (f fakeKind) FetchScope(ctx context.Context, params relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	return relsync.ScopeContent{}, nil
}

func (f fakeKind) Source() relsource.Source { return relsource.Source{} }

// A cursor carries the kind that minted it, so one kind can never consume
// another's opaque token. Slack's base64 next_cursor read as a SCIM
// startIndex does not error — it silently starts at the wrong offset, which
// is why this is a typed refusal rather than a convention.
func TestCursor_ForKindRefusesAnotherKindsToken(t *testing.T) {
	c := relsync.Cursor{Kind: "slack", Token: "dGVhbTpDMDE5"}

	_, err := c.ForKind("onepassword")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "slack")
	assert.Contains(t, err.Error(), "onepassword")
}

func TestCursor_ForKindAcceptsItsOwnToken(t *testing.T) {
	c := relsync.Cursor{Kind: "slack", Token: "dGVhbTpDMDE5"}

	tok, err := c.ForKind("slack")

	require.NoError(t, err)
	assert.Equal(t, "dGVhbTpDMDE5", tok)
}

// The zero Cursor means "start from the beginning" for any kind — a first
// pass has no token and must not be refused.
func TestCursor_ZeroValueIsAcceptedByAnyKind(t *testing.T) {
	tok, err := relsync.Cursor{}.ForKind("onepassword")

	require.NoError(t, err)
	assert.Equal(t, "", tok)
}

// A cursor with Kind set but an empty Token is still "this kind's cursor,
// nothing to resume from" — e.g. a kind that mints an empty-string token for
// its own first page rather than leaving Kind unset. Its own kind must
// accept it.
func TestCursor_KindSetTokenEmptyIsAcceptedByItsOwnKind(t *testing.T) {
	tok, err := relsync.Cursor{Kind: "slack"}.ForKind("slack")

	require.NoError(t, err)
	assert.Equal(t, "", tok)
}

// A cursor with a Token but no Kind tag is malformed — not the zero value
// (which means "start over"), and not a genuine tagged token from any real
// kind. It must be refused rather than treated as "start over", because
// silently accepting it would defeat the whole point of tagging: an
// untagged token could have been minted by anyone.
func TestCursor_TokenSetKindEmptyIsRefused(t *testing.T) {
	_, err := relsync.Cursor{Token: "x"}.ForKind("slack")

	require.Error(t, err)
}

// A cursor tagged for one kind but carrying an empty token is still that
// kind's cursor — not the universal zero value — and must be refused by a
// different kind exactly like a non-empty token would be. This is the
// boundary a naive "shortcut on c.Token == \"\" alone" simplification would
// erase: it would let an empty-token, wrong-kind cursor slip through as
// "start over" instead of being refused.
func TestCursor_KindSetTokenEmptyIsRefusedByOtherKind(t *testing.T) {
	_, err := relsync.Cursor{Kind: "slack"}.ForKind("onepassword")

	require.Error(t, err)
}

func TestRegistry_PanicsOnDuplicateKindName(t *testing.T) {
	relsync.Register(fakeKind{name: "dup-fixture"})

	assert.Panics(t, func() { relsync.Register(fakeKind{name: "dup-fixture"}) })
}
