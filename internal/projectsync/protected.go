package projectsync

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/files"
)

// protected keeps a sweep from reading inside the folders macOS guards with a
// privacy prompt. With no one at the screen to answer it, the first read there
// waits forever, and the sweep holds its lock while it waits.
type protected struct {
	roots []string
	// volumes holds mounts, each its own root: a network share that dropped
	// hangs reads under it the same way.
	volumes string
	open    func(string) error
	wait    time.Duration
	// stuck hears each folder whose first probe did not answer in wait.
	stuck func(root string)

	mu     sync.Mutex
	probes map[string]chan struct{}
}

// probeWait is how long a sweep waits for a folder's first probe. A folder the
// host may read answers at once; one waiting on a prompt never does.
const probeWait = time.Second

func newProtected(goos, home string) *protected {
	p := &protected{open: openDir, wait: probeWait, stuck: func(string) {}, probes: map[string]chan struct{}{}}
	if goos != "darwin" {
		return p
	}
	if home != "" {
		for _, dir := range []string{"Desktop", "Documents", "Downloads", "Library/Mobile Documents", "Library/CloudStorage"} {
			p.roots = append(p.roots, filepath.Join(home, dir))
		}
	}
	p.volumes = "/Volumes"
	return p
}

func openDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// readable reports whether reading inside path returns rather than waits. A
// read stuck on a prompt cannot be cancelled, so the probe is left to finish,
// and the folder reads as readable once it does, as when the user allows it.
func (p *protected) readable(path string) bool {
	root := p.rootOf(path)
	if root == "" {
		return true
	}
	p.mu.Lock()
	done, probed := p.probes[root]
	if !probed {
		done = make(chan struct{})
		p.probes[root] = done
		go func() {
			_ = p.open(root)
			close(done)
		}()
	}
	p.mu.Unlock()
	if probed {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(p.wait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		p.stuck(root)
		return false
	}
}

// rootOf is the protected folder path sits in, "" when it sits in none.
func (p *protected) rootOf(path string) string {
	for _, root := range p.roots {
		if files.Within(root, path) {
			return root
		}
	}
	if p.volumes != "" && files.Within(p.volumes, path) {
		rel, err := filepath.Rel(p.volumes, path)
		if err != nil || rel == "." {
			return p.volumes
		}
		return filepath.Join(p.volumes, strings.SplitN(rel, string(filepath.Separator), 2)[0])
	}
	return ""
}
