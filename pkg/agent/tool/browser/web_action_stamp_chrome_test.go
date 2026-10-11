package browser

// web_action_stamp_chrome_test.go drives Phase E's E8 classify-and-stamp pass
// (browserTool.NamedArgs) against a real headless Chrome: a mutate acting call
// stamps the web_action key + page fingerprint + mutate flag; a benign call
// stamps none; a judge error fails closed to a mutate stamp; and the
// deterministic fast-path skips the injected model for an obviously-benign link.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// countingJudge is an injectable ConsequenceJudge that records whether it was
// consulted, so a test can prove the deterministic fast-path skipped the model.
type countingJudge struct {
	verdict judge.Verdict
	err     error
	calls   int
}

func (c *countingJudge) Name() string { return "counting-fake" }

func (c *countingJudge) Classify(_ context.Context, _ judge.Action, _ judge.PageContext) (judge.Verdict, error) {
	c.calls++
	if c.err != nil {
		return judge.Verdict{}, c.err
	}
	return c.verdict, nil
}

// mutateVerdict/benignVerdict build the two distributions classifyMutate reads:
// it gates when Probs[TierMutate] >= defaultMutateThreshold (0.15).
func mutateVerdict() judge.Verdict {
	return judge.Verdict{Tier: judge.TierMutate, Probs: map[judge.Tier]float64{judge.TierMutate: 0.9}}
}
func benignVerdict() judge.Verdict {
	return judge.Verdict{Tier: judge.TierNavigate, Probs: map[judge.Tier]float64{judge.TierMutate: 0.0, judge.TierNavigate: 1.0}}
}

var fingerprintHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestNamedArgsWebActionStamp drives one live page offering a plain button (which
// the deterministic judge cannot clear, so the injected model decides) and a
// navigational link (which the deterministic fast-path clears without the model).
func TestNamedArgsWebActionStamp(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<div id="checkout"><button>Place order</button></div>` +
			`<a href="/next">Next page</a>` +
			`</body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	btnRef := refForRole(t, read.Content, "button")
	linkRef := refForRole(t, read.Content, "link")

	click, ok := toolBy(t, tools, "click").(*browserTool)
	require.True(t, ok)

	assertNoWebActionArgs := func(t *testing.T, named map[string]any) {
		t.Helper()
		assert.Equal(t, "site.test", named[hostArgKey], "the host is always stamped for an acting op")
		for _, k := range []string{browserschema.WebActionRequiredArg, browserschema.WebActionKeyArg, browserschema.WebActionFingerprintArg} {
			_, present := named[k]
			assert.False(t, present, "a benign call must NOT stamp %s", k)
		}
	}

	t.Run("mutate verdict stamps key, fingerprint and the mutate flag", func(t *testing.T) {
		cj := &countingJudge{verdict: mutateVerdict()}
		click.judge = cj

		named, ok := click.NamedArgs(map[string]any{"ref": btnRef})
		require.True(t, ok)

		assert.Equal(t, 1, cj.calls, "a button is not deterministically benign, so the model IS consulted")
		assert.Equal(t, true, named[browserschema.WebActionRequiredArg], "a mutate must stamp the mutate flag")
		key, _ := named[browserschema.WebActionKeyArg].(string)
		assert.Contains(t, key, "site.test/click/", "the web_action key names the host and op (E4 ActionKey)")
		assert.NotEmpty(t, key)
		fp, _ := named[browserschema.WebActionFingerprintArg].(string)
		assert.Regexp(t, fingerprintHex, fp, "a mutate must stamp a 64-hex page fingerprint (E5 PageFingerprint)")
	})

	t.Run("benign model verdict stamps no web_action args", func(t *testing.T) {
		cj := &countingJudge{verdict: benignVerdict()}
		click.judge = cj

		named, ok := click.NamedArgs(map[string]any{"ref": btnRef})
		require.True(t, ok)

		assert.Equal(t, 1, cj.calls, "the model is consulted (deterministic cannot clear a bare button)")
		assertNoWebActionArgs(t, named)
	})

	t.Run("judge error fails closed to a mutate stamp", func(t *testing.T) {
		cj := &countingJudge{err: errors.New("judge backend unavailable")}
		click.judge = cj

		named, ok := click.NamedArgs(map[string]any{"ref": btnRef})
		require.True(t, ok)

		assert.Equal(t, 1, cj.calls, "the model was consulted and errored")
		assert.Equal(t, true, named[browserschema.WebActionRequiredArg],
			"a judge error must be treated as a mutate (fail-closed), stamping the gate args")
		key, _ := named[browserschema.WebActionKeyArg].(string)
		assert.NotEmpty(t, key, "a fail-closed mutate still stamps a key so the second check runs")
	})

	t.Run("deterministic benign fast-path skips the model entirely", func(t *testing.T) {
		// A link-follow is positively benign: the deterministic fast-path must
		// resolve it to navigate WITHOUT consulting the injected model — proven by
		// a model wired to ERROR if it is ever called.
		cj := &countingJudge{err: errors.New("the model must not be consulted for a navigational link")}
		click.judge = cj

		named, ok := click.NamedArgs(map[string]any{"ref": linkRef})
		require.True(t, ok)

		assert.Equal(t, 0, cj.calls, "the deterministic fast-path must skip the model for a navigational link")
		assertNoWebActionArgs(t, named)
	})
}
