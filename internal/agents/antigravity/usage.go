package antigravity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agentusage"
	"github.com/repogoapp/host/internal/clitool"
)

// Antigravity's limits come from Google's quota API, the one agy itself asks:
// retrieveUserQuotaSummary on the Cloud Code host, with agy's own sign-in.
// It is undocumented, and answers only a caller that names itself as an
// Antigravity client; anything unexpected reads as no limits, never an error.

// quotaHosts are asked together: the daily host meters personal accounts,
// and its reading of a bucket wins over the other's.
var quotaHosts = []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"}

const (
	quotaPath   = "/v1internal:retrieveUserQuotaSummary"
	userInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
	// keychainService and keychainAccount are where agy keeps its sign-in in
	// the system keychain.
	keychainService = "gemini"
	keychainAccount = "antigravity"
	// tokenMargin is how much life a token needs to be used.
	tokenMargin = time.Minute
)

var quotaClient = &http.Client{Timeout: 10 * time.Second}

// limits is what the reader keeps between readings.
type limits struct {
	mu sync.Mutex
	// plan is the account's tier as agy named it when the models were read.
	plan string
	// account is the signed-in Google account, by the token it was read with.
	account      *agentusage.Account
	accountToken string
}

// Usage is the account's 5-hour and weekly limits per group of models, or
// last when they can't be read.
func (p *Provider) Usage(ctx context.Context, last agentusage.Usage) agentusage.Usage {
	if p.deps.Root != "" {
		// A test host never reads the user's own account.
		return agentusage.Usage{}
	}
	usage, err := p.readLimits(ctx)
	if err != nil {
		if p.deps.Log != nil {
			p.deps.Log.Info("antigravity limits unavailable", "err", err)
		}
		if len(last.Windows) > 0 {
			return last
		}
		return agentusage.Usage{}
	}
	return usage
}

func (p *Provider) readLimits(ctx context.Context) (agentusage.Usage, error) {
	token, err := freshToken(ctx)
	if err != nil {
		return agentusage.Usage{}, err
	}
	agent := userAgent(ctx)
	type answer struct {
		groups []quotaGroup
		err    error
	}
	answers := make([]answer, len(quotaHosts))
	var wg sync.WaitGroup
	for i, host := range quotaHosts {
		wg.Go(func() {
			answers[i].groups, answers[i].err = quotaSummary(ctx, host, token, agent)
		})
	}
	account := p.limits.accountFor(ctx, token)
	wg.Wait()
	var groups [][]quotaGroup
	for _, a := range answers {
		if a.err == nil {
			groups = append(groups, a.groups)
		}
	}
	if len(groups) == 0 {
		return agentusage.Usage{}, answers[0].err
	}
	windows := quotaWindows(groups...)
	if len(windows) == 0 {
		return agentusage.Usage{}, errors.New("the quota summary named no limits")
	}
	p.limits.mu.Lock()
	plan := p.limits.plan
	p.limits.mu.Unlock()
	return agentusage.Usage{Available: true, Plan: plan, Account: account, CapturedAtMS: time.Now().UnixMilli(), Windows: windows}, nil
}

// quotaGroup is one group of models sharing limits, as the API answers.
type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}

type quotaBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
	Disabled          bool     `json:"disabled"`
}

