package haiku

// register.go self-registers this backend into judge's registry
// (judge.Register) at package init, mirroring every other registry backend
// in this repo (AGENTS.md: "New variants of an aspect get registered, not
// branched on"). The wrinkle: unlike judge's built-in deterministic backend
// (which needs nothing to construct), this one needs a live llm.Provider,
// which in turn needs ANTHROPIC_API_KEY. Registration must not require that
// key to already be set -- a binary blank-importing this package does so
// unconditionally, the same way it blank-imports every channel kind or
// identity flow, often before any env var has been read.
//
// lazyJudge resolves the real *Judge on first Classify call instead of at
// init time. A resolution failure (key unset, providers.New erroring) is
// returned as a Classify error -- NOT cached permanently and NOT a panic --
// so it falls through the exact same fail-closed path as a live Send error:
// the caller (E8) maps any Classify error to TierMutate. A successful
// resolution IS cached, so the provider is constructed at most once per
// process even though the lazyJudge instance itself is registered once at
// init.
import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

// envAPIKey is the credential this backend resolves at first use in
// production.
const envAPIKey = "ANTHROPIC_API_KEY"

type lazyJudge struct {
	mu sync.Mutex
	j  *Judge
}

// Name implements judge.ConsequenceJudge.
func (l *lazyJudge) Name() string { return Name }

// Classify implements judge.ConsequenceJudge.
func (l *lazyJudge) Classify(ctx context.Context, a judge.Action, page judge.PageContext) (judge.Verdict, error) {
	j, err := l.resolve()
	if err != nil {
		return judge.Verdict{}, err
	}
	return j.Classify(ctx, a, page)
}

func (l *lazyJudge) resolve() (*Judge, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.j != nil {
		return l.j, nil
	}
	apiKey := os.Getenv(envAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("haiku judge: %s not set", envAPIKey)
	}
	j, err := NewFromAPIKey(apiKey)
	if err != nil {
		return nil, err
	}
	l.j = j
	return j, nil
}

func init() {
	judge.Register(&lazyJudge{})
}
