// Package promptinjection is a content-guard inspector that classifies tool I/O
// for prompt-injection risk by POSTing text to a co-located ONNX detector
// sidecar and mapping the risk score to a Finding. The detector endpoint is
// read from CONTENTGUARD_DETECTOR_ENDPOINT (a full URL set by the operator when
// the sidecar is injected); the inspector implements DetectorProvider so the
// operator knows which sidecar image to inject.
package promptinjection

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

const id = "prompt-injection"
const endpointEnv = "CONTENTGUARD_DETECTOR_ENDPOINT"

func init() { registry.Register(New()) }

// Config is the per-instance configuration parsed from the inspector's raw
// settings JSON. All fields are optional except detectorImage and port.
type Config struct {
	DetectorImage string   `json:"detectorImage"`
	Port          int32    `json:"port"`
	Threshold     *float64 `json:"threshold,omitempty"`  // [0,1]; default 0.8
	Action        string   `json:"action,omitempty"`     // approve(default)|block|pass
	OnError       string   `json:"onError,omitempty"`    // warn(default)|block
	Points        []string `json:"points,omitempty"`     // PreToolCall|PostToolCall (default both)
	TimeoutMs     int      `json:"timeoutMs,omitempty"`  // default 1000
	HealthPath    string   `json:"healthPath,omitempty"` // default "/healthz"
}

// Inspector is the registered factory for the prompt-injection content guard.
type Inspector struct{}

// New returns a new Inspector.
func New() *Inspector { return &Inspector{} }

// ID returns the registry key for this inspector.
func (Inspector) ID() string { return id }

// Detector implements contentguard.DetectorProvider. The operator calls this to
// determine which sidecar image to inject alongside the runner pod. configure
// and Detector share parse() so both fail on the same invalid inputs.
func (Inspector) Detector(raw json.RawMessage) (*contentguard.DetectorSpec, error) {
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	hp := cfg.HealthPath
	if hp == "" {
		hp = "/healthz"
	}
	return &contentguard.DetectorSpec{Image: cfg.DetectorImage, Port: cfg.Port, HealthPath: hp}, nil
}

// Configure parses and validates raw config once, returning a ready Instance.
// A non-nil error makes the settings object Invalid — a misconfigured guard
// must never run (fail-closed).
func (Inspector) Configure(raw json.RawMessage) (contentguard.Instance, error) {
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}

	inst := &instance{
		threshold:    0.8,
		action:       contentguard.Approve,
		onErrorBlock: false,
		timeout:      time.Second,
	}

	if cfg.TimeoutMs > 0 {
		inst.timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}

	if cfg.Threshold != nil {
		inst.threshold = *cfg.Threshold
	}

	switch cfg.Action {
	case "", "approve":
		inst.action = contentguard.Approve
	case "block":
		inst.action = contentguard.Block
	case "pass":
		inst.action = contentguard.Pass
	default:
		return nil, fmt.Errorf("prompt-injection: unknown action %q (want approve|block|pass)", cfg.Action)
	}

	switch cfg.OnError {
	case "", "warn":
		inst.onErrorBlock = false
	case "block":
		inst.onErrorBlock = true
	default:
		return nil, fmt.Errorf("prompt-injection: unknown onError %q (want warn|block)", cfg.OnError)
	}

	pts, err := parsePoints(cfg.Points)
	if err != nil {
		return nil, err
	}
	inst.points = pts

	return inst, nil
}

// parse is shared by Configure and Detector to validate the raw config.
func parse(raw json.RawMessage) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("prompt-injection: parse config: %w", err)
	}
	if strings.TrimSpace(cfg.DetectorImage) == "" {
		return cfg, fmt.Errorf("prompt-injection: detectorImage is required")
	}
	if cfg.Port <= 0 {
		return cfg, fmt.Errorf("prompt-injection: port must be > 0")
	}
	if cfg.Threshold != nil && (*cfg.Threshold < 0 || *cfg.Threshold > 1) {
		return cfg, fmt.Errorf("prompt-injection: threshold %.2f is not in [0,1]", *cfg.Threshold)
	}
	return cfg, nil
}

