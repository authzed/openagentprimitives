// Package contenttoken mints + verifies short-lived HMAC capability tokens
// that gate webd's sandbox-origin artifact endpoints: /content (the primary
// artifact framed by the live-view), /artifacts/a/ (a same-session
// secondary artifact referenced from the primary via `artifact:HANDLE`), and
// /mcpui-content + /mcpui-host (an MCP-UI interactive widget framed by the
// session-view page). webd mints a token on the trusted side (after the
// SpiceDB view/interact check, or — for asset tokens — after resolving the
// handle within the primary's own session) and embeds it in the URL; the
// cookieless sandbox endpoint verifies it before serving bytes. Both mint +
// verify happen in webd, so a single shared key suffices. Claims.Kind ties a
// token to exactly one of the three routes — see Sign/Verify,
// SignAsset/VerifyAsset, and SignWidget/VerifyWidget.
package contenttoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims is the content-capability token body. It carries exactly what the
// sandbox content/asset endpoints need to fetch the right render bytes.
type Claims struct {
	Ns         string `json:"ns"`         // namespace of the owning AgentSession
	Sess       string `json:"sess"`       // name of the owning AgentSession
	RenderName string `json:"render"`     // the ArtifactRender CR name
	ArtifactID string `json:"artifactId"` // for logging/audit
	ExpiresAt  int64  `json:"exp"`        // Unix seconds; sign refuses a zero value
	// Kind discriminates which sandbox route a token authorizes: KindContent for
	// the primary artifact framed by /content, KindAsset for a same-session
	// secondary fetched by /artifacts/a/, KindWidget for an MCP-UI widget fetched
	// by /mcpui-content and framed by /mcpui-host. Each Sign*/Verify* pair stamps
	// and requires its own kind, so a token minted for one route can never be
	// replayed at another — in particular, neither a /content nor an
	// /artifacts/a/ token reaches the script-enabled /mcpui-content route.
	Kind string `json:"kind,omitempty"`
}

// Token kinds — see Claims.Kind.
const (
	KindContent = "content"
	KindAsset   = "asset"
	KindWidget  = "widget"
)

// Signer signs + verifies content tokens with an HMAC-SHA256 key.
type Signer struct {
	key []byte
	now func() time.Time
}

func New(key []byte) *Signer { return &Signer{key: key, now: time.Now} }

var (
	ErrMalformed = errors.New("contenttoken: malformed")
	ErrSignature = errors.New("contenttoken: bad signature")
	ErrExpired   = errors.New("contenttoken: expired")
	// ErrWrongKind means the token verified (signature + expiry both good) but
	// was minted for a different route than the one verifying it — e.g. a
	// KindContent token presented at VerifyAsset. See Claims.Kind.
	ErrWrongKind = errors.New("contenttoken: wrong token kind")
)

// Sign mints a content-capability token (Kind=KindContent) for the sandbox
// /content route.
func (s *Signer) Sign(c Claims) (string, error) {
	c.Kind = KindContent
	return s.sign(c)
}

// Verify verifies a token minted by Sign. It rejects a well-signed,
// unexpired token that was minted with a different Kind (ErrWrongKind) —
// see Claims.Kind.
func (s *Signer) Verify(raw string) (Claims, error) {
	c, err := s.verify(raw)
	if err != nil {
		return Claims{}, err
	}
	if c.Kind != KindContent {
		return Claims{}, ErrWrongKind
	}
	return c, nil
}

// SignAsset mints an asset-capability token (Kind=KindAsset) for the sandbox
// /artifacts/a/ route — a same-session secondary artifact resolved from an
// `artifact:HANDLE` reference in a primary's live-view HTML.
func (s *Signer) SignAsset(c Claims) (string, error) {
	c.Kind = KindAsset
	return s.sign(c)
}

// VerifyAsset verifies a token minted by SignAsset. Like Verify, it rejects a
// well-signed, unexpired token minted with a different Kind — in particular a
// /content token can never be replayed at /artifacts/a/.
func (s *Signer) VerifyAsset(raw string) (Claims, error) {
	c, err := s.verify(raw)
	if err != nil {
		return Claims{}, err
	}
	if c.Kind != KindAsset {
		return Claims{}, ErrWrongKind
	}
	return c, nil
}

// SignWidget mints a widget-capability token (Kind=KindWidget) for the
// sandbox /mcpui-content + /mcpui-host routes — an MCP-UI interactive widget
// framed by the session-view page (pkg/web/webui/sessionview).
func (s *Signer) SignWidget(c Claims) (string, error) {
	c.Kind = KindWidget
	return s.sign(c)
}

// VerifyWidget verifies a token minted by SignWidget. Like Verify/VerifyAsset,
// it rejects a well-signed, unexpired token minted with a different Kind — a
// /content or /artifacts/a/ token can never be replayed at the
// script-enabled /mcpui-content route, and vice versa.
func (s *Signer) VerifyWidget(raw string) (Claims, error) {
	c, err := s.verify(raw)
	if err != nil {
		return Claims{}, err
	}
	if c.Kind != KindWidget {
		return Claims{}, ErrWrongKind
	}
	return c, nil
}

func (s *Signer) sign(c Claims) (string, error) {
	if c.ExpiresAt == 0 {
		return "", fmt.Errorf("contenttoken: Sign requires ExpiresAt")
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("contenttoken: marshal: %w", err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(b64))
	return b64 + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *Signer) verify(raw string) (Claims, error) {
	b64, sig, ok := strings.Cut(raw, ".")
	if !ok || b64 == "" || sig == "" {
		return Claims{}, ErrMalformed
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(b64))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		return Claims{}, ErrSignature
	}
	body, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if s.now().Unix() > c.ExpiresAt {
		return Claims{}, ErrExpired
	}
	return c, nil
}
