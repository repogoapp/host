package project

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// iconHash is one project's icon hash and the fingerprint it was taken under.
type iconHash struct {
	print string
	read  []string
	hash  string
}

// IconHash is the content_hash DetectIcon would answer for root, "" when it
// has none. root is a project the file service listed, so it is not contained
// again. Cached under a fingerprint of what detection looked at.
func (s *Service) IconHash(root string) string {
	s.mu.Lock()
	cached, ok := s.hashes[root]
	s.mu.Unlock()
	if ok && fingerprint(root, cached.read) == cached.print {
		return cached.hash
	}

	hit, read := detect(root)
	next := iconHash{print: fingerprint(root, read), read: read}
	if hit != nil {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(hit.path)))
		if err == nil && len(data) <= maxBytes(hit.path) {
			sum := sha256.Sum256(data)
			next.hash = hex.EncodeToString(sum[:])
		}
	}
	s.mu.Lock()
	s.hashes[root] = next
	s.mu.Unlock()
	return next.hash
}

// fingerprint changes when detection could answer differently: a probed
// folder gains or loses a file (its mtime moves), or a file detection looked
// at is written in place (its size or mtime moves).
func fingerprint(root string, read []string) string {
	var b strings.Builder
	for _, dir := range probeDirs {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			b.WriteString("-;")
			continue
		}
		fmt.Fprintf(&b, "%d;", info.ModTime().UnixNano())
	}
	for _, rel := range read {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			b.WriteString("-;")
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d;", rel, info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}
