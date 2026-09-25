package extract_test

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

type fakeExploder struct{ mimes []string }

func (f fakeExploder) MIMEs() []string { return f.mimes }

func (f fakeExploder) Explode(io.ReaderAt, int64, extract.Limits, func(extract.Member) error) (extract.Summary, error) {
	return extract.Summary{}, nil
}

func TestRegisterExploderPanicsOnDuplicateClaim(t *testing.T) {
	extract.RegisterExploder(fakeExploder{mimes: []string{"application/x-test-dup"}})
	assert.Panics(t, func() {
		extract.RegisterExploder(fakeExploder{mimes: []string{"application/x-test-dup"}})
	}, "two exploders claiming one MIME would make dispatch depend on import order")
}

func TestExploderForIgnoresParameters(t *testing.T) {
	extract.RegisterExploder(fakeExploder{mimes: []string{"application/x-test-params"}})
	_, ok := extract.ExploderFor("application/x-test-params; charset=binary")
	assert.True(t, ok, "parameters must not defeat the lookup, same as extract.For")
}

// The registries are separate because a MIME may legitimately have an exploder
// and no extractor. Sharing one map would make application/zip resolve to an
// Extractor, whose Result{Text} cannot express members.
func TestExploderRegistryIsSeparateFromExtractorRegistry(t *testing.T) {
	extract.RegisterExploder(fakeExploder{mimes: []string{"application/x-test-sep"}})

	_, isExtractor := extract.For("application/x-test-sep")
	assert.False(t, isExtractor, "an exploder must not answer extractor lookups")

	_, isExploder := extract.ExploderFor("text/plain")
	assert.False(t, isExploder, "an extractor must not answer exploder lookups")
}

func TestExplodableMIMEsIsSorted(t *testing.T) {
	extract.RegisterExploder(fakeExploder{mimes: []string{"application/x-test-zzz", "application/x-test-aaa"}})
	got := extract.ExplodableMIMEs()
	require.NotEmpty(t, got)
	assert.IsIncreasing(t, got, "readiness output and registry assertions both depend on a stable order")
}

func TestDefaultLimitsAreAllPositive(t *testing.T) {
	l := extract.DefaultLimits
	// A zero bound would mean "unlimited" to any check written as `n > lim`,
	// which is the one way a limits struct fails open.
	assert.Positive(t, l.MaxUncompressedTotal)
	assert.Positive(t, l.MaxMemberBytes)
	assert.Positive(t, l.MaxMembers)
	assert.Positive(t, l.MaxRatio)
	assert.Positive(t, l.MaxWallClock)
}
