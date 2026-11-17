// Package accountui provides the redacted account API shared by desktop setup
// and settings. The caller must mount it behind its loopback/authentication gate.
package accountui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/chatgpt"
)

type Handler struct {
	Client      *chatgpt.Client
	OpenBrowser func(string) error
	OnSignOut   func(context.Context) error
	mu          sync.Mutex
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var result any
	var err error
	switch r.URL.Path {
	case "/accounts":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		type publicAccount struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			SignedIn bool   `json:"signedIn"`
		}
		accounts, e := h.Client.Store.Accounts(r.Context())
		err = e
		visible := []publicAccount{}
		for _, a := range accounts {
			visible = append(visible, publicAccount{a.ClientID, a.Email + " · " + a.ClientID, a.RefreshToken != ""})
		}
		sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
		result = visible
	case "/accounts/models":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		result, err = h.Client.Models(r.Context(), r.URL.Query().Get("account"))
	case "/accounts/login", "/accounts/logout":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !h.mu.TryLock() {
			http.Error(w, "A sign-in attempt is already in progress.", 409)
			return
		}
		defer h.mu.Unlock()
		var input struct {
			Account string `json:"account"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
			http.Error(w, "Invalid account request", 400)
			return
		}
		if r.URL.Path == "/accounts/login" {
			if h.OpenBrowser == nil {
				http.Error(w, "Browser sign-in unavailable", 503)
				return
			}
			a, e := h.Client.Login(r.Context(), input.Account, h.OpenBrowser)
			err = e
			result = map[string]string{"account": a.ClientID}
		} else {
			err = h.Client.SignOut(r.Context(), input.Account)
			if h.OnSignOut != nil {
				err = errors.Join(err, h.OnSignOut(r.Context()))
			}
			result = map[string]string{"status": "signed out"}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(result); err != nil {
		fmt.Printf("desktop account response failed: %v\n", err)
	}
}
