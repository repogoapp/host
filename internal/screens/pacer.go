package screens

import (
	"sync"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

// The pacer's bounds. Before it has measured anything it lets
// startBudget be in flight; after, the measured budget, kept within these.
const (
	startBudget = 128 << 10
	// minBudget is about one frame while the picture changes (the encoder's
	// quantizer floor keeps most under 16 KB); a bigger one goes alone. A
	// larger floor is a standing queue on a slow link: 64 KB was 0.7 s on
	// a 0.8 Mbps link.
	minBudget = 16 << 10
	maxBudget = 4 << 20
	// budgetGain is the budget over one round trip's worth of the delivery
	// rate: room for a frame in transit without a standing queue.
	budgetGain = 1.5
	// rttWindow and rateWindow are how long the fastest round trip and the
	// best delivery rate are remembered: long enough to outlast an idle
	// spell, short enough to follow a network that got slower.
	rttWindow  = 10 * time.Second
	rateWindow = 2 * time.Second
	// A delivery rate is measured over at least rateSpan and at least the
	// fastest round trip: a slow link releases acks in bunches, and a
	// bunch over a short span reads as a burst of fast delivery.
	rateSpan = 50 * time.Millisecond
	// maxPending bounds the frames remembered for their round trip.
	maxPending = 256
	// reaskAfter is how long a credit may go unused before it's resent.
	reaskAfter = time.Second
	// The bitrate is cut (at most once per cutEvery) in a window where the
	// link backed up: frames waited for room, or even its fastest round trip
	// was past queuedRTT times the fastest recent one plus queuedSlack;
	// bitrateCutHard past deeplyQueuedRTT. It's raised in a window that
	// flowed with no waits and sent at least raiseWhenUsing of the target.
	congestedWaits  = 3
	queuedRTT       = 2.0
	queuedSlack     = 40 * time.Millisecond
	queuedSamples   = 3
	deeplyQueuedRTT = 4.0
	bitrateCut      = 0.8
	bitrateCutHard  = 0.5
	// fitDelivered is the share of a backed-up window's delivery rate a cut
	// goes to when that's lower than the cut: under what the link carried,
	// so the queue drains.
	fitDelivered = 0.85
	// heldMost and overDelivered make a window slow: held back at least
	// heldMost of it, with the target over overDelivered times what was
	// delivered.
	heldMost       = 0.5
	overDelivered  = 3.0
	bitrateRaise   = 1.05
	raiseWhenUsing = 0.7
	cutEvery       = time.Second
	flowingFrames  = 5
)

// bitrateRange is where a source's bitrate starts and stays.
type bitrateRange struct{ start, floor, ceiling int }

// bitrates is each kind of screen's range: a Mac's small text needs more.
func bitrates(kind Kind) bitrateRange {
	switch kind {
	case KindDisplay:
		return bitrateRange{start: 8_000_000, floor: 500_000, ceiling: 16_000_000}
	case KindSimulator:
		return bitrateRange{start: 4_000_000, floor: 300_000, ceiling: 10_000_000}
	default:
		return bitrateRange{start: 6_000_000, floor: 400_000, ceiling: 12_000_000}
	}
}

// pacer lets the encoder make one frame at a time, while what's sent but
// not acked is under the budget (delivery rate times fastest round trip):
// a slow link gets fewer, current frames, never a queue, and nothing is
// dropped after encoding. It sets the bitrate too. It paces only once the
// phone acks; an older phone gets every frame, as before.
type pacer struct {
	mu sync.Mutex
	// child takes the pacer's credit and bitrate; nil for a source that
	// can't be paced (a phone's screenrecord, already encoded).
	child func(capture.Input) error
	paced bool
	// asked is true while the encoder may make a frame it hasn't sent yet,
	// since askedAt.
	asked   bool
	askedAt time.Time

	sent, acked int64
	// pending is each video frame sent and not yet acked: where it ended in
	// the byte count, and when it went.
	pending []sentFrame
	rtts    []sample // round trips, by when they were measured
	rates   []sample // delivery rates in bytes a second
	// rateFrom is where the delivery rate is being measured from.
	rateFrom    time.Time
	rateFromAck int64

	bitrate int
	range_  bitrateRange
	// waits counts video frames after which the budget was full, this window.
	waits   int
	lastCut time.Time
	// tickSent, tickAcked and tickAt are the byte counts and time at the
	// last tick, for the rates the stream sent and was delivered at since.
	tickSent, tickAcked int64
	tickAt              time.Time
	// heldSince is when the pacer last found no room for the next frame
	// (zero while it isn't holding one back), and held the time it spent
	// so this stats window: most of a window held is a link that can't
	// keep up, where one big frame on a quiet screen holds it a round trip.
	heldSince time.Time
	held      time.Duration
	// lastBudget is the last budget measured, for a quiet spell after it:
	// samples age out when nothing is sent, and starting over at
	// startBudget would let the next burst queue a second on a slow link.
	lastBudget int64
}

type sentFrame struct {
	end int64
	at  time.Time
}

type sample struct {
	at    time.Time
	value float64
}

func newPacer() *pacer { return &pacer{} }

// attach lets the pacer allow frames from an encoder it can pace, and set
// its bitrate within rates, from rates.start, until the returned func
// detaches it.
func (p *pacer) attach(child func(capture.Input) error, rates bitrateRange) (detach func()) {
	p.mu.Lock()
	start := rates.start
	p.child, p.asked, p.range_, p.bitrate = child, false, rates, start
	p.mu.Unlock()
	_ = child(capture.Input{Bitrate: start})
	p.next()
	return func() {
		p.mu.Lock()
		p.child = nil
		p.mu.Unlock()
	}
}

// wrote counts a binary frame the socket took, video or sound; a video
// frame was the one the encoder was allowed, so it may make the next.
func (p *pacer) wrote(n int, video bool) {
	now := time.Now()
	p.mu.Lock()
	p.sent += int64(n)
	if video {
		// A phone that never acks would grow it without end.
		if len(p.pending) >= maxPending {
			p.pending = p.pending[1:]
		}
		p.pending = append(p.pending, sentFrame{end: p.sent, at: now})
		p.asked = false
		if p.paced && p.sent-p.acked >= p.budgetLocked(now) {
			p.waits++
		}
	}
	p.mu.Unlock()
	if video {
		p.next()
	}
}

// ack takes the phone's count of bytes received: the round trip of the
// newest frame it covers, the delivery rate since the last, and room for
// the next frame. The first ack starts pacing.
func (p *pacer) ack(bytes int64) {
	now := time.Now()
	p.mu.Lock()
	if bytes > p.acked {
		p.acked = bytes
		var newest *sentFrame
		for len(p.pending) > 0 && p.pending[0].end <= bytes {
			newest = &p.pending[0]
			p.pending = p.pending[1:]
		}
		if newest != nil {
			p.rtts = keep(append(p.rtts, sample{now, float64(now.Sub(newest.at))}), now, rttWindow)
		}
		if p.rateFrom.IsZero() {
			p.rateFrom, p.rateFromAck = now, bytes
		} else if span := now.Sub(p.rateFrom); span >= max(rateSpan, p.minRTTLocked(now)) {
			rate := float64(bytes-p.rateFromAck) / span.Seconds()
			p.rates = keep(append(p.rates, sample{now, rate}), now, rateWindow)
			p.rateFrom, p.rateFromAck = now, bytes
		}
	}
	p.paced = true
	p.mu.Unlock()
	p.next()
}

// next allows the encoder one frame when it isn't already allowed one and
// there's room in flight.
func (p *pacer) next() {
	now := time.Now()
	p.mu.Lock()
	child := p.child
	if child == nil || !p.paced || p.asked {
		p.mu.Unlock()
		return
	}
	if p.sent-p.acked >= p.budgetLocked(now) {
		if p.heldSince.IsZero() {
			p.heldSince = now
		}
		p.mu.Unlock()
		return
	}
	if !p.heldSince.IsZero() {
		p.held += now.Sub(p.heldSince)
		p.heldSince = time.Time{}
	}
	p.asked, p.askedAt = true, now
	p.mu.Unlock()
	if child(capture.Input{Credit: 1}) != nil {
		p.mu.Lock()
		p.asked = false
		p.mu.Unlock()
	}
}

// tick ends a stats window in which `frames` were sent: the bitrate goes
// down if the link backed up, up if frames flowed, none waited and the
// stream used most of the target.
func (p *pacer) tick(frames int, now time.Time) {
	p.mu.Lock()
	child, next := p.child, p.bitrate
	var sentBps, deliveredBps float64
	if !p.tickAt.IsZero() {
		if span := now.Sub(p.tickAt).Seconds(); span > 0 {
			sentBps = float64(p.sent-p.tickSent) * 8 / span
			deliveredBps = float64(p.acked-p.tickAcked) * 8 / span
		}
	}
	queued := p.queuedLocked(now)
	if !p.heldSince.IsZero() {
		p.held += now.Sub(p.heldSince)
		p.heldSince = now
	}
	// Held back most of the window while the target is several times what
	// got through: a link too slow for the target, even one that was slow
	// from the start, with no fast round trip to judge a queue by.
	// busy is a window held back for room most of its length: the link
	// was the limit, so what it delivered is what it can carry.
	var busy, slow bool
	if span := now.Sub(p.tickAt); !p.tickAt.IsZero() && span > 0 {
		busy = float64(p.held) >= heldMost*float64(span)
		slow = busy && deliveredBps > 0 && float64(p.bitrate) > overDelivered*deliveredBps
	}
	p.held = 0
	p.tickSent, p.tickAcked, p.tickAt = p.sent, p.acked, now
	if child == nil || !p.paced {
		p.waits = 0
		p.mu.Unlock()
		return
	}
	// A credit out this long is resent, in case the encoder spent it on
	// nothing: the frame it allowed never came, and the stream would wait
	// for it until the phone reconnected. A repeat only sets the encoder's
	// credit to 1 again, so on a still screen it's harmless.
	reask := p.asked && now.Sub(p.askedAt) >= reaskAfter
	if reask {
		p.askedAt = now
	}
	waited := p.waits >= congestedWaits || (p.waits > 0 && p.waits*2 >= frames)
	switch {
	case (waited || slow || queued > queuedRTT) && now.Sub(p.lastCut) >= cutEvery:
		cut := bitrateCut
		if queued > deeplyQueuedRTT {
			cut = bitrateCutHard
		}
		next = int(float64(p.bitrate) * cut)
		// Busy, what the link delivered is what it can carry: straight under
		// it, not a cut a second (8 to 0.5 Mbps took 20 s on 3G). Only then:
		// a quiet screen delivers little because little was sent (a Wi-Fi
		// hiccup on a still one took 6.7 Mbps to 0.5).
		if fit := int(fitDelivered * deliveredBps); busy && fit > 0 && fit < next {
			next = fit
		}
		next = max(p.range_.floor, next)
		p.lastCut = now
	case p.waits == 0 && frames >= flowingFrames && sentBps >= raiseWhenUsing*float64(p.bitrate):
		next = min(p.range_.ceiling, int(float64(p.bitrate)*bitrateRaise))
	}
	p.waits = 0
	changed := next != p.bitrate
	p.bitrate = next
	p.mu.Unlock()
	if changed {
		_ = child(capture.Input{Bitrate: next})
	}
	if reask {
		_ = child(capture.Input{Credit: 1})
	}
}

// pace is the pacer's state for the stats: in flight, and the budget and
// bitrate while pacing.
func (p *pacer) pace() (sent, inFlight int64, budget *int64, bitrate *int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paced && p.child != nil {
		b, r := p.budgetLocked(time.Now()), p.bitrate
		budget, bitrate = &b, &r
	}
	return p.sent, max(0, p.sent-p.acked), budget, bitrate
}

// budgetLocked is how many bytes may be in flight: the best recent
// delivery rate over the fastest recent round trip, with budgetGain; the
// last one measured while there are no recent samples, startBudget before
// any.
func (p *pacer) budgetLocked(now time.Time) int64 {
	p.rates = keep(p.rates, now, rateWindow)
	rtt := p.minRTTLocked(now)
	if rtt == 0 || len(p.rates) == 0 {
		if p.lastBudget > 0 {
			return p.lastBudget
		}
		return startBudget
	}
	rate := 0.0
	for _, s := range p.rates {
		rate = max(rate, s.value)
	}
	budget := min(max(int64(budgetGain*rate*rtt.Seconds()), minBudget), maxBudget)
	p.lastBudget = budget
	return budget
}

// minRTTLocked is the fastest recent round trip, 0 before any.
func (p *pacer) minRTTLocked(now time.Time) time.Duration {
	p.rtts = keep(p.rtts, now, rttWindow)
	if len(p.rtts) == 0 {
		return 0
	}
	rtt := p.rtts[0].value
	for _, s := range p.rtts {
		rtt = min(rtt, s.value)
	}
	return time.Duration(rtt)
}

// queuedLocked is how far the link has backed up since the last tick: the
// window's fastest round trip, less queuedSlack, over the fastest recent
// one. 0 when the window had fewer than queuedSamples round trips.
func (p *pacer) queuedLocked(now time.Time) float64 {
	base := p.minRTTLocked(now)
	if base == 0 {
		return 0
	}
	var fastest time.Duration
	samples := 0
	for _, s := range p.rtts {
		if s.at.After(p.tickAt) {
			samples++
			if fastest == 0 || time.Duration(s.value) < fastest {
				fastest = time.Duration(s.value)
			}
		}
	}
	// One or two slow round trips are a hiccup (Wi-Fi power saving), not
	// a queue.
	if samples < queuedSamples {
		return 0
	}
	return float64(max(0, fastest-queuedSlack)) / float64(base)
}

// keep drops samples older than window.
func keep(samples []sample, now time.Time, window time.Duration) []sample {
	i := 0
	for i < len(samples) && now.Sub(samples[i].at) > window {
		i++
	}
	return samples[i:]
}
