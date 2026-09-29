package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/provider"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/api/types"
)

// Render walks hold the files builtin plugin configs read to the rules every
// other kustomization ref follows. Kustomize configures each plugin a
// generators:, transformers: or validators: entry names with the listing
// kustomization's loader, and that loader's Load fetches an http(s) path
// itself, before any load restrictor applies (kustomize api
// internal/loader/fileloader.go, FileLoader.Load). Under
// LoadRestrictionsNone it then reads any path and follows any symlink;
// LoadRestrictionsRootOnly confines local reads to the listing directory
// after resolving symlinks (internal/loader/loadrestrictions.go,
// RestrictionRootOnly) but leaves http(s) paths alone. So every referent is
// checked whatever the load restrictor: it must be local, relative, inside
// the repository root, and free of symlinks.
//
// The check reproduces what kustomize reads rather than what the digest
// walk records: entries are split into inline documents and paths with
// kustomize's own resource factory, config documents are decoded by it
// (a kind: List expands into its items), and paths are used exactly as
// written, untrimmed. A config whose referents cannot be enumerated fails
// the render: the files it reads could be anywhere, including a remote URL
// no load restrictor stops.

// kustomizePluginReferentCheck is the state a render walk shares with the
// walks of the directory entries it meets.
type kustomizePluginReferentCheck struct {
	factory *resmap.Factory
	// walked records listing directory and directory entry pairs already
	// being checked, so entries that list each other terminate.
	walked map[string]struct{}
}

func newKustomizePluginReferentCheck() *kustomizePluginReferentCheck {
	return &kustomizePluginReferentCheck{
		factory: resmap.NewFactory(provider.NewDefaultDepProvider().GetResourceFactory()),
		walked:  make(map[string]struct{}),
	}
}

// validatePluginConfigReferents checks the referents of every builtin plugin
// config the generators:, transformers: and validators: entries of the
// kustomization in dir name, resolved against dir. It returns the
// in-repository files and directories those configs and their directory
// entries read, for the prepared workspace to copy.
func (v *kustomizeGraphValidator) validatePluginConfigReferents(ctx context.Context, dir string, kustomization *types.Kustomization) ([]string, error) {
	var inputs []string
	for _, list := range []struct {
		field   string
		entries []string
	}{
		{field: "generators", entries: kustomization.Generators},
		{field: "transformers", entries: kustomization.Transformers},
		{field: "validators", entries: kustomization.Validators},
	} {
		for _, entry := range list.entries {
			entryInputs, err := v.validatePluginConfigEntry(ctx, dir, list.field, entry)
			if err != nil {
				return nil, err
			}
			inputs = append(inputs, entryInputs...)
		}
	}
	return inputs, nil
}

// validatePluginConfigEntry checks one entry. Kustomize parses an entry as
// inline documents when its resource factory accepts it and as a path
// otherwise (kusttarget configureExternalGenerators and
// configureExternalTransformers). A remote path is left to the graph walk's
// remote policy: the prepared workspace acquires it and the walk of the
// workspace checks the acquired copy.
func (v *kustomizeGraphValidator) validatePluginConfigEntry(ctx context.Context, dir, field, entry string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(entry) == "" {
		return nil, nil
	}
	if inline, err := v.pluginReferents.factory.NewResMapFromBytes([]byte(entry)); err == nil {
		label := describeInlinePluginEntry(inline.Resources())
		inputs, err := v.validatePluginConfigResources(dir, field, inline.Resources())
		if err != nil {
			return nil, fmt.Errorf("kustomize %s entry %q: %w", field, label, err)
		}
		return inputs, nil
	}
	if isRemoteKustomizeRef(entry) {
		return nil, nil
	}
	path, info, err := v.validateLocalRef(dir, field, entry)
	if err != nil {
		return nil, err
	}
	if info == nil {
		// Missing: kustomize fails the build before configuring any plugin.
		return nil, nil
	}
	var (
		resources []*resource.Resource
		inputs    []string
	)
	switch {
	case info.IsDir():
		resources, inputs, err = v.pluginDirectoryEntryResources(ctx, dir, field, entry, path)
	case info.Mode().IsRegular():
		resources, err = v.pluginFileResources(path)
	default:
		err = fmt.Errorf("is not a regular file or directory")
	}
	if err != nil {
		return nil, fmt.Errorf("kustomize %s %q: %w", field, entry, err)
	}
	referents, err := v.validatePluginConfigResources(dir, field, resources)
	if err != nil {
		return nil, fmt.Errorf("kustomize %s %q: %w", field, entry, err)
	}
	return append(referents, inputs...), nil
}

// pluginFileResources decodes a config file the way kustomize accumulates
// it.
func (v *kustomizeGraphValidator) pluginFileResources(path string) ([]*resource.Resource, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return v.pluginReferents.configResources(content), nil
}

// configResources decodes config documents with kustomize's resource
// factory. Content kustomize cannot decode fails the build before any plugin
// is configured, so it yields no configs to check.
func (c *kustomizePluginReferentCheck) configResources(content []byte) []*resource.Resource {
	resources, err := c.factory.NewResMapFromBytes(content)
	if err != nil {
		return nil
	}
	return resources.Resources()
}

