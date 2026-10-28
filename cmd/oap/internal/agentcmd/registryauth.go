package agentcmd

import (
	"fmt"
	"os"
	"strconv"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// Registry credentials for push/sign/pull/verify/install are read from the
// environment rather than flags so a token never lands in shell history or a
// CI log's echoed command line. They are bound to the one host named by
// OAP_REGISTRY_HOST, not to whatever host a ref names: install refs can come
// from someone else (a pasted install link), and following the ref would hand
// the credential to that someone's registry.
const (
	registryUsernameEnv = "OAP_REGISTRY_USERNAME"
	registryPasswordEnv = "OAP_REGISTRY_PASSWORD"
	registryHostEnv     = "OAP_REGISTRY_HOST"

	// registryPlainHTTPCredsEnv opts in to sending the credential over
	// --plain-http to a non-loopback OAP_REGISTRY_HOST (a LAN or
	// docker-compose registry with no TLS).
	registryPlainHTTPCredsEnv = "OAP_REGISTRY_PLAIN_HTTP_CREDENTIALS"
)

// registryAuthHelp is appended to the Long help of every registry-touching
// agent verb so the env-var credential contract is discoverable from --help.
const registryAuthHelp = "Registry credentials are read from " + registryUsernameEnv + ",\n" +
	registryPasswordEnv + " and " + registryHostEnv + " (set all three, or none\n" +
	"for anonymous access). They are only sent to " + registryHostEnv + " (e.g.\n" +
	"ghcr.io or localhost:5000). push and sign refuse a <ref> on any other host;\n" +
	"pull, verify and install access it anonymously. Over --plain-http they are\n" +
	"sent only to loopback unless " + registryPlainHTTPCredsEnv + "=true."

// registryOptions builds the oci.Options every registry-touching agent verb
// uses. No env var set means anonymous access; a partial set is an error
// rather than a silent anonymous fallback, since a half-configured credential
// would otherwise surface later as a confusing 401/denied. Verbs whose ref the
// user chose themselves (push, sign) also set RequireCredentialHost.
func registryOptions(plainHTTP bool) (oci.Options, error) {
	opts := oci.Options{PlainHTTP: plainHTTP}
	user, pass, host := os.Getenv(registryUsernameEnv), os.Getenv(registryPasswordEnv), os.Getenv(registryHostEnv)
	switch {
	case user == "" && pass == "" && host == "":
		return opts, nil
	case user == "" || pass == "" || host == "":
		return oci.Options{}, fmt.Errorf("registry credentials: set all of %s, %s and %s, or none for anonymous access", registryUsernameEnv, registryPasswordEnv, registryHostEnv)
	}
	if err := oci.ValidateCredentialHost(host); err != nil {
		return oci.Options{}, fmt.Errorf("registry credentials: %s: %w", registryHostEnv, err)
	}
	if v := os.Getenv(registryPlainHTTPCredsEnv); v != "" {
		allow, err := strconv.ParseBool(v)
		if err != nil {
			return oci.Options{}, fmt.Errorf("registry credentials: %s must be true or false, got %q", registryPlainHTTPCredsEnv, v)
		}
		opts.AllowPlainHTTPCredential = allow
	}
	opts.Credential = &auth.Credential{Username: user, Password: pass}
	opts.CredentialHost = host
	return opts, nil
}
