package render

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// A repository's .git directory is never render input, and it can hold
// credentials: actions/checkout persists the job token in .git/config by
// default. These tests pin that no renderer reads it — not through any
// kustomize field, builtin plugin config, KSOPS emulation, Helm value file
// or file parameter, Jsonnet import or directory walk — also when the ref
// spells it .GIT, which case-insensitive filesystems resolve to the same
// directory. Every fixture's .git holds only a marker, never a credential.

const gitMetadataMarker = "gitdir-marker"

// gitRefField is one field that reads a file, pointed into .git.
type gitRefField struct {
	name string
	// files are written under .git; their content carries the marker.
	files map[string]string
	// target is the path under .git the field names.
	target string
	// listing returns the listing kustomization naming ref, and writes any
	// config files it lists into dir.
	listing func(t *testing.T, dir, ref string) string
	chart   bool
	ksops   bool
}

func gitListing(kustomization string) func(*testing.T, string, string) string {
	return func(_ *testing.T, _, ref string) string {
		return strings.ReplaceAll(kustomization, "REF", strconv.Quote(ref))
	}
}

func gitListingWithConfig(name, config, kustomization string) func(*testing.T, string, string) string {
	return func(t *testing.T, dir, ref string) string {
		writeFile(t, filepath.Join(dir, name), strings.ReplaceAll(config, "REF", strconv.Quote(ref)))
		return kustomization
	}
}

