package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
)

type installWorkflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

type workflowCatalog interface {
	List(context.Context, string, string) ([]installWorkflow, error)
}

type httpWorkflowCatalog struct {
	baseURL string
	client  *http.Client
}

func (c httpWorkflowCatalog) List(ctx context.Context, repo, token string) ([]installWorkflow, error) {
	var workflows []installWorkflow
	for page := 1; page <= 100; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/workflows?per_page=100&page=%d", strings.TrimRight(c.baseURL, "/"), repo, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("discover workflows for %s: %w", repo, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read workflows for %s: %w", repo, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close workflows response for %s: %w", repo, closeErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("discover workflows for %s: GitHub HTTP %d; verify repository access and Actions read permission", repo, resp.StatusCode)
		}
		var result struct {
			Workflows []installWorkflow `json:"workflows"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decode workflows for %s: %w", repo, err)
		}
		workflows = append(workflows, result.Workflows...)
		if len(result.Workflows) < 100 {
			return workflows, nil
		}
	}
	return nil, fmt.Errorf("discover workflows for %s exceeded 100 pages", repo)
}

func (w *wizard) validateWorkflowSelection(ctx context.Context, in channelkinds.WizardInput, answers map[string]string) error {
	cfg, err := effectiveHandlerConfig(in, answers)
	if err != nil {
		return err
	}
	if cfg.Trigger == nil {
		return nil
	}
	minter := w.workflowMinter
	if minter == nil {
		minter = githubapp.NewHTTPMinter()
	}
	minted, err := minter.Mint(ctx, githubapp.MintRequest{AppID: answers[keyAppID], InstallationID: answers[keyInstallationID], PrivateKeyPEM: []byte(answers[keyPrivateKeyPEM])})
	if err != nil {
		return fmt.Errorf("github workflow selection: mint installation token: %w", err)
	}
	catalog := w.workflowCatalog
	if catalog == nil {
		catalog = httpWorkflowCatalog{baseURL: "https://api.github.com", client: &http.Client{Timeout: 30 * time.Second}}
	}
	for _, repo := range cfg.Repositories {
		available, err := catalog.List(ctx, repo, string(minted.AccessToken.UnderlyingValue()))
		if err != nil {
			return err
		}
		paths := map[string]bool{}
		var choices []string
		for _, workflow := range available {
			if workflow.ID <= 0 || workflow.State != "active" {
				continue
			}
			paths[workflow.Path] = true
			choices = append(choices, workflow.Name+" ("+workflow.Path+")")
		}
		sort.Strings(choices)
		for _, selected := range cfg.Trigger.Workflows[repo] {
			if !paths[selected] {
				return fmt.Errorf("github workflows: %s does not have active workflow %q; available workflows: %s", repo, selected, strings.Join(choices, ", "))
			}
		}
	}
	return nil
}
