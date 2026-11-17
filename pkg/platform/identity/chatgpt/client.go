package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const issuer = "https://auth.openai.com"
const resource = "https://api.openai.com/v1"
const directScope = "chatgpt.tokens.use.direct"

// Client owns OAuth and refresh. Endpoint overrides are intentionally private;
// account tokens must only be sent to OpenAI, never OPENAI_BASE_URL.
type Client struct {
	Store                              Store
	http                               *http.Client
	authURL, tokenURL, jwksURL, apiURL string
}

func New(dir string) *Client {
	return &Client{Store: Store{Dir: dir}, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		authURL: issuer + "/api/accounts/authorize", tokenURL: issuer + "/api/accounts/oauth/token", jwksURL: issuer + "/.well-known/jwks.json", apiURL: resource}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (c *Client) exchange(ctx context.Context, values url.Values) (tokenResponse, error) {
	values.Set("resource", resource)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("chatgpt: token endpoint unavailable")
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("chatgpt: token exchange failed (HTTP %d); sign in again", resp.StatusCode)
	}
	var token tokenResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return tokenResponse{}, fmt.Errorf("chatgpt: malformed token response")
	}
	if token.AccessToken == "" || token.RefreshToken == "" || !strings.EqualFold(token.TokenType, "Bearer") || token.ExpiresIn <= 0 {
		return tokenResponse{}, fmt.Errorf("chatgpt: incomplete token response")
	}
	return token, nil
}

func applyToken(a *Account, t tokenResponse) {
	a.AccessToken = t.AccessToken
	a.RefreshToken = t.RefreshToken
	if t.IDToken != "" {
		a.IDToken = t.IDToken
	}
	if t.Scope != "" {
		a.Scope = t.Scope
	}
	a.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

// Access returns a fresh token, committing a rotating refresh token before
// releasing the cross-process lock. It never falls back to a stale token.
func (c *Client) Access(ctx context.Context, id string) (account Account, err error) {
	err = c.Store.transaction(ctx, func(st *state) error {
		a, ok := st.Accounts[id]
		if !ok || a.RefreshToken == "" {
			return fmt.Errorf("chatgpt: sign in to this account first")
		}
		if time.Until(a.ExpiresAt) < 5*time.Minute {
			t, e := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {a.ClientID}, "refresh_token": {a.RefreshToken}})
			if e != nil {
				return e
			}
			applyToken(&a, t)
			st.Accounts[id] = a
		}
		account = a
		return nil
	})
	if err == nil && !slices.Contains(strings.Fields(account.Scope), directScope) {
		err = fmt.Errorf("chatgpt: ChatGPT plan usage permission was not granted")
	}
	return
}

