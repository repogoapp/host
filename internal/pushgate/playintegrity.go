package pushgate

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Play Integrity is Android's App Attest: Google vouches that a grant was
// signed by RepoGo as Play installed it, on a genuine device. The phone asks
// for a token whose request hash is IntegrityHash of its grant; the relay has
// Google decode it once, then trusts the grant until it expires.
const playDecode = "https://playintegrity.googleapis.com/v1/%s:decodeIntegrityToken"

// A token must come from within this of the grant it vouches for, the
// freshness Google leaves to the server.
const integritySkew = 10 * time.Minute

// IntegrityHash is the request hash a phone gives Play Integrity for a grant:
// the grant's message and the key that signed it, so the token vouches for
// that key's grant and no other.
func IntegrityHash(pub, grantMessage []byte) string {
	sum := sha256.Sum256(append(append([]byte{}, pub...), grantMessage...))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// CheckIntegrity says whether token vouches for the grant msg that pub signed
// at grantedAt. A grant it hasn't passed yet costs a decode, which it takes
// only if allowDecode agrees.
type CheckIntegrity func(ctx context.Context, pub, msg []byte, grantedAt time.Time, token string, allowDecode func() bool) error

// PlayIntegrity checks with FCM_SERVICE_ACCOUNT's key, which must be in the
// Cloud project linked in Play Console, for PLAY_PACKAGE (app.repogo) signed
// with PLAY_CERT_SHA256 (the app signing key's fingerprint, as Play Console
// shows it). Passed grants are kept in HRELAY_VERDICTS (on the volume by
// default), since Google decodes a token only once. Nil without a key or a
// fingerprint, so the relay sends to trusted Android devices alone.
func PlayIntegrity(log *slog.Logger) CheckIntegrity {
	_, auth := googleAccount(log, playScope)
	cert, err := hex.DecodeString(strings.ReplaceAll(os.Getenv("PLAY_CERT_SHA256"), ":", ""))
	if auth == nil || err != nil || len(cert) != sha256.Size {
		if auth != nil {
			log.Warn("playintegrity: PLAY_CERT_SHA256 is missing or not a SHA-256 fingerprint; Android is limited to trusted devices")
		}
		return nil
	}
	pkg := cmp.Or(os.Getenv("PLAY_PACKAGE"), "app.repogo")
	p := &playChecker{
		auth: auth, pkg: pkg, cert: cert, endpoint: fmt.Sprintf(playDecode, pkg), now: time.Now,
		verdicts: openVerdicts(cmp.Or(os.Getenv("HRELAY_VERDICTS"), "/data/play-verdicts.json"), log),
	}
	return p.check
}

type playChecker struct {
	auth     *googleAuth
	pkg      string
	cert     []byte
	endpoint string
	now      func() time.Time
	verdicts *verdicts

	// One decode at a time: Google clears a token decoded twice, so two
	// pushes racing on a fresh grant must not both send it.
	decodeMu sync.Mutex
}

func (p *playChecker) check(ctx context.Context, pub, msg []byte, grantedAt time.Time, token string, allowDecode func() bool) error {
	hash := IntegrityHash(pub, msg)
	if p.verdicts.passed(hash, p.now()) {
		return nil
	}
	if token == "" {
		return errors.New("the grant carries no Play Integrity token")
	}
	p.decodeMu.Lock()
	defer p.decodeMu.Unlock()
	if p.verdicts.passed(hash, p.now()) {
		return nil
	}
	if !allowDecode() {
		return errors.New("too many new Android grants from this host; try again in a minute")
	}
	payload, err := p.decode(ctx, token)
	if err != nil {
		return err
	}
	if err := p.judge(payload, hash, grantedAt); err != nil {
		return err
	}
	p.verdicts.add(hash, grantedAt.Add(grantMaxAge), p.now())
	return nil
}

// integrityPayload is the part of Google's verdict the relay reads.
type integrityPayload struct {
	RequestDetails struct {
		RequestPackageName string `json:"requestPackageName"`
		RequestHash        string `json:"requestHash"`
		TimestampMillis    string `json:"timestampMillis"`
	} `json:"requestDetails"`
	AppIntegrity struct {
		AppRecognitionVerdict   string   `json:"appRecognitionVerdict"`
		PackageName             string   `json:"packageName"`
		CertificateSha256Digest []string `json:"certificateSha256Digest"`
	} `json:"appIntegrity"`
	DeviceIntegrity struct {
		DeviceRecognitionVerdict []string `json:"deviceRecognitionVerdict"`
	} `json:"deviceIntegrity"`
}

func (p *playChecker) decode(ctx context.Context, token string) (integrityPayload, error) {
	access, err := p.auth.token(ctx)
	if err != nil {
		return integrityPayload{}, err
	}
	body, _ := json.Marshal(map[string]string{"integrity_token": token})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	res, err := p.auth.client.Do(req)
	if err != nil {
		return integrityPayload{}, fmt.Errorf("playintegrity: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusUnauthorized {
			p.auth.forget()
		}
		return integrityPayload{}, fmt.Errorf("playintegrity: %d %s", res.StatusCode, fcmReason(res.Body))
	}
	var answer struct {
		TokenPayloadExternal integrityPayload `json:"tokenPayloadExternal"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&answer); err != nil {
		return integrityPayload{}, fmt.Errorf("playintegrity: unreadable verdict: %w", err)
	}
	return answer.TokenPayloadExternal, nil
}

// judge passes a verdict for this grant from RepoGo as Play signed it, on a
// genuine device, issued when the grant was signed.
func (p *playChecker) judge(v integrityPayload, hash string, grantedAt time.Time) error {
	at, _ := strconv.ParseInt(v.RequestDetails.TimestampMillis, 10, 64)
	issued := time.UnixMilli(at)
	switch {
	case v.RequestDetails.RequestPackageName != p.pkg || v.AppIntegrity.PackageName != p.pkg:
		return errors.New("play integrity: another app's token")
	case v.RequestDetails.RequestHash != hash:
		return errors.New("play integrity: the token is for another grant")
	case issued.Before(grantedAt.Add(-integritySkew)) || issued.After(grantedAt.Add(integritySkew)):
		return errors.New("play integrity: the token wasn't issued with the grant")
	case v.AppIntegrity.AppRecognitionVerdict != "PLAY_RECOGNIZED":
		return fmt.Errorf("play integrity: app %s, not as Play installed it", cmp.Or(v.AppIntegrity.AppRecognitionVerdict, "unevaluated"))
	case !slices.ContainsFunc(v.AppIntegrity.CertificateSha256Digest, p.isOurCert):
		return errors.New("play integrity: signed with another key")
	case !slices.Contains(v.DeviceIntegrity.DeviceRecognitionVerdict, "MEETS_DEVICE_INTEGRITY"):
		return errors.New("play integrity: the device doesn't meet device integrity")
	}
	return nil
}

// isOurCert reads Google's digest, URL-safe base64 with or without padding.
func (p *playChecker) isOurCert(digest string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(digest, "="))
	return err == nil && bytes.Equal(raw, p.cert)
}

// verdicts are the grants Play Integrity passed, by IntegrityHash, until each
// grant expires; saved to path so a restart needn't decode a token again.
type verdicts struct {
	path string
	log  *slog.Logger

	mu    sync.Mutex
	until map[string]int64
}

func openVerdicts(path string, log *slog.Logger) *verdicts {
	v := &verdicts{path: path, log: log, until: map[string]int64{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &v.until)
	}
	return v
}

func (v *verdicts) passed(hash string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.until[hash] > now.UnixMilli()
}

func (v *verdicts) add(hash string, until, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.until[hash] = until.UnixMilli()
	for h, at := range v.until {
		if at <= now.UnixMilli() {
			delete(v.until, h)
		}
	}
	if v.path == "" {
		return
	}
	raw, _ := json.Marshal(v.until)
	if err := writeFile(v.path, raw); err != nil {
		v.log.Warn("playintegrity: could not save verdicts", "err", err)
	}
}

// writeFile replaces path whole, so a crash leaves the old file or the new.
func writeFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
