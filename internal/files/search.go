package files

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"unicode/utf16"
	"unicode/utf8"
)

// SearchRange is one match's columns within its line.
type SearchRange struct {
	Start int `json:"start_col"`
	End   int `json:"end_col"`
}
type SearchMatch struct {
	Line    int           `json:"line"`
	Snippet string        `json:"snippet"`
	Ranges  []SearchRange `json:"ranges" wire:"array"`
}
type SearchFile struct {
	Path    string        `json:"path"`
	Matches []SearchMatch `json:"matches" wire:"array"`
}
type SearchResult struct {
	Results   []SearchFile `json:"results" wire:"array"`
	Truncated bool         `json:"truncated"`
}

// SearchMode is what a search matches: file names or file contents.
type SearchMode string

const (
	SearchFilename SearchMode = "filename"
	SearchContent  SearchMode = "content"
)

// Search bounds: a phone asked a question, it did not ask for the whole tree.
const (
	maxQueryBytes    = 4096
	defaultMatches   = 100
	maxMatches       = 1000
	maxSearchEntries = 20000
	maxSearchRead    = 32 << 20
	maxSnippetBytes  = 8192
	maxOutputBytes   = 512 << 10
)

// skippedDirs are build output and dependencies, never what a user searches for.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, ".build": true, "build": true, "dist": true, "vendor": true}

// Search finds query in the file names or contents under path, up to limit
// matches; ranges are UTF-16 columns, as the phone's text views count them.
func (s *Service) Search(path, query string, mode SearchMode, limit int, caseSensitive bool) (SearchResult, error) {
	result := SearchResult{Results: []SearchFile{}}
	if query == "" || len(query) > maxQueryBytes || (mode != SearchFilename && mode != SearchContent) || limit < 0 {
		return result, fmt.Errorf("%w: invalid search arguments", ErrInvalidOperation)
	}
	full, err := s.Contain(path)
	if err != nil {
		return result, err
	}
	root, err := os.OpenRoot(full)
	if err != nil {
		return result, wrap(err)
	}
	defer root.Close()
	pattern := regexp.QuoteMeta(query)
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	scan := &searchScan{root: root, matcher: regexp.MustCompile(pattern), limit: clampLimit(limit), result: &result}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		scan.entries++
		if scan.entries > maxSearchEntries || scan.matches >= scan.limit || scan.bytesRead >= maxSearchRead {
			result.Truncated = true
			return fs.SkipAll
		}
		if entry.IsDir() {
			if skippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if mode == SearchFilename {
			if scan.matcher.MatchString(name) {
				result.Results = append(result.Results, SearchFile{Path: filepath.ToSlash(name), Matches: []SearchMatch{}})
				scan.matches++
			}
			return nil
		}
		return scan.scanFile(name, entry)
	})
	return result, wrap(err)
}

func clampLimit(limit int) int {
	if limit == 0 {
		return defaultMatches
	}
	return min(limit, maxMatches)
}

// searchScan is one content search's running totals.
type searchScan struct {
	root    *os.Root
	matcher *regexp.Regexp
	limit   int
	result  *SearchResult

	entries, matches, outputBytes int
	bytesRead                     int64
}

// scanFile adds name's matching lines; a file too large, binary, or not UTF-8 is
// skipped, the first two marking the result truncated.
func (sc *searchScan) scanFile(name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > MaxFileBytes {
		sc.result.Truncated = true
		return nil
	}
	file, err := sc.root.Open(name)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	file.Close()
	if err != nil {
		return err
	}
	sc.bytesRead += int64(len(data))
	if len(data) > MaxFileBytes {
		sc.result.Truncated = true
		return nil
	}
	if isBinary(data) || !utf8.Valid(data) {
		return nil
	}
	found := SearchFile{Path: filepath.ToSlash(name), Matches: []SearchMatch{}}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), MaxFileBytes+1)
	for line := 0; scanner.Scan(); line++ {
		snippet := scanner.Text()
		if len(snippet) > maxSnippetBytes {
			sc.result.Truncated = true
			continue
		}
		locations := sc.matcher.FindAllStringIndex(snippet, sc.limit-sc.matches+1)
		if len(locations) == 0 {
			continue
		}
		if sc.matches >= sc.limit || sc.outputBytes+len(snippet) > maxOutputBytes {
			sc.result.Truncated = true
			break
		}
		sc.outputBytes += len(snippet)
		ranges := make([]SearchRange, 0, len(locations))
		for _, location := range locations {
			if sc.matches >= sc.limit {
				sc.result.Truncated = true
				break
			}
			ranges = append(ranges, SearchRange{utf16Len(snippet[:location[0]]), utf16Len(snippet[:location[1]])})
			sc.matches++
		}
		found.Matches = append(found.Matches, SearchMatch{line, snippet, ranges})
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(found.Matches) > 0 {
		sc.result.Results = append(sc.result.Results, found)
	}
	return nil
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }
