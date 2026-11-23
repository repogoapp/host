package antigravity

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/clitool"
)

// googleSignIn is the page `agy acp --login` prints; it redirects to
// antigravity.google, which shows the code to paste back.
var googleSignIn = regexp.MustCompile(`https://accounts\.google\.com/\S+`)

// loginLine reads the sign-in's output for its page.
func loginLine(line string, c *clilogin.Code) bool {
	if u := googleSignIn.FindString(line); u != "" {
		c.URL = u
	}
	return c.URL != ""
}

// statusTTL is how long a sign-in check stands: `agy models` asks Google, so
// it takes seconds, and a sign-in clears it at once.
const statusTTL = 5 * time.Minute

// statusTimeout bounds one check; agy unpacks itself on every start.
const statusTimeout = 30 * time.Second

// signIn caches whether agy is signed in. agy keeps its credentials in the
// system keychain and has no status command, so the answer is whether
// `agy models` lists models rather than asking to sign in. It names no
// account.
type signIn struct {
	mu     sync.Mutex
	at     time.Time
	authed bool
}

func (s *signIn) probe(ctx context.Context, path string) (bool, string, *clitool.Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.at) < statusTTL {
		return s.authed, "status", nil
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "models").Output()
	if ctx.Err() != nil {
		// A check that didn't finish says nothing; keep the last answer.
		return s.authed, "status", nil
	}
	s.authed, s.at = err == nil && listsModels(string(out)), time.Now()
	return s.authed, "status", nil
}

func (s *signIn) clear() {
	s.mu.Lock()
	s.at = time.Time{}
	s.mu.Unlock()
}

// listsModels reports `agy models` output that lists any: one "id\tname" line,
// where a signed-out agy prints "Please sign in".
func listsModels(out string) bool {
	if strings.Contains(strings.ToLower(out), "sign in") {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if id, _, ok := strings.Cut(line, "\t"); ok && id != "" {
			return true
		}
	}
	return false
}
