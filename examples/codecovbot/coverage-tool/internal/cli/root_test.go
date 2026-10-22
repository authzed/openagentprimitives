package cli

import (
	"bytes"
	"encoding/json"
	"github.com/authzed/openagentprimitives/examples/codecovbot/coverage-tool/internal/coverage"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeOffline(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module synthetic.test/unit\n", "unit.go": "package unit\nfunc f() {}\n", "coverage.out": "mode: set\nsynthetic.test/unit/unit.go:2.10,2.12 0 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	cmd := NewCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"analyze", "--profile", filepath.Join(dir, "coverage.out"), "--source", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"schema_version": "v1"`) {
		t.Fatalf("output=%s", out.String())
	}
}

func TestAnalyzeInlinePolicyCanRetainPureErrorReturns(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":       "module synthetic.test/unit\n",
		"unit.go":      "package unit\nfunc f(err error) error {\n if err != nil {\n  return err\n }\n return nil\n}\n",
		"coverage.out": "mode: atomic\nsynthetic.test/unit/unit.go:2.25,3.16 1 1\nsynthetic.test/unit/unit.go:3.16,5.3 1 0\nsynthetic.test/unit/unit.go:6.2,6.12 1 0\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		rule, policy string
		total        int64
	}{
		{"false", "Retain error returns; exclude nothing.", 3},
		{"true", "Exclude pure error propagation.", 2},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			var out bytes.Buffer
			cmd := NewCmd()
			cmd.SetOut(&out)
			policy := `{"schema_version":"v1","exclude_pure_error_returns":` + tc.rule + `,"text":"` + tc.policy + `"}`
			cmd.SetArgs([]string{"analyze", "--profile", filepath.Join(dir, "coverage.out"), "--source", dir, "--policy-json", policy})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var report coverage.Report
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Functional.Total != tc.total || report.Policy.Text != tc.policy {
				t.Fatalf("policy not applied: %+v", report)
			}
		})
	}
}

func TestDescribeBlocksSupportsCoverageBlindClassPolicy(t *testing.T) {
	dir := t.TempDir()
	src := "package unit\nfunc f() { println(1); println(2) }\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module synthetic.test/unit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unit.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, hits := range []string{"0", "9"} {
		profile := filepath.Join(dir, "coverage.out")
		if err := os.WriteFile(profile, []byte("mode: count\nunit.go:2.10,2.34 2 "+hits+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		cmd := NewCmd()
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"describe-blocks", "--profile", profile, "--source", dir, "--file", "unit.go", "--limit", "1"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Blocks []coverage.Exclusion `json:"blocks"`
		}
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Blocks) != 1 || response.Blocks[0].Statements != 2 || !strings.HasPrefix(response.Blocks[0].SourceDigest, "sha256:") {
			t.Fatalf("missing source-bound candidate: %s", out.String())
		}
		if strings.Contains(out.String(), "covered") || strings.Contains(out.String(), "hit") {
			t.Fatalf("revealed coverage to classifier: %s", out.String())
		}
		if previous != "" && previous != out.String() {
			t.Fatal("classification inputs changed with hit count")
		}
		previous = out.String()
	}
}
