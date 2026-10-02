package builds

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// App is one app a project can build: an Xcode project or workspace and its
// schemes, or a Gradle root and its application modules. Path is relative to
// the project; Targets and Configurations are what Start takes.
type App struct {
	Platform             string   `json:"platform"`
	Path                 string   `json:"path"`
	Name                 string   `json:"name"`
	BundleID             string   `json:"bundle_id"`
	Targets              []string `json:"targets" wire:"array"`
	DefaultTarget        string   `json:"default_target"`
	Configurations       []string `json:"configurations" wire:"array"`
	DefaultConfiguration string   `json:"default_configuration"`
}

const (
	// maxApps bounds one project's list; a monorepo's examples are not the app.
	maxApps = 20
	// searchDepth is how deep under the project an app is looked for.
	searchDepth = 6
)

// Build output and dependency trees hold thousands of folders and no app worth listing.
var skipDirs = map[string]bool{
	".git": true, ".gradle": true, ".idea": true, "node_modules": true, ".build": true, "build": true,
	"DerivedData": true, "Pods": true, "Carthage": true, ".next": true, "dist": true, "out": true,
	"vendor": true, ".turbo": true, "target": true, ".venv": true, "venv": true, ".cache": true, ".yarn": true,
}

// Apps lists what a project can build, iOS first. iOS apps are listed on any
// machine, so the phone can say why this one cannot build them.
func (s *Service) Apps(project string) ([]App, error) {
	root, err := s.cfg.Paths.Contain(project)
	if err != nil {
		return nil, err
	}
	var bundles, gradleRoots []string
	_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !e.IsDir() {
			if (e.Name() == "settings.gradle" || e.Name() == "settings.gradle.kts") && isFile(filepath.Join(filepath.Dir(path), "gradlew")) {
				gradleRoots = append(gradleRoots, filepath.Dir(path))
			}
			return nil
		}
		if path == root {
			return nil
		}
		// A bundle is a folder; inside it is only Xcode's own project.xcworkspace.
		if ext := filepath.Ext(path); ext == ".xcodeproj" || ext == ".xcworkspace" {
			bundles = append(bundles, path)
			return filepath.SkipDir
		}
		if skipDirs[e.Name()] || strings.Count(strings.TrimPrefix(path, root), string(filepath.Separator)) > searchDepth {
			return filepath.SkipDir
		}
		return nil
	})

	apps := []App{}
	for _, bundle := range preferWorkspaces(bundles) {
		apps = append(apps, xcodeApp(root, bundle))
	}
	for _, dir := range shallowestFirst(unique(gradleRoots)) {
		if app, ok := gradleApp(root, dir); ok {
			apps = append(apps, app)
		}
	}
	if len(apps) > maxApps {
		apps = apps[:maxApps]
	}
	return apps, nil
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

func shallowestFirst(paths []string) []string {
	sort.SliceStable(paths, func(i, j int) bool { return len(paths[i]) < len(paths[j]) })
	return paths
}

func unique(paths []string) []string {
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// pick is want when it is one of choices, else fallback when it is, else the first.
func pick(choices []string, want, fallback string) string {
	for _, c := range []string{want, fallback} {
		for _, choice := range choices {
			if choice == c {
				return c
			}
		}
	}
	if len(choices) > 0 {
		return choices[0]
	}
	return ""
}
