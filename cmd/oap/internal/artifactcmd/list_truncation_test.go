package artifactcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPageWindow pins how --limit interacts with the store's own paging. The
// interesting row is the boundary: a page that ends exactly on the limit leaves
// nothing behind, so only the continue token can say whether more exists — and
// treating that case as "truncated" unconditionally would print a notice over
// every listing whose count happens to divide by the page size.
func TestPageWindow(t *testing.T) {
	cases := []struct {
		name           string
		limit          int
		shown          int
		pageLen        int
		wantTake       int
		wantLeftBehind bool
	}{
		{name: "unlimited takes the whole page and leaves nothing", limit: 0, shown: 0, pageLen: 100, wantTake: 100, wantLeftBehind: false},
		{name: "limit larger than the page takes the whole page", limit: 500, shown: 0, pageLen: 100, wantTake: 100, wantLeftBehind: false},
		{name: "limit inside the page takes part of it and leaves the rest", limit: 30, shown: 0, pageLen: 100, wantTake: 30, wantLeftBehind: true},
		{name: "limit exactly the page length takes it all and leaves nothing", limit: 100, shown: 0, pageLen: 100, wantTake: 100, wantLeftBehind: false},
		{name: "limit already reached takes nothing and leaves the page behind", limit: 30, shown: 30, pageLen: 100, wantTake: 0, wantLeftBehind: true},
		{name: "remaining budget spans a later page", limit: 250, shown: 200, pageLen: 100, wantTake: 50, wantLeftBehind: true},
		{name: "an empty page leaves nothing behind whatever the limit", limit: 30, shown: 30, pageLen: 0, wantTake: 0, wantLeftBehind: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			take, leftBehind := pageWindow(tc.limit, tc.shown, tc.pageLen)
			assert.Equal(t, tc.wantTake, take, "items taken from this page")
			assert.Equal(t, tc.wantLeftBehind, leftBehind, "items the limit left unshown on this page")
		})
	}
}
