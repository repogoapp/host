package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const androidTimeout = 30 * time.Minute

// Module and variant become a Gradle task name, so they are held to Gradle's own characters.
var (
	modulePattern   = regexp.MustCompile(`^:[A-Za-z0-9_.-]+(?::[A-Za-z0-9_.-]+)*$`)
	variantPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	includedPattern = regexp.MustCompile(`["'](:[A-Za-z0-9_.:-]+)["']`)
	rootNamePattern = regexp.MustCompile(`rootProject\.name\s*=\s*["']([^"']+)["']`)
)

// gradleApp is a Gradle root with a wrapper and at least one application
// module; a library module has no APK to install.
func gradleApp(root, dir string) (App, bool) {
	settings := readFirst(filepath.Join(dir, "settings.gradle.kts"), filepath.Join(dir, "settings.gradle"))
	var modules []string
	seen := map[string]bool{}
	for _, m := range includedPattern.FindAllStringSubmatch(settings, -1) {
		if !seen[m[1]] && isApplication(moduleDir(dir, m[1])) {
			seen[m[1]] = true
			modules = append(modules, m[1])
		}
	}
	if len(modules) == 0 && isApplication(filepath.Join(dir, "app")) {
		modules = []string{":app"}
	}
	if len(modules) == 0 {
		return App{}, false
	}
	sort.Strings(modules)
	name := filepath.Base(dir)
	if m := rootNamePattern.FindStringSubmatch(settings); m != nil {
		name = m[1]
	}
	// Every application module has these two; flavors would need Gradle's model, a
	// full configuration pass. A typed flavored variant still builds.
	configurations := []string{"debug", "release"}
	return App{
		Platform: PlatformAndroid, Path: relative(root, dir), Name: name,
		Targets: modules, DefaultTarget: pick(modules, ":app", ""),
		Configurations: configurations, DefaultConfiguration: configurations[0],
	}, true
}

func isApplication(dir string) bool {
	text := readFirst(filepath.Join(dir, "build.gradle.kts"), filepath.Join(dir, "build.gradle"))
	return strings.Contains(text, "com.android.application") || strings.Contains(text, "android.application")
}

func moduleDir(root, module string) string {
	return filepath.Join(append([]string{root}, strings.Split(strings.TrimPrefix(module, ":"), ":")...)...)
}

func readFirst(paths ...string) string {
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	return ""
}

// checkAndroid validates an Android request, filling its defaults, and returns its pipeline.
func (s *Service) checkAndroid(b *Build) (func(context.Context, *Build, string, io.Writer) error, error) {
	if b.Path == "" {
		b.Path = "."
	}
	root, err := s.cfg.Paths.Contain(filepath.Join(b.Project, b.Path))
	if err != nil {
		return nil, err
	}
	if !isFile(filepath.Join(root, "gradlew")) {
		return nil, fmt.Errorf("%w: path must be a Gradle root with a gradlew wrapper", ErrInvalid)
	}
	if b.Target == "" {
		b.Target = ":app"
	}
	if b.Configuration == "" {
		b.Configuration = "debug"
	}
	if !modulePattern.MatchString(b.Target) {
		return nil, fmt.Errorf("%w: target must be a Gradle module path such as :app", ErrInvalid)
	}
	if !variantPattern.MatchString(b.Configuration) {
		return nil, fmt.Errorf("%w: configuration must be a variant name such as debug", ErrInvalid)
	}
	b.Method = ""
	return func(ctx context.Context, b *Build, dir string, log io.Writer) error {
		return s.assemble(ctx, b, root, dir, log)
	}, nil
}

// assemble runs the module's assemble task and keeps the APKs Gradle's own
// output-metadata.json names. Signing is the project's signingConfig.
func (s *Service) assemble(ctx context.Context, b *Build, root, dir string, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, androidTimeout)
	defer cancel()

	s.phase(b, "gradle", log)
	task := b.Target + ":assemble" + strings.ToUpper(b.Configuration[:1]) + b.Configuration[1:]
	if err := command(ctx, root, log, filepath.Join(root, "gradlew"), task, "--console=plain").Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(classifyGradle(logTail(dir, classifyBytes)), "Gradle %s failed: %v", task, err)
	}

	s.phase(b, "collect", log)
	metadataPath, metadata, ok := outputMetadata(filepath.Join(moduleDir(root, b.Target), "build", "outputs", "apk"), b.Configuration)
	if !ok {
		return fail("artifact_missing", "Gradle succeeded but wrote no APK metadata for %s", b.Configuration)
	}
	outputs := filepath.Dir(metadataPath)
	artifacts := []Artifact{}
	for _, element := range metadata.Elements {
		// The APK must sit beside its metadata, so a crafted build file cannot publish another file.
		src := filepath.Join(outputs, element.OutputFile)
		name := filepath.Base(src)
		if filepath.Dir(src) != outputs || !validAPK(name) || !isFile(src) {
			continue
		}
		if err := copyFile(src, filepath.Join(dir, name)); err != nil {
			return err
		}
		artifact, err := describe(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		artifacts = append(artifacts, artifact)
		b.SizeBytes += artifact.SizeBytes
	}
	if len(artifacts) == 0 {
		return fail("artifact_missing", "Gradle succeeded but produced no APK")
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	b.Artifacts = artifacts
	b.BundleID = metadata.ApplicationID
	if len(metadata.Elements) > 0 {
		b.Version = metadata.Elements[0].VersionName
		b.BuildNumber = fmt.Sprint(metadata.Elements[0].VersionCode)
	}
	return nil
}

type apkMetadata struct {
	ApplicationID string `json:"applicationId"`
	VariantName   string `json:"variantName"`
	Elements      []struct {
		VersionCode int64  `json:"versionCode"`
		VersionName string `json:"versionName"`
		OutputFile  string `json:"outputFile"`
	} `json:"elements"`
}

// outputMetadata finds the variant's output-metadata.json; androidTest APKs are instrumentation, not the app.
func outputMetadata(dir, variant string) (string, apkMetadata, bool) {
	var paths []string
	_ = filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && e.Name() == "output-metadata.json" && !strings.Contains(path, "androidTest") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m apkMetadata
		if json.Unmarshal(data, &m) == nil && strings.EqualFold(m.VariantName, variant) {
			return p, m, true
		}
	}
	return "", apkMetadata{}, false
}

var apkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.apk$`)

func validAPK(name string) bool { return apkName.MatchString(name) }

// describe is an artifact's name, size and digest; the phone shows the digest beside the link.
func describe(path string) (Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return Artifact{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Name: filepath.Base(path), SizeBytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// A missing toolchain is the common first failure, and it needs its own message.
func classifyGradle(tail string) string {
	text := strings.ToLower(tail)
	switch {
	case strings.Contains(text, "java_home"), strings.Contains(text, "no java runtime"),
		strings.Contains(text, "sdk location not found"), strings.Contains(text, "android_home"):
		return "toolchain_missing"
	case strings.Contains(text, "keystore"), strings.Contains(text, "signingconfig"):
		return "signing_failed"
	}
	return "build_failed"
}
