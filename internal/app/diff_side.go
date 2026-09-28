package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/sholdee/drydock/internal/acquisition"
	"github.com/sholdee/drydock/internal/cacheevent"
	"github.com/sholdee/drydock/internal/change"
	"github.com/sholdee/drydock/internal/diagnostic"
)

func (o Orchestrator) buildDiffSides(ctx context.Context, request DiffRequest) (BuildResult, BuildResult, []diagnostic.Diagnostic, error) {
	forbiddenRoots := diffForbiddenRoots(request)
	if err := validateDiffCacheRoots(request, forbiddenRoots); err != nil {
		return BuildResult{}, BuildResult{}, nil, err
	}

	leftBuildRequest := request.buildRequest(request.LeftPath, forbiddenRoots)
	rightBuildRequest := request.buildRequest(request.RightPath, forbiddenRoots)
	leftBuildRequest.rootRevision = request.leftPathRevision
	rightBuildRequest.rootRevision = request.rightPathRevision
	parallelism, err := normalizeParallelism(request.Parallelism)
	if err != nil {
		return BuildResult{}, BuildResult{}, nil, err
	}
	leftParallelism, rightParallelism, concurrent := splitSideParallelism(parallelism)
	leftBuildRequest.Parallelism = leftParallelism
	rightBuildRequest.Parallelism = rightParallelism
	snapshotSession, err := acquisition.NewSnapshotSession("drydock-cache-snapshots-*")
	if err != nil {
		return BuildResult{}, BuildResult{}, nil, err
	}
	defer snapshotSession.Close()
	leftBuildRequest.snapshotSession = snapshotSession
	rightBuildRequest.snapshotSession = snapshotSession

	var diagnostics []diagnostic.Diagnostic
	var leftList, rightList diffSideOutcome
	if request.ChangedOnly {
		changedPaths, err := filteredChangedOnlyPaths(request)
		if err != nil {
			return BuildResult{}, BuildResult{}, diagnostics, err
		}
		if len(changedPaths) == 0 {
			return BuildResult{}, BuildResult{}, diagnostics, nil
		}

		leftList, rightList = runDiffSidePair(ctx, concurrent, o.ListApplications, leftBuildRequest, rightBuildRequest)
		diagnostics = append(diagnostics, leftList.result.Diagnostics...)
		diagnostics = append(diagnostics, rightList.result.Diagnostics...)
		if err := errors.Join(leftList.err, rightList.err); err != nil {
			return BuildResult{}, BuildResult{}, diagnostics, err
		}
		leftBuildRequest.PluginOptions = leftList.result.pluginOptions
		leftBuildRequest.renderCache = leftList.result.renderCache
		leftBuildRequest.renderSettingsSignature = leftList.result.renderSettingsSignature
		leftBuildRequest.discovered = leftList.result.discovered
		rightBuildRequest.PluginOptions = rightList.result.pluginOptions
		rightBuildRequest.renderCache = rightList.result.renderCache
		rightBuildRequest.renderSettingsSignature = rightList.result.renderSettingsSignature
		rightBuildRequest.discovered = rightList.result.discovered

		// Each side owns the Kustomize graph of its own tree, so a base file
		// added or deleted by the change is owned by the side that has it.
		leftInputs := withKustomizeSelectionPaths(ctx, leftBuildRequest.Path, leftBuildRequest.RepoMaps, leftList.result.ApplicationInputs)
		rightInputs := withKustomizeSelectionPaths(ctx, rightBuildRequest.Path, rightBuildRequest.RepoMaps, rightList.result.ApplicationInputs)
		leftSelected, rightSelected, unowned := selectChangedDiffSides(leftInputs, rightInputs, changedPaths)
		if len(unowned) > 0 {
			diag := diagnostic.Diagnostic{
				Severity: diagnostic.SeverityWarning,
				Category: "changed-only",
				Message:  fmt.Sprintf("changed-only could not map %d changed path(s); rendering all Applications: %s", len(unowned), strings.Join(unowned, ", ")),
			}
			diagnostics = append(diagnostics, diag)
			if request.StrictChangedOnly || request.Strict {
				diagnostics[len(diagnostics)-1].Severity = diagnostic.SeverityError
				return BuildResult{}, BuildResult{}, diagnostics, fmt.Errorf("changed-only input ownership incomplete")
			}
			leftBuildRequest.Applications = leftList.result.Applications
			rightBuildRequest.Applications = rightList.result.Applications
		} else {
			leftBuildRequest.Applications = leftSelected
			rightBuildRequest.Applications = rightSelected
		}
	}

	leftBuild, rightBuild := runDiffSidePair(ctx, concurrent, o.Build, leftBuildRequest, rightBuildRequest)
	leftBuild.result.CacheEvents = append(append([]cacheevent.Event(nil), leftList.result.CacheEvents...), leftBuild.result.CacheEvents...)
	rightBuild.result.CacheEvents = append(append([]cacheevent.Event(nil), rightList.result.CacheEvents...), rightBuild.result.CacheEvents...)
	diagnostics = append(diagnostics, leftBuild.result.Diagnostics...)
	diagnostics = append(diagnostics, rightBuild.result.Diagnostics...)
	if err := errors.Join(leftBuild.err, rightBuild.err); err != nil {
		return leftBuild.result, rightBuild.result, diagnostics, err
	}
	return leftBuild.result, rightBuild.result, diagnostics, nil
}

