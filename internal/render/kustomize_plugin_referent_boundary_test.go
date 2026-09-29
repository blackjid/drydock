package render

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// These tests pin that the files builtin plugin configs read — a
// PatchTransformer path:, a ConfigMapGenerator files: entry, and so on —
// stay inside the repository and are never fetched remotely. Kustomize
// resolves them through the listing kustomization's loader, which fetches
// an http(s) path under every load restrictor and, under
// LoadRestrictionsNone, reads any path and follows any symlink. Every
// escaping row points at a file in a sibling temporary directory outside
// the repository that holds a marker; a render that reads it shows the
// marker. Every remote row points at a local test server that counts
// requests; the render must reject the referent before any request.

const (
	referentOutsideMarker = "outside-marker"
	referentInsideMarker  = "inside-marker"
)

var referentLoadRestrictionsNone = []string{"--load-restrictor=LoadRestrictionsNone"}

var referentRestrictors = []struct {
	name    string
	options []string
}{
	{name: "LoadRestrictionsNone", options: referentLoadRestrictionsNone},
	{name: "LoadRestrictionsRootOnly"},
}

// pluginReferentKind is one builtin plugin config field that names a file.
type pluginReferentKind struct {
	name string
	// list is the kustomization field listing the config.
	list string
	// file is the referent's base name.
	file string
	// content returns the referent's content carrying marker.
	content func(marker string) string
	// config returns the builtin config document reading ref.
	config func(ref string) string
}

func referentConfigMapPatch(marker string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  leaked: " + marker + "\n"
}

func referentKvGeneratorConfig(kind, field string) func(string) string {
	return func(ref string) string {
		return "apiVersion: builtin\nkind: " + kind + "\nmetadata:\n  name: generated\noptions:\n  disableNameSuffixHash: true\n" + field + ":\n  - " + strconv.Quote(ref) + "\n"
	}
}

func referentPatchTransformerConfig(ref string) string {
	return "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: referent\npath: " + strconv.Quote(ref) + "\ntarget:\n  kind: ConfigMap\n  name: demo\n"
}

var pluginReferentKinds = []pluginReferentKind{
	{
		name: "PatchTransformer.path", list: "transformers", file: "patch.yaml",
		content: referentConfigMapPatch,
		config:  referentPatchTransformerConfig,
	},
	{
		name: "PatchJson6902Transformer.path", list: "transformers", file: "ops.yaml",
		content: func(marker string) string {
			return "- op: add\n  path: /data/leaked\n  value: " + marker + "\n"
		},
		config: func(ref string) string {
			return "apiVersion: builtin\nkind: PatchJson6902Transformer\nmetadata:\n  name: referent\ntarget:\n  version: v1\n  kind: ConfigMap\n  name: demo\npath: " + strconv.Quote(ref) + "\n"
		},
	},
	{
		name: "PatchStrategicMergeTransformer.paths", list: "transformers", file: "smp.yaml",
		content: referentConfigMapPatch,
		config: func(ref string) string {
			return "apiVersion: builtin\nkind: PatchStrategicMergeTransformer\nmetadata:\n  name: referent\npaths:\n  - " + strconv.Quote(ref) + "\n"
		},
	},
	{
		name: "ReplacementTransformer.replacements.path", list: "transformers", file: "replacement.yaml",
		content: func(marker string) string {
			return "source:\n  kind: ConfigMap\n  name: demo\n  fieldPath: data.value\ntargets:\n  - select:\n      kind: ConfigMap\n      name: demo\n    fieldPaths:\n      - data." + marker + "\n    options:\n      create: true\n"
		},
		config: func(ref string) string {
			return "apiVersion: builtin\nkind: ReplacementTransformer\nmetadata:\n  name: referent\nreplacements:\n  - path: " + strconv.Quote(ref) + "\n"
		},
	},
	{
		name: "ValueAddTransformer.targetFilePath", list: "transformers", file: "targets.yaml",
		content: func(marker string) string {
			return "targets:\n  - selector:\n      kind: ConfigMap\n      name: demo\n    fieldPath: data/" + marker + "\n"
		},
		config: func(ref string) string {
			return "apiVersion: builtin\nkind: ValueAddTransformer\nmetadata:\n  name: referent\nvalue: added\ntargetFilePath: " + strconv.Quote(ref) + "\n"
		},
	},
	{
		name: "ConfigMapGenerator.files", list: "generators", file: "data.txt",
		content: func(marker string) string { return marker },
		config:  referentKvGeneratorConfig("ConfigMapGenerator", "files"),
	},
	{
		name: "ConfigMapGenerator.envs", list: "generators", file: "vars.env",
		content: func(marker string) string { return "leaked=" + marker + "\n" },
		config:  referentKvGeneratorConfig("ConfigMapGenerator", "envs"),
	},
	{
		name: "SecretGenerator.files", list: "generators", file: "data.txt",
		content: func(marker string) string { return marker },
		config:  referentKvGeneratorConfig("SecretGenerator", "files"),
	},
	{
		name: "SecretGenerator.envs", list: "generators", file: "vars.env",
		content: func(marker string) string { return "leaked=" + marker + "\n" },
		config:  referentKvGeneratorConfig("SecretGenerator", "envs"),
	},
}

// referentFixture is a repository at top/repo whose app apps/demo renders
// ConfigMap demo, next to a directory top/outside the render must never
// read. Both live in the test's own temporary directory.
type referentFixture struct {
	root    string
	outside string
	listing string
}

func newReferentFixture(t *testing.T) referentFixture {
	t.Helper()
	top := t.TempDir()
	fixture := referentFixture{
		root:    filepath.Join(top, "repo"),
		outside: filepath.Join(top, "outside"),
	}
	fixture.listing = filepath.Join(fixture.root, "apps", "demo")
	writeFile(t, filepath.Join(fixture.listing, "cm.yaml"), decodeTestConfigMap("demo", "safe"))
	return fixture
}

// referent writes kind's referent with marker at path.
func (f referentFixture) referent(t *testing.T, kind pluginReferentKind, path, marker string) {
	t.Helper()
	writeFile(t, path, kind.content(marker))
}

// escapingRef writes kind's referent with the outside marker into the
// outside directory and returns the ref an escape of the given form uses to
// name it from the listing directory.
func (f referentFixture) escapingRef(t *testing.T, kind pluginReferentKind, escape string) string {
	t.Helper()
	target := filepath.Join(f.outside, kind.file)
	f.referent(t, kind, target, referentOutsideMarker)
	switch escape {
	case "absolute":
		return filepath.ToSlash(target)
	case "dotdot":
		// Enough ../ to reach the filesystem root from any directory, the
		// prepared workspace's temporary copy included, then down into the
		// outside directory.
		return strings.Repeat("../", 64) + strings.TrimPrefix(filepath.ToSlash(target), "/")
	case "symlink":
		symlink(t, target, filepath.Join(f.listing, "linked-"+kind.file))
		return "linked-" + kind.file
	case "padded symlink":
		// Kustomize reads the name as written: "linked-x " is not "linked-x".
		symlink(t, target, filepath.Join(f.listing, "linked-"+kind.file+" "))
		return "linked-" + kind.file + " "
	default:
		t.Fatalf("unknown escape %q", escape)
		return ""
	}
}

// listWrapped wraps a config document in a kind: List, whose items
// kustomize expands into plugin configs.
func listWrapped(config string) string {
	return "apiVersion: v1\nkind: List\nitems:\n  - " + strings.ReplaceAll(strings.TrimSuffix(config, "\n"), "\n", "\n    ") + "\n"
}

// writeEntry writes the kustomization listing config under list in the
// given shape: a config file, an inline entry, a kustomization directory
// whose only resource is the config, or a file holding the config wrapped
// in a kind: List.
func (f referentFixture) writeEntry(t *testing.T, list, shape, config string) {
	t.Helper()
	var entry string
	switch shape {
	case "file":
		writeFile(t, filepath.Join(f.listing, "cfg", "config.yaml"), config)
		entry = "cfg/config.yaml"
	case "inline":
		// Graph validation also stats an inline entry as a path, and a
		// referent's ../ segments can make that path escape by accident. The
		// leading comment's separators absorb them, so only the referent
		// check can reject the escape.
		config = "# " + strings.Repeat("x/", 80) + "\n" + config
		entry = "|\n    " + strings.ReplaceAll(strings.TrimSuffix(config, "\n"), "\n", "\n    ")
	case "directory":
		writeFile(t, filepath.Join(f.listing, "cfgdir", "kustomization.yaml"), "resources:\n  - config.yaml\n")
		writeFile(t, filepath.Join(f.listing, "cfgdir", "config.yaml"), config)
		entry = "cfgdir"
	case "list":
		writeFile(t, filepath.Join(f.listing, "cfg", "config.yaml"), listWrapped(config))
		entry = "cfg/config.yaml"
	default:
		t.Fatalf("unknown shape %q", shape)
	}
	writeFile(t, filepath.Join(f.listing, "kustomization.yaml"), "resources:\n  - cm.yaml\n"+list+":\n  - "+entry+"\n")
}

// render renders apps/demo on the plain path, or with a source namePrefix
// on the prepared-workspace path. Generator-bearing apps always take the
// prepared path.
func (f referentFixture) render(prepared bool, buildOptions []string) ([]Manifest, error) {
	opts := RenderOptions{BuildOptions: buildOptions}
	if prepared {
		opts.Kustomize = &argoappv1.ApplicationSourceKustomize{NamePrefix: "p-"}
	}
	manifests, _, err := (KustomizeRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: f.root, Path: "apps/demo"}, opts)
	return manifests, err
}

// renderedContains reports whether marker appears anywhere in the rendered
// objects, as text or base64 (Secret data).
func renderedContains(t *testing.T, manifests []Manifest, marker string) bool {
	t.Helper()
	for _, manifest := range manifests {
		data, err := json.Marshal(manifest.Object.Object)
		if err != nil {
			t.Fatalf("marshal rendered object: %v", err)
		}
		if strings.Contains(string(data), marker) || strings.Contains(string(data), base64.StdEncoding.EncodeToString([]byte(marker))) {
			return true
		}
	}
	return false
}

// assertOutsideNotRead fails when the outside marker reached the rendered
// output or the error (a validator's read shows only there).
func assertOutsideNotRead(t *testing.T, manifests []Manifest, err error) {
	t.Helper()
	if renderedContains(t, manifests, referentOutsideMarker) || (err != nil && strings.Contains(err.Error(), referentOutsideMarker)) {
		t.Fatalf("Render() read the outside referent (error %v)", err)
	}
}

// referentServer is a local server that counts every request and answers
// with content carrying the outside marker.
func referentServer(t *testing.T, content string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, content)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func assertNoRequests(t *testing.T, requests *atomic.Int64, err error) {
	t.Helper()
	if got := requests.Load(); got != 0 {
		t.Fatalf("remote server received %d requests, want none (error %v)", got, err)
	}
}

func referentShapes(kind pluginReferentKind) []string {
	if kind.list == "generators" {
		// The prepared workspace, which every generator-bearing app takes,
		// rejects directory entries and non-generator documents such as a
		// List before anything reads them.
		return []string{"file", "inline"}
	}
	return []string{"file", "inline", "directory", "list"}
}

var referentEscapes = []struct{ name, wantErr string }{
	{name: "absolute", wantErr: "must be relative"},
	{name: "dotdot", wantErr: "escapes repository root"},
	{name: "symlink", wantErr: "includes symlink component"},
	{name: "padded symlink", wantErr: "includes symlink component"},
}

