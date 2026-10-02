// Package shipping keeps a ledger of what this machine's agents spent, one row
// per API request, in its own database that outlives the transcripts, and
// answers from it: the usage history the phone merges across environments,
// and the leaderboard's yearly report. Only counts and costs leave the host;
// transcript content and prompts never do.
package shipping

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
)

// Tokens is one request's token counts, split the way public rates price them.
type Tokens struct {
	// Uncached input only; Cached and the cache writes are counted apart.
	Uncached, Cached int64
	// Cache writes kept five minutes, and the dearer ones kept an hour.
	Creation, Creation1h int64
	// Output includes Reasoning; Reasoning is never added on top.
	Output, Reasoning int64
}

// Record is one billed request a provider's transcript parser found.
type Record struct {
	// Key names the request across files, so a resumed or forked chat's copy
	// of it is not counted again. Empty when the provider wrote no id; the
	// ledger then keys it by file and line.
	Key string
	// Session is the chat the request belongs to, as its provider names it.
	Session     string
	TimestampMs int64
	// Empty for a request its provider cannot price, such as an alias naming
	// a family rather than a billable model.
	Model  string
	Tokens Tokens
	// Claude's fast mode.
	Fast bool

	// The cost the provider wrote itself, preferred over a computed one.
	ReportedUSD float64
	HasReported bool
}

// Provider is an agent whose .jsonl transcripts under UsageRoots record token
// usage; NewUsageParser returns one file's per-line parser, which may carry
// state across lines and returns nil for a line that bills nothing.
type Provider interface {
	Kind() agent.Kind
	UsageRoots() []string
	NewUsageParser() func([]byte) *Record
}

// FileParser is a Provider that can read a whole file's usage at once, the
// Rust parser when the host links it. ok false means use NewUsageParser.
type FileParser interface {
	ParseUsageFile(path string) (records []Record, ok bool)
}

// Positive is a JSON token count as an integer; anything else counts as zero.
func Positive(v any) int64 {
	n, ok := v.(float64)
	if !ok || n <= 0 || n != float64(int64(n)) {
		return 0
	}
	return int64(n)
}

func micros(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(usd*1_000_000 + 0.5)
}

// machineID fingerprints the machine, not the host process, the same way v1
// did, so rows for one Mac keep their key and two hosts never double count.
func machineID() string {
	hostname, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	sum := sha256.Sum256([]byte(hostname + "\x00" + home))
	return hex.EncodeToString(sum[:8])
}

// zoneName is the IANA name the board's days are in; time.Local only calls itself "Local".
func zoneName(loc *time.Location) string {
	if loc != time.Local {
		return loc.String()
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			return name
		}
	}
	return loc.String()
}

type transcript struct {
	path          string
	source        Provider
	size, mtimeMs int64
}

// discover lists the .jsonl files under root. Symlinks are skipped so a link
// cannot pull another tree in.
func discover(source Provider, root string) []transcript {
	var out []transcript
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, transcript{path: path, source: source, size: info.Size(), mtimeMs: info.ModTime().UnixMilli()})
		return nil
	})
	return out
}
