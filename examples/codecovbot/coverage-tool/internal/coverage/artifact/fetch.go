// Package artifact downloads authenticated GitHub coverage artifacts without
// executing their contents. Resource limits and archive paths are fail-closed.
package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MaxArchiveBytes   int64 = 32 << 20
	MaxEntryBytes     int64 = 32 << 20
	MaxExtractedBytes int64 = 128 << 20
	MaxEntries              = 512
)

type Fetcher struct {
	// APIBaseURL and Client are dependency injection seams. The public CLI always
	// uses https://api.github.com and the default bounded-time HTTP client.
	APIBaseURL string
	Client     *http.Client
}
type Options struct {
	Repository                               string
	RunID, Attempt                           int64
	ArtifactName, Output, ExpectedSHA, Token string
}
type Metadata struct {
	PRNumber              int             `json:"pr_number,omitempty"`
	WorkflowRef           string          `json:"workflow_ref,omitempty"`
	ProducedAt            string          `json:"produced_at,omitempty"`
	Scope                 string          `json:"scope,omitempty"`
	Instrumentation       string          `json:"instrumentation,omitempty"`
	Toolchain             string          `json:"toolchain,omitempty"`
	TestResult            string          `json:"test_result,omitempty"`
	MeasurementDefinition json.RawMessage `json:"measurement_definition,omitempty"`
	MeasurementHash       string          `json:"measurement_hash,omitempty"`
	SchemaVersion         string          `json:"schema_version"`
	Repository            string          `json:"repository"`
	RunID                 int64           `json:"run_id"`
	RunAttempt            int64           `json:"run_attempt"`
	HeadSHA               string          `json:"head_sha"`
	TestedSHA             string          `json:"tested_sha"`
	BaseSHA               string          `json:"base_sha"`
	Profile               string          `json:"profile"`
	Source                string          `json:"source"`
}
type Result struct {
	RunHeadSHA    string   `json:"run_head_sha"`
	SchemaVersion string   `json:"schema_version"`
	Repository    string   `json:"repository"`
	RunID         int64    `json:"run_id"`
	RunAttempt    int64    `json:"run_attempt"`
	HeadSHA       string   `json:"head_sha"`
	ArtifactID    int64    `json:"artifact_id"`
	ArtifactName  string   `json:"artifact_name"`
	Output        string   `json:"output"`
	Profile       string   `json:"profile"`
	Source        string   `json:"source"`
	Metadata      Metadata `json:"metadata"`
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (f Fetcher) Fetch(ctx context.Context, o Options) (Result, error) {
	var result Result
	if !repositoryPattern.MatchString(o.Repository) || o.RunID <= 0 || o.Attempt <= 0 || o.ArtifactName == "" || o.Output == "" || o.Token == "" {
		return result, fmt.Errorf("repository, positive run-id/attempt, artifact-name, output and GITHUB_TOKEN are required")
	}
	base := strings.TrimRight(f.APIBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	client := http.Client{Timeout: 60 * time.Second}
	if f.Client != nil {
		client = *f.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var run struct {
		PullRequests []struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				SHA string `json:"sha"`
			} `json:"base"`
		} `json:"pull_requests"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		ID         int64  `json:"id"`
		Attempt    int64  `json:"run_attempt"`
		HeadSHA    string `json:"head_sha"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := getJSON(ctx, &client, base+fmt.Sprintf("/repos/%s/actions/runs/%d/attempts/%d", o.Repository, o.RunID, o.Attempt), o.Token, &run); err != nil {
		return result, err
	}
	if run.ID != o.RunID || run.Attempt != o.Attempt || !strings.EqualFold(run.Repository.FullName, o.Repository) || run.HeadSHA == "" {
		return result, fmt.Errorf("provider run/attempt/repository/SHA mismatch")
	}
	sourceHead := run.HeadSHA
	sourceBase := ""
	if len(run.PullRequests) > 1 {
		return result, fmt.Errorf("provider run has ambiguous PR associations")
	}
	if len(run.PullRequests) == 1 {
		sourceHead = run.PullRequests[0].Head.SHA
		sourceBase = run.PullRequests[0].Base.SHA
		if sourceHead == "" || sourceBase == "" || run.PullRequests[0].Number <= 0 {
			return result, fmt.Errorf("provider run has incomplete PR association")
		}
	}
	if o.ExpectedSHA != "" && o.ExpectedSHA != sourceHead {
		return result, fmt.Errorf("provider source head SHA mismatch")
	}
	if run.Status != "completed" || run.Conclusion != "success" {
		return result, fmt.Errorf("coverage producer run is not completed successfully")
	}
	type item struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Expired     bool   `json:"expired"`
		WorkflowRun struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_run"`
	}
	var matches []item
	for page := 1; page <= 10; page++ {
		var listing struct {
			Total     int    `json:"total_count"`
			Artifacts []item `json:"artifacts"`
		}
		endpoint := base + fmt.Sprintf("/repos/%s/actions/runs/%d/artifacts?per_page=100&page=%d", o.Repository, o.RunID, page)
		if err := getJSON(ctx, &client, endpoint, o.Token, &listing); err != nil {
			return result, err
		}
		if listing.Total > 1000 || listing.Total < 0 {
			return result, fmt.Errorf("artifact listing exceeds limit")
		}
		for _, a := range listing.Artifacts {
			if a.Name == o.ArtifactName && !a.Expired {
				if a.ID <= 0 || a.WorkflowRun.ID != o.RunID || a.WorkflowRun.HeadSHA != run.HeadSHA {
					return result, fmt.Errorf("artifact provider provenance mismatch")
				}
				matches = append(matches, a)
			}
		}
		if page*100 >= listing.Total {
			break
		}
	}
	if len(matches) != 1 {
		return result, fmt.Errorf("expected one nonexpired artifact named %q, found %d", o.ArtifactName, len(matches))
	}
	artifactID := matches[0].ID
	archiveURL := base + fmt.Sprintf("/repos/%s/actions/artifacts/%d/zip", o.Repository, artifactID)
	data, err := download(ctx, &client, archiveURL, base, o.Token)
	if err != nil {
		return result, err
	}
	meta, err := metadata(data)
	if err != nil {
		return result, err
	}
	if meta.SchemaVersion != "v1" || !strings.EqualFold(meta.Repository, o.Repository) || meta.RunID != o.RunID || meta.RunAttempt != o.Attempt || meta.HeadSHA != sourceHead || (sourceBase != "" && meta.BaseSHA != sourceBase) || (run.HeadSHA != sourceHead && meta.TestedSHA != run.HeadSHA) {
		return result, fmt.Errorf("artifact metadata does not match provider run/attempt/revision")
	}
	// If the workflow API names the source head while Actions checked out a
	// synthetic merge, authenticate that merge's exact source/base parents.
	if meta.TestedSHA != "" && meta.TestedSHA != sourceHead && meta.TestedSHA != run.HeadSHA {
		if sourceBase == "" || !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(meta.TestedSHA) {
			return result, fmt.Errorf("artifact checkout has no verifiable merge association")
		}
		var commit struct {
			SHA     string `json:"sha"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := getJSON(ctx, &client, base+"/repos/"+o.Repository+"/commits/"+meta.TestedSHA, o.Token, &commit); err != nil {
			return result, err
		}
		if commit.SHA != meta.TestedSHA || len(commit.Parents) != 2 || commit.Parents[0].SHA != sourceBase || commit.Parents[1].SHA != sourceHead {
			return result, fmt.Errorf("artifact checkout merge parents differ from authenticated PR head/base")
		}
	}
	if !safePath(meta.Profile) || !safePath(meta.Source) {
		return result, fmt.Errorf("unsafe profile/source metadata path")
	}
	if err := extract(data, o.Output); err != nil {
		return result, err
	}
	profile := filepath.Join(o.Output, filepath.FromSlash(meta.Profile))
	source := filepath.Join(o.Output, filepath.FromSlash(meta.Source))
	profileInfo, pErr := os.Stat(profile)
	sourceInfo, sErr := os.Stat(source)
	if pErr != nil || sErr != nil || !profileInfo.Mode().IsRegular() || !sourceInfo.IsDir() {
		cleanupErr := os.RemoveAll(o.Output)
		return result, errors.Join(fmt.Errorf("artifact profile/source missing or wrong type"), cleanupErr)
	}
	result = Result{SchemaVersion: "v1", Repository: o.Repository, RunID: o.RunID, RunAttempt: o.Attempt, HeadSHA: sourceHead, RunHeadSHA: run.HeadSHA, ArtifactID: artifactID, ArtifactName: o.ArtifactName, Output: o.Output, Profile: profile, Source: source, Metadata: meta}
	return result, nil
}
func request(ctx context.Context, client *http.Client, address, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}
func limited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("download exceeds byte limit")
	}
	return b, nil
}
func getJSON(ctx context.Context, client *http.Client, address, token string, v any) error {
	resp, err := request(ctx, client, address, token)
	if err != nil {
		return err
	}
	data, readErr := limited(resp.Body, 2<<20)
	closeErr := resp.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(data, v)
}
func allowedDownload(address, testBase string) bool {
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if testBase != "" {
		b, err := url.Parse(testBase)
		if err == nil && u.Scheme == b.Scheme && u.Host == b.Host {
			return true
		}
	}
	if u.Scheme != "https" || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "api.github.com" || strings.HasSuffix(host, ".blob.core.windows.net") || strings.HasSuffix(host, ".actions.githubusercontent.com") || host == "github.com"
}
func download(ctx context.Context, client *http.Client, address, base, token string) ([]byte, error) {
	testBase := ""
	if base != "https://api.github.com" {
		testBase = base
	}
	for redirects := 0; redirects <= 5; redirects++ {
		if !allowedDownload(address, testBase) {
			return nil, fmt.Errorf("download destination is not an allowed GitHub HTTPS origin")
		}
		resp, err := request(ctx, client, address, token)
		if err != nil {
			return nil, err
		}
		// Never forward the API credential to a signed download URL.
		token = ""
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			next, locationErr := resp.Location()
			closeErr := resp.Body.Close()
			if err := errors.Join(locationErr, closeErr); err != nil {
				return nil, err
			}
			address = next.String()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			closeErr := resp.Body.Close()
			return nil, errors.Join(fmt.Errorf("artifact download returned HTTP %d", resp.StatusCode), closeErr)
		}
		data, readErr := limited(resp.Body, MaxArchiveBytes)
		return data, errors.Join(readErr, resp.Body.Close())
	}
	return nil, fmt.Errorf("too many artifact redirects")
}
func safePath(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\:") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}
func inspect(data []byte) (*zip.Reader, error) {
	if int64(len(data)) > MaxArchiveBytes {
		return nil, fmt.Errorf("archive exceeds compressed limit")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	if len(z.File) > MaxEntries {
		return nil, fmt.Errorf("archive entry limit exceeded")
	}
	seen := map[string]bool{}
	var total uint64
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !safePath(name) || seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("unsafe or duplicate archive path %q", f.Name)
		}
		seen[strings.ToLower(name)] = true
		if f.Mode()&os.ModeSymlink != 0 || (!f.Mode().IsRegular() && !f.FileInfo().IsDir()) {
			return nil, fmt.Errorf("nonregular archive entry %q", f.Name)
		}
		if f.UncompressedSize64 > uint64(MaxEntryBytes) || f.UncompressedSize64 > uint64(MaxExtractedBytes)-total {
			return nil, fmt.Errorf("archive uncompressed size limit exceeded")
		}
		total += f.UncompressedSize64
	}
	return z, nil
}
func metadata(data []byte) (Metadata, error) {
	var meta Metadata
	z, err := inspect(data)
	if err != nil {
		return meta, err
	}
	for _, f := range z.File {
		if f.Name == "metadata.json" {
			r, err := f.Open()
			if err != nil {
				return meta, err
			}
			b, readErr := limited(r, 1<<20)
			if err := errors.Join(readErr, r.Close()); err != nil {
				return meta, err
			}
			if err := json.Unmarshal(b, &meta); err != nil {
				return meta, err
			}
			return meta, nil
		}
	}
	return meta, fmt.Errorf("artifact missing metadata.json")
}
func extract(data []byte, output string) (resultErr error) {
	z, err := inspect(data)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(output), ".coverage-artifact-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temp)) }()
	var written int64
	for _, f := range z.File {
		target := filepath.Join(temp, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		b, readErr := limited(r, MaxEntryBytes)
		if err := errors.Join(readErr, r.Close()); err != nil {
			return err
		}
		written += int64(len(b))
		if written > MaxExtractedBytes {
			return fmt.Errorf("extracted bytes exceed limit")
		}
		if err := os.WriteFile(target, b, 0600); err != nil {
			return err
		}
	}
	return os.Rename(temp, output)
}
