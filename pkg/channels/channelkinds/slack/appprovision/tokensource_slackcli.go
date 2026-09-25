// pkg/channels/channelkinds/slack/appprovision/tokensource_slackcli.go
//
// The Slack CLI as a token source: `slack auth token` prints a service token
// that the app-configuration API accepts.
//
// That command is Slack's ticket flow: it prints a /slackauthticket command to
// run inside Slack and then waits for the challenge code Slack shows in
// return. A person has to be there to carry the code between the two, which is
// what UnattendedReason below declares — the subprocess is run with its output
// captured and no terminal of its own, so a run with nobody watching is
// refused before it starts rather than left waiting at a prompt no one can see
// or answer.
package appprovision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// slackBin is the Slack CLI executable. A package var so tests can point the
// source at a stand-in without a real install — the same seam pkg/platform/cloud uses
// for gcloud.
var slackBin = "slack"

type slackCLISource struct{}

func init() { Register(slackCLISource{}) }

func (slackCLISource) Key() string   { return KeySlackCLI }
func (slackCLISource) Label() string { return "Use the Slack CLI (`slack auth token`)" }

// NeedsOperatorShell is TRUE, and it is checked before Available so that
// Available is never even asked off the operator's machine.
//
// Everything this source does happens at a terminal: the binary is looked up
// on a PATH, the subprocess is run on a filesystem, and the ticket flow waits
// for a person to carry a challenge code back from Slack. Under a server none
// of those are the operator's — the PATH belongs to the serving container, so
// a `slack` binary in the image would offer this route to someone who cannot
// see it, and choosing it would exec that binary and block on a person who is
// not there.
func (slackCLISource) NeedsOperatorShell() bool { return true }

// Available reports whether the binary can be executed. A single LookPath call
// is correct for both shapes slackBin takes: production leaves it as the bare
// name "slack", which LookPath resolves by searching PATH; tests point it at
// an absolute path in a temp dir, and LookPath's documented behavior is to try
// a file containing a slash directly rather than consulting PATH at all. There
// is no case that needs a second branch — a source whose binary is absent is
// never offered, so the user picks between routes that exist rather than
// discovering the gap after choosing.
//
// It answers for the host it is CALLED on, which is only the operator's when
// NeedsOperatorShell has already been honored. See that method.
func (slackCLISource) Available() bool {
	_, err := exec.LookPath(slackBin)
	return err == nil
}

func (slackCLISource) NeedsPastedToken() bool { return false }

// UnattendedReason names the challenge code as the thing no flag can supply.
// It states the reason only: what to do instead is phrased in the wizard's own
// --answer vocabulary, which belongs to the screen rather than to a source
// that knows nothing about State keys.
func (slackCLISource) UnattendedReason() string {
	return "the Slack CLI mints its token through Slack's ticket flow — it prints a /slackauthticket " +
		"command to run in Slack and waits for the challenge code that comes back, which needs a " +
		"person at the terminal"
}

func (slackCLISource) Token(ctx context.Context, _ string) (string, error) {
	cmd := exec.CommandContext(ctx, slackBin, "auth", "token")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("`%s auth token` failed: %s", slackBin, detail)
	}
	tok := strings.TrimSpace(stdout.String())
	if tok == "" {
		return "", errors.New("`slack auth token` printed nothing; run `slack login` first")
	}
	return tok, nil
}
