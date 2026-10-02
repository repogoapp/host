package builds

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/apphome"
)

// The installer (iOS installd, Android's package installer) sends no RepoGo
// credentials, so every artifact URL carries a token signed here, bound to one
// build and an expiry. The secret is kept on disk: links survive a restart.

// TokenTTL is how long an install link works.
const TokenTTL = time.Hour

// InstallLink is what opens the installer: URL is the itms-services link on
// iOS or the first APK on Android; PageURL is a web page with an Install button.
type InstallLink struct {
	URL       string `json:"url"`
	PageURL   string `json:"page_url"`
	ExpiresAt int64  `json:"expires_at"`
}

func loadSecret(path string) ([]byte, error) {
	secret, err := os.ReadFile(path)
	if err == nil && len(secret) == 32 {
		return secret, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	secret = make([]byte, 32)
	rand.Read(secret)
	return secret, apphome.WriteFile(path, secret, 0o600)
}

func (s *Service) sign(id string, expires int64) string {
	mac := hmac.New(sha256.New, s.secret)
	fmt.Fprintf(mac, "%s.%d", id, expires)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) token(id string, expires int64) string {
	return strconv.FormatInt(expires, 10) + "." + s.sign(id, expires)
}

func (s *Service) valid(id, token string) bool {
	exp, sig, ok := strings.Cut(token, ".")
	expires, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || expires < time.Now().Unix() {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.sign(id, expires)))
}

// InstallPort is the loopback port the artifacts are served on, which a
// tunnel must reach for a link to work; 0 before Listen.
func (s *Service) InstallPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return 0
	}
	return s.server.port
}

// InstallLink mints a link for a successful build on the public tunnel that
// reaches the install port.
func (s *Service) InstallLink(id string) (InstallLink, error) {
	b, err := s.Get(id)
	if err != nil {
		return InstallLink{}, err
	}
	if b.Status != StatusSuccess {
		return InstallLink{}, fmt.Errorf("%w: %s is %s", ErrNotReady, id, b.Status)
	}
	port := s.InstallPort()
	base := s.cfg.URLs()[port]
	if port == 0 || base == "" {
		return InstallLink{}, fmt.Errorf("%w: open a tunnel to port %d", ErrNoTunnel, port)
	}
	expires := time.Now().Add(TokenTTL).Unix()
	query := "?token=" + url.QueryEscape(s.token(id, expires))
	link := InstallLink{PageURL: base + "/install/" + id + query, ExpiresAt: expires * 1000}
	switch b.Platform {
	case PlatformIOS:
		manifest := base + "/ota/" + id + "/manifest.plist" + query
		link.URL = "itms-services://?action=download-manifest&url=" + url.QueryEscape(manifest)
	case PlatformAndroid:
		link.URL = base + "/android-install/" + id + "/" + b.Artifacts[0].Name + query
	}
	return link, nil
}

type server struct {
	port int
	srv  *http.Server
}

func (v *server) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = v.srv.Shutdown(ctx)
}

// Listen serves the artifacts on 127.0.0.1, on the port it used last so an
// open tunnel keeps working across a restart.
func (s *Service) Listen() error {
	portFile := filepath.Join(s.cfg.Dir, "port.json")
	var saved struct {
		Port int `json:"port"`
	}
	if _, err := apphome.ReadJSON(portFile, &saved); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", saved.Port))
	if err != nil && saved.Port != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if port != saved.Port {
		saved.Port = port
		if err := apphome.WriteJSON(portFile, saved, 0o600); err != nil {
			ln.Close()
			return err
		}
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.server = &server{port: port, srv: srv}
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Log.Warn("builds: install server stopped", "err", err)
		}
	}()
	return nil
}

// Handler serves only what a valid token names: the install page, the iOS
// manifest, IPA and icons, and a build's own APKs.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /install/{id}", s.guard(s.servePage))
	mux.HandleFunc("GET /ota/{id}/{file}", s.guard(s.serveIOS))
	mux.HandleFunc("GET /android-install/{id}/{file}", s.guard(s.serveAPK))
	return mux
}

// guard admits a request for a successful build with a valid token.
func (s *Service) guard(next func(http.ResponseWriter, *http.Request, Build)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) || !s.valid(id, r.URL.Query().Get("token")) {
			http.Error(w, "This install link has expired. Ask for a new one.", http.StatusForbidden)
			return
		}
		b, err := s.Get(id)
		if err != nil || b.Status != StatusSuccess {
			http.Error(w, "This build is gone.", http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store, private")
		next(w, r, b)
	}
}