// pluginDirectoryEntryResources returns the config documents of a directory
// entry. Kustomize builds the directory as a full kustomization target
// whatever the load restrictor (FileLoader.New confines nothing) and
// configures every resulting resource as a plugin, so its graph is walked
// with the usual boundary, symlink and remote checks — remote refs are not
// acquired inside it — and its configs are enumerable only when every
// kustomization in it lists plain resources: any other field could generate
// or rewrite the configs. It also returns the graph's directories and
// resources for the prepared workspace to copy.
func (v *kustomizeGraphValidator) pluginDirectoryEntryResources(ctx context.Context, listingDir, field, entry, entryDir string) ([]*resource.Resource, []string, error) {
	key := filepath.Clean(listingDir) + "\x00" + filepath.Clean(entryDir)
	if _, ok := v.pluginReferents.walked[key]; ok {
		return nil, nil, nil
	}
	v.pluginReferents.walked[key] = struct{}{}

	entryGraph := &kustomizeGraphValidator{
		repoRoot:        v.repoRoot,
		sourceRoot:      filepath.Clean(entryDir),
		visited:         make(map[string]struct{}),
		pluginReferents: v.pluginReferents,
	}
	if _, err := entryGraph.validateKustomizationDir(ctx, entryDir, ""); err != nil {
		return nil, nil, err
	}
	var (
		resources []*resource.Resource
		inputs    []string
	)
	// entryGraph.nodes may grow while it is read: a directory resource is
	// walked again here (a no-op once visited) so no node escapes the
	// plain-resources rule.
	for read := 0; read < len(entryGraph.nodes); {
		node := entryGraph.nodes[read]
		read++
		if err := requirePlainResourcesKustomization(node); err != nil {
			return nil, nil, fmt.Errorf("%w, so the files its builtin plugin configs read cannot be enumerated", err)
		}
		inputs = append(inputs, node.Dir)
		for _, ref := range node.Kustomization.Resources {
			if ref == "" {
				continue
			}
			if isRemoteKustomizeRef(ref) {
				return nil, nil, fmt.Errorf("%s: %w", node.ManifestPath, unsupportedRemoteKustomizeRefError("resources", ref))
			}
			path, info, err := entryGraph.validateLocalRef(node.Dir, "resources", ref)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", node.ManifestPath, err)
			}
			if info == nil {
				continue
			}
			inputs = append(inputs, path)
			if info.IsDir() {
				if _, err := entryGraph.validateKustomizationDir(ctx, path, ""); err != nil {
					return nil, nil, err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("%s: resource %q is not a regular file or directory", node.ManifestPath, ref)
			}
			fileResources, err := v.pluginFileResources(path)
			if err != nil {
				return nil, nil, err
			}
			resources = append(resources, fileResources...)
		}
	}
	return resources, inputs, nil
}

// validatePluginConfigResources checks the referents of builtin plugin
// configs against listingDir. Kustomize configures builtin kinds only (the
// plugin config drydock renders with), so other documents — KSOPS
// generators among them, which are emulated and checked separately — have
// no referents; so has HelmChartInflationGenerator, whose Config fails
// before reading anything because Helm is disabled. A builtin kind missing
// from builtinPluginReferentExtractors fails closed. It returns the
// referents that are files inside the repository.
func (v *kustomizeGraphValidator) validatePluginConfigResources(listingDir, field string, resources []*resource.Resource) ([]string, error) {
	var inputs []string
	for _, res := range resources {
		gvk := res.GetGvk()
		if gvk.Group != "" || gvk.Version != konfig.BuiltinPluginApiVersion || gvk.Kind == "HelmChartInflationGenerator" {
			continue
		}
		if _, known := builtinPluginReferentExtractors[gvk.Kind]; !known {
			return nil, fmt.Errorf("builtin plugin kind %q is unknown; its referents cannot be enumerated", gvk.Kind)
		}
		content, err := res.AsYAML()
		if err != nil {
			return nil, fmt.Errorf("builtin %s config: %w", gvk.Kind, err)
		}
		referents, err := builtinPluginReferents(gvk.Kind, content)
		if err != nil {
			return nil, fmt.Errorf("decode builtin %s config: %w; its referents cannot be enumerated", gvk.Kind, err)
		}
		for _, referent := range referents {
			if referent.path == "" {
				continue
			}
			referentField := field + "." + gvk.Kind + "." + referent.field
			if isRemoteKustomizeRef(referent.path) {
				return nil, fmt.Errorf("kustomize %s %q is a remote ref; remote builtin plugin config referents are unsupported", referentField, redactKustomizeRef(referent.path))
			}
			path, info, err := v.validateLocalRef(listingDir, referentField, referent.path)
			if err != nil {
				return nil, err
			}
			if info != nil && info.Mode().IsRegular() {
				inputs = append(inputs, path)
			}
		}
	}
	return inputs, nil
}

func describeInlinePluginEntry(resources []*resource.Resource) string {
	if len(resources) == 0 {
		return "inline"
	}
	gvk := resources[0].GetGvk()
	return "inline " + gvk.ApiVersion() + "/" + gvk.Kind
}
