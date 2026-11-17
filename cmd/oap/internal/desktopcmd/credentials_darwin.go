//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/accountui"
)

func (s *desktopState) accountHandler() *accountui.Handler {
	return &accountui.Handler{Client: desktop.ChatGPTClient(s.support), OpenBrowser: func(url string) error { return exec.Command("open", url).Run() }, OnSignOut: func(ctx context.Context) error {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			return nil
		}
		return s.syncModelCredentials(ctx)
	}}
}

func (s *desktopState) maintainModelCredentials(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			continue
		}
		stepCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := s.syncModelCredentials(stepCtx)
		cancel()
		if err != nil {
			s.logf("model credential refresh: %v", err)
		}
	}
}

func (s *desktopState) syncModelCredentials(ctx context.Context) error {
	// Sign-out's final publish must follow any in-flight refresh publish, so
	// the timer cannot restore an access token after sign-out returns.
	s.modelCredentialsMu.Lock()
	defer s.modelCredentialsMu.Unlock()
	kc, err := os.ReadFile(filepath.Join(s.support, kubeconfigFileName))
	if err != nil {
		return err
	}
	dg, err := s.writeKubeconfig(kc)
	if err != nil {
		return err
	}
	b, err := dg.Bundle()
	if err != nil {
		return err
	}
	return desktop.SyncAccountTokens(ctx, s.support, b.Controller)
}