// TestKustomizePluginConfigReferentsCannotEscapeRepository covers every
// builtin referent field in every entry shape, on the plain and prepared
// render paths, under LoadRestrictionsNone: an absolute referent, one that
// climbs out with ../, and in-repository symlinks to an outside file all
// fail, and nothing from outside reaches the output.
func TestKustomizePluginConfigReferentsCannotEscapeRepository(t *testing.T) {
	for _, kind := range pluginReferentKinds {
		for _, shape := range referentShapes(kind) {
			for _, escape := range referentEscapes {
				for _, prepared := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/prepared=%v", kind.name, shape, escape.name, prepared), func(t *testing.T) {
						fixture := newReferentFixture(t)
						fixture.writeEntry(t, kind.list, shape, kind.config(fixture.escapingRef(t, kind, escape.name)))

						manifests, err := fixture.render(prepared, referentLoadRestrictionsNone)
						assertOutsideNotRead(t, manifests, err)
						if err == nil || !strings.Contains(err.Error(), escape.wantErr) {
							t.Fatalf("Render() error = %v, want error containing %q", err, escape.wantErr)
						}
					})
				}
			}
		}
	}
}

// TestKustomizeValidatorConfigReferentsCannotEscapeRepository covers the
// validators: list. A validator must leave resources unchanged, so the
// escaping read shows in kustomize's error rather than the output.
func TestKustomizeValidatorConfigReferentsCannotEscapeRepository(t *testing.T) {
	kind := pluginReferentKinds[0]
	for _, shape := range referentShapes(kind) {
		for _, escape := range referentEscapes {
			for _, prepared := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/prepared=%v", shape, escape.name, prepared), func(t *testing.T) {
					fixture := newReferentFixture(t)
					fixture.writeEntry(t, "validators", shape, kind.config(fixture.escapingRef(t, kind, escape.name)))

					manifests, err := fixture.render(prepared, referentLoadRestrictionsNone)
					assertOutsideNotRead(t, manifests, err)
					if err == nil || !strings.Contains(err.Error(), escape.wantErr) {
						t.Fatalf("Render() error = %v, want error containing %q", err, escape.wantErr)
					}
				})
			}
		}
	}
}

// TestKustomizePluginConfigReferentsInsideRepositoryRender pins that the
// check leaves in-repository referents alone. Under LoadRestrictionsNone the
// referent sits outside the listing directory (repo/shared), so the prepared
// workspace must copy it; under LoadRestrictionsRootOnly kustomize requires
// it inside the listing directory.
func TestKustomizePluginConfigReferentsInsideRepositoryRender(t *testing.T) {
	for _, kind := range pluginReferentKinds {
		for _, shape := range referentShapes(kind) {
			for _, restrictor := range referentRestrictors {
				for _, prepared := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/prepared=%v", kind.name, shape, restrictor.name, prepared), func(t *testing.T) {
						fixture := newReferentFixture(t)
						ref := "local/" + kind.file
						path := filepath.Join(fixture.listing, "local", kind.file)
						if restrictor.options != nil {
							ref = "../../shared/" + kind.file
							path = filepath.Join(fixture.root, "shared", kind.file)
						}
						fixture.referent(t, kind, path, referentInsideMarker)
						fixture.writeEntry(t, kind.list, shape, kind.config(ref))

						manifests, err := fixture.render(prepared, restrictor.options)
						if err != nil {
							t.Fatalf("Render() error = %v", err)
						}
						if !renderedContains(t, manifests, referentInsideMarker) {
							t.Fatalf("Render() output lacks the referent's content: %#v", manifests)
						}
					})
				}
			}
		}
	}
}

// TestKustomizePluginConfigRemoteReferentsAreNotFetched pins that an http
// referent fails before kustomize fetches it — under
// LoadRestrictionsRootOnly too, since kustomize's loader fetches http(s)
// paths before applying any load restrictor. Drydock fetches remote
// content only through its own acquisition. Inline entries are left out:
// graph validation already rejects any entry containing "://" as a remote
// ref.
func TestKustomizePluginConfigRemoteReferentsAreNotFetched(t *testing.T) {
	for _, kind := range pluginReferentKinds {
		for _, shape := range referentShapes(kind) {
			if shape == "inline" {
				continue
			}
			for _, restrictor := range referentRestrictors {
				for _, prepared := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/prepared=%v", kind.name, shape, restrictor.name, prepared), func(t *testing.T) {
						server, requests := referentServer(t, kind.content(referentOutsideMarker))
						fixture := newReferentFixture(t)
						fixture.writeEntry(t, kind.list, shape, kind.config(server.URL+"/"+kind.file))

						manifests, err := fixture.render(prepared, restrictor.options)
						assertNoRequests(t, requests, err)
						assertOutsideNotRead(t, manifests, err)
						if err == nil || !strings.Contains(err.Error(), "is a remote ref") {
							t.Fatalf("Render() error = %v, want remote ref error", err)
						}
					})
				}
			}
		}
	}
}

