package app

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sholdee/drydock/internal/cacheevent"
)

// kustomize matches kustomization keys case-insensitively, so a capitalized
// key is a render input like its lowercase form. The persistent render
// cache's input digest walked only exact lowercase keys: a file named under
// Resources: or Transformers: rendered but was not digested, and editing it
// left a warm build serving the pre-edit render. Each case commits, fills
// the cache, edits ONLY the file the capitalized key names, and asserts a
// warm build re-renders and matches a fresh-cache-dir build.

func writePersistentCacheCapitalizedResourcesApp(t *testing.T, root, name, value string) {
	t.Helper()
	writePersistentCacheKustomizeApp(t, root, name, "Resources:\n  - ../../bases/"+name+"\n")
	writeTestFile(t, filepath.Join(root, "bases", name, "kustomization.yaml"), "resources:\n  - cm.yaml\n")
	writeTestFile(t, filepath.Join(root, "bases", name, "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  value: `+value+`
`)
}

func writePersistentCacheCapitalizedTransformersApp(t *testing.T, root, name, value string) {
	t.Helper()
	writePersistentCacheKustomizeApp(t, root, name, "resources:\n  - cm.yaml\nTransformers:\n  - cfg/transformer.yaml\n")
	writePersistentCacheDemoConfigMap(t, root, name)
	writeTestFile(t, filepath.Join(root, "manifests", name, "cfg", "transformer.yaml"), `apiVersion: builtin
kind: PatchTransformer
metadata:
  name: value-demo
path: patch.yaml
target:
  kind: ConfigMap
  name: demo
`)
	writePersistentCachePatchTransformerReferent(t, root, name, value)
}

func TestPersistentCacheCapitalizedKustomizationKeyInvalidatesCache(t *testing.T) {
	cases := []struct {
		name    string
		appName string
		write   func(t *testing.T, root, appName, value string)
	}{
		{name: "Resources", appName: "resources-app", write: writePersistentCacheCapitalizedResourcesApp},
		{name: "Transformers", appName: "transformers-app", write: writePersistentCacheCapitalizedTransformersApp},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			testCase.write(t, root, testCase.appName, "initial")
			gitCommitAll(t, root, "initial")
			cacheDir := t.TempDir()
			request := persistentBuildRequest(root, cacheDir)
			request.RecordCacheEvents = true

			coldResult, err := (Orchestrator{}).Build(context.Background(), request)
			if err != nil {
				t.Fatalf("cold Build() error = %v", err)
			}
			if got := persistentConfigMapDataValue(t, coldResult, testCase.appName, "demo", "value"); got != "initial" {
				t.Fatalf("cold value = %q, want %q", got, "initial")
			}
			if !hasRenderCacheEvent(coldResult.CacheEvents, cacheevent.ActionStore, "argocd/"+testCase.appName, "") {
				t.Fatalf("CacheEvents = %#v, want %s persistent store", coldResult.CacheEvents, testCase.appName)
			}

			testCase.write(t, root, testCase.appName, "changed")
			gitCommitAll(t, root, "change the capitalized key's file only")

			warmOrchestrator := Orchestrator{}
			warmRendered := renderedSourceCounts(&warmOrchestrator)
			warmResult, err := warmOrchestrator.Build(context.Background(), request)
			if err != nil {
				t.Fatalf("warm Build() error = %v", err)
			}
			if got := warmRendered["manifests/"+testCase.appName]; got == 0 {
				t.Fatalf("warm build after the edit performed 0 renders for %s; a stale cache entry served the pre-edit render", testCase.appName)
			}
			if got := persistentConfigMapDataValue(t, warmResult, testCase.appName, "demo", "value"); !strings.Contains(got, "changed") {
				t.Fatalf("warm value = %q, want %q", got, "changed")
			}

			freshResult, err := (Orchestrator{}).Build(context.Background(), persistentBuildRequest(root, t.TempDir()))
			if err != nil {
				t.Fatalf("fresh-cache Build() error = %v", err)
			}
			if !reflect.DeepEqual(applicationManifestObjects(warmResult, testCase.appName), applicationManifestObjects(freshResult, testCase.appName)) {
				t.Fatalf("warm %s manifests != fresh-cache manifests; warm build served a stale render", testCase.appName)
			}
		})
	}
}