func parsePoints(in []string) ([]pipeline.Point, error) {
	if len(in) == 0 {
		return []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}, nil
	}
	var out []pipeline.Point
	for _, p := range in {
		switch p {
		case "PreToolCall", "args":
			out = append(out, pipeline.PreToolCall)
		case "PostToolCall", "result":
			out = append(out, pipeline.PostToolCall)
		default:
			return nil, fmt.Errorf("prompt-injection: unknown point %q (want PreToolCall|args|PostToolCall|result)", p)
		}
	}
	return out, nil
}

type instance struct {
	points       []pipeline.Point
	threshold    float64
	action       contentguard.Action
	onErrorBlock bool
	timeout      time.Duration
}

func (i *instance) Points() []pipeline.Point { return i.points }

// Inspect evaluates the subject against the detector. On detector error, the
// behavior depends on onError: "block" returns an error (adapter fail-closes to
// Block); "warn" returns a Pass Finding with a loud Reason and no error (the
// audit still records the unavailability).
func (i *instance) Inspect(ctx context.Context, s contentguard.Subject) (contentguard.Finding, error) {
	// Subject.Text is the point's content capped at contentguard.MaxInspectBytes.
	// Reading s.Result / string(s.Args) directly would opt this inspector out of
	// that cap, and it is the inspector the cap exists for: the detector is a
	// local classifier under a short timeout whose default onError=warn turns a
	// stall into a Pass, so an uncapped payload chooses whether it is scanned.
	text := s.Text()

	score, err := i.classify(ctx, text)
	if err != nil {
		if i.onErrorBlock {
			// fail-closed: return error so the adapter blocks the tool call
			return contentguard.Finding{}, err
		}
		// warn: loud Pass — no error to the adapter, but the audit captures it
		return contentguard.Finding{
			Action:  contentguard.Pass,
			Reason:  "prompt-injection detector unavailable (warn-mode, not scanned)",
			Details: map[string]any{"detector_error": err.Error()},
		}, nil
	}

	if score < i.threshold {
		return contentguard.Finding{
			Action:  contentguard.Pass,
			Details: map[string]any{"score": score},
		}, nil
	}

	return contentguard.Finding{
		Action: i.action,
		Reason: fmt.Sprintf("possible prompt injection in %s %s (score %.2f >= %.2f)",
			s.ToolName, ioWord(s.Point), score, i.threshold),
		Details: map[string]any{
			"score":     score,
			"threshold": i.threshold,
			"point":     string(s.Point),
			"tool":      s.ToolName,
			"excerpt":   text,
		},
	}, nil
}

// classify POSTs text to the detector endpoint and returns the injection score.
// The endpoint URL is read from CONTENTGUARD_DETECTOR_ENDPOINT at call time
// (not Configure time) so unit tests can use t.Setenv.
func (i *instance) classify(ctx context.Context, text string) (float64, error) {
	endpoint := strings.TrimRight(os.Getenv(endpointEnv), "/")
	if endpoint == "" {
		return 0, fmt.Errorf("prompt-injection: %s not set (detector sidecar not injected)", endpointEnv)
	}

	body, _ := json.Marshal(map[string]string{"text": text})
	cctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint+"/", strings.NewReader(string(body)))
	if err != nil {
		return 0, fmt.Errorf("prompt-injection: build detector request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prompt-injection: detector request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prompt-injection: detector returned status %d", resp.StatusCode)
	}

	var out struct {
		Score float64 `json:"score"`
		Label string  `json:"label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("prompt-injection: decode detector response: %w", err)
	}

	return out.Score, nil
}

func ioWord(p pipeline.Point) string {
	if p == pipeline.PreToolCall {
		return "input"
	}
	return "output"
}
