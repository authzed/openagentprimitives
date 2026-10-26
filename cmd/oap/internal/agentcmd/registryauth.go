package agentcmd

import (
	"fmt"
	"os"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// Registry credentials for push/sign/pull/verify/install are read from the
// environment rather than flags so a token never lands in shell history or a
// CI log's echoed command line. oci.repository scopes the credential to the
// ref's own registry host, so it is never offered to any other host.
const (
	registryUsernameEnv = "OAP_REGISTRY_USERNAME"
	registryPasswordEnv = "OAP_REGISTRY_PASSWORD"
)

// registryOptions builds the oci.Options every registry-touching agent verb
// uses. Neither env var set means anonymous access; exactly one set is an
// error rather than a silent anonymous fallback, since a half-configured
// credential would otherwise surface later as a confusing 401/denied.
func registryOptions(plainHTTP bool) (oci.Options, error) {
	opts := oci.Options{PlainHTTP: plainHTTP}
	user, pass := os.Getenv(registryUsernameEnv), os.Getenv(registryPasswordEnv)
	switch {
	case user == "" && pass == "":
		return opts, nil
	case user == "" || pass == "":
		return oci.Options{}, fmt.Errorf("registry credentials: set both %s and %s, or neither for anonymous access", registryUsernameEnv, registryPasswordEnv)
	}
	opts.Credential = &auth.Credential{Username: user, Password: pass}
	return opts, nil
}
