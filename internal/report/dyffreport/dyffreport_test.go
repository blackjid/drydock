package dyffreport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gonvenience/bunt"
	"github.com/gonvenience/term"
	"github.com/sholdee/drydock/internal/diff"
)

func configMapDocument(name, value string) diff.Document {
	return diff.Document{
		Parent:   diff.Parent{Name: "demo"},
		Resource: diff.Resource{Kind: "ConfigMap", Namespace: "default", Name: name},
		Body: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name +
			"\n  namespace: default\ndata:\n  key: " + value + "\n",
	}
}

func runDiff(t *testing.T, left, right []diff.Document) []diff.Result {
	t.Helper()
	results, err := diff.Run(left, right, diff.Options{Unified: 3})
	if err != nil {
		t.Fatalf("diff.Run() error = %v", err)
	}
	return results
}

func render(t *testing.T, results []diff.Result, color bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := Write(&out, results, color); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return out.String()
}

func TestWriteModified(t *testing.T) {
	results := runDiff(t,
		[]diff.Document{configMapDocument("cm", "old")},
		[]diff.Document{configMapDocument("cm", "new")})

	got := render(t, results, false)

	for _, want := range []string{results[0].Header(), "data.key", "old", "new"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("uncolored output contains ANSI escapes:\n%q", got)
	}
	if strings.Contains(got, "between") {
		t.Errorf("output contains the dyff banner:\n%s", got)
	}
}

func TestWriteColor(t *testing.T) {
	results := runDiff(t,
		[]diff.Document{configMapDocument("cm", "old")},
		[]diff.Document{configMapDocument("cm", "new")})

	if got := render(t, results, true); !strings.Contains(got, "\x1b[") {
		t.Errorf("colored output has no ANSI escapes:\n%q", got)
	}
}

func TestWriteAddedAndRemoved(t *testing.T) {
	added := runDiff(t, nil, []diff.Document{configMapDocument("fresh", "v")})
	if got := render(t, added, false); !strings.Contains(got, "one document added") {
		t.Errorf("added output missing document-added report:\n%s", got)
	}
	removed := runDiff(t, []diff.Document{configMapDocument("gone", "v")}, nil)
	if got := render(t, removed, false); !strings.Contains(got, "one document removed") {
		t.Errorf("removed output missing document-removed report:\n%s", got)
	}
}

func TestWriteAddedWithoutMetadataName(t *testing.T) {
	doc := diff.Document{
		Parent:   diff.Parent{Name: "demo"},
		Resource: diff.Resource{Kind: "Job", Namespace: "default"},
		Body:     "apiVersion: batch/v1\nkind: Job\nmetadata:\n  generateName: run-\nspec:\n  parallelism: 1\n",
	}
	got := render(t, runDiff(t, nil, []diff.Document{doc}), false)
	if !strings.Contains(got, "run-") {
		t.Errorf("output missing generateName resource:\n%s", got)
	}
}

func TestWriteKeepsSecretValuesRedacted(t *testing.T) {
	secret := func(value string) diff.Document {
		return diff.Document{
			Parent:   diff.Parent{Name: "demo"},
			Resource: diff.Resource{Kind: "Secret", Namespace: "default", Name: "creds"},
			Body: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\n  namespace: default\n" +
				"stringData:\n  password: " + value + "\n",
		}
	}
	results := runDiff(t, []diff.Document{secret("hunter2-old")}, []diff.Document{secret("hunter2-new")})

	got := render(t, results, false)

	if strings.Contains(got, "hunter2") {
		t.Fatalf("output leaks Secret values:\n%s", got)
	}
	if !strings.Contains(got, "redacted") {
		t.Errorf("output missing redaction markers:\n%s", got)
	}
}

func TestWriteNoResults(t *testing.T) {
	if got := render(t, nil, false); got != "" {
		t.Errorf("output = %q, want empty", got)
	}
}

func TestWriteKeepsNumberLikeStringChanges(t *testing.T) {
	for _, change := range [][2]string{
		{`"1.0"`, `"1"`},
		{`"1e3"`, `"1000"`},
		{`"12345678901234567890"`, `"12345678901234567891"`},
	} {
		results := runDiff(t,
			[]diff.Document{configMapDocument("cm", change[0])},
			[]diff.Document{configMapDocument("cm", change[1])})

		got := render(t, results, false)

		if !strings.Contains(got, "data.key") {
			t.Errorf("%s -> %s: output hides the value change:\n%s", change[0], change[1], got)
		}
	}
}

