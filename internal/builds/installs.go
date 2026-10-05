package builds

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// closeAfter is how long the install tunnel outlives an IPA's last byte, so the
// gateway can finish relaying it and installd can fetch the icons.
var closeAfter = 30 * time.Second

// installs follows artifact downloads: an iOS link serves its IPA once, and the
// install tunnel closes when that IPA is out and nothing else is downloading.
type installs struct {
	mu      sync.Mutex
	active  int
	closing bool
	timer   *time.Timer
	used    map[string]int64 // iOS tokens whose IPA was served, by expiry
}

// ipaUsed reports whether token's IPA was already served whole.
func (s *Service) ipaUsed(token string) bool {
	s.installs.mu.Lock()
	defer s.installs.mu.Unlock()
	_, used := s.installs.used[token]
	return used
}

// ipaServed spends token and asks for the tunnel to close.
func (s *Service) ipaServed(token string) {
	in := &s.installs
	in.mu.Lock()
	defer in.mu.Unlock()
	now := time.Now().Unix()
	for t, expires := range in.used {
		if expires < now {
			delete(in.used, t)
		}
	}
	exp, _, _ := strings.Cut(token, ".")
	expires, _ := strconv.ParseInt(exp, 10, 64)
	if in.used == nil {
		in.used = map[string]int64{}
	}
	in.used[token] = expires
	in.closing = true
}

func (s *Service) downloadStarted() {
	in := &s.installs
	in.mu.Lock()
	defer in.mu.Unlock()
	in.active++
	if in.timer != nil {
		in.timer.Stop()
		in.timer = nil
	}
}

// downloadEnded arms the tunnel's close once the last download ends after an IPA went out.
func (s *Service) downloadEnded() {
	in := &s.installs
	in.mu.Lock()
	defer in.mu.Unlock()
	in.active--
	if in.active == 0 && in.closing && s.cfg.CloseTunnel != nil {
		in.timer = time.AfterFunc(closeAfter, s.closeInstallTunnel)
	}
}

func (s *Service) closeInstallTunnel() {
	in := &s.installs
	in.mu.Lock()
	if in.active > 0 || !in.closing {
		in.mu.Unlock()
		return
	}
	in.closing = false
	in.timer = nil
	in.mu.Unlock()
	if port := s.InstallPort(); port != 0 {
		if err := s.cfg.CloseTunnel(port); err != nil {
			s.cfg.Log.Warn("builds: closing the install tunnel failed", "err", err)
		}
	}
}

// download serves an artifact while counting it, so the tunnel stays open under it.
func (s *Service) download(w http.ResponseWriter, r *http.Request, path string) {
	s.downloadStarted()
	defer s.downloadEnded()
	serveFile(w, r, path)
}

// countingWriter records what a response sent, to tell when a download is whole.
type countingWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)
	return n, err
}

// whole is whether the response ended on the file's last byte: all of it, or
// the final range of a resumed download.
func (c *countingWriter) whole(size int64) bool {
	switch c.status {
	case http.StatusOK:
		return c.written == size
	case http.StatusPartialContent:
		var start, end, total int64
		if _, err := fmt.Sscanf(c.Header().Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
			return false
		}
		return end == size-1 && c.written == end-start+1
	}
	return false
}
