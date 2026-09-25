package githubapp

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// Adapt bridges a Minter (the JWT-signing/exchange implementation in
// minter.go, typically an *HTTPMinter) into credkind.Deps.GitHubApp's
// shape, so binaries wiring up a broker can write
// deps.GitHubApp = githubapp.Adapt(githubapp.NewHTTPMinter()) without
// hand-rolling the translation.
//
// The two interfaces cannot be the same type: credkind.GitHubAppMinter's
// method signature is deliberately primitive-typed rather than reusing
// MintRequest/MintedToken, because credkind (which declares Deps) cannot
// import this package — kind.go in this same package must import credkind
// to implement credkind.Kind, and the reverse edge would be a cycle. See
// credkind.GitHubAppMinter's own doc comment.
//
// Adapt(nil) returns a genuine nil interface, NOT a non-nil interface
// wrapping a nil Minter. Without this guard, minterAdapter{nil} would be a
// typed-nil-in-an-interface: deps.GitHubApp == nil would read false and
// Kind.Resolve's fail-closed check would never fire, so the panic would
// happen one call deeper, inside a reconcile where controller-runtime's
// panic recovery hides it — exactly the production scar this package's own
// nil-interface discipline exists to prevent.
func Adapt(m Minter) credkind.GitHubAppMinter {
	if m == nil {
		return nil
	}
	return minterAdapter{m}
}

type minterAdapter struct{ m Minter }

func (a minterAdapter) Mint(ctx context.Context, appID string, privateKeyPEM []byte, installationID string) (sensitive.SensitiveValue, time.Time, error) {
	mt, err := a.m.Mint(ctx, MintRequest{AppID: appID, PrivateKeyPEM: privateKeyPEM, InstallationID: installationID})
	if err != nil {
		return sensitive.SensitiveValue{}, time.Time{}, err
	}
	return mt.AccessToken, mt.ExpiresAt, nil
}
