// Package project discovers presentation metadata inside a project.
package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/repogo/host/internal/files"
)

const iconMaxBytes = 1 << 20

// The icon a user put in place on purpose gets more room than a guessed one:
// a 1024px PNG is often past 1 MB, and skipping it would pick a favicon over
// the file the user asked for. Base64 of 4 MB still fits one JSON-RPC message.
const (
	repogoIcon         = ".repogo/icon.png"
	repogoIconMaxBytes = 4 << 20
)

var iconExts = map[string]bool{"png": true, "ico": true, "webp": true}

// manifestNames are web manifests, in the order their icons are preferred.
var manifestNames = []string{"manifest.json", "manifest.webmanifest", "site.webmanifest"}

var probeDirs = []string{
	".", ".repogo",
	"public", "static", "assets", "app", "src", "www", "web", "site",
	"resources", "images", "img", "icons", "media",
	"public/icons", "public/images", "public/assets", "public/img", "public/img/icons",
	"assets/icons", "assets/images", "img/icons",
	"src/assets", "src/images", "src/icons", "src/app",
	"app/assets", "app/icons", "app/images",
	// Monorepos keep the site under apps/web; probe its conventional dirs too.
	"apps/web", "apps/web/public", "apps/web/app", "apps/web/src", "apps/web/src/app",
	"apps/web/public/icons", "apps/web/public/images", "apps/web/public/assets", "apps/web/public/img",
	"apps/web/assets", "apps/web/static", "apps/web/src/assets",
}

// iconNames are the conventional icon files in the order they win after
// .repogo/icon.png; manifestTurn marks where a manifest's own choice ranks.
var iconNames = []string{
	"repogo-icon.png",
	"apple-touch-icon-precomposed.png", "apple-touch-icon.png",
	manifestTurn,
	"icon.png", "logo.png", "icon-512.png", "android-chrome-512x512.png",
	"android-icon-foreground.png", "icon-192.png", "android-chrome-192x192.png",
	"favicon.ico",
}

const manifestTurn = ""

type Result struct {
	Path        string `json:"path"`
	Source      string `json:"source"`
	ContentHash string `json:"content_hash"`
	ContentType string `json:"content_type"`
	Bytes       []byte `json:"bytes,omitempty"`
	NotModified bool   `json:"not_modified,omitempty"`

	// Remote is `owner/repo` from this checkout's origin, empty when there is none.
	// It rides the icon reply because it wants the same lifetime: read once, kept
	// for days.
	Remote string `json:"remote,omitempty"`
}

// Repos is the repository each project checks out, as projectsync stored it.
type Repos interface {
	Repo(path string) (owner, name string, err error)
}

type Service struct {
	files *files.Service
	repos Repos

	mu     sync.Mutex
	hashes map[string]iconHash // by project root; see IconHash
}

func New(files *files.Service, repos Repos) *Service {
	return &Service{files: files, repos: repos, hashes: map[string]iconHash{}}
}

// DetectIcon returns the best conventional project icon without walking the
// repository. The path is contained before any directory is inspected.
func (s *Service) DetectIcon(ctx context.Context, path, ifNoneMatch string) (Result, error) {
	root, err := s.files.Contain(path)
	if err != nil {
		return Result{}, err
	}

	// Read before the early return: a project with no icon still has a
	// repository, and without it cannot group with its worktrees.
	owner, name, err := s.repos.Repo(path)
	if err != nil {
		return Result{}, err
	}
	var remote string
	if owner != "" {
		remote = owner + "/" + name
	}

	hit, _ := detect(root)
	if hit == nil {
		return Result{Remote: remote}, nil
	}
	result := Result{
		Path: hit.path, Source: hit.source,
		ContentType: contentType(hit.path), Remote: remote,
	}
	iconPath, err := s.files.Contain(filepath.Join(root, filepath.FromSlash(hit.path)))
	if err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(iconPath)
	if err != nil || len(data) > maxBytes(hit.path) {
		return result, nil
	}
	sum := sha256.Sum256(data)
	result.ContentHash = hex.EncodeToString(sum[:])
	if ifNoneMatch != "" && ifNoneMatch == result.ContentHash {
		result.NotModified = true
	} else {
		result.Bytes = data
	}
	return result, nil
}

type hit struct{ path, source string }

