package session

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
)

// Listing thousands of sessions has to stay cheap: peeks read a fixed window
// rather than the file, and results are cached against (size, mtime) so an
// idle session is parsed once.
const (
	peekHead = 64 << 10
	peekTail = 256 << 10
)

// PeekCache holds each file's last peek, keyed by path, so a provider parses
// an unchanged session once.
type PeekCache[T any] struct {
	mu      sync.Mutex
	entries map[string]cachedPeek[T]
}

type cachedPeek[T any] struct {
	size  int64
	mtime int64
	res   T
}

// NewPeekCache returns an empty cache.
func NewPeekCache[T any]() *PeekCache[T] {
	return &PeekCache[T]{entries: make(map[string]cachedPeek[T])}
}

// Get returns a cached peek if the file has not changed since it was taken.
// Size and mtime together are enough: a session file is append-only, so any
// content change moves at least one of them.
func (c *PeekCache[T]) Get(path string, info os.FileInfo) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[path]
	if !ok || e.size != info.Size() || e.mtime != info.ModTime().UnixNano() {
		var zero T
		return zero, false
	}
	return e.res, true
}

// Put records res as path's peek at the size and mtime in info.
func (c *PeekCache[T]) Put(path string, info os.FileInfo, res T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[path] = cachedPeek[T]{size: info.Size(), mtime: info.ModTime().UnixNano(), res: res}
}

// PeekFile is one transcript a provider's List found, before its peek.
type PeekFile struct {
	Path string
	Info os.FileInfo
}

// PeekEach runs peek on every file across the cores, results in input order:
// a cold List is thousands of small reads, and one at a time is most of its cost.
func PeekEach[T any](files []PeekFile, peek func(path string, info os.FileInfo) T) []T {
	out := make([]T, len(files))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < len(files); i = int(next.Add(1) - 1) {
				out[i] = peek(files[i].Path, files[i].Info)
			}
		}()
	}
	wg.Wait()
	return out
}

// ScanHead runs fn over complete lines from the start of the file, stopping
// after peekHead bytes or when fn returns false.
func ScanHead(path string, fn func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanLimited(io.LimitReader(f, peekHead), fn)
}

// ScanTail runs fn over complete lines from the last peekTail bytes. The first
// line is almost certainly a fragment and is skipped.
func ScanTail(path string, size int64, fn func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	start := size - peekTail
	if start <= 0 {
		scanLimited(f, fn)
		return
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 64<<10)
	if _, err := r.ReadBytes('\n'); err != nil {
		return
	}
	scanLimited(r, fn)
}

func scanLimited(r io.Reader, fn func(line []byte) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), LineLimit)
	for sc.Scan() {
		if !fn(sc.Bytes()) {
			return
		}
	}
}
