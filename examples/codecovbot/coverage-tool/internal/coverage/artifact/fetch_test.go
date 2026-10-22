package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for n, v := range files {
		f, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestFetchValidatesProviderAndExtracts(t *testing.T) {
	metadata := `{"schema_version":"v1","repository":"owner/unit","run_id":17,"run_attempt":2,"head_sha":"abc","profile":"coverage.out","source":"source"}`
	zipData := archive(t, map[string]string{"metadata.json": metadata, "coverage.out": "mode: set\n", "source/go.mod": "module synthetic.test/unit\n"})
	var leaked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/unit/actions/runs/17/attempts/2":
			fmt.Fprint(w, `{"id":17,"run_attempt":2,"head_sha":"abc","status":"completed","conclusion":"success","repository":{"full_name":"owner/unit"}}`)
		case "/repos/owner/unit/actions/runs/17/artifacts":
			fmt.Fprint(w, `{"total_count":1,"artifacts":[{"id":19,"name":"coverage-report","expired":false,"workflow_run":{"id":17,"head_sha":"abc"}}]}`)
		case "/repos/owner/unit/actions/artifacts/19/zip":
			http.Redirect(w, r, "/download", http.StatusFound)
		case "/download":
			leaked = r.Header.Get("Authorization") != ""
			_, _ = w.Write(zipData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "fetched")
	got, err := (Fetcher{APIBaseURL: srv.URL, Client: srv.Client()}).Fetch(context.Background(), Options{Repository: "owner/unit", RunID: 17, Attempt: 2, ArtifactName: "coverage-report", Output: out, ExpectedSHA: "abc", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("API token forwarded to download redirect")
	}
	if got.ArtifactID != 19 || got.Output != out {
		t.Fatalf("%+v", got)
	}
	b, err := os.ReadFile(filepath.Join(out, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
}
func TestArchiveSafety(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "source/../../escape", "source\\escape", "C:/escape"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "out")
			if err := extract(archive(t, map[string]string{name: "bad"}), dir); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("partial output remains")
			}
		})
	}
}
func TestRejectUntrustedRedirect(t *testing.T) {
	if allowedDownload("https://attacker.invalid/archive", "") {
		t.Fatal("untrusted download permitted")
	}
	if allowedDownload("http://api.github.com/archive", "") {
		t.Fatal("HTTP permitted")
	}
	if !allowedDownload("https://production.blob.core.windows.net/archive", "") {
		t.Fatal("provider signed download denied")
	}
	if allowedDownload("https://blob.core.windows.net.attacker.invalid/archive", "") {
		t.Fatal("suffix spoof permitted")
	}
}

