package settingsui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

// seedConfig writes cfg to dir/config.json via desktop.Config.Save, so a
// test can point a server's SupportDir at fixture state it controls.
func seedConfig(t *testing.T, dir string, cfg desktop.Config) desktop.Config {
	t.Helper()
	require.NoError(t, cfg.Save(dir))
	return cfg
}

// hashForTest bcrypt-hashes pw at bcrypt.MinCost — fast for test fixtures.
// Production hashing always goes through passwordkind.HashPassword's fixed
// cost (see config.go); this helper is only ever used to seed a stored hash.
func hashForTest(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

func TestConfigGET(t *testing.T) {
	cases := []struct {
		name  string
		seed  desktop.Config
		check func(t *testing.T, resp configResponse)
	}{
		{
			name: "unset fields render empty/false, health defaults enabled",
			seed: desktop.Config{Model: desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-abc"}},
			check: func(t *testing.T, resp configResponse) {
				assert.Equal(t, "anthropic", resp.Model.Provider)
				assert.False(t, resp.NgrokConfigured)
				assert.True(t, resp.HealthNotifications)
				assert.False(t, resp.PasswordSet)
			},
		},
		{
			name: "short apiKey (<=4 chars) masked entirely, no suffix leaked",
			seed: desktop.Config{Model: desktop.ModelConfig{Provider: "openai", APIKey: "ab12"}},
			check: func(t *testing.T, resp configResponse) {
				assert.Equal(t, "••••", resp.Model.APIKeyMasked)
			},
		},
		{
			name: "empty apiKey masks to empty string",
			seed: desktop.Config{Model: desktop.ModelConfig{Provider: "openai", APIKey: ""}},
			check: func(t *testing.T, resp configResponse) {
				assert.Equal(t, "", resp.Model.APIKeyMasked)
			},
		},
		{
			name: "long apiKey masked with last four",
			seed: desktop.Config{Model: desktop.ModelConfig{Provider: "openai", APIKey: "sk-super-secret-1234"}},
			check: func(t *testing.T, resp configResponse) {
				assert.Equal(t, "••••1234", resp.Model.APIKeyMasked)
			},
		},
		{
			name: "ngrok configured, password set, health disabled render as booleans",
			seed: desktop.Config{
				Model:               desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-1234567890"},
				Ngrok:               &desktop.NgrokConfig{AuthToken: "ngrok-tunnel-token-xyz"},
				AdminPasswordHash:   hashForTest(t, "hunter2"),
				HealthNotifications: boolPtr(false),
			},
			check: func(t *testing.T, resp configResponse) {
				assert.True(t, resp.NgrokConfigured)
				assert.True(t, resp.PasswordSet)
				assert.False(t, resp.HealthNotifications)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seedConfig(t, dir, tc.seed)
			s := startTestServer(t, Deps{SupportDir: dir})
			c := authedClient(t, s)

			resp, err := c.Get("http://" + s.Addr() + "/api/config")
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			// Redaction is absolute: none of the seeded secrets ever appear
			// in the raw response bytes, whichever case seeded them.
			if tc.seed.Model.APIKey != "" {
				assert.NotContains(t, string(body), tc.seed.Model.APIKey)
			}
			if tc.seed.Ngrok != nil {
				assert.NotContains(t, string(body), tc.seed.Ngrok.AuthToken)
			}
			if tc.seed.AdminPasswordHash != "" {
				assert.NotContains(t, string(body), tc.seed.AdminPasswordHash)
			}

			var got configResponse
			require.NoError(t, json.Unmarshal(body, &got))
			assert.Equal(t, filepath.Join(dir, "config.json"), got.ConfigPath)
			assert.Equal(t, desktop.ModelProviderNames(), got.Providers)
			tc.check(t, got)
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// putReq/putReqModel/putReqPassword mirror configUpdateRequest's JSON wire
// shape independently of the production type, so a test can build a request
// body via plain field assignment rather than spelling out Go's anonymous
// struct type at every call site.
type putReqModel struct {
	Provider string `json:"provider"`
	APIKey   string `json:"apiKey"`
	Name     string `json:"name"`
}
type putReqPassword struct {
	Current string `json:"current"`
	New     string `json:"new"`
}
type putReq struct {
	Model               putReqModel     `json:"model"`
	NgrokAuthToken      string          `json:"ngrokAuthToken"`
	ClearNgrok          bool            `json:"clearNgrok"`
	HealthNotifications bool            `json:"healthNotifications"`
	Password            *putReqPassword `json:"password,omitempty"`
}

// reapplyRecorder records every ReapplyConfig invocation: a call counter plus
// the captured cfg AND scope arguments, so a test can assert the handler
// targeted exactly the field group that changed (Model-only vs Password-only).
type reapplyRecorder struct {
	calls  int
	cfgs   []desktop.Config
	scopes []ReapplyScope
	err    error
}

func (r *reapplyRecorder) fn(_ context.Context, cfg desktop.Config, scope ReapplyScope) error {
	r.calls++
	r.cfgs = append(r.cfgs, cfg)
	r.scopes = append(r.scopes, scope)
	return r.err
}

// healthRecorder records every OnHealthNotificationsSaved invocation.
type healthRecorder struct {
	calls int
	vals  []bool
}

func (r *healthRecorder) fn(enabled bool) {
	r.calls++
	r.vals = append(r.vals, enabled)
}

// baseSeed is the starting on-disk config shared by most PUT cases: a valid
// anthropic config with a stored API key and no password/ngrok/health
// override, so a case that doesn't intend to touch a given field can leave
// it alone and rely on this default.
func baseSeed() desktop.Config {
	return desktop.Config{Model: desktop.ModelConfig{Provider: "anthropic", APIKey: "sk-original-key-0001"}}
}

// keepModelBody builds a putReqModel that reproduces seed's model fields
// unchanged (empty apiKey = keep stored, per the wire contract).
func keepModelBody(seed desktop.Config) putReqModel {
	return putReqModel{Provider: seed.Model.Provider, APIKey: "", Name: seed.Model.Name}
}

func TestConfigPUT(t *testing.T) {
	cases := []struct {
		name             string
		seed             func() desktop.Config
		body             func(seed desktop.Config) putReq
		running          bool
		reapplyErr       error
		nilHealthSeam    bool // leave Deps.OnHealthNotificationsSaved nil
		wantStatus       int
		wantOutcomes     []fieldOutcome // nil = skip exact outcome-list check
		wantReapplyCalls int
		wantHealthCalls  int
		check            func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome)
	}{
		{
			name: "keep-key: empty apiKey keeps stored, nothing else changed, empty outcomes",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				return putReq{Model: keepModelBody(seed), HealthNotifications: true}
			},
			wantStatus:   http.StatusOK,
			wantOutcomes: []fieldOutcome{},
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, seed.Model.APIKey, saved.Model.APIKey)
			},
		},
		{
			name: "new-key: apiKey replaced, stopped so next-start",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Model.APIKey = "sk-new-key-9999"
				return b
			},
			running:          false,
			wantStatus:       http.StatusOK,
			wantOutcomes:     []fieldOutcome{{Field: "model", Outcome: "next-start"}},
			wantReapplyCalls: 0,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, "sk-new-key-9999", saved.Model.APIKey)
			},
		},
		{
			name: "invalid-provider-400: Validate() failure, nothing saved",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Model.Provider = "bogus-provider"
				return b
			},
			wantStatus: http.StatusBadRequest,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, seed, saved, "nothing must be saved on a Validate() failure")
			},
		},
		{
			name: "wrong-current-password-403-nothing-saved",
			seed: func() desktop.Config {
				c := baseSeed()
				c.AdminPasswordHash = hashForTest(t, "correcthorse")
				return c
			},
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Password = &putReqPassword{Current: "wrongpass", New: "newpassword123"}
				return b
			},
			wantStatus: http.StatusForbidden,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, seed, saved, "nothing must be saved on a password mismatch")
			},
		},
		{
			name: "new-password-too-short-400-nothing-saved",
			seed: func() desktop.Config {
				c := baseSeed()
				c.AdminPasswordHash = hashForTest(t, "correcthorse")
				return c
			},
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Password = &putReqPassword{Current: "correcthorse", New: "short1"}
				return b
			},
			wantStatus: http.StatusBadRequest,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, seed, saved, "nothing must be saved on a too-short new password")
			},
		},
		{
			name: "new-password-too-short-checked-before-current-password: 400 not 403",
			seed: func() desktop.Config {
				c := baseSeed()
				c.AdminPasswordHash = hashForTest(t, "correcthorse")
				return c
			},
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				// Current is WRONG too — if this returns 400 rather than 403, the
				// length check ran first, as the brief requires.
				b.Password = &putReqPassword{Current: "wrongpass", New: "short1"}
				return b
			},
			wantStatus: http.StatusBadRequest,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, seed, saved, "nothing must be saved on a too-short new password")
			},
		},
		{
			name: "password-change-applied: verified, hashed, applied while running",
			seed: func() desktop.Config {
				c := baseSeed()
				c.AdminPasswordHash = hashForTest(t, "correcthorse")
				return c
			},
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Password = &putReqPassword{Current: "correcthorse", New: "newpassword123"}
				return b
			},
			running:          true,
			wantStatus:       http.StatusOK,
			wantOutcomes:     []fieldOutcome{{Field: "password", Outcome: "applied"}},
			wantReapplyCalls: 1,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.NotEqual(t, seed.AdminPasswordHash, saved.AdminPasswordHash)
				assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(saved.AdminPasswordHash), []byte("newpassword123")))
				require.Len(t, reap.cfgs, 1)
				assert.Equal(t, saved.AdminPasswordHash, reap.cfgs[0].AdminPasswordHash)
				// Only the password changed (model kept), so the re-apply must
				// target Password alone — not re-run the model re-apply.
				require.Len(t, reap.scopes, 1)
				assert.Equal(t, ReapplyScope{Model: false, Password: true}, reap.scopes[0])
			},
		},
		{
			name: "running-model-change-calls-reapply-once",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Model.APIKey = "sk-new-key-7777"
				return b
			},
			running:          true,
			wantStatus:       http.StatusOK,
			wantOutcomes:     []fieldOutcome{{Field: "model", Outcome: "applied"}},
			wantReapplyCalls: 1,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, "sk-new-key-7777", saved.Model.APIKey)
				require.Len(t, reap.cfgs, 1)
				assert.Equal(t, "sk-new-key-7777", reap.cfgs[0].Model.APIKey)
				// Only the model changed, so the re-apply must target Model
				// alone — not re-provision the password IdP.
				require.Len(t, reap.scopes, 1)
				assert.Equal(t, ReapplyScope{Model: true, Password: false}, reap.scopes[0])
			},
		},
		{
			name: "stopped-model-change-next-start: reapply not called",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Model.APIKey = "sk-new-key-6666"
				return b
			},
			running:          false,
			wantStatus:       http.StatusOK,
			wantOutcomes:     []fieldOutcome{{Field: "model", Outcome: "next-start"}},
			wantReapplyCalls: 0,
		},
		{
			name: "reapply-error-marks-failed-but-saves",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.Model.APIKey = "sk-new-key-5555"
				return b
			},
			running:          true,
			reapplyErr:       assertionError("cluster unreachable"),
			wantStatus:       http.StatusOK,
			wantReapplyCalls: 1,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.Equal(t, "sk-new-key-5555", saved.Model.APIKey, "save stands even though apply failed")
				require.Len(t, outcomes, 1)
				assert.Equal(t, "model", outcomes[0].Field)
				assert.Equal(t, "failed", outcomes[0].Outcome)
				assert.Contains(t, outcomes[0].Message, "cluster unreachable")
			},
		},
		{
			name: "ngrok-always-next-start: even while running, model/password unchanged",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				b := putReq{Model: keepModelBody(seed), HealthNotifications: true}
				b.NgrokAuthToken = "ngrok-new-token-555"
				return b
			},
			running:          true,
			wantStatus:       http.StatusOK,
			wantOutcomes:     []fieldOutcome{{Field: "ngrok", Outcome: "next-start"}},
			wantReapplyCalls: 0,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				require.NotNil(t, saved.Ngrok)
				assert.Equal(t, "ngrok-new-token-555", saved.Ngrok.AuthToken)
			},
		},
		{
			name: "health-toggle-calls-seam",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				return putReq{Model: keepModelBody(seed), HealthNotifications: false}
			},
			running:          false,
			wantStatus:       http.StatusOK,
			wantOutcomes:     []fieldOutcome{{Field: "healthNotifications", Outcome: "applied"}},
			wantReapplyCalls: 0,
			wantHealthCalls:  1,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.False(t, saved.HealthNotificationsEnabled())
				require.Len(t, health.vals, 1)
				assert.False(t, health.vals[0])
			},
		},
		{
			name: "health-toggle-nil-seam-reports-failed-but-saves",
			seed: baseSeed,
			body: func(seed desktop.Config) putReq {
				return putReq{Model: keepModelBody(seed), HealthNotifications: false}
			},
			running:          false,
			nilHealthSeam:    true,
			wantStatus:       http.StatusOK,
			wantReapplyCalls: 0,
			wantHealthCalls:  0,
			check: func(t *testing.T, seed, saved desktop.Config, reap *reapplyRecorder, health *healthRecorder, outcomes []fieldOutcome) {
				assert.False(t, saved.HealthNotificationsEnabled(), "save stands even though the sync seam is absent")
				require.Len(t, outcomes, 1)
				assert.Equal(t, "healthNotifications", outcomes[0].Field)
				assert.Equal(t, "failed", outcomes[0].Outcome)
				assert.Contains(t, outcomes[0].Message, "not configured")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seed := tc.seed()
			seedConfig(t, dir, seed)

			reap := &reapplyRecorder{err: tc.reapplyErr}
			health := &healthRecorder{}
			deps := Deps{
				SupportDir:                 dir,
				State:                      func() State { return State{Running: tc.running} },
				ReapplyConfig:              reap.fn,
				OnHealthNotificationsSaved: health.fn,
			}
			if tc.nilHealthSeam {
				deps.OnHealthNotificationsSaved = nil
			}
			s := startTestServer(t, deps)
			c := authedClient(t, s)

			bodyBytes, err := json.Marshal(tc.body(seed))
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodPut, "http://"+s.Addr()+"/api/config", bytes.NewReader(bodyBytes))
			require.NoError(t, err)
			resp, err := c.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			respBody, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode, "body: %s", respBody)

			var outcomes []fieldOutcome
			if resp.StatusCode == http.StatusOK {
				var got configUpdateResponse
				require.NoError(t, json.Unmarshal(respBody, &got))
				outcomes = got.Outcomes
				if tc.wantOutcomes != nil {
					require.Len(t, outcomes, len(tc.wantOutcomes))
					for i := range tc.wantOutcomes {
						assert.Equal(t, tc.wantOutcomes[i].Field, outcomes[i].Field)
						assert.Equal(t, tc.wantOutcomes[i].Outcome, outcomes[i].Outcome)
					}
				}
			}

			saved, err := desktop.Load(dir)
			require.NoError(t, err)

			assert.Equal(t, tc.wantReapplyCalls, reap.calls, "ReapplyConfig call count")
			assert.Equal(t, tc.wantHealthCalls, health.calls, "OnHealthNotificationsSaved call count")

			if tc.check != nil {
				tc.check(t, seed, saved, reap, health, outcomes)
			}
		})
	}
}

// assertionError is a trivial error type for reapplyErr fixtures — avoids an
// "errors" import for a single test-local sentinel.
type assertionError string

func (e assertionError) Error() string { return string(e) }
