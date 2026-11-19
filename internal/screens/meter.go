package screens

import (
	"sync"
	"time"
)

// statsEvery is how often a stream reports its stats to the phone.
const statsEvery = 500 * time.Millisecond

// maxEncodeDelay bounds a frame's capture-to-send delay that counts: a
// larger one is from another clock (a source that stamps its own time).
const maxEncodeDelay = 2 * time.Second

// meter counts a stream's video frames and their delay from capture to
// send, per stats window, and reports them with the pacer's state.
type meter struct {
	pace *pacer

	mu       sync.Mutex
	frames   int
	encodeUS int64
	encodes  int
	since    time.Time
}

func newMeter(pace *pacer) *meter { return &meter{pace: pace, since: time.Now()} }

// video counts a video frame sent, and the delay from its capture to now
// when it was stamped on the host's uptime clock.
func (m *meter) video(capturedUS uint64) {
	var delay int64
	if now := uptimeUS(); capturedUS > 0 && now > 0 && now >= int64(capturedUS) {
		delay = now - int64(capturedUS)
	}
	m.mu.Lock()
	m.frames++
	if delay > 0 && delay < maxEncodeDelay.Microseconds() {
		m.encodeUS += delay
		m.encodes++
	}
	m.mu.Unlock()
}

// report is the stats since the last report, which it starts again; false
// until the first frame is sent.
func (m *meter) report() (StreamStats, bool) {
	sent, inFlight, budget, bitrate := m.pace.pace()
	if sent == 0 {
		return StreamStats{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	stats := StreamStats{
		SentBytes:     sent,
		InFlightBytes: inFlight,
		BudgetBytes:   budget,
		BitrateBps:    bitrate,
		Frames:        m.frames,
		WindowMS:      int(now.Sub(m.since).Milliseconds()),
	}
	if m.encodes > 0 {
		ms := float64(m.encodeUS) / float64(m.encodes) / 1000
		stats.EncodeMS = &ms
	}
	m.frames, m.encodeUS, m.encodes, m.since = 0, 0, 0, now
	return stats, true
}