func TestWriteFallsBackToUnifiedDiffWithoutStructuralChanges(t *testing.T) {
	// A Before/After pair dyff sees as identical while the unified diff is not.
	result := diff.Result{
		Parent:   diff.Parent{Name: "demo"},
		Resource: diff.Resource{Kind: "ConfigMap", Namespace: "default", Name: "cm"},
		Change:   diff.ChangeModified,
		Diff:     "--- x\n+++ x\n@@ -1 +1 @@\n-# old comment\n+# new comment\n",
		Before:   "# old comment\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n",
		After:    "# new comment\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n",
	}

	got := render(t, []diff.Result{result}, false)

	for _, want := range []string{"showing unified diff", "+# new comment"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteFallsBackOnUnparsableBodyAndContinues(t *testing.T) {
	broken := diff.Result{
		Parent:   diff.Parent{Name: "demo"},
		Resource: diff.Resource{Kind: "ConfigMap", Namespace: "default", Name: "broken"},
		Change:   diff.ChangeModified,
		Diff:     "-a\n+b\n",
		Before:   "key: [unterminated\n",
		After:    "key: [unterminated2\n",
	}
	good := runDiff(t,
		[]diff.Document{configMapDocument("cm", "old")},
		[]diff.Document{configMapDocument("cm", "new")})

	got := render(t, append([]diff.Result{broken}, good...), false)

	if !strings.Contains(got, "showing unified diff") || !strings.Contains(got, "data.key") {
		t.Errorf("want fallback for the broken resource and a dyff report for the next:\n%s", got)
	}
}

func TestWriteResetsColorSettings(t *testing.T) {
	results := runDiff(t,
		[]diff.Document{configMapDocument("cm", "old")},
		[]diff.Document{configMapDocument("cm", "new")})
	render(t, results, true)

	if got := bunt.ColorSetting.String(); got != "auto" {
		t.Errorf("bunt color setting = %q after Write, want auto", got)
	}
}

func TestGitHubBodyMatchesDyffGitHubStyle(t *testing.T) {
	left := configMapDocument("cm", "old")
	left.Body = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: default\ndata:\n  image: registry.example/app:v1\n"
	right := left
	right.Body = strings.Replace(left.Body, "app:v1", "app:v2", 1)
	results := runDiff(t, []diff.Document{left}, []diff.Document{right})

	got := GitHubBody(results[0])

	want := "@@ data.image @@\n" +
		"# v1/ConfigMap/default/cm\n" +
		"! \u00b1 value change\n" +
		"- registry.example/app:v1\n" +
		"+ registry.example/app:v2\n"
	if got != want {
		t.Errorf("GitHubBody() =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("GitHubBody() contains ANSI escapes: %q", got)
	}
}

func TestGitHubBodyAddedAndRemoved(t *testing.T) {
	for name, results := range map[string][]diff.Result{
		"added":   runDiff(t, nil, []diff.Document{configMapDocument("fresh", "v")}),
		"removed": runDiff(t, []diff.Document{configMapDocument("gone", "v")}, nil),
	} {
		got := GitHubBody(results[0])
		if !strings.Contains(got, "@@ ") || !strings.Contains(got, "document "+name) {
			t.Errorf("%s: GitHubBody() missing path or %q report:\n%s", name, "document "+name, got)
		}
		if strings.Contains(got, "showing unified diff") {
			t.Errorf("%s: GitHubBody() fell back to the unified diff:\n%s", name, got)
		}
	}
}

func TestGitHubBodyKeepsSecretValuesRedacted(t *testing.T) {
	secret := func(value string) diff.Document {
		return diff.Document{
			Parent:   diff.Parent{Name: "demo"},
			Resource: diff.Resource{Kind: "Secret", Namespace: "default", Name: "creds"},
			Body: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\n  namespace: default\n" +
				"stringData:\n  password: " + value + "\n",
		}
	}
	results := runDiff(t, []diff.Document{secret("hunter2-old")}, []diff.Document{secret("hunter2-new")})

	if got := GitHubBody(results[0]); strings.Contains(got, "hunter2") {
		t.Fatalf("GitHubBody() leaks Secret values:\n%s", got)
	}
}

func TestGitHubBodyFallsBackToUnifiedDiff(t *testing.T) {
	result := diff.Result{
		Diff:   "--- x\n+++ x\n-a\n+b\n",
		Before: "key: [unterminated\n",
		After:  "key: [unterminated2\n",
	}

	got := GitHubBody(result)

	if !strings.Contains(got, "showing unified diff") || !strings.Contains(got, "+b\n") {
		t.Errorf("GitHubBody() = %q, want unified fallback", got)
	}
}

func TestControlCharactersFallBackToUnifiedDiff(t *testing.T) {
	for name, value := range map[string]string{
		"escape sequence": `"new\x1b[1A\x1b]8;;https://evil\x07"`,
		"carriage return": `"new\rfake"`,
	} {
		results := runDiff(t,
			[]diff.Document{configMapDocument("cm", `"old"`)},
			[]diff.Document{configMapDocument("cm", value)})

		for output, got := range map[string]string{
			"Write":      render(t, results, false),
			"GitHubBody": GitHubBody(results[0]),
		} {
			if strings.ContainsAny(got, "\x1b\x07\r") {
				t.Errorf("%s/%s: output contains raw control characters: %q", name, output, got)
			}
			if !strings.Contains(got, "values contain control characters") {
				t.Errorf("%s/%s: output did not fall back to the unified diff:\n%s", name, output, got)
			}
		}
	}
}

func TestMultilineValuesAndTabsAreNotControlCharacters(t *testing.T) {
	results := runDiff(t,
		[]diff.Document{configMapDocument("cm", `"a\nb\tc"`)},
		[]diff.Document{configMapDocument("cm", `"a\nb\tchanged"`)})

	if got := GitHubBody(results[0]); strings.Contains(got, "showing unified diff") {
		t.Errorf("GitHubBody() fell back for multi-line value:\n%s", got)
	}
}

func TestMultilineKeysFallBackToUnifiedDiff(t *testing.T) {
	doc := configMapDocument("cm", "old")
	forged := configMapDocument("cm", "old")
	forged.Body = strings.Replace(doc.Body, "  key: old", "  \"k\\n@@ forged.path @@\": old", 1)
	results := runDiff(t, []diff.Document{doc}, []diff.Document{forged})

	got := GitHubBody(results[0])

	if strings.Contains(got, "\n@@ forged.path @@") || !strings.Contains(got, "showing unified diff") {
		t.Errorf("GitHubBody() let a multi-line key forge a path line:\n%s", got)
	}
}

func TestGitHubBodyAddedResourceHasIdentityLine(t *testing.T) {
	got := GitHubBody(runDiff(t, nil, []diff.Document{configMapDocument("fresh", "v")})[0])

	if !strings.Contains(got, "# v1/ConfigMap/default/fresh") {
		t.Errorf("GitHubBody() missing identity line for added resource:\n%s", got)
	}
}

func TestGitHubBodyIgnoresTerminalSize(t *testing.T) {
	body := func(items ...string) diff.Document {
		return diff.Document{
			Parent:   diff.Parent{Name: "demo"},
			Resource: diff.Resource{Kind: "ConfigMap", Namespace: "default", Name: "cm"},
			Body: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: default\nitems:\n  - " +
				strings.Join(items, "\n  - ") + "\n",
		}
	}
	results := runDiff(t, []diff.Document{body("alpha", "bravo", "charlie")}, []diff.Document{body("charlie", "bravo", "alpha")})

	term.FixedTerminalWidth, term.FixedTerminalHeight = 20, 5
	t.Cleanup(func() { term.FixedTerminalWidth, term.FixedTerminalHeight = -1, -1 })
	got := GitHubBody(results[0])

	if !strings.Contains(got, "- alpha, bravo, charlie\n+ charlie, bravo, alpha\n") {
		t.Errorf("GitHubBody() order change depends on the terminal size:\n%s", got)
	}
	if term.FixedTerminalWidth != 20 || term.FixedTerminalHeight != 5 {
		t.Errorf("terminal size = %dx%d after GitHubBody, want the previous 20x5",
			term.FixedTerminalWidth, term.FixedTerminalHeight)
	}
}

func TestGitHubBodyResetsColorSettings(t *testing.T) {
	results := runDiff(t,
		[]diff.Document{configMapDocument("cm", "old")},
		[]diff.Document{configMapDocument("cm", "new")})
	GitHubBody(results[0])

	if got := bunt.ColorSetting.String(); got != "auto" {
		t.Errorf("bunt color setting = %q after GitHubBody, want auto", got)
	}
}
