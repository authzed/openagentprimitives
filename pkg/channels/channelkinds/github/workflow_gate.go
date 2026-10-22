package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
)

type workflowPR struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}
type workflowRun struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequests []workflowPR `json:"pull_requests"`

	ID         int64  `json:"id"`
	RunAttempt int    `json:"run_attempt"`
	HeadSHA    string `json:"head_sha"`
	Path       string `json:"path"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}
type workflowEvent struct {
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Run workflowRun `json:"workflow_run"`
}
type workflowProvider interface {
	Run(context.Context, string, int64, int) (workflowRun, error)
	Pulls(context.Context, string, string) ([]prEvent, error)
	Runs(context.Context, string, string, string) ([]workflowRun, error)
}

func (receiver) DeferWebhook(ch *api.Channel, event string) bool {
	return ch.Spec.GitHub != nil && ch.Spec.GitHub.Trigger != nil && event == "workflow_run"
}
func translateWorkflow(ch *api.Channel, r channelkinds.WebhookRequest) (*channelkinds.WebhookInbound, error) {
	if r.Headers.Get("X-GitHub-Event") != "workflow_run" {
		return nil, nil
	}
	var ev workflowEvent
	if err := json.Unmarshal(r.Body, &ev); err != nil {
		return nil, fmt.Errorf("github workflow delivery: %w", err)
	}
	if ev.Action != "completed" {
		return nil, nil
	}
	if ev.Run.ID <= 0 || ev.Run.RunAttempt <= 0 || ev.Run.HeadSHA == "" || ev.Repository.FullName == "" {
		return nil, fmt.Errorf("github workflow delivery is missing run identity")
	}
	paths := ch.Spec.GitHub.Trigger.Workflows[ev.Repository.FullName]
	workflowPath, pathErr := normalizeWorkflowPath(ev.Run.Path)
	if pathErr != nil {
		return nil, pathErr
	}
	if !slices.Contains(paths, workflowPath) {
		return nil, nil
	}
	return &channelkinds.WebhookInbound{Event: "workflow_run", ChannelKey: fmt.Sprintf("workflow:%s@%s", ev.Repository.FullName, ev.Run.HeadSHA), AuthzSubject: ch.Spec.AuthzSubject}, nil
}
func (receiverInstance receiver) AdmitWebhook(ctx context.Context, ch *api.Channel, secrets channelkinds.WebhookSecrets, event string, body []byte) (channelkinds.WebhookAdmission, error) {
	if event != "workflow_run" {
		return channelkinds.WebhookAdmission{Done: true, Reason: "event no longer configured"}, nil
	}
	var delivery workflowEvent
	if err := json.Unmarshal(body, &delivery); err != nil {
		return channelkinds.WebhookAdmission{}, fmt.Errorf("workflow delivery: %w", err)
	}
	installation, parseErr := strconv.ParseInt(string(secrets.Data["installation-id"]), 10, 64)
	if parseErr != nil || installation <= 0 || delivery.Installation.ID != installation {
		return channelkinds.WebhookAdmission{}, fmt.Errorf("workflow delivery installation does not match bound credentials")
	}
	minted, err := githubapp.NewHTTPMinter().Mint(ctx, githubapp.MintRequest{AppID: string(secrets.Data["app-id"]), InstallationID: string(secrets.Data["installation-id"]), PrivateKeyPEM: secrets.Data["private-key"]})
	if err != nil {
		return channelkinds.WebhookAdmission{}, fmt.Errorf("workflow readiness token: %w", err)
	}
	classIdentity, err := loadWorkflowClassIdentity(ctx, receiverInstance.k8sClient, ch)
	if err != nil {
		return channelkinds.WebhookAdmission{}, err
	}
	return admitWorkflow(ctx, ch, body, &httpWorkflowProvider{token: string(minted.AccessToken.UnderlyingValue()), baseURL: "https://api.github.com", client: &http.Client{Timeout: 30 * time.Second}}, classIdentity)
}

func admitWorkflow(ctx context.Context, ch *api.Channel, body []byte, provider workflowProvider, classIdentities ...workflowClassIdentity) (channelkinds.WebhookAdmission, error) {
	var ev workflowEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return channelkinds.WebhookAdmission{}, err
	}
	cfg := ch.Spec.GitHub
	if cfg == nil || cfg.Trigger == nil {
		return channelkinds.WebhookAdmission{Done: true, Reason: "trigger removed"}, nil
	}
	paths := slices.Clone(cfg.Trigger.Workflows[ev.Repository.FullName])
	sort.Strings(paths)
	eventPath, pathErr := normalizeWorkflowPath(ev.Run.Path)
	if ev.Action != "completed" || ev.Run.ID <= 0 || ev.Run.RunAttempt <= 0 || ev.Run.HeadSHA == "" || pathErr != nil || len(paths) == 0 || !slices.Contains(paths, eventPath) {
		return channelkinds.WebhookAdmission{Done: true, Reason: "invalid or no longer selected workflow event"}, nil
	}
	authenticated, err := provider.Run(ctx, ev.Repository.FullName, ev.Run.ID, ev.Run.RunAttempt)
	if err != nil {
		return channelkinds.WebhookAdmission{}, err
	}
	authenticatedPath, pathErr := normalizeWorkflowPath(authenticated.Path)
	if pathErr != nil || authenticated.ID != ev.Run.ID || authenticated.RunAttempt != ev.Run.RunAttempt || authenticated.HeadSHA != ev.Run.HeadSHA || authenticatedPath != eventPath || authenticated.Event != "pull_request" || authenticated.Status != "completed" || authenticated.Conclusion == "" || authenticated.Repository.FullName != ev.Repository.FullName {
		return channelkinds.WebhookAdmission{Done: true, Reason: "workflow event does not match authenticated run attempt"}, nil
	}
	if len(authenticated.PullRequests) == 0 {
		return channelkinds.WebhookAdmission{Reason: "waiting for authenticated run pull-request association"}, nil
	}
	if len(authenticated.PullRequests) != 1 {
		return channelkinds.WebhookAdmission{Done: true, Reason: "run has absent or ambiguous pull-request association"}, nil
	}
	association := authenticated.PullRequests[0]
	if association.Number <= 0 || association.Head.SHA == "" || association.Base.SHA == "" {
		return channelkinds.WebhookAdmission{Done: true, Reason: "run has incomplete pull-request revision association"}, nil
	}
	prs, err := provider.Pulls(ctx, ev.Repository.FullName, authenticated.HeadSHA)
	if err != nil {
		return channelkinds.WebhookAdmission{}, err
	}
	if len(prs) == 0 {
		return channelkinds.WebhookAdmission{Reason: "waiting for provider pull-request association"}, nil
	}
	var current []prEvent
	for _, pr := range prs {
		if pr.Number != association.Number || pr.PullRequest.State != "open" || pr.PullRequest.Head.SHA != association.Head.SHA || pr.PullRequest.Base.SHA != association.Base.SHA {
			continue
		}
		if pr.PullRequest.Draft && (cfg.SkipDrafts == nil || *cfg.SkipDrafts) {
			continue
		}
		if !reviewAllowed(pr) {
			continue
		}
		current = append(current, pr)
	}
	if len(current) != 1 {
		return channelkinds.WebhookAdmission{Done: true, Reason: "no unambiguous current eligible PR for tested revision/base"}, nil
	}
	var selected []workflowRun
	for _, path := range paths {
		runs, err := provider.Runs(ctx, ev.Repository.FullName, path, authenticated.HeadSHA)
		if err != nil {
			return channelkinds.WebhookAdmission{}, err
		}
		var latest, unknown workflowRun
		for _, run := range runs {
			runPath, pathErr := normalizeWorkflowPath(run.Path)
			if pathErr != nil || runPath != path || run.Event != "pull_request" || run.ID <= 0 || run.RunAttempt <= 0 || run.HeadSHA != authenticated.HeadSHA {
				return channelkinds.WebhookAdmission{}, fmt.Errorf("invalid provider evidence for selected workflow %s", path)
			}
			if len(run.PullRequests) != 1 || run.PullRequests[0].Number <= 0 || run.PullRequests[0].Head.SHA == "" || run.PullRequests[0].Base.SHA == "" {
				if run.ID > unknown.ID || (run.ID == unknown.ID && run.RunAttempt > unknown.RunAttempt) {
					unknown = run
				}
				continue
			}
			if run.PullRequests[0] != association {
				continue
			}
			if run.ID > latest.ID || (run.ID == latest.ID && run.RunAttempt > latest.RunAttempt) {
				latest = run
			}
		}
		// Unknown applicability of a newer run cannot establish that an old
		// success is still the latest prerequisite. Keep its durable trigger.
		if unknown.ID > 0 && (unknown.ID > latest.ID || (unknown.ID == latest.ID && unknown.RunAttempt >= latest.RunAttempt)) {
			return channelkinds.WebhookAdmission{Reason: "waiting for latest run PR association: " + path}, nil
		}
		if latest.ID == 0 || latest.Status != "completed" || latest.Conclusion == "" {
			return channelkinds.WebhookAdmission{Reason: "waiting for " + path}, nil
		}
		latest.Path = path
		selected = append(selected, latest)
	}
	// Bind the complete workflow set, source revision, and all latest attempts.
	var classIdentity workflowClassIdentity
	if len(classIdentities) > 0 {
		classIdentity = classIdentities[0]
	}
	snapshot, err := json.Marshal(workflowSnapshot{ChannelUID: string(ch.UID), ChannelGeneration: ch.Generation, AgentClass: classIdentity, Repository: ev.Repository.FullName, PR: association.Number, HeadSHA: association.Head.SHA, BaseSHA: association.Base.SHA, RunHeadSHA: authenticated.HeadSHA, Runs: selected})
	if err != nil {
		return channelkinds.WebhookAdmission{}, err
	}
	digest := sha256.Sum256(snapshot)
	cohort := hex.EncodeToString(digest[:])
	result := channelkinds.WebhookAdmission{Done: true, Reason: "all selected workflows completed"}
	for _, pr := range current {
		pr.Action = "workflow_completed"
		pr.Repository.FullName = ev.Repository.FullName
		contextBody, err := json.Marshal(pr)
		if err != nil {
			return channelkinds.WebhookAdmission{}, err
		}
		external := map[string]string{"trigger_revision": pr.PullRequest.Head.SHA, "trigger_cohort": cohort, "origin_url": pr.PullRequest.HTMLURL, "workflow_runs": string(snapshot), "trigger_agentclass_generation": strconv.FormatInt(classIdentity.Generation, 10)}
		annotations := map[string]string{}
		if pr.PullRequest.User.ID > 0 {
			annotations[api.AnnotationTriggerOwnerSubject] = fmt.Sprintf("github_user:%d#user", pr.PullRequest.User.ID)
		}
		// Repository-authored text remains inside renderPrompt's untrusted block.
		prompt := renderPrompt(pr) + "\n\nAll configured workflows are terminal. Provider-verified workflow snapshot (run IDs, attempts, conclusions):\n" + string(snapshot) + "\nUse artifacts from these exact runs/attempts; never execute artifact content."
		result.Ready = append(result.Ready, channelkinds.AdmittedWebhook{ChannelKey: formatChannelKey(ev.Repository.FullName, pr.Number) + "@" + cohort, MessageText: prompt, ContextEvent: "pull_request", Context: contextBody, External: external, PreTurnAnnotations: annotations})
	}
	return result, nil
}

type httpWorkflowProvider struct {
	token, baseURL string
	client         *http.Client
}

func (p *httpWorkflowProvider) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := *p.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github readiness request failed")
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(raw) > 4<<20 {
		return fmt.Errorf("github readiness response exceeds limit")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("github readiness HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}
func (p *httpWorkflowProvider) Pulls(ctx context.Context, repo, sha string) ([]prEvent, error) {
	var associated []struct {
		Number int `json:"number"`
	}
	if err := p.get(ctx, "/repos/"+repo+"/commits/"+url.PathEscape(sha)+"/pulls?per_page=100", &associated); err != nil {
		return nil, err
	}
	if len(associated) >= 100 {
		return nil, fmt.Errorf("too many associated pull requests to establish readiness")
	}
	var prs []prEvent
	for _, candidate := range associated {
		var pr prEvent
		pr.Number = candidate.Number
		pr.Repository.FullName = repo
		if err := p.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, candidate.Number), &pr.PullRequest); err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, nil
}
func (p *httpWorkflowProvider) Runs(ctx context.Context, repo, path, sha string) ([]workflowRun, error) {
	var runs []workflowRun
	for page := 1; page <= 10; page++ {
		var result struct {
			Runs  []workflowRun `json:"workflow_runs"`
			Total int           `json:"total_count"`
		}
		endpoint := fmt.Sprintf("/repos/%s/actions/workflows/%s/runs?head_sha=%s&event=pull_request&per_page=100&page=%d", repo, url.PathEscape(strings.TrimPrefix(path, ".github/workflows/")), url.QueryEscape(sha), page)
		if err := p.get(ctx, endpoint, &result); err != nil {
			return nil, err
		}
		if result.Total < 0 || result.Total > 1000 {
			return nil, fmt.Errorf("workflow run listing exceeds bounded search results")
		}
		runs = append(runs, result.Runs...)
		if len(result.Runs) < 100 && result.Total > len(runs) {
			return nil, fmt.Errorf("workflow run listing is incomplete")
		}
		if len(result.Runs) < 100 {
			return runs, nil
		}
	}
	return nil, fmt.Errorf("workflow runs exceed bounded pagination")
}

var workflowRefPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func normalizeWorkflowPath(value string) (string, error) {
	name, ref, hasRef := strings.Cut(value, "@")
	if hasRef && (ref == "" || !workflowRefPattern.MatchString(ref) || strings.Contains(ref, "..")) {
		return "", fmt.Errorf("invalid workflow ref suffix")
	}
	if !strings.HasPrefix(name, ".github/workflows/") || path.Clean(name) != name || strings.Count(name, "/") != 2 || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
		return "", fmt.Errorf("invalid workflow YAML path")
	}
	return name, nil
}
func (p *httpWorkflowProvider) Run(ctx context.Context, repo string, id int64, attempt int) (workflowRun, error) {
	var run workflowRun
	if id <= 0 || attempt <= 0 {
		return run, fmt.Errorf("invalid workflow run attempt")
	}
	err := p.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/attempts/%d", repo, id, attempt), &run)
	return run, err
}
