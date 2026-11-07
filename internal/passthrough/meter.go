package passthrough

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repogoapp/host/internal/device"
)

// Limits bound what one host's phone streams carry through the relay, counted
// together across all its phones and both directions. The relay never reads
// what it carries, so bytes are all it can meter. Zero fields are no limit.
type Limits struct {
	// Burst is how many bytes may pass at full speed before Rate applies.
	Burst int64
	// Rate is the sustained bytes per second once the burst is spent. Over it,
	// streams are slowed, never cut.
	Rate int64
	// DailyWarn logs a host once a UTC day when it passes this many bytes.
	DailyWarn int64
	// DailyCap refuses a host's streams for the rest of the UTC day once it
	// has passed this many bytes, and ends the ones open.
	DailyCap int64
	// Idle ends a stream that carried no bytes either way for this long.
	Idle time.Duration
}

// meter is one host's byte budget: a token bucket for speed and a counter
// for the UTC day.
type meter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	day    string
	used   int64
	warned bool
	capped bool
}

// errOverCap ends a stream whose host passed its daily cap.
var errOverCap = errors.New("passthrough: host over its daily cap")

func (r *Relay) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func (r *Relay) pause(d time.Duration) {
	if r.sleep != nil {
		r.sleep(d)
		return
	}
	time.Sleep(d)
}

// meterOf is id's meter, made with a full bucket on first use. Meters from an
// earlier day are dropped as new ones are made, so the map holds about one
// day's hosts.
func (r *Relay) meterOf(id device.ID) *meter {
	now := r.now()
	day := now.UTC().Format(time.DateOnly)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.meters == nil {
		r.meters = map[device.ID]*meter{}
	}
	if m := r.meters[id]; m != nil {
		return m
	}
	for other, m := range r.meters {
		m.mu.Lock()
		stale := m.day != day
		m.mu.Unlock()
		if stale {
			delete(r.meters, other)
		}
	}
	m := &meter{tokens: float64(r.Limits.Burst), last: now, day: day}
	r.meters[id] = m
	return m
}

// overCap reports whether id has passed today's cap, so a new stream is refused.
func (r *Relay) overCap(id device.ID) bool {
	if r.Limits.DailyCap <= 0 {
		return false
	}
	m := r.meterOf(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDay(r.now())
	return m.capped
}

// take counts n bytes for id and says how long to wait before passing them
// on, or errOverCap once the day's cap is spent.
func (r *Relay) take(id device.ID, m *meter, n int) (time.Duration, error) {
	lim := r.Limits
	now := r.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDay(now)
	if m.capped {
		return 0, errOverCap
	}
	m.used += int64(n)
	if lim.DailyWarn > 0 && !m.warned && m.used >= lim.DailyWarn {
		m.warned = true
		r.Log.Warn("passthrough: host passed its daily warning", "host", id, "bytes", m.used)
	}
	if lim.DailyCap > 0 && m.used >= lim.DailyCap {
		m.capped = true
		r.Log.Warn("passthrough: host passed its daily cap; refused until midnight UTC", "host", id, "bytes", m.used)
	}
	if lim.Rate <= 0 {
		return 0, nil
	}
	// The bucket refills at Rate up to Burst; a deficit is paid by waiting, so
	// a host's streams share one speed however many there are.
	m.tokens = min(float64(lim.Burst), m.tokens+now.Sub(m.last).Seconds()*float64(lim.Rate))
	m.last = now
	m.tokens -= float64(n)
	if m.tokens >= 0 {
		return 0, nil
	}
	return time.Duration(-m.tokens / float64(lim.Rate) * float64(time.Second)), nil
}

// rollDay starts a new count at midnight UTC. The bucket carries over.
func (m *meter) rollDay(now time.Time) {
	if day := now.UTC().Format(time.DateOnly); day != m.day {
		m.day, m.used, m.warned, m.capped = day, 0, false, false
	}
}

// splice pipes a phone's stream and its host's both ways under id's limits,
// until both directions end, the stream sits idle, or the host passes its cap.
func (r *Relay) splice(id device.ID, a, b *replayConn) {
	m := r.meterOf(id)
	var active atomic.Int64
	active.Store(time.Now().UnixNano())
	var stop sync.Once
	end := func() { stop.Do(func() { a.Close(); b.Close() }) }
	done := make(chan struct{}, 2)
	pipe := func(dst, src *replayConn) {
		defer func() { done <- struct{}{} }()
		if err := r.copyMetered(id, m, dst, src, &active); err != nil {
			// Idle or over the cap: the whole stream goes, not one direction.
			end()
			return
		}
		dst.CloseWrite()
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
}

// errIdle ends a stream that carried nothing either way for Limits.Idle.
var errIdle = errors.New("passthrough: stream idle")

// copyMetered copies src to dst, metering each read. It returns nil when src
// ends, errIdle or errOverCap when the stream must close, or a socket error.
func (r *Relay) copyMetered(id device.ID, m *meter, dst, src *replayConn, active *atomic.Int64) error {
	idle := r.Limits.Idle
	buf := make([]byte, 32<<10)
	for {
		if idle > 0 {
			src.SetReadDeadline(time.Now().Add(idle))
		}
		n, err := src.Read(buf)
		if n > 0 {
			active.Store(time.Now().UnixNano())
			wait, cerr := r.take(id, m, n)
			if cerr != nil {
				return cerr
			}
			if wait > 0 {
				r.pause(wait)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return nil
		case isTimeout(err) && idle > 0:
			// This direction went quiet; the stream is idle only if the other did too.
			if time.Since(time.Unix(0, active.Load())) >= idle {
				return errIdle
			}
		default:
			return err
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