// quotaSummary asks one host for the account's limits.
func quotaSummary(ctx context.Context, host, token, agent string) ([]quotaGroup, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+quotaPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", agent)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", host, resp.Status)
	}
	var body struct {
		Groups  []quotaGroup  `json:"groups"`
		Buckets []quotaBucket `json:"buckets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Groups) == 0 && len(body.Buckets) > 0 {
		body.Groups = []quotaGroup{{Buckets: body.Buckets}}
	}
	return body.Groups, nil
}

// quotaWindows is the limits as windows, by bucket, the first answer's
// reading of a bucket winning. A window's label names its group, since every
// group has its own 5-hour and weekly limit.
func quotaWindows(answers ...[]quotaGroup) []agentusage.Window {
	var out []agentusage.Window
	seen := map[string]bool{}
	for _, groups := range answers {
		for _, g := range groups {
			scope := groupName(g.DisplayName)
			for _, b := range g.Buckets {
				if b.Disabled || b.RemainingFraction == nil || b.BucketID == "" || seen[b.BucketID] {
					continue
				}
				seen[b.BucketID] = true
				left := min(max(*b.RemainingFraction, 0), 1)
				minutes := map[string]int64{"5h": 300, "weekly": 10080}[b.Window]
				w := agentusage.Window{
					ID: b.BucketID, UsedPercent: (1 - left) * 100, WindowMinutes: minutes, Scope: scope,
					Label: agentusage.WindowLabel(minutes, b.DisplayName),
				}
				if scope != "" {
					w.Label += " · " + scope
				}
				// An untouched bucket reports a rolling full-window reset,
				// which isn't a time anything happens.
				if left < 1 {
					if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
						w.ResetsAt = t.Unix()
					}
				}
				if left <= 0 {
					w.Severity = "exceeded"
				}
				out = append(out, w)
			}
		}
	}
	return out
}

// groupName is a group's name for a window's label: "Gemini Models" is
// "Gemini".
func groupName(name string) string {
	name = strings.TrimSpace(name)
	for _, suffix := range []string{" models", " Models"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}

// notePlan keeps the plan agy names in a session's _meta, for the usage row.
func (p *Provider) notePlan(meta json.RawMessage) {
	var m struct {
		Antigravity struct {
			Tier struct {
				Name string `json:"name"`
			} `json:"tier"`
		} `json:"antigravity"`
	}
	if json.Unmarshal(meta, &m) != nil || m.Antigravity.Tier.Name == "" {
		return
	}
	p.limits.mu.Lock()
	p.limits.plan = m.Antigravity.Tier.Name
	p.limits.mu.Unlock()
}

// accountFor is the Google account a token signs in, read once per token.
func (l *limits) accountFor(ctx context.Context, token string) *agentusage.Account {
	l.mu.Lock()
	if l.accountToken == token {
		defer l.mu.Unlock()
		return l.account
	}
	l.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var info struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&info) != nil || info.Email == "" {
		return nil
	}
	account := &agentusage.Account{Email: info.Email, Key: clitool.AccountKey("antigravity", info.ID, "google")}
	l.mu.Lock()
	l.account, l.accountToken = account, token
	l.mu.Unlock()
	return account
}

// storedToken is agy's sign-in as it saves it.
type storedToken struct {
	AccessToken string    `json:"access_token"`
	Expiry      time.Time `json:"expiry"`
}

// freshToken is agy's access token with time left on it. One about to expire
// is renewed by agy itself: any call it makes to Google refreshes and saves
// it, so the host never holds agy's refresh token or client.
func freshToken(ctx context.Context) (string, error) {
	t, err := readToken()
	if err == nil && time.Until(t.Expiry) > tokenMargin {
		return t.AccessToken, nil
	}
	renew, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	_ = exec.CommandContext(renew, executable(), "models").Run()
	if t, err = readToken(); err != nil {
		return "", err
	}
	if time.Until(t.Expiry) <= tokenMargin {
		return "", errors.New("agy's sign-in has expired")
	}
	return t.AccessToken, nil
}

// readToken reads agy's saved sign-in: the system keychain on a Mac, else
// the file agy falls back to where there is no keyring (a headless Linux).
func readToken() (storedToken, error) {
	var raw string
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-a", keychainAccount, "-w").Output()
		if err != nil {
			return storedToken{}, errors.New("agy's sign-in isn't in the keychain")
		}
		raw = strings.TrimSpace(string(out))
	} else {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".gemini", "antigravity-cli")
		for _, name := range []string{"jetski-standalone-oauth-token", "antigravity-oauth-token"} {
			if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
				raw = strings.TrimSpace(string(b))
				break
			}
		}
		if raw == "" {
			return storedToken{}, errors.New("agy's sign-in file wasn't found")
		}
	}
	return parseToken(raw)
}

// parseToken reads a saved sign-in, as go-keyring encodes it in the
// keychain or plain: {"token": {...}} or the token alone.
func parseToken(raw string) (storedToken, error) {
	switch {
	case strings.HasPrefix(raw, "go-keyring-base64:"):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "go-keyring-base64:"))
		if err != nil {
			return storedToken{}, err
		}
		raw = string(b)
	case strings.HasPrefix(raw, "go-keyring-encoded:"):
		b, err := hex.DecodeString(strings.TrimPrefix(raw, "go-keyring-encoded:"))
		if err != nil {
			return storedToken{}, err
		}
		raw = string(b)
	}
	var wrapped struct {
		Token *storedToken `json:"token"`
	}
	if json.Unmarshal([]byte(raw), &wrapped) == nil && wrapped.Token != nil && wrapped.Token.AccessToken != "" {
		return *wrapped.Token, nil
	}
	var bare storedToken
	if json.Unmarshal([]byte(raw), &bare) != nil || bare.AccessToken == "" {
		return storedToken{}, errors.New("agy's sign-in can't be read")
	}
	return bare, nil
}

// userAgent names the host as the Antigravity CLI it runs, which is what
// the quota API answers.
func userAgent(ctx context.Context) string {
	// The version it was tested with, for an agy that doesn't say its own.
	version := "1.5.14"
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(probe, executable(), "--version").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); semver.MatchString(v) {
			version = v
		}
	}
	return fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s)", version, runtime.GOOS, runtime.GOARCH)
}
