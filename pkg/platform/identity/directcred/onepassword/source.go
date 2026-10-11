// Package onepassword implements the directcred.Source backed by 1Password,
// over the official 1Password Go SDK. It lists the Login items a service
// account can see in one vault and maps each item's autofill websites onto
// the host patterns the browser toolbox fills against, never carrying a field
// value into a listing.
//
// Layout follows a strict seam (spec 4.2): this file holds ALL mapping,
// Verify, List, and Fetch logic over a thin package-internal client
// interface; sdk.go is the ONLY file that imports the SDK and implements that
// interface by boring projection; init.go registers the Source. The seam
// exists because the SDK's core is a wasm module that cannot be pointed at an
// httptest server -- the logic here is unit-tested through a fake client, and
// the SDK adapter stays untested-but-trivial.
//
// Security posture (load-bearing): the service-account token is read from
// cfg.SecretData["token"] and is NEVER interpolated into an error or a log.
// Every error this package returns passes through scrubToken, so even an SDK
// core error that embedded the token cannot carry it out. A Ref carries only
// an item's id, title, and mapped hosts -- never a username, password, or any
// field value (directcred.Ref's contract).
package onepassword

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

const (
	// secretKeyToken is the SecretData key holding the 1Password service-account
	// token.
	secretKeyToken = "token"

	// Built-in Login-item field ids the SDK assigns to the username and
	// password fields. The SDK carries no "purpose" concept (see the report's
	// SDK-vs-brief notes); these stable ids are how a Login item's
	// username/password are identified.
	fieldIDUsername = "username"
	fieldIDPassword = "password"

	// fieldTypeTOTP is the SDK's ItemFieldType string for a one-time-code
	// field (opsdk.ItemFieldTypeTOTP), kept here as a plain string so source.go
	// needs no SDK import.
	fieldTypeTOTP = "Totp"

	// autofill* are the SDK's AutofillBehavior strings this package acts on. A
	// website with any other behavior (notably "Never") is not auto-fillable
	// and is dropped.
	autofillAnywhere    = "AnywhereOnWebsite"
	autofillExactDomain = "ExactDomain"
)

// client is the package-internal seam between the mapping/Verify/List/Fetch
// logic (this file) and the real 1Password SDK (sdk.go). It exists ONLY so
// that logic can be unit-tested: the SDK's core is a wasm module that cannot
// be pointed at an httptest server, so source_test.go injects a fake client
// and the SDK adapter in sdk.go stays thin and unconditionally boring.
//
// Every method returns the package-internal neutral projections below, never
// an SDK type -- that is what keeps the SDK import confined to sdk.go.
type client interface {
	// ListVaults returns every vault the service-account token can see.
	ListVaults(ctx context.Context) ([]vault, error)
	// ListLoginItems returns the Login-category items in the given vault.
	ListLoginItems(ctx context.Context, vaultID string) ([]listItem, error)
	// GetItem returns one item's full detail (including any freshly computed
	// OTP code), by vault and item id.
	GetItem(ctx context.Context, vaultID, itemID string) (itemDetail, error)
}

// vault is a neutral projection of the SDK's VaultOverview.
type vault struct {
	ID    string
	Title string
}

// website is a neutral projection of the SDK's Website. Autofill is the SDK's
// autofill-behavior string verbatim (one of the autofill* constants above);
// the mapping decision lives here in source.go, not in the adapter.
type website struct {
	URL      string
	Autofill string
}

// listItem is a neutral projection of the SDK's ItemOverview, carrying only
// what List needs -- never a field value.
type listItem struct {
	ID       string
	Title    string
	Websites []website
}

// field is a neutral projection of the SDK's ItemField. OTP is non-nil only
// for a TOTP field whose current code the SDK core computed on this read.
type field struct {
	ID        string
	FieldType string
	Value     string
	OTP       *otp
}

// otp carries a TOTP field's freshly computed code, or the reason it could
// not be computed. Never cached: produced anew on every GetItem.
type otp struct {
	Code  string
	Error string
}

// itemDetail is a neutral projection of the SDK's Item, carrying the fields
// Fetch needs.
type itemDetail struct {
	ID     string
	Title  string
	Fields []field
}

// Source is the directcred.Source impl backed by 1Password over the official
// SDK. It holds no per-call state: newClient builds a fresh backend client
// from the service-account token for each Verify/List/Fetch, matching the
// stateless Source contract.
type Source struct {
	// newClient builds the backend client from a service-account token. The
	// default (New) wires the real SDK adapter in sdk.go; tests inject a fake
	// so the logic below runs without the SDK's wasm core.
	newClient func(ctx context.Context, token string) (client, error)
}

