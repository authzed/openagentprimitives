// pkg/channels/channelkinds/slack/appprovision/tokensource.go
//
// Where a run's app-configuration token comes from. Two sources today; a third
// is a registration, not a branch at the consumer.
package appprovision

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Source keys. Also the --answer vocabulary, so they are stable strings.
const (
	KeyPaste    = "paste"
	KeySlackCLI = "slack-cli"
)

// TokenSource yields an app-configuration token for one provisioning run.
//
// Available, NeedsPastedToken and UnattendedReason are methods rather than
// facts a caller looks up per key: the screen offering the choice must not
// know which source needs a binary, which needs a paste and which needs a
// human, or adding a third source means editing the screen.
type TokenSource interface {
	// Key identifies the source in State and in --answer.
	Key() string
	// Label is what the choice screen shows.
	Label() string
	// Available reports whether this source can run on this machine.
	//
	// "This machine" is the one the call happens on, which is not always the
	// operator's — see NeedsOperatorShell, which a caller must consult FIRST.
	// A source that needs the operator's own shell answers this by probing the
	// local filesystem, and under a server that filesystem belongs to the
	// serving container.
	Available() bool
	// NeedsOperatorShell reports whether this source only makes sense when the
	// wizard is running in the operator's own shell — same machine, same
	// terminal, same PATH as the person answering.
	//
	// It is asked BEFORE Available, and a caller that cannot offer that shell
	// must skip such a source without probing it at all. The order is the
	// point: Available's own answer is computed from the host the call lands
	// on, so asking it first under a server both offers a route the operator
	// cannot use AND makes the offered set depend on what happens to be
	// installed in a container.
	//
	// A method rather than a fact a caller looks up per key, for the same
	// reason the three around it are: the code offering the choice must not
	// know which source shells out, or a third source means editing it.
	NeedsOperatorShell() bool
	// NeedsPastedToken reports whether the screen must collect a token before
	// Token can be called.
	NeedsPastedToken() bool
	// UnattendedReason reports why this source cannot serve a run with nobody
	// at the terminal, or "" when it can.
	//
	// It is the ONE home for that rule. "Needs a human" is not the same
	// question as "needs a pasted token" — a source can mint its own token and
	// still be unusable unattended — and inferring one from the other at a
	// consumer is how the two answers drift. Both sites that care read this:
	// the wizard's up-front refusal of a --non-interactive run, and the
	// fail-closed guard in front of the call itself.
	//
	// A reason, not a bool, because the refusal is shown to a user who has to
	// decide what to do instead, and only the source knows what it is waiting
	// for.
	UnattendedReason() string
	// Token returns a configuration token. pasted is what the screen collected,
	// and is ignored by a source that needs none.
	Token(ctx context.Context, pasted string) (string, error)
}

var registry = map[string]TokenSource{}

// Register adds a source. Called from init; a duplicate key panics, because two
// sources answering to one name is a build-time mistake rather than a runtime
// condition anything could recover from.
func Register(s TokenSource) {
	if _, dup := registry[s.Key()]; dup {
		panic("appprovision: duplicate token source " + s.Key())
	}
	registry[s.Key()] = s
}

// Sources returns every registered source, sorted by key so a select's option
// order does not churn between runs.
func Sources() []TokenSource {
	out := make([]TokenSource, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// SourceFor looks up one source by key.
func SourceFor(key string) (TokenSource, bool) {
	s, ok := registry[key]
	return s, ok
}

func init() { Register(pasteSource{}) }

// pasteSource is the documented route: the user generates a token at
// api.slack.com and pastes it. Always available, which makes it the floor
// every machine has.
type pasteSource struct{}

func (pasteSource) Key() string   { return KeyPaste }
func (pasteSource) Label() string { return "Paste a configuration token from api.slack.com" }

func (pasteSource) Available() bool { return true }

// NeedsOperatorShell is false: the token arrives as an answer, so this source
// runs identically wherever the wizard is rendered. It is what keeps the
// provisioning route offering at least one option to a server-side client.
func (pasteSource) NeedsOperatorShell() bool { return false }

func (pasteSource) NeedsPastedToken() bool { return true }

// UnattendedReason is empty: the token arrives as an answer, so a flag can
// supply it and nothing here waits on a person. This is the source a
// --non-interactive run uses.
func (pasteSource) UnattendedReason() string { return "" }

func (pasteSource) Token(_ context.Context, pasted string) (string, error) {
	tok := strings.TrimSpace(pasted)
	if tok == "" {
		return "", errors.New("no configuration token was supplied")
	}
	return tok, nil
}
