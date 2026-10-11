// Package secretsource implements the directcred.Source backed by a plain
// Kubernetes Secret: one login per host, laid out as the three keys
// "<host>.username", "<host>.password", and an optional "<host>.totp"
// (an otpauth:// URI or a bare base32 seed -- see totp.go). It registers
// itself under directcred.SourceSecret via init.go, mirroring every other
// kind registry in this repo.
//
// Security posture: every error returned by this package names a SECRET
// KEY (e.g. "a.example.com.username"), never a credential VALUE -- not a
// username, not a password, not a TOTP seed or URI. Callers may log these
// errors freely.
package secretsource

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

const (
	suffixUsername = "username"
	suffixPassword = "password"
	suffixTOTP     = "totp"
)

// Source is the directcred.Source impl for Config.SecretData laid out per
// the package doc comment. It holds no state: every call is a pure
// function of the Config it's given.
type Source struct{}

// New returns a fresh Source.
func New() *Source { return &Source{} }

var _ directcred.Source = (*Source)(nil)

func (*Source) Kind() string { return directcred.SourceSecret }

// Verify validates cfg.SecretData's layout and returns a zero Pin: the
// secret backend has no vault concept to confirm. A non-nil error reports
// only malformed data (an orphaned username/password key, an
// unrecognized key suffix, or a non-canonical host key) -- the same
// checks List and Fetch run.
func (*Source) Verify(_ context.Context, cfg directcred.Config) (directcred.Pin, error) {
	if _, err := entriesByCanonicalHost(cfg.SecretData); err != nil {
		return directcred.Pin{}, err
	}
	return directcred.Pin{}, nil
}

// List returns one Ref per host found in cfg.SecretData, sorted by ID
// (the canonical host). Ref.Hosts always carries exactly one non-exact
// Host, matching the secret backend's per-host (not per-subdomain) grant.
func (*Source) List(_ context.Context, cfg directcred.Config) ([]directcred.Ref, error) {
	entries, err := entriesByCanonicalHost(cfg.SecretData)
	if err != nil {
		return nil, err
	}

	refs := make([]directcred.Ref, 0, len(entries))
	for host := range entries {
		refs = append(refs, directcred.Ref{
			ID:    host,
			Label: host,
			Hosts: []directcred.Host{{Name: host, Exact: false}},
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

// Fetch returns the host's username/password, and -- iff the host has a
// .totp key -- an OTP func that computes a fresh code per call. id must
// be a canonical host previously returned as a Ref.ID by List against the
// same cfg; an unknown id is an error naming id (never a value).
func (*Source) Fetch(_ context.Context, cfg directcred.Config, id string) (directcred.Credential, error) {
	entries, err := entriesByCanonicalHost(cfg.SecretData)
	if err != nil {
		return directcred.Credential{}, err
	}

	e, ok := entries[id]
	if !ok {
		return directcred.Credential{}, fmt.Errorf("secretsource: unknown id %q", id)
	}

	cred := directcred.Credential{
		Username: *e.username,
		Password: *e.password,
	}
	if e.totp != nil {
		cred.OTP = newOTPFunc(*e.totp)
	}
	return cred, nil
}

// hostEntry collects the raw field values found under one host's keys,
// before validate cross-checks them into either an error or a usable
// entry.
type hostEntry struct {
	username *string
	password *string
	totp     *string
}

// parseEntries walks data's keys, bucketing each by the host portion of
// its key (everything before the LAST '.') and recording the value under
// its suffix (everything after). It performs no cross-validation (orphan
// pairing, host canonicalization) -- see entriesByCanonicalHost -- so this
// pass alone just explains "which key, if any, has no recognized suffix."
//
// Keys are sorted before iterating so that, when data carries more than one
// malformed key, the FIRST fault returned is always the same one across
// repeated calls -- map iteration order is randomized per Go's spec, and an
// unsorted range here reported a different offending key on every call
// against the same multi-fault Secret.
func parseEntries(data map[string][]byte) (map[string]*hostEntry, error) {
	entries := make(map[string]*hostEntry, len(data))
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := data[key]
		idx := strings.LastIndex(key, ".")
		if idx <= 0 || idx == len(key)-1 {
			return nil, fmt.Errorf("secretsource: key %q has no recognized suffix", key)
		}
		host, suffix := key[:idx], key[idx+1:]

		e := entries[host]
		if e == nil {
			e = &hostEntry{}
			entries[host] = e
		}

		val := string(value)
		switch suffix {
		case suffixUsername:
			e.username = &val
		case suffixPassword:
			e.password = &val
		case suffixTOTP:
			e.totp = &val
		default:
			return nil, fmt.Errorf("secretsource: key %q has no recognized suffix", key)
		}
	}
	return entries, nil
}

// entriesByCanonicalHost parses data (see parseEntries) and then
// cross-validates each host's entry: username and password must both be
// present (a lone one is an orphan, named in the error), and the host
// portion of the key must already be exactly what
// browserpolicy.CanonicalHost would produce from it -- a host key is
// rejected rather than silently lowercased/normalized, because the
// operator typed it and should fix it, not have it reinterpreted. The
// returned map is keyed by that canonical host.
//
// Hosts are sorted before iterating for the same reason parseEntries sorts
// its keys: with more than one faulting host, the first-returned error must
// be deterministic across repeated calls, not whichever host Go's map
// iteration happened to visit first.
func entriesByCanonicalHost(data map[string][]byte) (map[string]*hostEntry, error) {
	raw, err := parseEntries(data)
	if err != nil {
		return nil, err
	}

	hosts := make([]string, 0, len(raw))
	for host := range raw {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	out := make(map[string]*hostEntry, len(raw))
	for _, host := range hosts {
		e := raw[host]
		switch {
		case e.username != nil && e.password == nil:
			return nil, fmt.Errorf("secretsource: key %q has no matching %q key",
				host+"."+suffixUsername, host+"."+suffixPassword)
		case e.password != nil && e.username == nil:
			return nil, fmt.Errorf("secretsource: key %q has no matching %q key",
				host+"."+suffixPassword, host+"."+suffixUsername)
		case e.username == nil && e.password == nil:
			// Only a .totp key was present for this host -- a one-time code
			// with nothing to attach it to is just as orphaned.
			return nil, fmt.Errorf("secretsource: key %q has no matching %q/%q keys",
				host+"."+suffixTOTP, host+"."+suffixUsername, host+"."+suffixPassword)
		}

		canon, err := browserpolicy.CanonicalHost(host)
		if err != nil {
			return nil, fmt.Errorf("secretsource: host key %q: %w", host, err)
		}
		if canon != host {
			return nil, fmt.Errorf(
				"secretsource: host key %q is not canonical (canonical form is %q): fix the key, it will not be normalized silently",
				host, canon)
		}

		out[canon] = e
	}
	return out, nil
}
