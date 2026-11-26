package tunnelgateway

import (
	"container/list"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cacheMaxBytes = 128 * 1024 * 1024
	cacheMaxItem  = 32 * 1024 * 1024
	// A rebuilt page keeps its URL, so a document is never held longer than
	// this no matter how large the app's s-maxage is. A hard reload
	// (Cache-Control: no-cache) bypasses it entirely.
	documentMaxFresh = 60 * time.Second
)

type ResponseCache struct {
	mu       sync.Mutex
	maxBytes int64
	maxItem  int
	total    int64
	items    map[string]*cacheEntry
	lru      *list.List
}

type cacheEntry struct {
	key            string
	status         int
	headerLines    []string
	body           []byte
	etag           string
	lastModified   string
	bodyFreshUntil time.Time
	// Request header values named by the response's Vary, captured at store
	// time. Next serves an RSC payload and an HTML document at the same URL,
	// distinguished only by the headers it lists there.
	vary map[string]string
	elem *list.Element
}

func NewResponseCache() *ResponseCache {
	return &ResponseCache{
		maxBytes: cacheMaxBytes,
		maxItem:  cacheMaxItem,
		items:    map[string]*cacheEntry{},
		lru:      list.New(),
	}
}

func (c *ResponseCache) MaxItemBytes() int {
	return c.maxItem
}

func (c *ResponseCache) Get(key string) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := c.items[key]
	if entry == nil {
		return nil, false
	}
	c.lru.MoveToFront(entry.elem)
	return entry.copy(), true
}

