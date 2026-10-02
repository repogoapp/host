package session

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/agent"
)

// A user's images (a URL, or pasted base64 stored once in the attachment store)
// become `[@Image 1](…)` links in the prompt, the form the bridges write for a
// file sent from the app, so a client draws a tile for each.

// ImageLink is the prompt text for the nth image of a message, or empty when
// it cannot be linked.
func ImageLink(n int, url, mediaType, data string) string {
	target := ""
	switch {
	case strings.HasPrefix(url, "https://"):
		target = url
	case strings.HasPrefix(url, "data:"):
		mediaType, data = splitDataURL(url)
		fallthrough
	case data != "":
		if path := inlineImagePath(mediaType, data); path != "" {
			target = "file://" + path
		}
	}
	if target == "" {
		return ""
	}
	return fmt.Sprintf("[@Image %d](%s)", n, target)
}

// splitDataURL is `data:<type>;base64,<data>` taken apart.
func splitDataURL(url string) (mediaType, data string) {
	head, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || !strings.HasSuffix(head, ";base64") {
		return "", ""
	}
	return strings.TrimSuffix(head, ";base64"), data
}

// inlineImagePath writes base64 image data once to attachments/transcripts,
// named by FNV-1a over the text as native/rust also names it; empty when it
// cannot be stored.
func inlineImagePath(mediaType, data string) string {
	dir, err := agent.AttachmentsDir()
	if err != nil || data == "" {
		return ""
	}
	path := filepath.Join(dir, "transcripts", fmt.Sprintf("%016x.%s", FNV64(data), imageExt(mediaType)))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(raw) > agent.MaxUploadBytes {
		return ""
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return ""
	}
	// Written beside and renamed, so a reader never sees half an image.
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return ""
	}
	return path
}

func imageExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/heic":
		return "heic"
	}
	// A client picks the tile by extension and decodes whatever the bytes are.
	return "png"
}

// FNV64 is the 64-bit FNV-1a hash of s: a stable, cheap name for content,
// not a security hash.
func FNV64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// WithLinks appends image links to a prompt's text, one per line.
func WithLinks(text string, links []string) string {
	if len(links) == 0 {
		return text
	}
	all := strings.Join(links, "\n")
	if text == "" {
		return all
	}
	return text + "\n" + all
}