// writeNonPlainDirectoryEntry lists a directory entry whose kustomization
// sets more than resources: its configs are the output of a build, so
// their referents cannot be enumerated. The entry's patch overrides the
// config's path: with patchedPath when it is set.
func (f referentFixture) writeNonPlainDirectoryEntry(t *testing.T, configRef, patchedPath string) {
	t.Helper()
	f.writeEntry(t, "transformers", "directory", referentPatchTransformerConfig(configRef))
	kustomization := "namePrefix: cfg-\nresources:\n  - config.yaml\n"
	if patchedPath != "" {
		kustomization += "patches:\n  - target:\n      kind: PatchTransformer\n    patch: |\n      - op: replace\n        path: /path\n        value: " + strconv.Quote(patchedPath) + "\n"
	}
	writeFile(t, filepath.Join(f.listing, "cfgdir", "kustomization.yaml"), kustomization)
}

// TestKustomizeUnenumerablePluginConfigsFailClosed pins the rule for configs
// whose referents cannot be enumerated: they fail under every load
// restrictor. LoadRestrictionsRootOnly confines local reads to the listing
// directory, but kustomize's loader fetches an http(s) path before any
// restrictor applies, so a directory entry that patches its config's path:
// into a URL would be fetched — the RootOnly remote row below.
func TestKustomizeUnenumerablePluginConfigsFailClosed(t *testing.T) {
	const wantErr = "cannot be enumerated"
	kind := pluginReferentKinds[0]
	for _, prepared := range []bool{false, true} {
		for _, restrictor := range referentRestrictors {
			t.Run(fmt.Sprintf("in-repo/%s/prepared=%v", restrictor.name, prepared), func(t *testing.T) {
				fixture := newReferentFixture(t)
				fixture.referent(t, kind, filepath.Join(fixture.listing, "local", kind.file), referentInsideMarker)
				fixture.writeNonPlainDirectoryEntry(t, "local/"+kind.file, "")

				_, err := fixture.render(prepared, restrictor.options)
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("Render() error = %v, want error containing %q", err, wantErr)
				}
			})
			t.Run(fmt.Sprintf("remote/%s/prepared=%v", restrictor.name, prepared), func(t *testing.T) {
				server, requests := referentServer(t, kind.content(referentOutsideMarker))
				fixture := newReferentFixture(t)
				fixture.referent(t, kind, filepath.Join(fixture.listing, "local", kind.file), referentInsideMarker)
				fixture.writeNonPlainDirectoryEntry(t, "local/"+kind.file, server.URL+"/"+kind.file)

				manifests, err := fixture.render(prepared, restrictor.options)
				assertNoRequests(t, requests, err)
				assertOutsideNotRead(t, manifests, err)
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("Render() error = %v, want error containing %q", err, wantErr)
				}
			})
		}
		for _, escape := range []string{"absolute", "dotdot", "symlink"} {
			t.Run(fmt.Sprintf("%s/LoadRestrictionsNone/prepared=%v", escape, prepared), func(t *testing.T) {
				fixture := newReferentFixture(t)
				fixture.writeNonPlainDirectoryEntry(t, fixture.escapingRef(t, kind, escape), "")

				manifests, err := fixture.render(prepared, referentLoadRestrictionsNone)
				assertOutsideNotRead(t, manifests, err)
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("Render() error = %v, want error containing %q", err, wantErr)
				}
			})
		}
	}
}