func (c *ResponseCache) Set(key string, status int, headerLines []string, body []byte, bodyFreshFor time.Duration, reqHeader http.Header) {
	if status != http.StatusOK || len(body) == 0 || len(body) > c.maxItem || bodyFreshFor <= 0 {
		return
	}

	entry := &cacheEntry{
		key:            key,
		status:         status,
		headerLines:    append([]string(nil), headerLines...),
		body:           append([]byte(nil), body...),
		etag:           headerLineValue(headerLines, "etag"),
		lastModified:   headerLineValue(headerLines, "last-modified"),
		bodyFreshUntil: time.Now().Add(bodyFreshFor),
		vary:           varyValues(headerLines, reqHeader),
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if old := c.items[key]; old != nil {
		c.remove(old)
	}
	entry.elem = c.lru.PushFront(key)
	c.items[key] = entry
	c.total += int64(len(entry.body))

	for c.total > c.maxBytes {
		back := c.lru.Back()
		if back == nil {
			return
		}
		oldKey, _ := back.Value.(string)
		c.remove(c.items[oldKey])
	}
}

func (c *ResponseCache) remove(entry *cacheEntry) {
	if entry == nil {
		return
	}
	delete(c.items, entry.key)
	if entry.elem != nil {
		c.lru.Remove(entry.elem)
	}
	c.total -= int64(len(entry.body))
	if c.total < 0 {
		c.total = 0
	}
}

func (e *cacheEntry) copy() *cacheEntry {
	return &cacheEntry{
		key:            e.key,
		status:         e.status,
		headerLines:    append([]string(nil), e.headerLines...),
		body:           e.body,
		etag:           e.etag,
		lastModified:   e.lastModified,
		bodyFreshUntil: e.bodyFreshUntil,
		vary:           e.vary,
	}
}

func varyValues(headerLines []string, reqHeader http.Header) map[string]string {
	vary := headerLineValue(headerLines, "vary")
	if vary == "" {
		return nil
	}
	values := map[string]string{}
	for _, name := range strings.Split(vary, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || name == "accept-encoding" {
			// The gateway re-encodes on every serve, so the client's
			// Accept-Encoding never selects a different stored body.
			continue
		}
		values[name] = reqHeader.Get(name)
	}
	return values
}

func (e *cacheEntry) variesMatch(r *http.Request) bool {
	for name, value := range e.vary {
		if r.Header.Get(name) != value {
			return false
		}
	}
	return true
}

func (e *cacheEntry) matchesValidators(r *http.Request) bool {
	if requestForcesRevalidation(r) || !e.variesMatch(r) {
		return false
	}
	if !time.Now().Before(e.bodyFreshUntil) {
		return false
	}
	if e.etag != "" && etagMatches(r.Header.Get("If-None-Match"), e.etag) {
		return true
	}
	if e.lastModified == "" {
		return false
	}
	ifModifiedSince := r.Header.Get("If-Modified-Since")
	if ifModifiedSince == "" {
		return false
	}
	requestTime, err := http.ParseTime(ifModifiedSince)
	if err != nil {
		return false
	}
	modifiedTime, err := http.ParseTime(e.lastModified)
	if err != nil {
		return false
	}
	return !modifiedTime.After(requestTime)
}

func (e *cacheEntry) canServeBody(r *http.Request) bool {
	if requestForcesRevalidation(r) || !e.variesMatch(r) {
		return false
	}
	return time.Now().Before(e.bodyFreshUntil)
}

func etagMatches(header string, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, token := range strings.Split(header, ",") {
		if strings.TrimSpace(token) == "*" || strings.TrimSpace(token) == etag {
			return true
		}
	}
	return false
}

func requestForcesRevalidation(r *http.Request) bool {
	cacheControl := strings.ToLower(r.Header.Get("Cache-Control"))
	return strings.Contains(cacheControl, "no-cache") ||
		strings.Contains(cacheControl, "max-age=0") ||
		strings.EqualFold(r.Header.Get("Pragma"), "no-cache")
}

func headerLineValue(lines []string, name string) string {
	name = strings.ToLower(name)
	for _, line := range lines {
		idx := strings.Index(line, ": ")
		if idx <= 0 {
			continue
		}
		if strings.ToLower(line[:idx]) == name {
			return line[idx+2:]
		}
	}
	return ""
}

func cacheFreshDuration(path string) time.Duration {
	path = strings.ToLower(path)
	if strings.HasPrefix(path, "/_next/static/") {
		return 5 * time.Minute
	}
	if path == "/_next/image" {
		return 30 * time.Second
	}
	return 60 * time.Second
}

func responseBodyFreshDuration(path string, headerLines []string) time.Duration {
	if !isStaticAsset(strings.ToLower(path)) {
		// Documents are cached only on the app's explicit shared-cache opt-in
		// (a Next prerender sends s-maxage). Dev-server HTML never carries
		// one, so a live tunnel stays live.
		return min(cacheDirectiveSeconds(headerLines, "s-maxage"), documentMaxFresh)
	}
	if ttl := cacheMaxAge(headerLines); ttl > 0 {
		return ttl
	}
	if responseForcesRevalidation(headerLines) && !canOverrideRevalidation(path) {
		return 0
	}
	return cacheFreshDuration(path)
}

func cacheMaxAge(headerLines []string) time.Duration {
	return cacheDirectiveSeconds(headerLines, "max-age")
}

func cacheDirectiveSeconds(headerLines []string, directive string) time.Duration {
	cacheControl := strings.ToLower(headerLineValue(headerLines, "cache-control"))
	if cacheControl == "" {
		return 0
	}
	prefix := directive + "="
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		seconds, err := strconv.Atoi(strings.TrimPrefix(part, prefix))
		if err != nil || seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func responseAllowsCache(headerLines []string) bool {
	cacheControl := strings.ToLower(headerLineValue(headerLines, "cache-control"))
	vary := strings.ToLower(headerLineValue(headerLines, "vary"))
	return !strings.Contains(cacheControl, "no-store") &&
		!strings.Contains(cacheControl, "private") &&
		!strings.Contains(vary, "*") &&
		!strings.Contains(vary, "authorization") &&
		!strings.Contains(vary, "cookie") &&
		headerLineValue(headerLines, "set-cookie") == ""
}

func responseForcesRevalidation(headerLines []string) bool {
	cacheControl := strings.ToLower(headerLineValue(headerLines, "cache-control"))
	return strings.Contains(cacheControl, "no-cache") ||
		strings.Contains(cacheControl, "must-revalidate") ||
		strings.Contains(cacheControl, "max-age=0")
}

func canOverrideRevalidation(path string) bool {
	path = strings.ToLower(path)
	if path == "/_next/image" {
		return true
	}
	switch {
	case strings.HasSuffix(path, ".avif"),
		strings.HasSuffix(path, ".gif"),
		strings.HasSuffix(path, ".ico"),
		strings.HasSuffix(path, ".jpeg"),
		strings.HasSuffix(path, ".jpg"),
		strings.HasSuffix(path, ".mp4"),
		strings.HasSuffix(path, ".otf"),
		strings.HasSuffix(path, ".png"),
		strings.HasSuffix(path, ".svg"),
		strings.HasSuffix(path, ".ttf"),
		strings.HasSuffix(path, ".webm"),
		strings.HasSuffix(path, ".webp"),
		strings.HasSuffix(path, ".woff"),
		strings.HasSuffix(path, ".woff2"):
		return true
	default:
		return false
	}
}
