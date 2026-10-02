package session

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session/native"
)

const (
	// LineLimit matches the live-stream parser: tool results in every format
	// run to tens of KB and one Codex output line measured over 40KB, so
	// bufio's 64KB default would truncate real conversations.
	LineLimit = 16 << 20

	tailBuffer   = 256
	pollInterval = 250 * time.Millisecond
)

// ReadFile parses from byteOffset to the last complete line and returns the
// offset it stopped at; an agent is mid-write often enough that a half-line is
// the normal case.
func ReadFile(path string, byteOffset int64, p Provider, sessionID string, startSeq uint64) ([]agent.Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, byteOffset, err
	}
	defer f.Close()

	if byteOffset > 0 {
		if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
			return nil, byteOffset, err
		}
	}

	var (
		events   []agent.Event
		consumed = byteOffset
		seq      = startSeq
	)

	r := bufio.NewReaderSize(f, 64<<10)
	var longLine []byte
	for {
		line, err := r.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull {
			// No trailing newline means the writer is mid-line. Leave the
			// offset before it so the next pass re-reads it whole.
			break
		}
		// Parse borrows the reader buffer; only lines spanning buffers need a copy.
		if err == bufio.ErrBufferFull || len(longLine) > 0 {
			longLine = append(longLine, line...)
			if err == bufio.ErrBufferFull {
				continue
			}
			line = longLine
			longLine = longLine[:0]
		}
		consumed += int64(len(line))
		if len(line) > LineLimit {
			continue
		}
		for _, e := range Parse(p, line) {
			seq++
			e.Seq = seq
			e.SessionID = sessionID
			events = append(events, e)
		}
	}
	return events, consumed, nil
}

// readSegment is ReadFile from the start of a file, which a sync does
// thousands of times: the native parser when built in, else (or when it
// declines) ReadFile, so a sync never fails on the library's account.
func readSegment(path string, p Provider, sessionID string, startSeq uint64) ([]agent.Event, error) {
	if reader, ok := p.(FileParser); ok && native.Available {
		events, err := reader.ParseFile(path, sessionID)
		if err == nil {
			seq := startSeq
			for i := range events {
				seq++
				events[i].Seq = seq
				events[i].SessionID = sessionID
			}
			return events, nil
		}
	}
	events, _, err := ReadFile(path, 0, p, sessionID, startSeq)
	return events, err
}

// ReadAll parses every segment of a session in order, carrying the sequence
// across file boundaries so the result reads as one conversation, then names
// each row's turn from the whole of it.
func ReadAll(m Meta, p Provider) ([]agent.Event, error) {
	if reader, ok := p.(SnapshotReader); ok {
		return reader.Snapshot(m)
	}
	events, err := readSegments(m, p)
	if err != nil {
		return events, err
	}
	if normalizer, ok := p.(Normalizer); ok {
		events = normalizer.Normalize(events)
	}
	return events, nil
}

func readSegments(m Meta, p Provider) ([]agent.Event, error) {
	files := m.Files()
	if len(files) == 1 {
		return readSegment(files[0], p, m.ID, 0)
	}
	var all []agent.Event
	var seq uint64
	for _, f := range files {
		events, err := readSegment(f, p, m.ID, seq)
		if err != nil {
			// A missing or unreadable segment loses that slice of history but
			// must not lose the rest.
			continue
		}
		if len(events) > 0 {
			seq = events[len(events)-1].Seq
		}
		all = append(all, events...)
	}
	return all, nil
}

// tail replays the file then polls it for appends. Polling rather than
// fsnotify: a 250ms stat is imperceptible, identical on every platform, and
// avoids editor-rename and coalesced-event cases.
func tail(ctx context.Context, path string, p Provider, sessionID string, out chan<- agent.Event) {
	defer close(out)

	var (
		offset int64
		seq    uint64
	)

	emit := func(events []agent.Event) bool {
		for _, e := range events {
			select {
			case out <- e:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}

	events, next, err := ReadFile(path, 0, p, sessionID, seq)
	if err != nil {
		return
	}
	offset = next
	if len(events) > 0 {
		seq = events[len(events)-1].Seq
	}
	if !emit(events) {
		return
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := os.Stat(path)
			if err != nil {
				// A session file can be compacted or archived out from under
				// us. Nothing useful to say; stop cleanly.
				return
			}
			switch {
			case st.Size() == offset:
				continue
			case st.Size() < offset:
				// Truncated or replaced. Re-read from the top rather than
				// seeking into the middle of a different file.
				offset, seq = 0, 0
			}

			events, next, err := ReadFile(path, offset, p, sessionID, seq)
			if err != nil {
				return
			}
			offset = next
			if len(events) > 0 {
				seq = events[len(events)-1].Seq
			}
			if !emit(events) {
				return
			}
		}
	}
}