var gitRefFields = []gitRefField{
	{
		name: "resources file", target: "cm.yaml",
		files:   map[string]string{"cm.yaml": decodeTestConfigMap("gitcm", gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\n  - REF\n"),
	},
	{
		name: "resources directory", target: "kdir",
		files:   map[string]string{"kdir/kustomization.yaml": "resources:\n  - cm.yaml\n", "kdir/cm.yaml": decodeTestConfigMap("gitcm", gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\n  - REF\n"),
	},
	{
		name: "components", target: "comp",
		files:   map[string]string{"comp/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources:\n  - cm.yaml\n", "comp/cm.yaml": decodeTestConfigMap("gitcm", gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\ncomponents:\n  - REF\n"),
	},
	{
		name: "patches.path", target: "patch.yaml",
		files:   map[string]string{"patch.yaml": paddedJSONPatch(gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\npatches:\n  - path: REF\n    target:\n      kind: ConfigMap\n      name: demo\n"),
	},
	{
		name: "patchesStrategicMerge", target: "smp.yaml",
		files:   map[string]string{"smp.yaml": referentConfigMapPatch(gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\npatchesStrategicMerge:\n  - REF\n"),
	},
	{
		name: "patchesJson6902.path", target: "ops.yaml",
		files:   map[string]string{"ops.yaml": paddedJSONPatch(gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\npatchesJson6902:\n  - target:\n      version: v1\n      kind: ConfigMap\n      name: demo\n    path: REF\n"),
	},
	{
		name: "configMapGenerator.files", target: "config",
		files:   map[string]string{"config": gitMetadataMarker},
		listing: gitListing("resources:\n  - cm.yaml\nconfigMapGenerator:\n  - name: generated\n    options:\n      disableNameSuffixHash: true\n    files:\n      - REF\n"),
	},
	{
		name: "configMapGenerator.envs", target: "vars.env",
		files:   map[string]string{"vars.env": "leaked=" + gitMetadataMarker + "\n"},
		listing: gitListing("resources:\n  - cm.yaml\nconfigMapGenerator:\n  - name: generated\n    options:\n      disableNameSuffixHash: true\n    envs:\n      - REF\n"),
	},
	{
		name: "secretGenerator.files", target: "config",
		files:   map[string]string{"config": gitMetadataMarker},
		listing: gitListing("resources:\n  - cm.yaml\nsecretGenerator:\n  - name: generated\n    options:\n      disableNameSuffixHash: true\n    files:\n      - REF\n"),
	},
	{
		name: "replacements.path", target: "replacement.yaml",
		files:   map[string]string{"replacement.yaml": pluginReferentKinds[3].content(gitMetadataMarker)},
		listing: gitListing("resources:\n  - cm.yaml\nreplacements:\n  - path: REF\n"),
	},
	{
		name: "helmCharts.valuesFile", target: "values.yaml", chart: true,
		files:   map[string]string{"values.yaml": "value: " + gitMetadataMarker + "\n"},
		listing: gitListing("helmCharts:\n  - name: chart\n    releaseName: demo\n    valuesFile: REF\n"),
	},
	{
		name: "KSOPS files", target: "secret.sops.yaml", ksops: true,
		files:   map[string]string{"secret.sops.yaml": ksopsTestSopsSecret(gitMetadataMarker, "TOKEN", "c2VjcmV0")},
		listing: gitListingWithConfig("ksops.yaml", "apiVersion: viaduct.ai/v1\nkind: ksops\nmetadata:\n  name: secrets\nfiles:\n  - REF\n", "resources:\n  - cm.yaml\ngenerators:\n  - ksops.yaml\n"),
	},
	{
		name: "PatchTransformer.path", target: "patch.yaml",
		files:   map[string]string{"patch.yaml": paddedJSONPatch(gitMetadataMarker)},
		listing: gitListingWithConfig("xf.yaml", "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: git\npath: REF\ntarget:\n  kind: ConfigMap\n  name: demo\n", "resources:\n  - cm.yaml\ntransformers:\n  - xf.yaml\n"),
	},
	{
		name: "ConfigMapGenerator.files", target: "config",
		files: map[string]string{"config": gitMetadataMarker},
		listing: func(t *testing.T, dir, ref string) string {
			writeFile(t, filepath.Join(dir, "gen.yaml"), referentKvGeneratorConfig("ConfigMapGenerator", "files")(ref))
			return "resources:\n  - cm.yaml\ngenerators:\n  - gen.yaml\n"
		},
	},
	{
		name: "transformers entry", target: "xf.yaml",
		files:   map[string]string{"xf.yaml": "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: git\npatch: |\n" + indentLines(paddedJSONPatch(gitMetadataMarker), "  ") + "target:\n  kind: ConfigMap\n  name: demo\n"},
		listing: gitListing("resources:\n  - cm.yaml\ntransformers:\n  - REF\n"),
	},
}

// gitRefLayouts: the app at the repository root naming .git/<file>, under
// both load restrictors, and a nested app naming ../../.git/<file>, which
// only LoadRestrictionsNone lets kustomize itself read.
var gitRefLayouts = []struct {
	name, sourcePath, prefix string
	restrictors              []string
}{
	{name: "root", sourcePath: ".", restrictors: []string{"LoadRestrictionsNone", "LoadRestrictionsRootOnly"}},
	{name: "nested", sourcePath: "apps/demo", prefix: "../../", restrictors: []string{"LoadRestrictionsNone"}},
}

func gitRestrictorOptions(name string) []string {
	if name == "LoadRestrictionsNone" {
		return referentLoadRestrictionsNone
	}
	return nil
}

// TestKustomizeRefsCannotReadGitMetadata covers every field kind on both
// render paths.
func TestKustomizeRefsCannotReadGitMetadata(t *testing.T) {
	for _, field := range gitRefFields {
		for _, layout := range gitRefLayouts {
			for _, restrictor := range layout.restrictors {
				for _, gitDir := range []string{".git", ".GIT"} {
					for _, prepared := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/%s/%s/prepared=%v", field.name, layout.name, restrictor, gitDir, prepared), func(t *testing.T) {
							root := t.TempDir()
							for name, content := range field.files {
								writeFile(t, filepath.Join(root, ".git", filepath.FromSlash(name)), content)
							}
							listing := filepath.Join(root, filepath.FromSlash(layout.sourcePath))
							writeFile(t, filepath.Join(listing, "cm.yaml"), decodeTestConfigMap("demo", "safe"))
							ref := layout.prefix + gitDir + "/" + field.target
							writeFile(t, filepath.Join(listing, "kustomization.yaml"), field.listing(t, listing, ref))
							if field.chart {
								writeValueChart(t, filepath.Join(listing, "charts", "chart"))
							}
							opts := RenderOptions{BuildOptions: gitRestrictorOptions(restrictor), EnableKSOPSCompat: field.ksops}
							if prepared {
								opts.Kustomize = &argoappv1.ApplicationSourceKustomize{NamePrefix: "p-"}
							}

							manifests, _, err := (KustomizeRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: root, Path: layout.sourcePath}, opts)
							assertGitMetadataNotRead(t, manifests, err)
						})
					}
				}
			}
		}
	}
}

// TestKustomizeAcquiredRemoteGitCannotReadItsGitMetadata pins the same for
// acquired remote Git content, bounded by the acquired repository: a real
// clone's .git holds its remote's configuration.
func TestKustomizeAcquiredRemoteGitCannotReadItsGitMetadata(t *testing.T) {
	for _, field := range []string{"configMapGenerator.files", "PatchTransformer.path"} {
		for _, gitDir := range []string{".git", ".GIT"} {
			t.Run(field+"/"+gitDir, func(t *testing.T) {
				root := t.TempDir()
				repo := filepath.Join(root, "repo")
				acquired := filepath.Join(root, "acquired")
				writeFile(t, filepath.Join(acquired, ".git", "config"), gitMetadataMarker)
				writeFile(t, filepath.Join(acquired, ".git", "patch.yaml"), paddedJSONPatch(gitMetadataMarker))
				base := filepath.Join(acquired, "base")
				writeFile(t, filepath.Join(base, "cm.yaml"), decodeTestConfigMap("remote", "safe"))
				switch field {
				case "configMapGenerator.files":
					writeFile(t, filepath.Join(base, "kustomization.yaml"), "resources:\n  - cm.yaml\nconfigMapGenerator:\n  - name: generated\n    files:\n      - ../"+gitDir+"/config\n")
				case "PatchTransformer.path":
					writeFile(t, filepath.Join(base, "kustomization.yaml"), "resources:\n  - cm.yaml\ntransformers:\n  - config.yaml\n")
					writeFile(t, filepath.Join(base, "config.yaml"), "apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: git\npath: ../"+gitDir+"/patch.yaml\ntarget:\n  kind: ConfigMap\n")
				}
				writeFile(t, filepath.Join(repo, "apps", "demo", "kustomization.yaml"), "resources:\n  - https://github.com/example/repo.git//base?ref=v1.2.3\n")

				manifests, _, err := (KustomizeRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: repo, Path: "apps/demo"}, RenderOptions{
					BuildOptions:           referentLoadRestrictionsNone,
					RemoteResourceAcquirer: &fakeRemoteAcquirer{path: acquired},
					RemoteResourceCacheDir: t.TempDir(),
				})
				assertGitMetadataNotRead(t, manifests, err)
			})
		}
	}
}

// TestHelmRendererCannotReadGitMetadata covers value files, value file
// globs and file parameters.
func TestHelmRendererCannotReadGitMetadata(t *testing.T) {
	for _, gitDir := range []string{".git", ".GIT"} {
		for _, input := range []string{"value file", "value file glob", "file parameter"} {
			if input == "value file glob" && gitDir != ".git" {
				// Glob matching compares names case-sensitively.
				continue
			}
			t.Run(input+"/"+gitDir, func(t *testing.T) {
				root := t.TempDir()
				writeValueChart(t, filepath.Join(root, "chart"))
				writeFile(t, filepath.Join(root, ".git", "config"), gitMetadataMarker)
				writeFile(t, filepath.Join(root, ".git", "values.yaml"), "value: "+gitMetadataMarker+"\n")
				opts := RenderOptions{AppName: "demo"}
				switch input {
				case "value file":
					opts.ValueFiles = []string{"../" + gitDir + "/values.yaml"}
				case "value file glob":
					// The pattern does not spell .git; its match does.
					opts.ValueFiles = []string{"../.gi?/values.yaml"}
				case "file parameter":
					opts.HelmFileParameters = []argoappv1.HelmFileParameter{{Name: "value", Path: "../" + gitDir + "/config"}}
				}

				manifests, _, err := (HelmRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: root, Path: "chart"}, opts)
				assertGitMetadataNotRead(t, manifests, err)
			})
		}
	}
}

// TestDirectoryRendererCannotReadGitMetadata covers Jsonnet imports from
// the application directory and from a library path, a library path inside
// .git, a source path inside .git, and a recursive walk of the repository
// root.
func TestDirectoryRendererCannotReadGitMetadata(t *testing.T) {
	const importing = "{apiVersion: 'v1', kind: 'ConfigMap', metadata: {name: 'j'}, data: {value: importstr 'GITDIR/config'}}\n"
	for _, gitDir := range []string{".git", ".GIT"} {
		for _, input := range []string{"jsonnet import", "jsonnet library import", "jsonnet library", "source path", "recursive walk"} {
			t.Run(input+"/"+gitDir, func(t *testing.T) {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git", "config"), gitMetadataMarker)
				writeFile(t, filepath.Join(root, ".git", "leak.yaml"), decodeTestConfigMap("gitcm", gitMetadataMarker))
				source := ResolvedSource{RepoRoot: root, Path: "."}
				opts := RenderOptions{}
				switch input {
				case "jsonnet import":
					writeFile(t, filepath.Join(root, "main.jsonnet"), strings.ReplaceAll(importing, "GITDIR", gitDir))
				case "jsonnet library import":
					source.Path = "apps/j"
					writeFile(t, filepath.Join(root, "apps", "j", "main.jsonnet"), strings.ReplaceAll(importing, "GITDIR", "lib/"+gitDir))
					writeFile(t, filepath.Join(root, "lib", gitDir, "config"), gitMetadataMarker)
					opts.Jsonnet.Libs = []string{"."}
				case "jsonnet library":
					source.Path = "apps/j"
					writeFile(t, filepath.Join(root, "apps", "j", "main.jsonnet"), strings.ReplaceAll(importing, "GITDIR/", ""))
					opts.Jsonnet.Libs = []string{gitDir}
				case "source path":
					source.Path = gitDir
				case "recursive walk":
					writeFile(t, filepath.Join(root, "cm.yaml"), decodeTestConfigMap("demo", "safe"))
					opts.DirectoryRecurse = true
				}

				manifests, _, err := (DirectoryRenderer{}).Render(context.Background(), source, opts)
				if input == "recursive walk" {
					// The walk skips .git and renders the rest.
					if err != nil || len(manifests) != 1 || renderedContains(t, manifests, gitMetadataMarker) {
						t.Fatalf("Render() = %d manifests, error %v; want only the root ConfigMap", len(manifests), err)
					}
					return
				}
				assertGitMetadataNotRead(t, manifests, err)
			})
		}
	}
}

// assertGitMetadataNotRead fails unless the render failed naming .git and
// nothing from .git reached the output or the error.
func assertGitMetadataNotRead(t *testing.T, manifests []Manifest, err error) {
	t.Helper()
	if renderedContains(t, manifests, gitMetadataMarker) || (err != nil && strings.Contains(err.Error(), gitMetadataMarker)) {
		t.Fatalf("Render() read .git content (error %v)", err)
	}
	if err == nil || !strings.Contains(err.Error(), "enters a .git directory") {
		t.Fatalf("Render() error = %v, want error containing %q", err, "enters a .git directory")
	}
}

// gitFilesChartTemplate renders ConfigMap name from the chart's .Files: an
// ordinary file (which must keep loading) and .git content — a .git
// directory's config or a .git file itself — by .Files.Get or by
// .Files.Glob.
func gitFilesChartTemplate(name, access string) string {
	head := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\ndata:\n  inside: {{ .Files.Get \"files/data.txt\" | quote }}\n"
	if access == "Files.Get" {
		return head + "  git: {{ .Files.Get \".git/config\" | quote }}\n  gitfile: {{ .Files.Get \".git\" | quote }}\n"
	}
	return head + "{{- with .Files.Glob \".git/*\" }}{{ .AsConfig | nindent 2 }}{{ end }}\n{{- with .Files.Glob \".git\" }}{{ .AsConfig | nindent 2 }}{{ end }}\n"
}

func writeGitFilesChart(t *testing.T, dir, name, access string) {
	t.Helper()
	writeNamedTestChart(t, dir, name, "0.1.0", gitFilesChartTemplate(name, access))
	writeFile(t, filepath.Join(dir, "files", "data.txt"), referentInsideMarker)
}

// TestHelmChartFilesCannotReadGitMetadata pins that Helm's .Files never
// holds .git content: Helm's loader reads every file of a chart directory,
// so a chart at the repository root (path: .), a chart that is itself a
// checkout, or a vendored subchart holding .git would otherwise hand
// .git/config to {{ .Files.Get }} and .Files.Glob. The chart still renders,
// with its other files, on the cached and the uncached load path (the
// cached one twice: first load, then from memory). A chart in a
// subdirectory never saw the repository's .git; that row is a control.
func TestHelmChartFilesCannotReadGitMetadata(t *testing.T) {
	layouts := []struct {
		name string
		// write builds the repository at root and returns the source path.
		write func(t *testing.T, root, access string) string
		leaks bool
	}{
		{name: "root chart", write: func(t *testing.T, root, access string) string {
			writeGitFilesChart(t, root, "demo", access)
			writeFile(t, filepath.Join(root, ".git", "config"), gitMetadataMarker)
			return "."
		}},
		{name: "root chart with .git file", write: func(t *testing.T, root, access string) string {
			// A worktree or submodule checkout: .git is a file naming the
			// real git directory, here a temporary directory outside the
			// repository whose name carries the marker.
			writeGitFilesChart(t, root, "demo", access)
			gitDir := filepath.Join(t.TempDir(), gitMetadataMarker+"-store")
			writeFile(t, filepath.Join(gitDir, "config"), gitMetadataMarker)
			writeFile(t, filepath.Join(root, ".git"), "gitdir: "+gitDir+"\n")
			return "."
		}},
		{name: "root chart with .git symlink", write: func(t *testing.T, root, access string) string {
			writeGitFilesChart(t, root, "demo", access)
			gitDir := t.TempDir()
			writeFile(t, filepath.Join(gitDir, "config"), gitMetadataMarker)
			symlink(t, gitDir, filepath.Join(root, ".git"))
			return "."
		}},
		{name: "chart holding .git", write: func(t *testing.T, root, access string) string {
			writeGitFilesChart(t, filepath.Join(root, "chart"), "demo", access)
			writeFile(t, filepath.Join(root, "chart", ".git", "config"), gitMetadataMarker)
			return "chart"
		}},
		{name: "subchart holding .git", write: func(t *testing.T, root, access string) string {
			writeNamedTestChart(t, filepath.Join(root, "chart"), "demo", "0.1.0", decodeTestConfigMap("parent", "safe"))
			writeGitFilesChart(t, filepath.Join(root, "chart", "charts", "sub"), "sub", access)
			writeFile(t, filepath.Join(root, "chart", "charts", "sub", ".git", "config"), gitMetadataMarker)
			return "chart"
		}},
		{name: "subdirectory chart", write: func(t *testing.T, root, access string) string {
			writeGitFilesChart(t, filepath.Join(root, "chart"), "demo", access)
			writeFile(t, filepath.Join(root, ".git", "config"), gitMetadataMarker)
			return "chart"
		}},
	}
	for _, layout := range layouts {
		for _, access := range []string{"Files.Get", "Files.Glob"} {
			for _, cached := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/cached=%v", layout.name, access, cached), func(t *testing.T) {
					root := t.TempDir()
					sourcePath := layout.write(t, root, access)
					opts := RenderOptions{AppName: "demo"}
					renders := 1
					if cached {
						opts.HelmChartLoadCache = NewHelmChartLoadCache()
						renders = 2
					}
					for range renders {
						manifests, _, err := (HelmRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: root, Path: sourcePath}, opts)
						if err != nil {
							t.Fatalf("Render() error = %v", err)
						}
						if renderedContains(t, manifests, gitMetadataMarker) {
							t.Fatalf("Render() exposed .git content through .Files: %#v", manifests)
						}
						if !renderedContains(t, manifests, referentInsideMarker) {
							t.Fatalf("Render() lost the chart's own files: %#v", manifests)
						}
					}
				})
			}
		}
	}
}

// TestKustomizeHelmChartFilesCannotReadGitMetadata pins the same for a
// kustomize helmCharts local chart, which renders from the prepared
// workspace: its copy leaves .git out.
func TestKustomizeHelmChartFilesCannotReadGitMetadata(t *testing.T) {
	for _, access := range []string{"Files.Get", "Files.Glob"} {
		t.Run(access, func(t *testing.T) {
			root := t.TempDir()
			listing := filepath.Join(root, "apps", "demo")
			writeGitFilesChart(t, filepath.Join(listing, "charts", "demo"), "demo", access)
			writeFile(t, filepath.Join(listing, "charts", "demo", ".git", "config"), gitMetadataMarker)
			writeFile(t, filepath.Join(listing, "kustomization.yaml"), "helmCharts:\n  - name: demo\n    releaseName: demo\n")

			manifests, _, err := (KustomizeRenderer{}).Render(context.Background(), ResolvedSource{RepoRoot: root, Path: "apps/demo"}, RenderOptions{})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if renderedContains(t, manifests, gitMetadataMarker) || !renderedContains(t, manifests, referentInsideMarker) {
				t.Fatalf("Render() = %#v, want the chart's files without .git", manifests)
			}
		})
	}
}
