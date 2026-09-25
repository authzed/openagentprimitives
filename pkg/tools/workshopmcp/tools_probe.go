// tools_probe.go implements the three discovery tools design spec §2.4's
// "Tools" loop row calls for: `probe_mcp` (a live `tools/list` against a
// candidate MCP server URL) and `probe_image` / `cli_help` (discovering an
// image's tool surface / a CLI's --help text). The sidecar holds no `pods`
// verb (design spec §2.3: "The sidecar holds no LLM credential and has no
// pods verb: it cannot think and it cannot run an image") — probe_mcp needs
// no pod at all (it is a plain outbound HTTP call from THIS pod, guarded
// against SSRF), but probe_image/cli_help have to run an arbitrary
// caller-named image, which only the plan-3a WorkshopProbe controller is
// privileged to do. This file creates the CR and waits; it never runs a pod
// itself.
package workshopmcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// Tool names announced on the MCP surface.
const (
	toolProbeMCP   = "probe_mcp"
	toolProbeImage = "probe_image"
	toolCLIHelp    = "cli_help"
)

// probeHTTPTimeout bounds probe_mcp's tools/list round trip against a
// caller-supplied MCP server — generous enough for a cold-starting server,
// short enough that a hung/unreachable target does not stall the builder
// turn indefinitely.
const probeHTTPTimeout = 20 * time.Second

// defaultProbeTimeoutSeconds mirrors WorkshopProbeSpec.TimeoutSeconds' own
// +kubebuilder:default=120 (pkg/apis/v1alpha1/workshopprobe_types.go):
// applied explicitly here, rather than left to apiserver CRD defaulting,
// so createAndAwaitProbe's own wait deadline (which reads back
// spec.TimeoutSeconds) is correct even against a fake client in tests, which
// runs no structural-schema defaulting.
const defaultProbeTimeoutSeconds int32 = 120

// probePollInterval governs createAndAwaitProbe's re-Get cadence. A package
// var, not a const, so a test can shrink it rather than waiting out the real
// interval against a fake client that will never advance a WorkshopProbe's
// phase on its own.
var probePollInterval = 500 * time.Millisecond

// probePollMargin is added on top of spec.TimeoutSeconds when bounding how
// long createAndAwaitProbe waits for a terminal phase: the operator's
// WorkshopProbe controller enforces the pod's OWN timeout and needs a moment
// after that to observe the failure and persist status — this margin is
// slack for that write, not a second probe timeout.
const probePollMargin = 30 * time.Second

// registerProbes wires `probe_mcp`, `probe_image` and `cli_help` onto
// mcpSrv.
func (s *Server) registerProbes(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolProbeMCP,
		Description: "List the tools an MCP server exposes (tools/list), from this sidecar pod. " +
			"The URL is reached through an SSRF-guarded client — a loopback/private/link-local/" +
			"cloud-metadata target is refused, surfaced as an error. Use this to verify a candidate " +
			"MCPServer's actual surface before authoring its spec.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The MCP server URL to probe, e.g. https://mcp.example.com/mcp.",
				},
			},
			"required": []any{"url"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleProbeMCP)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolProbeImage,
		Description: "Run a container image and discover the MCP tool surface it declares. This " +
			"sidecar holds no pods verb, so the pod itself is run by the platform's WorkshopProbe " +
			"controller — this call creates a WorkshopProbe and waits for it to finish (up to " +
			"timeout_seconds, default 120). A pod failure surfaces as an error naming the failure.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"image": map[string]any{
					"type":        "string",
					"description": "The container image reference to run and probe.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "How long to let the probe pod run before it is considered failed. Defaults to 120.",
				},
			},
			"required": []any{"image"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleProbeImage)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolCLIHelp,
		Description: "Run a container image's CLI binary with --help (plus any given args, e.g. a " +
			"subcommand) and return its help text, for authoring a SpiceboxToolspec against a real " +
			"CLI's actual flags. Runs through the same WorkshopProbe controller as probe_image, since " +
			"this sidecar holds no pods verb. A pod failure surfaces as an error naming the failure.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"image": map[string]any{
					"type":        "string",
					"description": "The container image that holds the CLI binary.",
				},
				"binary": map[string]any{
					"type":        "string",
					"description": "The binary to run --help against.",
				},
				"args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Extra args before --help, e.g. a subcommand name.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "How long to let the probe pod run before it is considered failed. Defaults to 120.",
				},
			},
			"required": []any{"image", "binary"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleCLIHelp)
}

// probeMCPArgs is probe_mcp's own argument shape.
type probeMCPArgs struct {
	URL string `json:"url"`
}

// handleProbeMCP answers the `probe_mcp` tool call.
func (s *Server) handleProbeMCP(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a probeMCPArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("probe_mcp: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.URL) == "" {
		return s.toolErr("probe_mcp: url is required"), nil
	}

	c := &probe.Client{HTTP: s.ProbeHTTP, URL: a.URL, Timeout: probeHTTPTimeout}
	tools, err := c.ListTools(ctx, "", "")
	if err != nil {
		// Covers both a genuine protocol/HTTP failure and the SSRF guard's
		// own refusal (safehttp's guarded dialer returns a plain error, never
		// hangs) — either way this is an ERROR result, never a partial or
		// fabricated tool list.
		return s.toolErr("probe_mcp: tools/list against %s failed: %v", a.URL, err), nil
	}
	return s.jsonResult(map[string]any{"tools": tools})
}

// probeImageArgs is probe_image's own argument shape.
type probeImageArgs struct {
	Image          string `json:"image"`
	TimeoutSeconds int32  `json:"timeout_seconds,omitempty"`
}