// outsideConfigsKustomization writes, in the outside directory, a
// kustomization whose PatchTransformer config writes the outside marker
// into ConfigMap demo, and returns its directory.
func (f referentFixture) outsideConfigsKustomization(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(f.outside, "configs")
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "resources:\n  - config.yaml\n")
	writeFile(t, filepath.Join(dir, "config.yaml"), outsidePatchTransformerConfig())
	return dir
}

func outsidePatchTransformerConfig() string {
	return "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: outside\npatch: |\n  apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: demo\n  data:\n    leaked: " + referentOutsideMarker + "\ntarget:\n  kind: ConfigMap\n  name: demo\n"
}

// TestKustomizePluginConfigDirectoryEntryGraphCannotEscapeRepository pins
// that a directory entry's own kustomization graph is walked like any
// other: its resources may not leave the repository, pass through a
// symlink, or name a remote URL. Kustomize builds a directory entry from
// anywhere, whatever the load restrictor, so without the walk an outside
// kustomization's configs would transform the render.
func TestKustomizePluginConfigDirectoryEntryGraphCannotEscapeRepository(t *testing.T) {
	for _, restrictor := range referentRestrictors {
		for _, prepared := range []bool{false, true} {
			for _, escape := range []struct{ name, wantErr string }{
				{name: "dotdot", wantErr: "escapes repository root"},
				{name: "symlink", wantErr: "symlink"},
				{name: "remote", wantErr: "is a remote ref"},
			} {
				t.Run(fmt.Sprintf("%s/%s/prepared=%v", escape.name, restrictor.name, prepared), func(t *testing.T) {
					server, requests := referentServer(t, outsidePatchTransformerConfig())
					fixture := newReferentFixture(t)
					fixture.writeEntry(t, "transformers", "directory", "")
					var resource string
					switch escape.name {
					case "dotdot":
						resource = strings.Repeat("../", 64) + strings.TrimPrefix(filepath.ToSlash(fixture.outsideConfigsKustomization(t)), "/")
					case "symlink":
						symlink(t, fixture.outsideConfigsKustomization(t), filepath.Join(fixture.listing, "cfgdir", "linked"))
						resource = "linked"
					case "remote":
						resource = server.URL + "/config.yaml"
					}
					writeFile(t, filepath.Join(fixture.listing, "cfgdir", "kustomization.yaml"), "resources:\n  - "+resource+"\n")

					manifests, err := fixture.render(prepared, restrictor.options)
					assertNoRequests(t, requests, err)
					assertOutsideNotRead(t, manifests, err)
					if err == nil || !strings.Contains(err.Error(), escape.wantErr) {
						t.Fatalf("Render() error = %v, want error containing %q", err, escape.wantErr)
					}
				})
			}
		}
	}
}

// TestKustomizePluginConfigDirectoryEntriesListingEachOtherTerminate pins
// that the walk of directory entries is cycle-safe: an entry whose own
// kustomization lists the listing directory as a directory entry ends in an
// error instead of recursing forever.
func TestKustomizePluginConfigDirectoryEntriesListingEachOtherTerminate(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprintf("prepared=%v", prepared), func(t *testing.T) {
			fixture := newReferentFixture(t)
			fixture.writeEntry(t, "transformers", "directory", referentPatchTransformerConfig("patch.yaml"))
			writeFile(t, filepath.Join(fixture.listing, "cfgdir", "kustomization.yaml"), "resources:\n  - config.yaml\ntransformers:\n  - ..\n")
			fixture.referent(t, pluginReferentKinds[0], filepath.Join(fixture.listing, "patch.yaml"), referentInsideMarker)

			_, err := fixture.render(prepared, nil)
			if err == nil || !strings.Contains(err.Error(), "cannot be enumerated") {
				t.Fatalf("Render() error = %v, want error containing %q", err, "cannot be enumerated")
			}
		})
	}
}

