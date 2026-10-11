package browser

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestWaitForClampsTimeout(t *testing.T) {
	cases := []struct {
		name        string
		seconds     int
		wantDur     time.Duration
		wantClamped bool
	}{
		{name: "huge value clamps to 30s", seconds: 3600, wantDur: 30 * time.Second, wantClamped: true},
		{name: "above-max clamps to 30s", seconds: 31, wantDur: 30 * time.Second, wantClamped: true},
		{name: "in-range passes through unclamped", seconds: 5, wantDur: 5 * time.Second, wantClamped: false},
		{name: "exactly max is not clamped", seconds: 30, wantDur: 30 * time.Second, wantClamped: false},
		{name: "exactly min is not clamped", seconds: 1, wantDur: 1 * time.Second, wantClamped: false},
		{name: "negative clamps up to 1s", seconds: -5, wantDur: 1 * time.Second, wantClamped: true},
		{name: "unset (0) defaults to 30s, not reported as clamped", seconds: 0, wantDur: 30 * time.Second, wantClamped: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, clamped := clampWaitTimeout(tc.seconds)
			assert.Equal(t, tc.wantDur, d)
			assert.Equal(t, tc.wantClamped, clamped)
			assert.LessOrEqual(t, d, 30*time.Second, "effective deadline must never exceed 30s")
		})
	}
}

func TestScreenshotOfferedOnlyWithImages(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedBrowserToolbox{
		Name: "demo",
		Ref:  "demo-browser",
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access:      spiceboxv1alpha1.BrowserAccess{Mode: "blocklist"},
			Interaction: "readonly",
			State:       "ephemeral",
		},
	}

	hasScreenshot := func(acceptsImages bool) bool {
		tools, err := Synthesize(rt, "http://127.0.0.1:1", "tok", acceptsImages, nil)
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		for _, tl := range tools {
			if strings.HasSuffix(tl.Name(), "_screenshot") {
				return true
			}
		}
		return false
	}

	assert.False(t, hasScreenshot(false), "screenshot must be absent when the model does not accept images")
	assert.True(t, hasScreenshot(true), "screenshot must be offered when the model accepts images")
}

// TestActingToolsOfferedOnlyInActMode proves the acting tools
// (click/press/select/submit) are offered in act mode and ABSENT in readonly,
// while the observation set is identical across both.
func TestActingToolsOfferedOnlyInActMode(t *testing.T) {
	base := spiceboxv1alpha1.ResolvedBrowserToolbox{
		Name: "demo",
		Ref:  "demo-browser",
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{Mode: "blocklist"},
			State:  "ephemeral",
		},
	}
	readonly := base
	readonly.Spec.Interaction = "readonly"
	act := base
	act.Spec.Interaction = "act"

	ro, err := Synthesize(readonly, "http://127.0.0.1:1", "tok", true, nil)
	assert.NoError(t, err)
	ac, err := Synthesize(act, "http://127.0.0.1:1", "tok", true, nil)
	assert.NoError(t, err)

	actingSuffixes := []string{"_type", "_click", "_press", "_select", "_submit"}
	for _, suf := range actingSuffixes {
		assert.False(t, hasSuffix(toolNames(ro), suf), "readonly must NOT offer %s", suf)
		assert.True(t, hasSuffix(toolNames(ac), suf), "act must offer %s", suf)
	}

	// Removing the acting tools from the act set must leave exactly the
	// readonly (observation) set — nothing else differs between the two modes.
	assert.Equal(t, toolNames(ro), withoutSuffixes(toolNames(ac), actingSuffixes),
		"act = readonly observation set plus exactly the acting tools")
}

func hasSuffix(names []string, suffix string) bool {
	for _, n := range names {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

func withoutSuffixes(names, suffixes []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		drop := false
		for _, s := range suffixes {
			if strings.HasSuffix(n, s) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, n)
		}
	}
	return out
}