func filteredChangedOnlyPaths(request DiffRequest) ([]string, error) {
	filter, err := changedOnlyPathFilter(request)
	if err != nil {
		return nil, err
	}
	changedPaths := request.changedPaths
	if changedPaths == nil {
		changedPaths, err = change.Detect(request.LeftPath, request.RightPath)
		if err != nil {
			return nil, err
		}
	}
	return filter.Apply(changedPaths).Paths, nil
}

func changedOnlyPathFilter(request DiffRequest) (change.PathFilter, error) {
	return change.NewPathFilter(change.PathFilterConfig{
		Includes: request.ChangedOnlyIncludeGlobs,
		Ignores:  request.ChangedOnlyIgnoreGlobs,
	})
}

// selectChangedDiffSides selects, on each side, every Application that either
// side's inputs select, plus the changed paths neither side owns. Ownership
// can differ per tree — a file the change deletes is only in the left tree's
// Kustomize graph — and rendering an Application on one side only would
// report a false all-added or all-deleted diff instead of the real change or
// render failure.
func selectChangedDiffSides(leftInputs, rightInputs []ApplicationSelectionInput, changedPaths []string) ([]argoappv1.Application, []argoappv1.Application, []string) {
	leftSelected, leftUnowned := SelectChangedApplicationInputs(leftInputs, changedPaths)
	rightSelected, rightUnowned := SelectChangedApplicationInputs(rightInputs, changedPaths)
	selectedKeys := make(map[string]struct{}, len(leftSelected)+len(rightSelected))
	for _, application := range append(append([]argoappv1.Application(nil), leftSelected...), rightSelected...) {
		selectedKeys[applicationKey(application)] = struct{}{}
	}
	return selectedByKey(leftInputs, selectedKeys), selectedByKey(rightInputs, selectedKeys), unownedByNeitherSide(leftUnowned, rightUnowned)
}

func selectedByKey(inputs []ApplicationSelectionInput, keys map[string]struct{}) []argoappv1.Application {
	selected := make([]argoappv1.Application, 0, len(keys))
	for _, input := range inputs {
		if _, ok := keys[applicationKey(input.Application)]; ok {
			selected = append(selected, input.Application)
		}
	}
	return selected
}

func unownedByNeitherSide(leftUnowned, rightUnowned []string) []string {
	left := make(map[string]struct{}, len(leftUnowned))
	for _, changedPath := range leftUnowned {
		left[changedPath] = struct{}{}
	}

	var unowned []string
	for _, changedPath := range rightUnowned {
		if _, ok := left[changedPath]; ok {
			unowned = append(unowned, changedPath)
		}
	}
	return unowned
}