// New returns a Source wired to the real 1Password SDK adapter.
func New() *Source { return &Source{newClient: newSDKClient} }

var _ directcred.Source = (*Source)(nil)

// Kind returns the registration key, directcred.SourceOnePassword.
func (*Source) Kind() string { return directcred.SourceOnePassword }

// Verify lists the vaults the token can see and confirms the configuration
// resolves to exactly one. Exactly one visible vault AND (cfg.Vault == "" or
// its name matches) yields its Pin; zero or many vaults, or a name mismatch,
// is an error naming the vault names/count found -- never the token.
func (s *Source) Verify(ctx context.Context, cfg directcred.Config) (directcred.Pin, error) {
	c, token, err := s.clientFor(ctx, cfg)
	if err != nil {
		return directcred.Pin{}, err
	}
	vaults, err := c.ListVaults(ctx)
	if err != nil {
		return directcred.Pin{}, scrubToken(wrapErr("listing vaults", err), token)
	}
	if len(vaults) != 1 {
		return directcred.Pin{}, fmt.Errorf(
			"onepassword: expected exactly one accessible vault, found %d: %v",
			len(vaults), vaultTitles(vaults))
	}
	v := vaults[0]
	if cfg.Vault != "" && v.Title != cfg.Vault {
		return directcred.Pin{}, fmt.Errorf(
			"onepassword: the accessible vault %q does not match the configured vault %q",
			v.Title, cfg.Vault)
	}
	return directcred.Pin{VaultID: v.ID, VaultName: v.Title}, nil
}

