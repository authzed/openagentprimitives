package browser

// pagestate_test.go covers PageFingerprint as a pure function over hand-built
// PageState values: sort-stability (list order must never affect the hash)
// and field-sensitivity (a change to any one field must change the hash).
// This runs with no Chrome at all; pagestate_chrome_test.go covers the real
// DOM-extraction half (capturePageState) against a live browser.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPageFingerprintSortStabilityAndFieldSensitivity(t *testing.T) {
	base := PageState{
		URLPath:        "/trips/AA1234/seats",
		Skeleton:       []string{"button|confirm seat 14c|/trips/seats/update|false", "link|my trips|/trips|false"},
		TargetSig:      "button|confirm seat 14c|/trips/seats/update|false",
		ContainerSigs:  []string{"button|cancel||false", "button|confirm seat 14c|/trips/seats/update|false"},
		ContainerTexts: []string{"seat change fee: $342.50"},
	}
	baseFP := PageFingerprint(base)

	t.Run("identical PageState is deterministic across repeated calls", func(t *testing.T) {
		assert.Equal(t, baseFP, PageFingerprint(base))
	})

	t.Run("reordered lists produce the identical fingerprint", func(t *testing.T) {
		reordered := base
		reordered.Skeleton = []string{base.Skeleton[1], base.Skeleton[0]}
		reordered.ContainerSigs = []string{base.ContainerSigs[1], base.ContainerSigs[0]}
		assert.Equal(t, baseFP, PageFingerprint(reordered), "sorting must make list order irrelevant")
	})

	t.Run("PageFingerprint does not mutate its input slices", func(t *testing.T) {
		before := append([]string(nil), base.Skeleton...)
		_ = PageFingerprint(base)
		assert.Equal(t, before, base.Skeleton, "hashing must not reorder the caller's slice in place")
	})

	cases := []struct {
		name   string
		mutate func(PageState) PageState
	}{
		{"url path differs", func(ps PageState) PageState {
			ps.URLPath = "/login"
			return ps
		}},
		{"skeleton differs", func(ps PageState) PageState {
			ps.Skeleton = append([]string{}, ps.Skeleton...)
			ps.Skeleton[0] = "button|shop hotels|/promo|false"
			return ps
		}},
		{"target sig differs (e.g. disabled flipped)", func(ps PageState) PageState {
			ps.TargetSig = "button|confirm seat 14c|/trips/seats/update|true"
			return ps
		}},
		{"container sigs differ", func(ps PageState) PageState {
			ps.ContainerSigs = append([]string{}, ps.ContainerSigs...)
			ps.ContainerSigs[0] = "button|cancel order||false"
			return ps
		}},
		{"container texts differ (the price-change TOCTOU case)", func(ps PageState) PageState {
			ps.ContainerTexts = []string{"seat change fee: $402.50"}
			return ps
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" -> different fingerprint", func(t *testing.T) {
			assert.NotEqual(t, baseFP, PageFingerprint(tc.mutate(base)))
		})
	}
}
