package render

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	helmchart "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/chart/loader/archive"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartv2loader "helm.sh/helm/v4/pkg/chart/v2/loader"
)

// HelmChartLoadCache memoizes chart file reads and tree validation for the
// lifetime of one render session. Load returns a freshly parsed chart on every
// call so Helm dependency processing cannot leak mutations between renders.
type HelmChartLoadCache struct {
	mu      sync.Mutex
	entries map[string]*helmChartCacheEntry
}

type helmChartCacheEntry struct {
	once sync.Once
	// hasGit records that the chart tree holds a .git entry, so every load
	// reads a copy without it (loadHelmChartDir).
	hasGit      bool
	files       []helmChartRawFile
	cacheable   bool
	validateErr error
	loadErr     error
}

type helmChartRawFile struct {
	name    string
	modTime time.Time
	data    []byte
}

func NewHelmChartLoadCache() *HelmChartLoadCache {
	return &HelmChartLoadCache{entries: map[string]*helmChartCacheEntry{}}
}

func (cache *HelmChartLoadCache) Load(chartPath string) (helmchart.Charter, error) {
	if cache == nil {
		return loadValidatedHelmChart(chartPath)
	}
	cache.mu.Lock()
	entry, ok := cache.entries[chartPath]
	if !ok {
		entry = &helmChartCacheEntry{}
		cache.entries[chartPath] = entry
	}
	cache.mu.Unlock()

	entry.once.Do(func() {
		entry.hasGit, entry.validateErr = validateHelmChartTree(chartPath)
		if entry.validateErr != nil {
			return
		}
		loaded, err := loadHelmChartDir(chartPath, entry.hasGit)
		if err != nil {
			entry.loadErr = err
			return
		}
		v2chart, ok := loaded.(*chartv2.Chart)
		if !ok {
			return
		}
		entry.files = rawFilesFromV2Chart(v2chart)
		entry.cacheable = true
	})
	if entry.validateErr != nil {
		return nil, entry.validateErr
	}
	if entry.loadErr != nil {
		return nil, entry.loadErr
	}
	if !entry.cacheable {
		return loadHelmChartDir(chartPath, entry.hasGit)
	}
	return chartv2loader.LoadFiles(bufferedFilesFromRawFiles(entry.files))
}

func loadValidatedHelmChart(chartPath string) (helmchart.Charter, error) {
	hasGit, err := validateHelmChartTree(chartPath)
	if err != nil {
		return nil, err
	}
	return loadHelmChartDir(chartPath, hasGit)
}

// loadHelmChartDir loads the chart directory at chartPath. When the tree
// holds a .git entry (validateHelmChartTree) it loads a temporary copy made
// by copyRegularTree, which leaves out .git at every depth and refuses
// symlinks, so no .Files lookup can reach repository metadata. A chart at a
// repository root keeps rendering. Helm reads the whole chart into memory,
// so the copy is removed before returning.
func loadHelmChartDir(chartPath string, hasGit bool) (helmchart.Charter, error) {
	if !hasGit {
		return loader.Load(chartPath)
	}
	tempDir, err := os.MkdirTemp("", "drydock-helm-chart-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary helm chart copy: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	chartCopy := filepath.Join(tempDir, "chart")
	if err := copyRegularTree(chartPath, chartCopy); err != nil {
		return nil, fmt.Errorf("copy helm chart without .git: %w", err)
	}
	return loader.Load(chartCopy)
}

func rawFilesFromV2Chart(chart *chartv2.Chart) []helmChartRawFile {
	files := make([]helmChartRawFile, 0, len(chart.Raw))
	for _, file := range chart.Raw {
		files = append(files, helmChartRawFile{
			name:    file.Name,
			modTime: file.ModTime,
			data:    append([]byte(nil), file.Data...),
		})
	}
	return files
}

func bufferedFilesFromRawFiles(files []helmChartRawFile) []*archive.BufferedFile {
	out := make([]*archive.BufferedFile, 0, len(files))
	for _, file := range files {
		out = append(out, &archive.BufferedFile{
			Name:    file.name,
			ModTime: file.modTime,
			Data:    append([]byte(nil), file.data...),
		})
	}
	return out
}