// List returns one Ref per Login item in the pinned vault whose websites map
// to at least one usable host, sorted by id. An item with no usable host is
// omitted. A Ref carries only id, title, and hosts -- never a field value.
func (s *Source) List(ctx context.Context, cfg directcred.Config) ([]directcred.Ref, error) {
	c, token, err := s.clientFor(ctx, cfg)
	if err != nil {
		return nil, err
	}
	vaultID, err := s.resolveVaultID(ctx, c, cfg, token)
	if err != nil {
		return nil, err
	}
	items, err := c.ListLoginItems(ctx, vaultID)
	if err != nil {
		return nil, scrubToken(wrapErr("listing login items", err), token)
	}

	refs := make([]directcred.Ref, 0, len(items))
	for _, it := range items {
		hosts := mapWebsites(it.Websites)
		if len(hosts) == 0 {
			// Spec: an item with no usable host is omitted.
			continue
		}
		refs = append(refs, directcred.Ref{ID: it.ID, Label: it.Title, Hosts: hosts})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

// Fetch returns one Login item's username/password and -- iff the item has a
// TOTP field -- an OTP func that re-reads the item for a fresh code on every
// call. id must be a Ref.ID previously returned by List for the same cfg.
func (s *Source) Fetch(ctx context.Context, cfg directcred.Config, id string) (directcred.Credential, error) {
	c, token, err := s.clientFor(ctx, cfg)
	if err != nil {
		return directcred.Credential{}, err
	}
	vaultID, err := s.resolveVaultID(ctx, c, cfg, token)
	if err != nil {
		return directcred.Credential{}, err
	}
	item, err := c.GetItem(ctx, vaultID, id)
	if err != nil {
		return directcred.Credential{}, scrubToken(wrapErr(fmt.Sprintf("reading item %s", id), err), token)
	}

	var cred directcred.Credential
	hasTOTP := false
	for _, f := range item.Fields {
		switch f.ID {
		case fieldIDUsername:
			cred.Username = f.Value
		case fieldIDPassword:
			cred.Password = f.Value
		}
		if f.FieldType == fieldTypeTOTP {
			hasTOTP = true
		}
	}
	if hasTOTP {
		// Capture the already-built client: each call re-reads the item so the
		// SDK core computes the CURRENT code. Nothing is cached.
		cred.OTP = func() (string, error) { return freshOTP(ctx, c, vaultID, id, token) }
	}
	return cred, nil
}

// clientFor extracts the service-account token, rejects a missing one before
// touching the backend, and builds the client. It returns the token so the
// caller can scrub it from any later error.
func (s *Source) clientFor(ctx context.Context, cfg directcred.Config) (client, string, error) {
	token := string(cfg.SecretData[secretKeyToken])
	if token == "" {
		return nil, "", fmt.Errorf("onepassword: secret is missing the %q key", secretKeyToken)
	}
	c, err := s.newClient(ctx, token)
	if err != nil {
		return nil, token, scrubToken(wrapErr("creating client", err), token)
	}
	return c, token, nil
}

// resolveVaultID returns cfg.VaultID when a prior Verify pinned one; otherwise
// it resolves cfg.Vault by name against the live vault list. A name matching
// zero or multiple vaults is an error naming what was found -- never the token.
func (s *Source) resolveVaultID(ctx context.Context, c client, cfg directcred.Config, token string) (string, error) {
	if cfg.VaultID != "" {
		return cfg.VaultID, nil
	}
	vaults, err := c.ListVaults(ctx)
	if err != nil {
		return "", scrubToken(wrapErr("listing vaults", err), token)
	}
	var matches []vault
	for _, v := range vaults {
		if v.Title == cfg.Vault {
			matches = append(matches, v)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("onepassword: no accessible vault named %q (found: %v)",
			cfg.Vault, vaultTitles(vaults))
	default:
		return "", fmt.Errorf("onepassword: %d accessible vaults named %q; cannot disambiguate",
			len(matches), cfg.Vault)
	}
}

// mapWebsites turns an item's autofill websites into host patterns:
// AnywhereOnWebsite -> suffix (non-exact) host, ExactDomain -> exact host,
// anything else (Never, or an unknown behavior) -> dropped. Each website
// value is treated as possibly a URL, so the hostname is parsed out first and
// then canonicalized; a host browserpolicy rejects is dropped. Input order is
// preserved.
func mapWebsites(sites []website) []directcred.Host {
	var hosts []directcred.Host
	for _, w := range sites {
		var exact bool
		switch w.Autofill {
		case autofillAnywhere:
			exact = false
		case autofillExactDomain:
			exact = true
		default:
			continue // Never, or any unknown behavior: not auto-fillable.
		}
		canon, err := browserpolicy.CanonicalHost(hostFromWebsiteValue(w.URL))
		if err != nil {
			continue // a host the policy rejects is dropped.
		}
		hosts = append(hosts, directcred.Host{Name: canon, Exact: exact})
	}
	return hosts
}

// hostFromWebsiteValue extracts a hostname from a 1Password website value,
// which may be a bare host ("example.com"), a full URL
// ("https://example.com/login"), or a host:port. CanonicalHost does the final
// validation; this only strips scheme/path/port so a URL-shaped value does
// not reach CanonicalHost (which rejects schemes and ports) and get dropped.
func hostFromWebsiteValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if u, err := url.Parse(v); err == nil && u.Host != "" {
		return u.Hostname()
	}
	// A scheme-less "example.com[:port][/path]" parses with an empty Host;
	// re-parse as an authority so the host is recovered.
	if u, err := url.Parse("//" + v); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return v
}

// freshOTP re-reads the item and returns its current one-time code. Called
// anew on every Credential.OTP() invocation: the SDK core recomputes the code
// at read time, so this must never cache.
func freshOTP(ctx context.Context, c client, vaultID, id, token string) (string, error) {
	item, err := c.GetItem(ctx, vaultID, id)
	if err != nil {
		return "", scrubToken(wrapErr(fmt.Sprintf("reading item %s for its one-time code", id), err), token)
	}
	for _, f := range item.Fields {
		if f.FieldType != fieldTypeTOTP {
			continue
		}
		switch {
		case f.OTP == nil:
			return "", fmt.Errorf("onepassword: item %s has a one-time-code field but no computed code", id)
		case f.OTP.Error != "":
			return "", fmt.Errorf("onepassword: item %s one-time code could not be computed: %s", id, f.OTP.Error)
		default:
			return f.OTP.Code, nil
		}
	}
	return "", fmt.Errorf("onepassword: item %s no longer has a one-time-code field", id)
}

// vaultTitles lists vault names for an error message. Vault names are not
// secret (the contract wants them named); a token never reaches here.
func vaultTitles(vaults []vault) []string {
	titles := make([]string, len(vaults))
	for i, v := range vaults {
		titles[i] = v.Title
	}
	return titles
}

// wrapErr adds operation context to an error without introducing the token.
func wrapErr(op string, err error) error {
	return fmt.Errorf("onepassword: %s: %w", op, err)
}

const redactedToken = "[redacted 1Password service-account token]"

// scrubToken guarantees the service-account token never rides out inside an
// error string. The SDK's own error unmarshalling (its errors.go) surfaces a
// core-supplied message, and while a bad-token failure today reports a
// format/auth message rather than echoing the token, that is not a contract
// we control -- so if the token literal appears anywhere in the error text it
// is replaced. The %w chain is preserved in the common (no-match) case.
func scrubToken(err error, token string) error {
	if err == nil {
		return nil
	}
	if token != "" && strings.Contains(err.Error(), token) {
		return errors.New(strings.ReplaceAll(err.Error(), token, redactedToken))
	}
	return err
}