// TestKustomizePluginConfigReferentsOfWorkspaceInputsAreChecked covers
// kustomizations that only the prepared workspace holds, which the walk of
// the workspace checks before krusty builds it: a spec.source.kustomize
// component and an acquired remote Git resource. Their configs' referents
// follow the same rules.
func TestKustomizePluginConfigReferentsOfWorkspaceInputsAreChecked(t *testing.T) {
	kind := pluginReferentKinds[0]
	for _, input := range []string{"source component", "remote git resource"} {
		for _, escape := range []struct{ name, wantErr string }{
			{name: "absolute", wantErr: "must be relative"},
			{name: "remote", wantErr: "is a remote ref"},
		} {
			t.Run(input+"/"+escape.name, func(t *testing.T) {
				server, requests := referentServer(t, kind.content(referentOutsideMarker))
				fixture := newReferentFixture(t)
				ref := server.URL + "/" + kind.file
				buildOptions := []string(nil)
				if escape.name == "absolute" {
					ref = fixture.escapingRef(t, kind, "absolute")
					buildOptions = referentLoadRestrictionsNone
				}
				opts := RenderOptions{BuildOptions: buildOptions}
				switch input {
				case "source component":
					component := filepath.Join(fixture.root, "components", "extra")
					writeFile(t, filepath.Join(component, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\ntransformers:\n  - config.yaml\n")
					writeFile(t, filepath.Join(component, "config.yaml"), kind.config(ref))
					writeFile(t, filepath.Join(fixture.listing, "kustomization.yaml"), "resources:\n  - cm.yaml\n")
					opts.Kustomize = &argoappv1.ApplicationSourceKustomize{Components: []string{"../../components/extra"}}
				case "remote git resource":
					// The acquired copy is a directory of this test's own.
					acquired := filepath.Join(filepath.Dir(fixture.root), "acquired")
					writeFile(t, filepath.Join(acquired, "base", "kustomization.yaml"), "resources:\n  - cm.yaml\ntransformers:\n  - config.yaml\n")
					writeFile(t, filepath.Join(acquired, "base", "cm.yaml"), decodeTestConfigMap("demo", "safe"))
					writeFile(t, filepath.Join(acquired, "base", "config.yaml"), kind.config(ref))
					writeFile(t, filepath.Join(fixture.listing, "kustomization.yaml"), "resources:\n  - https://github.com/example/repo.git//base?ref=v1.2.3\n")
					opts.RemoteResourceAcquirer = &fakeRemoteAcquirer{path: acquired}
					opts.RemoteResourceCacheDir = t.TempDir()
				}

				manifests, _, err := (KustomizeRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: fixture.root, Path: "apps/demo"}, opts)
				assertNoRequests(t, requests, err)
				assertOutsideNotRead(t, manifests, err)
				if err == nil || !strings.Contains(err.Error(), escape.wantErr) {
					t.Fatalf("Render() error = %v, want error containing %q", err, escape.wantErr)
				}
			})
		}
	}
}

// TestKustomizeAcquiredRemoteGitReferentsStayInAcquiredRepository pins that
// acquired remote Git content is bounded by the acquired repository, for
// builtin plugin config referents and directory entries as for every other
// ref. The prepared workspace copies the acquired repository to
// apps/demo/.drydock/git/<name>, so from its base directory ../../../../
// reaches apps/demo of the local repository: bounded only by the
// workspace, a remote base could read local files into its configs under
// LoadRestrictionsNone.
func TestKustomizeAcquiredRemoteGitReferentsStayInAcquiredRepository(t *testing.T) {
	const localMarker = "local-marker"
	for _, shape := range []string{"referent", "directory entry"} {
		t.Run(shape, func(t *testing.T) {
			fixture := newReferentFixture(t)
			acquired := filepath.Join(filepath.Dir(fixture.root), "acquired")
			base := filepath.Join(acquired, "base")
			writeFile(t, filepath.Join(base, "cm.yaml"), decodeTestConfigMap("remote", "safe"))
			switch shape {
			case "referent":
				writeFile(t, filepath.Join(fixture.listing, "local-patch.yaml"), paddedJSONPatch(localMarker))
				writeFile(t, filepath.Join(base, "kustomization.yaml"), "resources:\n  - cm.yaml\ntransformers:\n  - config.yaml\n")
				writeFile(t, filepath.Join(base, "config.yaml"), "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: local\npath: ../../../../local-patch.yaml\ntarget:\n  kind: ConfigMap\n")
			case "directory entry":
				writeFile(t, filepath.Join(fixture.listing, "local-config.yaml"), "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: local\npatch: |\n"+indentLines(paddedJSONPatch(localMarker), "  ")+"target:\n  kind: ConfigMap\n")
				writeFile(t, filepath.Join(base, "kustomization.yaml"), "resources:\n  - cm.yaml\ntransformers:\n  - cfgdir\n")
				writeFile(t, filepath.Join(base, "cfgdir", "kustomization.yaml"), "resources:\n  - ../../../../../local-config.yaml\n")
			}
			writeFile(t, filepath.Join(fixture.listing, "kustomization.yaml"), "resources:\n  - cm.yaml\n  - https://github.com/example/repo.git//base?ref=v1.2.3\n")

			manifests, _, err := (KustomizeRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: fixture.root, Path: "apps/demo"}, RenderOptions{
				BuildOptions:           referentLoadRestrictionsNone,
				RemoteResourceAcquirer: &fakeRemoteAcquirer{path: acquired},
				RemoteResourceCacheDir: t.TempDir(),
			})
			if renderedContains(t, manifests, localMarker) {
				t.Fatalf("Render() read a local file through the acquired base (error %v)", err)
			}
			if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
				t.Fatalf("Render() error = %v, want error containing %q", err, "escapes repository root")
			}
		})
	}
}

