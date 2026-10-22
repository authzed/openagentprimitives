package coverage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pureReturnPolicy() Policy {
	p := DefaultPolicy()
	p.ExcludePureErrorReturns = true
	p.Text = "Exclude only pure error-propagation returns."
	return p
}

func fixture(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module synthetic.test/unit\n\ngo 1.26\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unit.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}
func TestUnionAndPureErrorReturn(t *testing.T) {
	dir := fixture(t, "package unit\nfunc f(err error) error {\n if err != nil {\n  return err\n }\n return nil\n}\n")
	profile := "mode: atomic\nsynthetic.test/unit/unit.go:2.25,3.16 1 1\nsynthetic.test/unit/unit.go:3.16,5.3 1 0\nsynthetic.test/unit/unit.go:6.2,6.12 1 0\nmode: atomic\nsynthetic.test/unit/unit.go:2.25,3.16 1 0\n"
	got, err := Analyze(strings.NewReader(profile), dir, pureReturnPolicy(), Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw.Covered != 1 || got.Raw.Total != 3 || got.Functional.Covered != 1 || got.Functional.Total != 2 || len(got.Exclusions) != 1 {
		t.Fatalf("unexpected report: %+v", got)
	}
}

func TestPureRulesAndMeaningfulWork(t *testing.T) {
	for _, tc := range []struct {
		body    string
		exclude bool
	}{
		{"return err", true}, {"return nil, err", true}, {"return 0, false, \"\", err", true},
		{"return wrap(err)", false}, {"log(err); return err", false}, {"cleanup(); return err", false},
		{"return cached, err", false}, {"return 1, err", false}, {"return nil", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			src := "package unit\nfunc f(err error) {\n if err != nil { " + tc.body + " }\n}\n"
			dir := fixture(t, src)
			// The whole guard body is one instrumented block, including its braces.
			line := strings.Split(src, "\n")[2]
			profile := fmt.Sprintf("mode: set\nunit.go:3.16,3.%d 1 0\n", len(line)+1)
			got, err := Analyze(strings.NewReader(profile), dir, pureReturnPolicy(), Manifest{})
			if err != nil {
				t.Fatal(err)
			}
			if (len(got.Exclusions) == 1) != tc.exclude {
				t.Fatalf("exclusions=%+v", got.Exclusions)
			}
		})
	}
}
func TestRejectMalformedProfiles(t *testing.T) {
	dir := fixture(t, "package unit\nfunc f() {}\n")
	for _, profile := range []string{
		"mode: nope\n", "mode: set\n../unit.go:2.1,2.5 1 0\n", "mode: set\nabsent.go:2.1,2.5 1 0\n",
		"mode: set\nunit.go:2.1,2.500 1 0\n", "mode: set\nunit.go:2.1,2.5 1 -1\n",
		"mode: set\nunit.go:2.1,2.5 1 0\nunit.go:2.1,2.5 2 0\n", "mode: set\nmode: count\n",
	} {
		if _, err := Analyze(strings.NewReader(profile), dir, pureReturnPolicy(), Manifest{}); err == nil {
			t.Errorf("accepted %q", profile)
		}
	}
}
func TestExplicitExclusionAndStaleDigest(t *testing.T) {
	src := "package unit\nfunc f() {}\n"
	dir := fixture(t, src)
	e := Exclusion{File: "unit.go", SourceDigest: digest([]byte(src)), StartLine: 2, StartColumn: 1, EndLine: 2, EndColumn: 12, Statements: 1, Reason: "reviewed policy"}
	p := "mode: count\nunit.go:2.1,2.12 1 8\n"
	m := Manifest{SchemaVersion: "v1", Exclusions: []Exclusion{e}}
	got, err := Analyze(strings.NewReader(p), dir, pureReturnPolicy(), m)
	if err != nil {
		t.Fatal(err)
	}
	if got.Functional.Total != 0 || got.Functional.Percent != nil || got.Excluded.Covered != 1 {
		t.Fatalf("unexpected %+v", got)
	}
	m.Exclusions[0].SourceDigest = "sha256:stale"
	if _, err := Analyze(strings.NewReader(p), dir, pureReturnPolicy(), m); err == nil {
		t.Fatal("accepted stale source")
	}
}
func TestComparisonAndPolicyIdentity(t *testing.T) {
	base := Report{SchemaVersion: "v1", AnalyzerVersion: AnalyzerVersion, PolicyHash: "same", Raw: metric(9, 10), Functional: metric(9, 10)}
	current := base
	current.Functional = metric(8, 10)
	got, err := Compare(current, &base, "functional", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compatible || !got.Regression || got.DropPP == nil || *got.DropPP != 10 {
		t.Fatalf("%+v", got)
	}
	current.PolicyHash = "changed"
	got, err = Compare(current, &base, "functional", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Compatible || got.Regression || got.DropPP != nil {
		t.Fatalf("incompatible baseline compared: %+v", got)
	}
}
func TestClassPolicyCanExcludeWholeMultiStatementBlock(t *testing.T) {
	src := "package unit\nfunc f() { println(1); println(2) }\n"
	dir := fixture(t, src)
	e := Exclusion{File: "unit.go", SourceDigest: digest([]byte(src)), StartLine: 2, StartColumn: 10, EndLine: 2, EndColumn: 34, Statements: 2, Reason: "Class policy excludes all diagnostic output in this block"}
	policy := Policy{SchemaVersion: "v1", Text: "Exclude diagnostic output; retain all other statements."}
	got, err := Analyze(strings.NewReader("mode: set\nunit.go:2.10,2.34 2 0\n"), dir, policy, Manifest{SchemaVersion: "v1", Exclusions: []Exclusion{e}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw.Total != 2 || got.Functional.Total != 0 || got.Excluded.Total != 2 || got.Policy.Text != policy.Text {
		t.Fatalf("class policy not applied: %+v", got)
	}
	retained, err := Analyze(strings.NewReader("mode: set\nunit.go:2.10,2.34 2 0\n"), dir, Policy{SchemaVersion: "v1", Text: "Retain every statement."}, Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if retained.Raw.Total != got.Raw.Total || retained.Functional.Total != 2 || retained.PolicyHash == got.PolicyHash {
		t.Fatalf("different class definitions were conflated: %+v %+v", retained, got)
	}
	e.Statements = 1
	if _, err := Analyze(strings.NewReader("mode: set\nunit.go:2.10,2.34 2 0\n"), dir, policy, Manifest{SchemaVersion: "v1", Exclusions: []Exclusion{e}}); err == nil {
		t.Fatal("accepted partial block exclusion")
	}
}

func TestDefaultPolicyRetainsAllStatements(t *testing.T) {
	dir := fixture(t, "package unit\nfunc f(err error) error {\n if err != nil { return err }\n return nil\n}\n")
	got, err := Analyze(strings.NewReader("mode: count\nunit.go:3.16,3.30 1 0\n"), dir, DefaultPolicy(), Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Exclusions) != 0 || got.Functional.Total != got.Raw.Total {
		t.Fatalf("tool imposed its own functional definition: %+v", got)
	}
}

func TestCoveredAndUncoveredPolicyIdentical(t *testing.T) {
	dir := fixture(t, "package unit\nfunc f(err error) error {\n if err != nil { return err }\n return nil\n}\n")
	a, err := Analyze(strings.NewReader("mode: count\nunit.go:3.16,3.30 1 0\n"), dir, pureReturnPolicy(), Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Analyze(strings.NewReader("mode: count\nunit.go:3.16,3.30 1 7\n"), dir, pureReturnPolicy(), Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Exclusions) != 1 || len(b.Exclusions) != 1 || a.PolicyHash != b.PolicyHash || a.Excluded.Covered != 0 || b.Excluded.Covered != 1 || a.Functional.Total != 0 || b.Functional.Total != 0 {
		t.Fatalf("%+v %+v", a, b)
	}
}
func TestComparisonThresholdBoundary(t *testing.T) {
	b := Report{SchemaVersion: "v1", AnalyzerVersion: AnalyzerVersion, PolicyHash: "same", Functional: metric(9, 10)}
	c := b
	c.Functional = metric(8, 10)
	got, err := Compare(c, &b, "functional", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Regression {
		t.Fatal("positive drop equal to threshold should regress")
	}
	c = b
	got, err = Compare(c, &b, "functional", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Regression {
		t.Fatal("no drop is not a regression")
	}
}
func TestRejectConflictingBlockExtents(t *testing.T) {
	dir := fixture(t, "package unit\nfunc f() { println(1) }\n")
	_, err := Analyze(strings.NewReader("mode: count\nunit.go:2.10,2.23 1 1\nunit.go:2.10,2.22 1 1\n"), dir, pureReturnPolicy(), Manifest{})
	if err == nil {
		t.Fatal("conflicting block extents can double count the same statements")
	}
}