// handleProbeImage answers the `probe_image` tool call.
func (s *Server) handleProbeImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a probeImageArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("probe_image: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Image) == "" {
		return s.toolErr("probe_image: image is required"), nil
	}

	status, err := s.createAndAwaitProbe(ctx, spiceboxv1alpha1.WorkshopProbeSpec{
		Image:          a.Image,
		TimeoutSeconds: a.TimeoutSeconds,
	}, "wprobe-image-")
	if err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("probe_image: %v", err), nil
	}
	if probeFailed(status) {
		return s.toolErr("probe_image: probe failed: %s", status.PodFailure), nil
	}
	return s.jsonResult(map[string]any{"tools": status.Tools})
}

// cliHelpArgs is cli_help's own argument shape.
type cliHelpArgs struct {
	Image          string   `json:"image"`
	Binary         string   `json:"binary"`
	Args           []string `json:"args,omitempty"`
	TimeoutSeconds int32    `json:"timeout_seconds,omitempty"`
}

// handleCLIHelp answers the `cli_help` tool call.
func (s *Server) handleCLIHelp(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a cliHelpArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("cli_help: decode arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Image) == "" || strings.TrimSpace(a.Binary) == "" {
		return s.toolErr("cli_help: image and binary are both required"), nil
	}

	status, err := s.createAndAwaitProbe(ctx, spiceboxv1alpha1.WorkshopProbeSpec{
		CliHelp: &spiceboxv1alpha1.WorkshopProbeCliHelp{
			Image:  a.Image,
			Binary: a.Binary,
			Args:   a.Args,
		},
		TimeoutSeconds: a.TimeoutSeconds,
	}, "wprobe-cli-")
	if err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("cli_help: %v", err), nil
	}
	if probeFailed(status) {
		return s.toolErr("cli_help: probe failed: %s", status.PodFailure), nil
	}
	return s.jsonResult(map[string]any{"help_text": status.HelpText})
}

// probeFailed reports whether a terminal WorkshopProbeStatus represents a
// failure the caller must surface as a tool error — either signal alone is
// sufficient (the controller always sets both together on a real failure;
// checking both is defensive, not a claim they can diverge).
func probeFailed(status spiceboxv1alpha1.WorkshopProbeStatus) bool {
	return status.Phase == spiceboxv1alpha1.WorkshopProbePhaseFailed || status.PodFailure != ""
}

// createAndAwaitProbe creates a WorkshopProbe carrying spec in the workshop
// namespace (GenerateName'd under namePrefix — the apiserver names it, same
// reasoning as every other GenerateName use in this codebase: a name picked
// here could collide, and the caller has no way to retry differently against
// a name it never chose) and waits for it to reach a terminal phase via
// awaitProbe. A zero spec.TimeoutSeconds is defaulted to
// defaultProbeTimeoutSeconds before Create, so awaitProbe's own wait window
// is correct without depending on apiserver/CRD defaulting having run.
func (s *Server) createAndAwaitProbe(ctx context.Context, spec spiceboxv1alpha1.WorkshopProbeSpec, namePrefix string) (spiceboxv1alpha1.WorkshopProbeStatus, error) {
	if spec.TimeoutSeconds <= 0 {
		spec.TimeoutSeconds = defaultProbeTimeoutSeconds
	}
	wp := &spiceboxv1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: namePrefix,
			Namespace:    s.Identity.Namespace,
		},
		Spec: spec,
	}
	if err := s.K8s.Create(ctx, wp); err != nil {
		return spiceboxv1alpha1.WorkshopProbeStatus{}, fmt.Errorf("creating WorkshopProbe: %w", err)
	}
	return s.awaitProbe(ctx, wp)
}

// awaitProbe polls wp's own status (by re-Get, never watch — this is a
// short-lived, single-shot wait inside one tool call, not a controller) until
// status.phase reaches WorkshopProbePhaseSucceeded or
// WorkshopProbePhaseFailed, or the deadline (spec.TimeoutSeconds +
// probePollMargin) elapses — whichever comes first. The sidecar NEVER runs
// the probe pod itself; the plan-3a WorkshopProbe controller
// (pkg/controllers/workshopprobe) does, and this is only what watches for it
// to finish. A deadline elapsing without a terminal phase is returned as an
// error (distinct from a terminal Failed phase, which is a normal — if
// unwelcome — RESULT, not a wait failure); the caller does not distinguish
// them further today, but the message does.
func (s *Server) awaitProbe(ctx context.Context, wp *spiceboxv1alpha1.WorkshopProbe) (spiceboxv1alpha1.WorkshopProbeStatus, error) {
	timeout := time.Duration(wp.Spec.TimeoutSeconds)*time.Second + probePollMargin
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	key := client.ObjectKeyFromObject(wp)
	for {
		var current spiceboxv1alpha1.WorkshopProbe
		if err := s.K8s.Get(ctx, key, &current); err != nil {
			return spiceboxv1alpha1.WorkshopProbeStatus{}, fmt.Errorf("re-reading WorkshopProbe %s: %w", key, err)
		}
		switch current.Status.Phase {
		case spiceboxv1alpha1.WorkshopProbePhaseSucceeded, spiceboxv1alpha1.WorkshopProbePhaseFailed:
			return current.Status, nil
		}
		select {
		case <-pollCtx.Done():
			return spiceboxv1alpha1.WorkshopProbeStatus{}, fmt.Errorf(
				"timed out waiting for WorkshopProbe %s to reach a terminal phase (last observed phase: %q)",
				key, current.Status.Phase)
		case <-time.After(probePollInterval):
		}
	}
}
