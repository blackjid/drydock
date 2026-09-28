package app

import (
	"context"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/sholdee/drydock/internal/ociartifact"
	"github.com/sholdee/drydock/internal/render"
	sourcepkg "github.com/sholdee/drydock/internal/source"
)

// withKustomizeSelectionPaths returns copies of inputs whose Paths also carry
// the local Kustomize graph inputs (bases, components, helmCharts values
// files, patches, generator files) of every source that renders from root.
// It exists for changed-only ownership only: ApplicationSelectionInput.Paths
// from discovery also key the persistent render cache, so the listed inputs
// are never mutated. A source whose graph cannot be read keeps its
// spec.source.path ownership.
func withKustomizeSelectionPaths(ctx context.Context, root string, repoMaps []sourcepkg.RepoMap, inputs []ApplicationSelectionInput) []ApplicationSelectionInput {
	resolver := sourcepkg.NewResolver(sourcepkg.Options{RepoMaps: repoMaps})
	memo := map[string][]string{}
	out := make([]ApplicationSelectionInput, len(inputs))
	for i, input := range inputs {
		out[i] = ApplicationSelectionInput{
			Application: input.Application,
			Paths:       append([]string(nil), input.Paths...),
		}
		for _, source := range applicationSources(input.Application) {
			if !kustomizeSelectionSourceIsLocal(root, resolver, source) {
				continue
			}
			paths, ok := memo[source.Path]
			if !ok {
				paths, _ = render.KustomizeSelectionPaths(ctx, root, source.Path)
				memo[source.Path] = paths
			}
			out[i].Paths = append(out[i].Paths, paths...)
		}
	}
	return out
}

func applicationSources(app argoappv1.Application) argoappv1.ApplicationSources {
	if len(app.Spec.Sources) == 0 && app.Spec.Source != nil {
		return argoappv1.ApplicationSources{*app.Spec.Source}
	}
	return app.Spec.Sources
}

// kustomizeSelectionSourceIsLocal mirrors localProvider.resolveSourceRootIdentity:
// a path source renders from the local tree when it is not repo-mapped
// elsewhere, not OCI, and its path exists under root — whatever its repoURL. Sources fetched
// from another repository never contribute local ownership. Explicit
// non-Kustomize types are skipped; implicit ones are detected by
// KustomizeSelectionPaths finding (or not) a kustomization file.
func kustomizeSelectionSourceIsLocal(root string, resolver *sourcepkg.Resolver, source argoappv1.ApplicationSource) bool {
	if source.Path == "" || source.Chart != "" {
		return false
	}
	if mappedPath, mapped := resolver.MappedPath(source.RepoURL); mapped && !sameLocalPath(mappedPath, root) {
		return false
	}
	if ociartifact.IsOCIURL(source.RepoURL) {
		return false
	}
	explicitType, err := source.ExplicitType()
	if err != nil || explicitType != nil && *explicitType != argoappv1.ApplicationSourceTypeKustomize {
		return false
	}
	exists, err := sourcePathExists(root, source.Path)
	return err == nil && exists
}