func TestDuplicateAndSymlinkArchiveRejected(t *testing.T) {
	for _, kind := range []string{"duplicate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			var b bytes.Buffer
			w := zip.NewWriter(&b)
			if kind == "duplicate" {
				for i := 0; i < 2; i++ {
					f, err := w.Create("same")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.Write([]byte("value")); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				h := &zip.FileHeader{Name: "link"}
				h.SetMode(os.ModeSymlink | 0777)
				f, err := w.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.Write([]byte("../escape")); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := inspect(b.Bytes()); err == nil {
				t.Fatal("unsafe zip accepted")
			}
		})
	}
}
func TestArchiveLimitsAndExistingOutput(t *testing.T) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i := 0; i <= MaxEntries; i++ {
		if _, err := w.Create(fmt.Sprintf("item%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect(b.Bytes()); err == nil {
		t.Fatal("entry cap ignored")
	}
	dir := t.TempDir()
	if err := extract(archive(t, map[string]string{"ok": "value"}), dir); err == nil {
		t.Fatal("existing output overwritten")
	}
}
func TestRejectCaseAliasingArchive(t *testing.T) {
	data := archive(t, map[string]string{"metadata.json": "verified", "Metadata.json": "different"})
	if _, err := inspect(data); err == nil {
		t.Fatal("case-aliasing names can overwrite verified metadata on case-insensitive filesystems")
	}
}
func TestRejectFailedProducerBeforeArtifactDownload(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/owner/unit/actions/runs/17/attempts/2" {
			t.Error("must refuse failed producer before listing or downloading artifacts")
		}
		fmt.Fprint(w, `{"id":17,"run_attempt":2,"head_sha":"abc","status":"completed","conclusion":"failure","repository":{"full_name":"owner/unit"}}`)
	}))
	defer srv.Close()
	_, err := (Fetcher{APIBaseURL: srv.URL, Client: srv.Client()}).Fetch(context.Background(), Options{Repository: "owner/unit", RunID: 17, Attempt: 2, ArtifactName: "coverage-report", Output: filepath.Join(t.TempDir(), "out"), Token: "secret"})
	if err == nil {
		t.Fatal("failed producer must be unavailable")
	}
	if calls != 1 {
		t.Fatalf("made %d provider calls for failed producer", calls)
	}
}
func TestFetchKeepsPRHeadSeparateFromProviderMerge(t *testing.T) {
	data := archive(t, map[string]string{"metadata.json": `{"schema_version":"v1","repository":"owner/unit","run_id":17,"run_attempt":2,"head_sha":"source","tested_sha":"merge","base_sha":"base","profile":"coverage.out","source":"source"}`, "coverage.out": "mode: set\n", "source/go.mod": "module synthetic.test/unit\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/unit/actions/runs/17/attempts/2":
			fmt.Fprint(w, `{"id":17,"run_attempt":2,"head_sha":"merge","status":"completed","conclusion":"success","repository":{"full_name":"owner/unit"},"pull_requests":[{"number":7,"head":{"sha":"source"},"base":{"sha":"base"}}]}`)
		case "/repos/owner/unit/actions/runs/17/artifacts":
			fmt.Fprint(w, `{"total_count":1,"artifacts":[{"id":19,"name":"coverage-report","workflow_run":{"id":17,"head_sha":"merge"}}]}`)
		case "/repos/owner/unit/actions/artifacts/19/zip":
			_, err := w.Write(data)
			if err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	result, err := (Fetcher{APIBaseURL: srv.URL, Client: srv.Client()}).Fetch(context.Background(), Options{Repository: "owner/unit", RunID: 17, Attempt: 2, ArtifactName: "coverage-report", Output: filepath.Join(t.TempDir(), "out"), ExpectedSHA: "source", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if result.HeadSHA != "source" || result.Metadata.TestedSHA != "merge" {
		t.Fatalf("conflated source and merge: %+v", result)
	}
}
func TestFetchVerifiesDistinctCheckoutMergeParents(t *testing.T) {
	mergeSHA := "cccccccccccccccccccccccccccccccccccccccc"
	metadataJSON := fmt.Sprintf(`{"schema_version":"v1","repository":"owner/unit","run_id":17,"run_attempt":2,"head_sha":"source","tested_sha":%q,"base_sha":"base","profile":"coverage.out","source":"source"}`, mergeSHA)
	data := archive(t, map[string]string{"metadata.json": metadataJSON, "coverage.out": "mode: set\n", "source/go.mod": "module synthetic.test/unit\n"})
	checked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/unit/actions/runs/17/attempts/2":
			fmt.Fprint(w, `{"id":17,"run_attempt":2,"head_sha":"source","status":"completed","conclusion":"success","repository":{"full_name":"owner/unit"},"pull_requests":[{"number":7,"head":{"sha":"source"},"base":{"sha":"base"}}]}`)
		case "/repos/owner/unit/actions/runs/17/artifacts":
			fmt.Fprint(w, `{"total_count":1,"artifacts":[{"id":19,"name":"coverage-report","workflow_run":{"id":17,"head_sha":"source"}}]}`)
		case "/repos/owner/unit/actions/artifacts/19/zip":
			if _, err := w.Write(data); err != nil {
				t.Error(err)
			}
		case "/repos/owner/unit/commits/" + mergeSHA:
			checked = true
			fmt.Fprintf(w, `{"sha":%q,"parents":[{"sha":"base"},{"sha":"source"}]}`, mergeSHA)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_, err := (Fetcher{APIBaseURL: srv.URL, Client: srv.Client()}).Fetch(context.Background(), Options{Repository: "owner/unit", RunID: 17, Attempt: 2, ArtifactName: "coverage-report", Output: filepath.Join(t.TempDir(), "out"), ExpectedSHA: "source", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("artifact checkout SHA differing from run SHA was trusted without authenticated merge-parent evidence")
	}
}
