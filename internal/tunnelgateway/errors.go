package tunnelgateway

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// writeError renders an error the gateway itself raised: a themed card for
// browsers, plain text for everything else. Responses from the host pass
// through untouched unless the host marked them with GatewayErrorHeader.

// GatewayErrorHeader marks a response the host made up, such as "local server
// unreachable": the value is the title and the body the description.
const GatewayErrorHeader = "x-repogo-gateway-error"

// extractGatewayErrorTitle is the title of a response the host made up.
func extractGatewayErrorTitle(headerLines []string) (string, bool) {
	for _, line := range headerLines {
		idx := strings.Index(line, ": ")
		if idx <= 0 {
			continue
		}
		if strings.EqualFold(line[:idx], GatewayErrorHeader) {
			return strings.TrimSpace(line[idx+2:]), true
		}
	}
	return "", false
}

type errorPage struct {
	Status      int
	StatusText  string
	Title       string
	Description string
}

var errorTemplate = template.Must(template.New("error").Parse(errorTmpl))

func writeError(w http.ResponseWriter, r *http.Request, status int, title, description string) {
	wantsHTML := acceptsHTML(r)
	if wantsHTML {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("X-Repogo-Gateway-Error", title)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if wantsHTML {
		_ = errorTemplate.Execute(w, errorPage{
			Status:      status,
			StatusText:  http.StatusText(status),
			Title:       title,
			Description: description,
		})
	} else {
		fmt.Fprintf(w, "%d %s\n\n%s\n%s\n", status, http.StatusText(status), title, description)
	}
}

func acceptsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") && hasBrowserHints(r)
}

// hasBrowserHints tells a browser's `*/*` from curl's.
func hasBrowserHints(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Mode") != "" {
		return true
	}
	ua := r.Header.Get("User-Agent")
	return strings.HasPrefix(ua, "Mozilla/")
}

const errorTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Status}} · {{.Title}}</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #fafafa;
  --fg: #111;
  --muted: #6b7280;
  --card: #ffffff;
  --border: #e5e7eb;
  --accent: #ef4444;
  --shadow: 0 1px 2px rgba(0,0,0,0.04), 0 8px 32px rgba(0,0,0,0.08);
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0a0a0a;
    --fg: #f5f5f5;
    --muted: #9ca3af;
    --card: #141414;
    --border: #262626;
    --accent: #f87171;
    --shadow: 0 1px 2px rgba(0,0,0,0.4), 0 8px 32px rgba(0,0,0,0.4);
  }
}
* { box-sizing: border-box; }
html, body { height: 100%; margin: 0; }
body {
  font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Inter", "SF Pro Text", "Segoe UI", Roboto, system-ui, sans-serif;
  background: var(--bg);
  color: var(--fg);
  display: grid;
  place-items: center;
  padding: 24px;
  -webkit-font-smoothing: antialiased;
}
main.card {
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: 14px;
  padding: 36px 36px 32px;
  max-width: 460px;
  width: 100%;
  box-shadow: var(--shadow);
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  font-weight: 600;
  color: var(--accent);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  margin: 0 0 14px;
}
.status::before {
  content: "";
  display: inline-block;
  width: 8px; height: 8px;
  border-radius: 50%;
  background: var(--accent);
}
h1.title {
  font-size: 22px;
  font-weight: 600;
  line-height: 1.3;
  margin: 0 0 12px;
  letter-spacing: -0.01em;
}
p.desc {
  font-size: 15px;
  color: var(--muted);
  margin: 0;
  line-height: 1.55;
}
footer {
  margin-top: 28px;
  padding-top: 20px;
  border-top: 1px solid var(--border);
  font-size: 11px;
  color: var(--muted);
  letter-spacing: 0.04em;
  opacity: 0.7;
}
@media (max-width: 480px) {
  main.card { padding: 28px 24px 24px; border-radius: 12px; }
  h1.title { font-size: 20px; }
}
</style>
</head>
<body>
<main class="card">
  <p class="status">{{.Status}} · {{.StatusText}}</p>
  <h1 class="title">{{.Title}}</h1>
  <p class="desc">{{.Description}}</p>
  <footer>RepoGo Tunnel Gateway</footer>
</main>
</body>
</html>
`
