package slack_bot_token

import (
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
)

// The registered flow is the only one that opens a real browser and the only
// one that writes a file, and this is the only line that gives it either
// ability. Everything else — every test, every caller constructing a Flow of
// its own — has to supply both, so "opens a window on somebody's desktop" and
// "leaves a file in somebody's directory" are capabilities that are handed
// over rather than ones that have to be taken away.
//
// The manifest lands beside the run, in the directory oap was invoked from.
// "." rather than a resolved absolute path because that is what the Slack
// channel wizard's own manifest copy is given (WorkingDir, set by
// `oap channel create` and `oap agent install`), and two surfaces that leave
// the same kind of file in the same place should say where the same way.
func init() { builtins.Register(New(browser.Open, ".")) }