// detect picks the project's icon, and reports every file it looked at, as
// paths under root: the icon's content and every candidate's size can change
// the answer, so IconHash fingerprints them.
func detect(root string) (*hit, []string) {
	var read []string
	// Each map is a file name to its shortest path under root.
	icons := map[string]string{}
	manifests := map[string]string{}
	wanted := map[string]bool{}
	for _, name := range iconNames {
		wanted[name] = name != manifestTurn
	}
	for _, name := range manifestNames {
		wanted[name] = true
	}

	type dirEntry struct {
		dir   string
		names map[string]bool
	}
	dirFiles := make([]dirEntry, 0, len(probeDirs))
	for _, dir := range probeDirs {
		full := root
		if dir != "." {
			full = filepath.Join(root, filepath.FromSlash(dir))
		}
		info, err := os.Lstat(full)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(full)
		if err != nil {
			continue
		}
		names := make(map[string]bool, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			names[entry.Name()] = true
		}
		dirFiles = append(dirFiles, dirEntry{dir: dir, names: names})

		for name := range names {
			if !wanted[name] {
				continue
			}
			rel := relPath(dir, name)
			read = append(read, rel)
			if slices.Contains(manifestNames, name) {
				record(manifests, name, rel)
				continue
			}
			if !fileFits(filepath.Join(root, filepath.FromSlash(rel)), maxBytes(rel)) {
				continue
			}
			key := name
			if rel == repogoIcon {
				key = rel
			}
			record(icons, key, rel)
		}
	}

	parsed := map[string][]manifestIcon{}
	referenced := map[string]bool{}
	for name, path := range manifests {
		parsed[name] = readManifest(root, path)
		for _, entry := range parsed[name] {
			if filename, ok := manifestFilename(entry); ok && icons[filename] == "" && isCandidate(filename) {
				referenced[filename] = true
			}
		}
	}
	for _, entry := range dirFiles {
		for name := range entry.names {
			if !referenced[name] {
				continue
			}
			rel := relPath(entry.dir, name)
			read = append(read, rel)
			if fileFits(filepath.Join(root, filepath.FromSlash(rel)), iconMaxBytes) {
				record(icons, name, rel)
			}
		}
	}
	return choose(icons, parsed), read
}

func relPath(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func choose(icons map[string]string, manifests map[string][]manifestIcon) *hit {
	if path, ok := icons[repogoIcon]; ok {
		return &hit{path: path, source: repogoIcon}
	}
	for _, name := range iconNames {
		if name == manifestTurn {
			if found := chooseManifest(icons, manifests); found != nil {
				return found
			}
			continue
		}
		if path, ok := icons[name]; ok {
			return &hit{path: path, source: name}
		}
	}
	return nil
}

type manifestIcon struct {
	Src   string `json:"src"`
	Type  string `json:"type"`
	Sizes string `json:"sizes"`
}

func readManifest(root, path string) []manifestIcon {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return nil
	}
	var doc struct {
		Icons []manifestIcon `json:"icons"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.Icons
}

// chooseManifest is the largest raster icon the first manifest names that
// is also on disk.
func chooseManifest(icons map[string]string, manifests map[string][]manifestIcon) *hit {
	for _, manifestName := range manifestNames {
		type candidate struct {
			filename string
			area     int
		}
		var candidates []candidate
		for _, entry := range manifests[manifestName] {
			filename, ok := manifestFilename(entry)
			if !ok || entry.Type == "image/svg+xml" || strings.HasSuffix(strings.ToLower(entry.Src), ".svg") {
				continue
			}
			candidates = append(candidates, candidate{filename: filename, area: largestArea(entry.Sizes)})
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].area > candidates[j].area })
		for _, candidate := range candidates {
			if path, ok := icons[candidate.filename]; ok {
				return &hit{path: path, source: manifestName + "→" + candidate.filename}
			}
		}
	}
	return nil
}

// largestArea is the biggest WxH in a manifest's sizes, 0 when none parses.
func largestArea(sizes string) int {
	area := 0
	for _, token := range strings.Fields(sizes) {
		w, h, ok := strings.Cut(token, "x")
		if !ok {
			continue
		}
		width, errW := strconv.Atoi(w)
		height, errH := strconv.Atoi(h)
		if errW == nil && errH == nil && width*height > area {
			area = width * height
		}
	}
	return area
}

func manifestFilename(icon manifestIcon) (string, bool) {
	if icon.Src == "" || strings.HasPrefix(icon.Src, "http://") || strings.HasPrefix(icon.Src, "https://") {
		return "", false
	}
	name := filepath.Base(filepath.FromSlash(strings.SplitN(icon.Src, "?", 2)[0]))
	return name, name != "." && name != ""
}

// record keeps the shortest path seen for name.
func record(paths map[string]string, name, path string) {
	if existing, ok := paths[name]; ok && len(existing) <= len(path) {
		return
	}
	paths[name] = path
}

func isCandidate(name string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return iconExts[ext]
}

func maxBytes(rel string) int {
	if rel == repogoIcon {
		return repogoIconMaxBytes
	}
	return iconMaxBytes
}

func fileFits(path string, limit int) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() <= int64(limit)
}

func contentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}
