package provenance

import (
	"context"
	"sync"
)

// TagLedger carries, for the span of ONE turn's tool dispatch and result-wrap,
// the one pt-tag fact that crosses the hook↔runner boundary: the tag minted for
// each tool result, so the emitter can wrap that result in its pt-untrusted
// envelope. It lives in ctx because the writer (the PostToolCall mint hook,
// package hooks) and the reader (the runner's result-wrap, package runner) never
// share a struct — the same reason toolguard.WithProbeLedger threads its per-call
// ledger here.
type TagLedger struct {
	mu     sync.Mutex
	minted map[string]string // toolUseID -> minted tag id
}

type tagLedgerKey struct{}

// WithTagLedger returns a ctx carrying a fresh ledger and the ledger itself.
func WithTagLedger(ctx context.Context) (context.Context, *TagLedger) {
	l := &TagLedger{minted: map[string]string{}}
	return context.WithValue(ctx, tagLedgerKey{}, l), l
}

// TagLedgerFrom returns the ledger carried by ctx, or nil when none was opened.
// Callers MUST nil-check: a code path with no ledger (a test, a non-tagging
// session) is legal and means "record nothing".
func TagLedgerFrom(ctx context.Context) *TagLedger {
	l, _ := ctx.Value(tagLedgerKey{}).(*TagLedger)
	return l
}

func (l *TagLedger) RecordMinted(useID, tagID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.minted[useID] = tagID
}

func (l *TagLedger) MintedFor(useID string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, ok := l.minted[useID]
	return id, ok
}
