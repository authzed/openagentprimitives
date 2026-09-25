package main

import (
	"fmt"
	"strconv"

	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// sidecarProbeURL returns the MCP probe URL for a ResolvedSidecarToolbox and
// whether the sidecar is ready to probe.
//
// Separate-pod sidecars (RunMode=="separate-pod"):
//   - AwaitingSecret==true  → ("", false): secret not produced yet; skip silently.
//   - SidecarPodIP==""       → ("", false): pod not Ready yet; caller must poll.
//   - otherwise              → ("http://<SidecarPodIP>:<rt.Port>", true).
//
// In-pod sidecars (all other RunMode values, including "" and "in-pod"):
//   - MCP_PORT_<EnvPrefix(name)> env absent → ("", false).
//   - otherwise                              → ("http://127.0.0.1:<port>", true).
//
// portEnvLookup is called with the full env-var name (e.g. "MCP_PORT_ECHO")
// and returns the value, or "" if absent. Pass os.Getenv in production.
func sidecarProbeURL(rt spiceboxv1alpha1.ResolvedSidecarToolbox, portEnvLookup func(string) string) (url string, ready bool) {
	if rt.RunMode == "separate-pod" {
		if rt.AwaitingSecret {
			return "", false
		}
		if rt.SidecarPodIP == "" {
			return "", false
		}
		return fmt.Sprintf("http://%s:%d%s", rt.SidecarPodIP, rt.Port, sidecartoolboxsynth.EndpointPath(rt)), true
	}

	// In-pod: port is passed via MCP_PORT_<EnvPrefix(name)> env injected by the
	// operator into the runner container. Parse it into an int32 port.
	envKey := "MCP_PORT_" + agentsession.EnvPrefix(rt.Name)
	portStr := portEnvLookup(envKey)
	if portStr == "" {
		return "", false
	}
	p, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", p, sidecartoolboxsynth.EndpointPath(rt)), true
}
