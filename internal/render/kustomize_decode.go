package render

import (
	"fmt"

	"sigs.k8s.io/kustomize/api/types"
)

// decodeKustomization decodes a kustomization file the way krusty loads it
// (KustTarget.Load) and kustomize edit reads it, so every walk of the graph
// reads the fields the render reads:
//
//   - types.Kustomization.Unmarshal converts the YAML to JSON and decodes it
//     with encoding/json and DisallowUnknownFields. Keys match fields
//     case-insensitively (Resources: is resources:). The conversion sorts
//     keys and the last one decoded wins, so of two keys folding to one field
//     resources: beats Resources: wherever they sit, and a repeated key keeps
//     its last value. An unknown key or a mistyped value fails.
//   - FixKustomization folds the deprecated fields into their replacements:
//     bases into resources, imageTags into images, a generator's env into its
//     envs. Nothing reads the deprecated fields after it.
//
// helmChartInflationGenerator is unsupported; it is rejected, as a policy
// error rather than a decode error, before FixKustomization would fold it
// into helmCharts. Both errors name manifestPath. krusty's other load steps
// only warn (CheckDeprecatedFields) or fail (CheckEmpty, EnforceFields and,
// when a directory is accumulated, the Component kind check) without changing
// which fields are read, so they are left to the render.
func decodeKustomization(manifestPath string, content []byte) (types.Kustomization, error) {
	var kustomization types.Kustomization
	if err := kustomization.Unmarshal(content); err != nil {
		return types.Kustomization{}, fmt.Errorf("decode kustomization %s: %w", manifestPath, err)
	}
	if len(kustomization.HelmChartInflationGenerator) != 0 {
		return types.Kustomization{}, fmt.Errorf("%s: helmChartInflationGenerator is deprecated and unsupported", manifestPath)
	}
	kustomization.FixKustomization()
	return kustomization, nil
}