func (s *Service) serveIOS(w http.ResponseWriter, r *http.Request, b Build) {
	if b.Platform != PlatformIOS {
		http.NotFound(w, r)
		return
	}
	switch file := r.PathValue("file"); file {
	case "manifest.plist":
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, manifest(b, publicBase(r)+"/ota/"+b.ID, "?token="+url.QueryEscape(r.URL.Query().Get("token"))))
	case ipaName, "icon-57.png", "icon-512.png":
		s.serveFile(w, r, filepath.Join(s.dir(b.ID), file))
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) serveAPK(w http.ResponseWriter, r *http.Request, b Build) {
	name := r.PathValue("file")
	for _, a := range b.Artifacts {
		if b.Platform == PlatformAndroid && a.Name == name && validAPK(name) {
			w.Header().Set("Content-Type", "application/vnd.android.package-archive")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			s.serveFile(w, r, filepath.Join(s.dir(b.ID), name))
			return
		}
	}
	http.NotFound(w, r)
}

// serveFile answers Range and HEAD as the installers send them.
func (s *Service) serveFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// publicBase is the tunnel's own origin, which the tunnel names in X-Forwarded-Host.
func publicBase(r *http.Request) string {
	if host := r.Header.Get("X-Forwarded-Host"); host != "" {
		return "https://" + host
	}
	return "https://" + r.Host
}

// title is what the install prompt and page call the app.
func title(b Build) string {
	if b.Platform == PlatformIOS && b.Target != "" {
		return b.Target
	}
	return b.BundleID
}

// manifest is the plist installd reads first; it names the IPA and icons.
func manifest(b Build, base, query string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>items</key>
  <array>
    <dict>
      <key>assets</key>
      <array>
        <dict>
          <key>kind</key><string>software-package</string>
          <key>url</key><string>` + xmlEscape(base+"/"+ipaName+query) + `</string>
        </dict>
        <dict>
          <key>kind</key><string>display-image</string>
          <key>url</key><string>` + xmlEscape(base+"/icon-57.png"+query) + `</string>
        </dict>
        <dict>
          <key>kind</key><string>full-size-image</string>
          <key>url</key><string>` + xmlEscape(base+"/icon-512.png"+query) + `</string>
        </dict>
      </array>
      <key>metadata</key>
      <dict>
        <key>bundle-identifier</key><string>` + xmlEscape(b.BundleID) + `</string>
        <key>bundle-version</key><string>` + xmlEscape(b.BuildNumber) + `</string>
        <key>kind</key><string>software</string>
        <key>title</key><string>` + xmlEscape(title(b)) + `</string>
      </dict>
    </dict>
  </array>
</dict>
</plist>
`
}

var xmlReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(s string) string { return xmlReplacer.Replace(s) }

type pageLink struct {
	Label string
	URL   template.URL
}

var page = template.Must(template.New("install").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Install {{.Title}}</title>
<style>body{font:17px -apple-system,system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1.5rem;color:#111}
a.button{display:block;margin:1rem 0;padding:.9rem;border-radius:12px;background:#111;color:#fff;text-align:center;text-decoration:none;font-weight:600}
p{color:#555}@media(prefers-color-scheme:dark){body{background:#000;color:#eee}a.button{background:#fff;color:#000}p{color:#aaa}}</style>
</head><body>
<h1>{{.Title}}</h1>
<p>{{.Detail}}</p>
{{range .Links}}<a class="button" href="{{.URL}}">{{.Label}}</a>{{end}}
<p>{{.Note}}</p>
</body></html>
`))

// servePage is the https page a link in a chat opens; its button starts the installer.
func (s *Service) servePage(w http.ResponseWriter, r *http.Request, b Build) {
	base := publicBase(r)
	query := "?token=" + url.QueryEscape(r.URL.Query().Get("token"))
	data := struct {
		Title, Detail, Note string
		Links               []pageLink
	}{Title: title(b), Detail: fmt.Sprintf("%s %s (%s), %.1f MB", b.BundleID, b.Version, b.BuildNumber, float64(b.SizeBytes)/1e6)}
	switch b.Platform {
	case PlatformIOS:
		manifestURL := base + "/ota/" + b.ID + "/manifest.plist" + query
		data.Links = []pageLink{{Label: "Install", URL: template.URL("itms-services://?action=download-manifest&url=" + url.QueryEscape(manifestURL))}}
		data.Note = "Open this page in Safari. The iPhone must be on the team's provisioning profile; a Debug build also needs Developer Mode."
	case PlatformAndroid:
		for _, a := range b.Artifacts {
			data.Links = append(data.Links, pageLink{Label: "Download " + a.Name, URL: template.URL(base + "/android-install/" + b.ID + "/" + a.Name + query)})
		}
		data.Note = "Open the downloaded APK to install it; Android asks once to allow installs from the browser."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, data)
}
