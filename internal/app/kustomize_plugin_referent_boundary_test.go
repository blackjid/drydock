package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// TestBuildRejectsBuiltinPluginConfigReferentsOutsideRepository drives a
// build end to end with a builtin PatchTransformer config whose path: names
// a file outside the repository (a sibling temporary directory holding a
// marker ConfigMap patch) or a local test server. The build fails with the
// boundary or remote error, nothing from outside reaches the output, and
// the server receives no request.
func TestBuildRejectsBuiltinPluginConfigReferentsOutsideRepository(t *testing.T) {
	const marker = "outside-marker"
	markerPatch := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  leaked: " + marker + "\n"
	for _, tt := range []struct {
		name         string
		buildOptions string
		ref          func(serverURL string) string
		wantErr      string
	}{
		{
			name:         "outside file under LoadRestrictionsNone",
			buildOptions: "--load-restrictor=LoadRestrictionsNone",
			ref:          func(string) string { return "../../../outside/patch.yaml" },
			wantErr:      "escapes repository root",
		},
		{
			name: "remote file under LoadRestrictionsRootOnly",
			ref:  func(serverURL string) string { return serverURL + "/patch.yaml" },
			// Kustomize fetches http(s) paths before its load restrictor applies.
			wantErr: "is a remote ref",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, markerPatch)
			}))
			t.Cleanup(server.Close)

			top := t.TempDir()
			root := filepath.Join(top, "repo")
			writeTestFile(t, filepath.Join(top, "outside", "patch.yaml"), markerPatch)
			if tt.buildOptions != "" {
				writeTestFile(t, filepath.Join(root, "settings", "argocd-cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: argocd-cm\ndata:\n  kustomize.buildOptions: "+tt.buildOptions+"\n")
			}
			writeTestFile(t, filepath.Join(root, "apps", "demo", "app.yaml"), `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: demo
spec:
  project: default
  destination:
    namespace: default
    server: https://kubernetes.default.svc
  source:
    repoURL: https://example.test/repo.git
    targetRevision: HEAD
    path: apps/demo
`)
			writeTestFile(t, filepath.Join(root, "apps", "demo", "kustomization.yaml"), "resources:\n  - cm.yaml\ntransformers:\n  - cfg/transformer.yaml\n")
			writeTestFile(t, filepath.Join(root, "apps", "demo", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  value: safe\n")
			writeTestFile(t, filepath.Join(root, "apps", "demo", "cfg", "transformer.yaml"), "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: referent\npath: "+strconv.Quote(tt.ref(server.URL))+"\ntarget:\n  kind: ConfigMap\n  name: demo\n")

			result, err := (Orchestrator{}).Build(context.Background(), BuildRequest{Path: root})
			if got := requests.Load(); got != 0 {
				t.Fatalf("remote server received %d requests, want none", got)
			}
			for _, item := range result.Manifests {
				data, marshalErr := json.Marshal(item.Object.Object)
				if marshalErr != nil {
					t.Fatalf("marshal manifest: %v", marshalErr)
				}
				if strings.Contains(string(data), marker) {
					t.Fatalf("Build() rendered the outside referent: %s", data)
				}
			}
			if err == nil {
				t.Fatal("Build() error = nil, want the referent rejected")
			}
			if len(result.Statuses) != 1 || !strings.Contains(result.Statuses[0].Message, tt.wantErr) {
				t.Fatalf("Statuses = %#v, want one status containing %q", result.Statuses, tt.wantErr)
			}
		})
	}
}