// Login runs a fresh PKCE/OIDC attempt in the system browser. selected is an
// issued client ID for reauthorization, or empty for a new registration.
func (c *Client) Login(ctx context.Context, selected string, open func(string) error) (account Account, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var host string
	var previous Account
	if err = c.Store.transaction(ctx, func(st *state) error {
		host = st.HostID
		if selected != "" {
			var ok bool
			previous, ok = st.Accounts[selected]
			if !ok {
				return fmt.Errorf("chatgpt: unknown account registration")
			}
		}
		return nil
	}); err != nil {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return account, err
	}
	redirect := "http://" + ln.Addr().String() + "/auth/callback"
	stateValue, nonce, verifier := rand.Text(), rand.Text(), oauth2.GenerateVerifier()
	result := make(chan url.Values, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		q := r.URL.Query()
		if r.Method != http.MethodGet || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(stateValue)) != 1 {
			http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
			return
		}
		select {
		case result <- q:
			http.Error(w, "Return to OAP Desktop to finish signing in.", http.StatusOK)
		default:
			http.Error(w, "This sign-in attempt was already used.", http.StatusConflict)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			serveErr <- e
		}
	}()
	defer func() { err = errors.Join(err, srv.Close()) }()
	clientID := selected
	if clientID == "" {
		clientID = "dynamic_agent_client"
	}
	conf := oauth2.Config{ClientID: clientID, RedirectURL: redirect, Scopes: strings.Fields("openid profile email offline_access resource.invoke " + directScope), Endpoint: oauth2.Endpoint{AuthURL: c.authURL}}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce), oauth2.SetAuthURLParam("resource", resource), oauth2.SetAuthURLParam("ext_agent_host_id", host)}
	if selected == "" {
		opts = append(opts, oauth2.SetAuthURLParam("agent_name_hint", "Open Agent Primitives"))
	} else if previous.IDToken != "" {
		opts = append(opts, oauth2.SetAuthURLParam("id_token_hint", previous.IDToken))
	}
	if err = open(conf.AuthCodeURL(stateValue, opts...)); err != nil {
		return account, fmt.Errorf("chatgpt: could not open sign-in browser")
	}
	var q url.Values
	select {
	case q = <-result:
	case err = <-serveErr:
		return
	case <-ctx.Done():
		return account, fmt.Errorf("chatgpt: sign-in cancelled or timed out")
	}
	if q.Get("error") != "" {
		return account, fmt.Errorf("chatgpt: sign-in was declined; no account was changed")
	}
	issued := q.Get("client_id")
	if selected != "" {
		if issued != "" && issued != selected {
			return account, fmt.Errorf("chatgpt: callback changed the selected registration")
		}
		issued = selected
	}
	if issued == "" || issued == "dynamic_agent_client" || q.Get("code") == "" {
		return account, fmt.Errorf("chatgpt: incomplete registration callback")
	}
	token, err := c.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {issued}, "code": {q.Get("code")}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
	if err != nil {
		return account, err
	}
	verifyCtx := oidc.ClientContext(ctx, c.http)
	v := oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(verifyCtx, c.jwksURL), &oidc.Config{ClientID: issued})
	id, err := v.Verify(verifyCtx, token.IDToken)
	if err != nil {
		return account, fmt.Errorf("chatgpt: ID token verification failed")
	}
	if id.Nonce != nonce || id.Subject == "" || (selected != "" && id.Subject != previous.Subject) {
		return account, fmt.Errorf("chatgpt: sign-in identity does not match the pending attempt")
	}
	if !slices.Contains(strings.Fields(token.Scope), directScope) {
		return account, fmt.Errorf("chatgpt: ChatGPT plan usage permission was not granted")
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err = id.Claims(&claims); err != nil {
		return account, fmt.Errorf("chatgpt: invalid identity claims")
	}
	account = Account{ClientID: issued, Subject: id.Subject, Email: claims.Email}
	applyToken(&account, token)
	err = c.Store.transaction(ctx, func(st *state) error {
		if old, ok := st.Accounts[issued]; ok && old.Subject != account.Subject {
			return fmt.Errorf("chatgpt: account registration identity changed")
		}
		st.Accounts[issued] = account
		return nil
	})
	return
}

type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
}

func (c *Client) Models(ctx context.Context, id string) ([]Model, error) {
	a, err := c.Access(ctx, id)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: model catalog unavailable")
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chatgpt: model catalog returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Models []Model `json:"models"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("chatgpt: invalid model catalog")
	}
	return slices.DeleteFunc(body.Models, func(m Model) bool { return m.Visibility != "list" || m.Slug == "" }), nil
}

// SignOut retains the registration identity for reauthorization. A failed remote
// revocation is reported even though local tokens are cleared.
func (c *Client) SignOut(ctx context.Context, id string) error {
	var remoteErr error
	err := c.Store.transaction(ctx, func(st *state) error {
		a, ok := st.Accounts[id]
		if !ok {
			return fmt.Errorf("chatgpt: unknown account")
		}
		if a.RefreshToken != "" {
			remoteErr = c.revoke(ctx, a)
		}
		a.AccessToken = ""
		a.RefreshToken = ""
		a.IDToken = ""
		a.ExpiresAt = time.Time{}
		st.Accounts[id] = a
		return nil
	})
	return errors.Join(err, remoteErr)
}

func (c *Client) revoke(ctx context.Context, a Account) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("chatgpt: remote revocation unconfirmed; disconnect the app in ChatGPT Settings")
	}
	defer closeBody(resp.Body)
	var config struct {
		Endpoint string `json:"revocation_endpoint"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&config) != nil {
		return fmt.Errorf("chatgpt: remote revocation discovery failed; disconnect the app in ChatGPT Settings")
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "auth.openai.com" {
		return fmt.Errorf("chatgpt: untrusted revocation endpoint")
	}
	for attempt := 0; attempt < 3; attempt++ {
		form := url.Values{"token": {a.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {a.ClientID}}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		result, e := c.http.Do(req)
		if e == nil {
			closeBody(result.Body)
			if result.StatusCode == 200 {
				return nil
			}
			if result.StatusCode < 500 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return fmt.Errorf("chatgpt: signed out locally; remote revocation unconfirmed; disconnect the app in ChatGPT Settings")
}

func closeBody(body io.Closer) {
	if err := body.Close(); err != nil {
		log.Printf("chatgpt: close HTTP response: %v", err)
	}
}