func indentLines(text, indent string) string {
	return indent + strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\n", "\n"+indent) + "\n"
}

// TestKustomizePluginConfigReferentProbeShapes pins config shapes a
// referent check could miss: a differently-cased key (the plugins decode
// case-insensitively), a YAML merge key, an escape in the second document
// of a multi-document config, and a key= prefixed absolute generator file.
func TestKustomizePluginConfigReferentProbeShapes(t *testing.T) {
	patch := pluginReferentKinds[0]
	files := pluginReferentKinds[5]
	for _, probe := range []struct {
		name    string
		kind    pluginReferentKind
		config  func(ref string) string
		escape  string
		wantErr string
	}{
		{
			name: "PATH key", kind: patch, escape: "dotdot", wantErr: "escapes repository root",
			config: func(ref string) string {
				return "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: referent\nPATH: " + strconv.Quote(ref) + "\ntarget:\n  kind: ConfigMap\n  name: demo\n"
			},
		},
		{
			name: "merge key", kind: patch, escape: "dotdot", wantErr: "escapes repository root",
			config: func(ref string) string {
				return "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: referent\n<<:\n  path: " + strconv.Quote(ref) + "\ntarget:\n  kind: ConfigMap\n  name: demo\n"
			},
		},
		{
			name: "second document", kind: patch, escape: "dotdot", wantErr: "escapes repository root",
			config: func(ref string) string {
				return "apiVersion: builtin\nkind: LabelTransformer\nmetadata:\n  name: first\nlabels:\n  a: b\nfieldSpecs:\n  - path: metadata/labels\n    create: true\n---\n" + referentPatchTransformerConfig(ref)
			},
		},
		{
			// The subtest name must hold no "=": it is part of the temporary
			// directory path, and a second "=" makes kustomize reject the entry
			// before reading it.
			name: "kv keyed absolute", kind: files, escape: "absolute", wantErr: "must be relative",
			config: func(ref string) string {
				return referentKvGeneratorConfig("ConfigMapGenerator", "files")("leaked=" + ref)
			},
		},
	} {
		for _, prepared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prepared=%v", probe.name, prepared), func(t *testing.T) {
				fixture := newReferentFixture(t)
				fixture.writeEntry(t, probe.kind.list, "file", probe.config(fixture.escapingRef(t, probe.kind, probe.escape)))

				manifests, err := fixture.render(prepared, referentLoadRestrictionsNone)
				assertOutsideNotRead(t, manifests, err)
				if err == nil || !strings.Contains(err.Error(), probe.wantErr) {
					t.Fatalf("Render() error = %v, want error containing %q", err, probe.wantErr)
				}
			})
		}
	}
}
