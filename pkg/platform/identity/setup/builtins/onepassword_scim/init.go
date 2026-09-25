package onepassword_scim

import (
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
)

// The registered flow is the only one that opens a real browser, and this is
// the only line that gives it the ability to. Everything else — every test,
// every caller constructing a Flow of its own — has to supply an opener, so
// "opens a window on somebody's desktop" is a capability that is handed over
// rather than one that has to be taken away.
func init() { builtins.Register(New(browser.Open)) }
