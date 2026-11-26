package tunnelgateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResponseCacheHonorsFreshnessForValidators(t *testing.T) {
	cache := NewResponseCache()
	cache.Set("asset", http.StatusOK, []string{
		`ETag: "asset-v1"`,
		"Cache-Control: public, max-age=60",
	}, []byte("body"), 10*time.Millisecond, nil)

	entry, ok := cache.Get("asset")
	if !ok {
		t.Fatal("expected cached entry")
	}

	req := httptest.NewRequest(http.MethodGet, "/asset.png", nil)
	req.Header.Set("If-None-Match", `"asset-v1"`)
	if !entry.matchesValidators(req) {
		t.Fatal("expected fresh cached validator to match")
	}

	entry.bodyFreshUntil = time.Now().Add(-time.Second)
	if entry.matchesValidators(req) {
		t.Fatal("expected stale cached validator to miss")
	}
}

func TestResponseCacheValidatorRespectsForcedRevalidation(t *testing.T) {
	cache := NewResponseCache()
	cache.Set("asset", http.StatusOK, []string{
		`ETag: "asset-v1"`,
		"Cache-Control: public, max-age=60",
	}, []byte("body"), time.Minute, nil)

	entry, ok := cache.Get("asset")
	if !ok {
		t.Fatal("expected cached entry")
	}

	req := httptest.NewRequest(http.MethodGet, "/asset.png", nil)
	req.Header.Set("If-None-Match", `"asset-v1"`)
	req.Header.Set("Cache-Control", "no-cache")

	if entry.matchesValidators(req) {
		t.Fatal("expected hard reload validator to bypass the cache")
	}
}

func TestResponseBodyFreshDurationAvoidsDevJSButAllowsImages(t *testing.T) {
	if got := responseBodyFreshDuration("/_next/static/chunks/main-app.js", []string{
		"Cache-Control: no-cache, must-revalidate",
	}); got != 0 {
		t.Fatalf("expected dev JS to require origin revalidation, got %s", got)
	}

	if got := responseBodyFreshDuration("/_next/image", []string{
		"Cache-Control: public, max-age=0, must-revalidate",
	}); got != 30*time.Second {
		t.Fatalf("expected short image freshness override, got %s", got)
	}
}

func TestDocumentFreshnessRequiresSharedMaxAge(t *testing.T) {
	if got := responseBodyFreshDuration("/", []string{
		"Content-Type: text/html; charset=utf-8",
	}); got != 0 {
		t.Fatalf("expected dev-server HTML to stay uncached, got %s", got)
	}
	if got := responseBodyFreshDuration("/", []string{
		"Cache-Control: s-maxage=31536000",
	}); got != documentMaxFresh {
		t.Fatalf("expected prerendered HTML capped at %s, got %s", documentMaxFresh, got)
	}
	if got := responseBodyFreshDuration("/api/items", []string{
		"Cache-Control: public, max-age=300",
	}); got != 0 {
		t.Fatalf("expected max-age alone not to cache a document, got %s", got)
	}
}

func TestCacheEntryHonorsVary(t *testing.T) {
	cache := NewResponseCache()
	html := httptest.NewRequest(http.MethodGet, "/", nil)
	cache.Set("doc", http.StatusOK, []string{
		"Cache-Control: s-maxage=60",
		"Vary: RSC, Accept-Encoding",
	}, []byte("<html>"), time.Minute, html.Header)

	entry, _ := cache.Get("doc")
	if !entry.canServeBody(html) {
		t.Fatal("expected HTML request to hit the stored document")
	}
	rsc := httptest.NewRequest(http.MethodGet, "/", nil)
	rsc.Header.Set("RSC", "1")
	if entry.canServeBody(rsc) {
		t.Fatal("expected RSC request to miss the HTML document")
	}
}

func TestCacheKeyRejectsPersonalizedRequests(t *testing.T) {
	server := &Server{Cache: NewResponseCache()}
	row := &Tunnel{HostID: "host", Target: "3000"}
	req := httptest.NewRequest(http.MethodGet, "https://abcdefghij12.repogo.dev/_next/image?url=%2Fhero.png&w=640&q=75", nil)
	req.Host = "abcdefghij12.repogo.dev"
	req.Header.Set("Cookie", "sid=private")

	if _, ok := server.cacheKey(req, row); ok {
		t.Fatal("expected cookie-bearing request to bypass the cache")
	}
}

func TestCacheKeyRejectsKeysAndTokens(t *testing.T) {
	server := &Server{Cache: NewResponseCache()}
	row := &Tunnel{HostID: "host", Target: "3000"}
	for _, name := range []string{"Authorization", "X-API-Key", "apikey", "X-Auth-Token", "X-Session-Token"} {
		req := httptest.NewRequest(http.MethodGet, "https://abcdefghij12.repogo.dev/app.js", nil)
		req.Header.Set(name, "secret")
		if _, ok := server.cacheKey(req, row); ok {
			t.Errorf("a request with %s was cacheable", name)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "https://abcdefghij12.repogo.dev/app.js", nil)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Safari")
	if _, ok := server.cacheKey(req, row); !ok {
		t.Error("an ordinary asset request was not cacheable")
	}
}

// JSON is an API's answer more often than an asset, so it waits for the
// server's own s-maxage.
func TestJSONNeedsTheServersSay(t *testing.T) {
	if got := responseBodyFreshDuration("/api/me.json", nil); got != 0 {
		t.Fatalf("headerless JSON cached for %s", got)
	}
	if got := responseBodyFreshDuration("/app.js", nil); got != 60*time.Second {
		t.Fatalf("headerless JS cached for %s", got)
	}
}

func TestResponseEncodingCompressibleStaticResponse(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/main-app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")

	if got := responseEncoding(req, http.StatusOK, []string{
		"Content-Type: application/javascript; charset=UTF-8",
	}); got != "br" {
		t.Fatalf("expected JS response to be brotli encoded for browser clients, got %q", got)
	}

	req.Header.Set("Accept-Encoding", "gzip, deflate")
	if got := responseEncoding(req, http.StatusOK, []string{
		"Content-Type: application/javascript; charset=UTF-8",
	}); got != "gzip" {
		t.Fatalf("expected gzip fallback when brotli is not accepted, got %q", got)
	}

	if got := responseEncoding(req, http.StatusOK, []string{
		"Content-Type: image/png",
	}); got != "" {
		t.Fatalf("did not expect image response to be encoded, got %q", got)
	}

	req.Header.Set("Range", "bytes=0-1023")
	if got := responseEncoding(req, http.StatusOK, []string{
		"Content-Type: application/javascript; charset=UTF-8",
	}); got != "" {
		t.Fatalf("did not expect range response to be encoded, got %q", got)
	}
}
