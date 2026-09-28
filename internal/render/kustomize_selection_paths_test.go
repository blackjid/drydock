package render

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// writeKustomizeOverlayBaseFixture writes the canonical overlay -> base ->
// helmCharts layout: the Application path is an overlay, and the values file
// the PR edits lives under the shared base.
func writeKustomizeOverlayBaseFixture(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "apps", "demo", "overlays", "staging", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base
components:
  - ../../components/monitoring
patches:
  - path: replicas.yaml
`)
	writeFile(t, filepath.Join(root, "apps", "demo", "overlays", "staging", "replicas.yaml"), `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  replicas: 2
`)
	writeFile(t, filepath.Join(root, "apps", "demo", "base", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - helm-release
`)
	writeFile(t, filepath.Join(root, "apps", "demo", "base", "helm-release", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
helmCharts:
  - name: demo
    repo: https://charts.example.com
    version: 1.0.0
    releaseName: demo
    valuesFile: values.yaml
    additionalValuesFiles:
      - ../../shared/values-common.yaml
`)
	writeFile(t, filepath.Join(root, "apps", "demo", "base", "helm-release", "values.yaml"), "replicaCount: 1\n")
	writeFile(t, filepath.Join(root, "apps", "demo", "shared", "values-common.yaml"), "image: {}\n")
	writeFile(t, filepath.Join(root, "apps", "demo", "components", "monitoring", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
`)
}

func TestKustomizeSelectionPathsWalksOverlayBaseAndHelmValues(t *testing.T) {
	root := t.TempDir()
	writeKustomizeOverlayBaseFixture(t, root)

	paths, err := KustomizeSelectionPaths(context.Background(), root, "apps/demo/overlays/staging", nil)
	if err != nil {
		t.Fatalf("KustomizeSelectionPaths() error = %v", err)
	}
	for _, want := range []string{
		"apps/demo/base",
		"apps/demo/base/helm-release",
		"apps/demo/base/helm-release/values.yaml",
		"apps/demo/shared/values-common.yaml",
		"apps/demo/components/monitoring",
		"apps/demo/overlays/staging/replicas.yaml",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("KustomizeSelectionPaths() = %v, missing %q", paths, want)
		}
	}
	if !slices.IsSorted(paths) {
		t.Errorf("KustomizeSelectionPaths() = %v, want sorted", paths)
	}
}

// TestKustomizeSelectionPathsSkipsUncollectableRefs pins the best-effort
// contract: refs the digest walk rejects (unpinned remotes, repo escapes,
// remote Helm values) drop out instead of failing ownership for the whole
// Application.
func TestKustomizeSelectionPathsSkipsUncollectableRefs(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	writeKustomizeOverlayBaseFixture(t, repo)
	writeFile(t, filepath.Join(root, "outside.yaml"), "kind: ConfigMap\n")
	writeFile(t, filepath.Join(repo, "apps", "demo", "base", "helm-release", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
helmCharts:
  - name: demo
    repo: https://charts.example.com
    version: 1.0.0
    releaseName: demo
    valuesFile: values.yaml
    additionalValuesFiles:
      - https://example.com/values.yaml
`)
	writeFile(t, filepath.Join(repo, "apps", "demo", "overlays", "staging", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base
configurations:
  - ../../../../outside.yaml
`)

	if _, err := KustomizeInputDigestPaths(context.Background(), ResolvedSource{RepoRoot: repo, Path: "apps/demo/overlays/staging"}, RenderOptions{}); err == nil {
		t.Fatalf("KustomizeInputDigestPaths() error = nil, want strict digest walk to reject the fixture")
	}
	paths, err := KustomizeSelectionPaths(context.Background(), repo, "apps/demo/overlays/staging", nil)
	if err != nil {
		t.Fatalf("KustomizeSelectionPaths() error = %v", err)
	}
	if !slices.Contains(paths, "apps/demo/base/helm-release/values.yaml") {
		t.Errorf("KustomizeSelectionPaths() = %v, missing local values file", paths)
	}
	for _, path := range paths {
		if filepath.IsAbs(path) || path == ".." || len(path) > 2 && path[:3] == "../" {
			t.Errorf("KustomizeSelectionPaths() returned out-of-repo path %q", path)
		}
	}
}

// TestKustomizeSelectionPathsOwnsSourceKustomizeOptions pins that
// spec.source.kustomize components and patches own their inputs like
// kustomization refs do: rendering merges them into the source kustomization.
// A component whose graph cannot be read keeps its own directory without
// dropping the rest of the walk.
func TestKustomizeSelectionPathsOwnsSourceKustomizeOptions(t *testing.T) {
	root := t.TempDir()
	writeKustomizeOverlayBaseFixture(t, root)
	writeFile(t, filepath.Join(root, "apps", "demo", "components", "extra", "kustomization.yaml"), `
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
resources:
  - ../../shared/extra.yaml
`)
	writeFile(t, filepath.Join(root, "apps", "demo", "shared", "extra.yaml"), "kind: ConfigMap\n")
	writeFile(t, filepath.Join(root, "apps", "demo", "components", "broken", "kustomization.yaml"), "resources: [\n")
	writeFile(t, filepath.Join(root, "apps", "demo", "patches", "labels.yaml"), "kind: Deployment\n")

	paths, err := KustomizeSelectionPaths(context.Background(), root, "apps/demo/overlays/staging", &argoappv1.ApplicationSourceKustomize{
		Components: []string{"../../components/extra", "../../components/broken"},
		Patches:    argoappv1.KustomizePatches{{Path: "../../patches/labels.yaml"}},
	})
	if err != nil {
		t.Fatalf("KustomizeSelectionPaths() error = %v", err)
	}
	for _, want := range []string{
		"apps/demo/base/helm-release/values.yaml",
		"apps/demo/components/extra",
		"apps/demo/shared/extra.yaml",
		"apps/demo/components/broken",
		"apps/demo/patches/labels.yaml",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("KustomizeSelectionPaths() = %v, missing %q", paths, want)
		}
	}
}

func TestKustomizeSelectionPathsErrorsWithoutKustomization(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apps", "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := KustomizeSelectionPaths(context.Background(), root, "apps/plain", nil); err == nil {
		t.Fatalf("KustomizeSelectionPaths() error = nil, want missing kustomization error")
	}
}
