package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	sourcepkg "github.com/sholdee/drydock/internal/source"
)

// writeKustomizeOverlays writes two Applications pointing at sibling
// overlays that share workloads/demo/base, outside either Application's
// spec.source.path. An unrelated Application proves selection stays narrow.
func writeKustomizeOverlays(t *testing.T, root string) {
	t.Helper()
	for _, env := range []string{"staging", "production"} {
		writeTestFile(t, filepath.Join(root, "apps", "demo-"+env+".yaml"), `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: demo-`+env+`
  namespace: argocd
spec:
  source:
    repoURL: https://github.com/example/repo
    targetRevision: main
    path: workloads/demo/overlays/`+env+`
  destination:
    name: in-cluster
    namespace: demo-`+env+`
`)
		writeTestFile(t, filepath.Join(root, "workloads", "demo", "overlays", env, "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: demo-`+env+`
resources:
  - ../../base
`)
	}
	writeDiffApplication(t, root, "other", "other", "same")
}

// writeKustomizePlainBaseApps writes a plain base: the edited file is an
// ordinary resource under the base, no Helm involved.
func writeKustomizePlainBaseApps(t *testing.T, root, value string) {
	t.Helper()
	writeKustomizeOverlays(t, root)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "configmap.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  value: `+value+`
`)
}

// writeKustomizeOverlayApps writes the overlay -> base -> helmCharts.valuesFile
// layout: the Helm values live under the shared base.
func writeKustomizeOverlayApps(t *testing.T, root, value string) {
	t.Helper()
	writeKustomizeOverlays(t, root)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - helm-release
`)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "helm-release", "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
helmCharts:
  - name: demo
    releaseName: demo
    valuesFile: values.yaml
`)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "helm-release", "charts", "demo", "Chart.yaml"), `apiVersion: v2
name: demo
version: 0.1.0
`)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "helm-release", "charts", "demo", "templates", "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  value: {{ .Values.value | quote }}
`)
	writeTestFile(t, filepath.Join(root, "workloads", "demo", "base", "helm-release", "values.yaml"), "value: "+value+"\n")
}

func TestOrchestratorDiffAppsStrictChangedOnlyOwnsKustomizeBase(t *testing.T) {
	for name, write := range map[string]func(*testing.T, string, string){
		"plain base resource":    writeKustomizePlainBaseApps,
		"base helmCharts values": writeKustomizeOverlayApps,
	} {
		t.Run(name, func(t *testing.T) {
			assertStrictChangedOnlySelectsBothOverlays(t, write)
		})
	}
}

func assertStrictChangedOnlySelectsBothOverlays(t *testing.T, write func(*testing.T, string, string)) {
	t.Helper()
	root := t.TempDir()
	left := filepath.Join(root, "left")
	right := filepath.Join(root, "right")
	write(t, left, "old")
	write(t, right, "new")

	result, err := Orchestrator{}.DiffApps(context.Background(), DiffRequest{
		LeftPath:          left,
		RightPath:         right,
		ChangedOnly:       true,
		StrictChangedOnly: true,
		Unified:           3,
	})
	if err != nil {
		t.Fatalf("DiffApps() error = %v, diagnostics = %#v", err, result.Diagnostics)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("Diagnostics = %#v, want none", result.Diagnostics)
	}
	// Both overlays share the base: neither is collapsed into the other.
	if len(result.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2 (staging and production)", len(result.Results))
	}
	got := make([]string, 0, len(result.Results))
	for _, diffResult := range result.Results {
		got = append(got, diffResult.Diff)
		if strings.Contains(diffResult.Diff, "argocd/other") {
			t.Fatalf("diff included unrelated Application:\n%s", diffResult.Diff)
		}
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"argocd/demo-staging", "argocd/demo-production", "-  value: old", "+  value: new"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("diffs missing %q:\n%s", want, joined)
		}
	}
}

func TestOrchestratorDiffAppsChangedOnlyKustomizeGraphKeepsUnownedFallback(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "left")
	right := filepath.Join(root, "right")
	writeKustomizeOverlayApps(t, left, "old")
	writeKustomizeOverlayApps(t, right, "new")
	writeTestFile(t, filepath.Join(left, "README.md"), "left\n")
	writeTestFile(t, filepath.Join(right, "README.md"), "right\n")

	result, err := Orchestrator{}.DiffApps(context.Background(), DiffRequest{
		LeftPath:    left,
		RightPath:   right,
		ChangedOnly: true,
		Unified:     3,
	})
	if err != nil {
		t.Fatalf("DiffApps() error = %v", err)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Category != "changed-only" || !strings.Contains(result.Diagnostics[0].Message, "README.md") {
		t.Fatalf("Diagnostics = %#v, want one changed-only warning naming only README.md", result.Diagnostics)
	}
	if strings.Contains(result.Diagnostics[0].Message, "values.yaml") {
		t.Fatalf("diagnostic %q reports the owned values file as unowned", result.Diagnostics[0].Message)
	}
}

func TestWithKustomizeSelectionPathsOnlyWalksLocallyRenderedSources(t *testing.T) {
	root := t.TempDir()
	writeKustomizeOverlayApps(t, root, "v")
	const overlay = "workloads/demo/overlays/staging"
	const values = "workloads/demo/base/helm-release/values.yaml"

	kustomizeApp := func(name string, sources ...argoappv1.ApplicationSource) ApplicationSelectionInput {
		app := argoappv1.Application{Name: name, Spec: argoappv1.ApplicationSpec{Sources: sources}}
		return ApplicationSelectionInput{Application: app, Paths: []string{"apps/" + name + ".yaml"}}
	}
	inputs := []ApplicationSelectionInput{
		kustomizeApp("local", argoappv1.ApplicationSource{RepoURL: "https://github.com/example/repo", Path: overlay}),
		// Multi-source: the local Kustomize source still contributes.
		kustomizeApp("multi",
			argoappv1.ApplicationSource{RepoURL: "https://charts.example.com", Chart: "demo", TargetRevision: "1.0.0"},
			argoappv1.ApplicationSource{RepoURL: "https://github.com/example/repo", Path: overlay},
		),
		// Split-repo: the path lives in another repository, not this tree.
		kustomizeApp("foreign", argoappv1.ApplicationSource{RepoURL: "https://github.com/example/other", Path: "deploy/overlays/staging"}),
		// Repo-mapped to a different checkout: renders from there, not root.
		kustomizeApp("mapped", argoappv1.ApplicationSource{RepoURL: "https://github.com/example/mapped", Path: overlay}),
		// Explicit non-Kustomize type over a Kustomize directory.
		kustomizeApp("directory", argoappv1.ApplicationSource{RepoURL: "https://github.com/example/repo", Path: overlay, Directory: &argoappv1.ApplicationSourceDirectory{Recurse: true}}),
		kustomizeApp("oci", argoappv1.ApplicationSource{RepoURL: "oci://registry.example.com/demo", Path: overlay}),
	}
	repoMaps := []sourcepkg.RepoMap{{URL: "https://github.com/example/mapped", Path: t.TempDir()}}

	got := withKustomizeSelectionPaths(context.Background(), root, repoMaps, inputs)

	owns := map[string]bool{}
	for _, input := range got {
		owns[input.Application.Name] = slices.Contains(input.Paths, values)
	}
	want := map[string]bool{"local": true, "multi": true, "foreign": false, "mapped": false, "directory": false, "oci": false}
	for name, wantOwns := range want {
		if owns[name] != wantOwns {
			t.Errorf("%s owns %s = %v, want %v", name, values, owns[name], wantOwns)
		}
	}
	for i := range inputs {
		if len(inputs[i].Paths) != 1 {
			t.Fatalf("input %s Paths mutated to %v; listed inputs also key the render cache", inputs[i].Application.Name, inputs[i].Paths)
		}
	}
	selected, unowned := SelectChangedApplicationInputs(got, []string{values, "README.md"})
	if len(selected) != 2 || !slices.Equal(unowned, []string{"README.md"}) {
		t.Fatalf("selected = %d apps, unowned = %v; want local+multi selected and only README.md unowned", len(selected), unowned)
	}
}

// TestSelectChangedDiffSidesUnionsApplicationsAcrossSides pins cross-side
// selection. With --load-restrictor LoadRestrictionsNone an overlay can
// reference a file outside any directory ref; when the change deletes that
// file but keeps the reference, only the left tree's graph owns it. The right
// side must still render the Application so the broken render surfaces
// instead of an all-resources-deleted diff.
func TestSelectChangedDiffSidesUnionsApplicationsAcrossSides(t *testing.T) {
	application := func(name string) argoappv1.Application {
		return argoappv1.Application{Name: name, Namespace: "argocd"}
	}
	const deleted = "workloads/demo/shared/extra.yaml"
	left := []ApplicationSelectionInput{
		{Application: application("demo"), Paths: []string{deleted}},
		{Application: application("other"), Paths: []string{"manifests/other"}},
	}
	right := []ApplicationSelectionInput{
		{Application: application("demo"), Paths: []string{"workloads/demo/overlays/staging"}},
		{Application: application("other"), Paths: []string{"manifests/other"}},
	}

	leftSelected, rightSelected, unowned := selectChangedDiffSides(left, right, []string{deleted})

	names := func(apps []argoappv1.Application) []string {
		out := make([]string, 0, len(apps))
		for _, app := range apps {
			out = append(out, app.Name)
		}
		return out
	}
	if !slices.Equal(names(leftSelected), []string{"demo"}) || !slices.Equal(names(rightSelected), []string{"demo"}) {
		t.Fatalf("selected left = %v, right = %v; want demo on both sides", names(leftSelected), names(rightSelected))
	}
	if len(unowned) != 0 {
		t.Fatalf("unowned = %v, want none", unowned)
	}
}
