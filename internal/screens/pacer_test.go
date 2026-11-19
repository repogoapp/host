package screens

import (
	"testing"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

var testRates = bitrateRange{start: 8_000_000, floor: 2_000_000, ceiling: 16_000_000}

// pacedTest is a pacer attached to a counter of the frames it allowed.
func pacedTest(t *testing.T) (*pacer, *int) {
	p, allowed, _ := pacedTestBitrates(t)
	return p, allowed
}

// pacedTestBitrates also keeps each bitrate the pacer set.
func pacedTestBitrates(t *testing.T) (*pacer, *int, *[]int) {
	t.Helper()
	p := newPacer()
	allowed, set := 0, []int{}
	t.Cleanup(p.attach(func(in capture.Input) error {
		if in.Credit > 0 {
			allowed++
		}
		if in.Bitrate > 0 {
			set = append(set, in.Bitrate)
		}
		return nil
	}, testRates))
	return p, &allowed, &set
}

func TestPacerLeavesAPhoneThatNeverAcksUnpaced(t *testing.T) {
	p, allowed := pacedTest(t)
	for range 300 {
		p.wrote(10_000, true)
	}
	if *allowed != 0 {
		t.Fatalf("allowed %d frames before any ack", *allowed)
	}
	if len(p.pending) > maxPending {
		t.Fatalf("remembered %d frames", len(p.pending))
	}
	if _, _, budget, _ := p.pace(); budget != nil {
		t.Fatalf("a budget while unpaced: %d", *budget)
	}
}

func TestPacerAllowsOneFrameAtATime(t *testing.T) {
	p, allowed := pacedTest(t)
	p.ack(0)
	if *allowed != 1 {
		t.Fatalf("first ack allowed %d", *allowed)
	}
	// More acks while that frame is still being made allow nothing more.
	p.ack(0)
	p.ack(0)
	if *allowed != 1 {
		t.Fatalf("allowed %d while one was out", *allowed)
	}
	// Sound doesn't count as the frame it allowed.
	p.wrote(300, false)
	if *allowed != 1 {
		t.Fatalf("sound allowed another: %d", *allowed)
	}
	p.wrote(20_000, true)
	if *allowed != 2 {
		t.Fatalf("a frame sent didn't allow the next: %d", *allowed)
	}
}

func TestPacerWaitsForRoomInFlight(t *testing.T) {
	p, allowed := pacedTest(t)
	p.ack(0)
	// A keyframe past the budget goes out alone; the next waits for its ack.
	p.wrote(startBudget+1, true)
	if *allowed != 1 {
		t.Fatalf("allowed %d with the budget full", *allowed)
	}
	p.ack(startBudget + 1)
	if *allowed != 2 {
		t.Fatalf("the ack didn't allow the next frame: %d", *allowed)
	}
	if len(p.rtts) != 1 {
		t.Fatalf("round trips = %d", len(p.rtts))
	}
}

func TestPacerBudgetIsRateTimesRoundTrip(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		rtt  time.Duration
		rate float64
		want int64
	}{
		{40 * time.Millisecond, 5e6, 300_000},   // 1.5 × 5 MB/s × 40 ms
		{40 * time.Millisecond, 2e5, minBudget}, // a slow link keeps one frame's room
		{2 * time.Second, 50e6, maxBudget},
	} {
		p := newPacer()
		// The fastest round trip and the best rate count.
		p.rtts = []sample{{now, float64(c.rtt)}, {now, float64(3 * c.rtt)}}
		p.rates = []sample{{now, c.rate / 2}, {now, c.rate}}
		if got := p.budgetLocked(now); got != c.want {
			t.Errorf("rtt %v, rate %v: budget %d, want %d", c.rtt, c.rate, got, c.want)
		}
	}
	// Old samples are forgotten: back to the starting budget.
	p := newPacer()
	p.rtts = []sample{{now.Add(-rttWindow - time.Second), float64(time.Millisecond)}}
	p.rates = []sample{{now, 1e6}}
	if got := p.budgetLocked(now); got != startBudget {
		t.Fatalf("stale round trip: budget %d", got)
	}
}

func TestPacerBitrateFollowsTheLink(t *testing.T) {
	p, _, set := pacedTestBitrates(t)
	at := time.Now()
	window := func() time.Time { at = at.Add(statsEvery); return at }
	// send writes bitsPerSecond for one window in small acked frames, so none waits.
	send := func(bitsPerSecond float64) {
		total := int(bitsPerSecond / 8 * statsEvery.Seconds())
		for sent := 0; sent < total; sent += 10_000 {
			p.wrote(10_000, true)
			p.ack(p.sent)
		}
	}
	// Unpaced (no ack yet): left alone.
	p.tick(30, window())
	if len(*set) != 1 {
		t.Fatalf("changed before pacing: %v", *set)
	}
	p.ack(0)
	// Frames flowing well under the target (a still or simple screen): no raise.
	send(2_000_000)
	p.tick(30, window())
	if len(*set) != 1 {
		t.Fatalf("raised while using a quarter of it: %v", *set)
	}
	// Using most of it, none waiting: up 5%.
	send(7_000_000)
	p.tick(30, window())
	if got := (*set)[len(*set)-1]; got != 8_400_000 {
		t.Fatalf("raised to %v", *set)
	}
	// Frames that wait for room, three in a window: down 20%.
	for range congestedWaits {
		p.wrote(startBudget, true)
	}
	p.tick(30, window())
	if got := (*set)[len(*set)-1]; got != 6_720_000 {
		t.Fatalf("cut to %d", got)
	}
	// Another congested window within a second isn't cut again yet.
	for range congestedWaits {
		p.wrote(startBudget, true)
	}
	n := len(*set)
	p.tick(30, window())
	if len(*set) != n {
		t.Fatalf("cut twice in a second: %v", *set)
	}
	// An idle window changes nothing.
	p.ack(p.sent)
	p.tick(0, window())
	if len(*set) != n {
		t.Fatalf("idle window changed it: %v", *set)
	}
	if p.bitrate != 6_720_000 {
		t.Fatalf("settled at %d", p.bitrate)
	}
}

// slowLink is a paced pacer whose fastest round trip was 40 ms, ticked
// once so the next window starts now.
func slowLink(t *testing.T) (*pacer, *[]int, time.Time) {
	t.Helper()
	p, _, set := pacedTestBitrates(t)
	p.ack(0)
	at := time.Now()
	p.rtts = []sample{{at, float64(40 * time.Millisecond)}}
	p.tick(0, at)
	return p, set, at
}

func TestPacerCutsTheBitrateWhenRoundTripsGrow(t *testing.T) {
	p, set, at := slowLink(t)
	// Two frames a second (nothing like three waits), but even the fastest
	// round trip this window is 150 ms against 40: a standing queue.
	slowTrips(p, at, 150*time.Millisecond)
	p.tick(1, at.Add(statsEvery))
	if got := (*set)[len(*set)-1]; got != 6_400_000 {
		t.Fatalf("cut to %v", *set)
	}
	// Deeply queued, a second later: cut by half.
	at = at.Add(cutEvery + statsEvery)
	slowTrips(p, at.Add(-statsEvery), 900*time.Millisecond)
	p.tick(1, at)
	if got := (*set)[len(*set)-1]; got != 3_200_000 {
		t.Fatalf("hard cut to %v", *set)
	}
	// Down to the floor, never past it.
	for range 10 {
		at = at.Add(cutEvery)
		slowTrips(p, at.Add(-statsEvery), 900*time.Millisecond)
		p.tick(1, at)
	}
	if got := (*set)[len(*set)-1]; got != testRates.floor {
		t.Fatalf("floor: %v", *set)
	}
}

func TestPacerCutsWhenHalfOfFewFramesWaited(t *testing.T) {
	p, set, at := slowLink(t)
	// One frame in the window, and it waited for room.
	p.wrote(startBudget, true)
	p.tick(1, at.Add(statsEvery))
	if got := (*set)[len(*set)-1]; got != 6_400_000 {
		t.Fatalf("cut to %v", *set)
	}
}

func TestPacerMeasuresDeliveryOverARoundTrip(t *testing.T) {
	p, _, _ := pacedTestBitrates(t)
	p.rtts = []sample{{time.Now(), float64(time.Second)}}
	p.ack(0)
	// A bunch of acks arriving at once over a link with a 1 s round trip:
	// no rate from spans shorter than the round trip.
	for i := range 20 {
		p.wrote(10_000, true)
		p.ack(int64((i + 1) * 10_000))
	}
	if len(p.rates) != 0 {
		t.Fatalf("rates from a bunch of acks: %v", p.rates)
	}
}

func TestPacerCutsStraightToWhatTheLinkDelivered(t *testing.T) {
	p, set, at := slowLink(t)
	// A backed-up window in which the phone got 300 KB in half a second
	// (4.8 Mbps), against an 8 Mbps target: down to 85 % of that at once,
	// past the 20 % step (6.4).
	p.wrote(320_000, true)
	p.ack(300_000)
	slowTrips(p, at, 150*time.Millisecond)
	p.held = 400 * time.Millisecond // held back most of the window: the link was the limit
	p.tick(1, at.Add(statsEvery))
	if got := (*set)[len(*set)-1]; got != 4_080_000 {
		t.Fatalf("cut to %v", *set)
	}
	// One big frame waiting on a quiet screen, round trips as fast as
	// ever: a step, not a crash to what the quiet window delivered.
	p3, set3, at3 := slowLink(t)
	p3.wrote(200_000, true)
	p3.ack(200_000) // a fast link: through within a round trip
	p3.tick(1, at3.Add(statsEvery))
	if got := (*set3)[len(*set3)-1]; got != 6_400_000 {
		t.Fatalf("quiet-window cut to %v", *set3)
	}
	// A stalled window (nothing delivered) falls back to the step.
	p2, set2, at2 := slowLink(t)
	slowTrips(p2, at2, 150*time.Millisecond)
	p2.tick(1, at2.Add(statsEvery))
	if got := (*set2)[len(*set2)-1]; got != 6_400_000 {
		t.Fatalf("stalled cut to %v", *set2)
	}
}

func TestPacerKeepsItsBudgetThroughAQuietSpell(t *testing.T) {
	p := newPacer()
	now := time.Now()
	p.rtts = []sample{{now, float64(250 * time.Millisecond)}}
	p.rates = []sample{{now, 60_000}} // a 3G link: 0.5 Mbps
	measured := p.budgetLocked(now)
	if measured >= startBudget {
		t.Fatalf("measured %d", measured)
	}
	// Ten seconds of a still screen: every sample has aged out.
	if got := p.budgetLocked(now.Add(rttWindow + time.Second)); got != measured {
		t.Fatalf("after a quiet spell: %d, want the measured %d", got, measured)
	}
}

func TestPacerBitrateStartsAtItsSourcesStart(t *testing.T) {
	if _, _, set := pacedTestBitrates(t); (*set)[0] != testRates.start {
		t.Fatalf("start = %v", *set)
	}
}

func TestPacerCutsALinkSlowFromTheStart(t *testing.T) {
	// A stream that starts on a slow link: its fastest round trip is the
	// slow one, so it never looks queued. Held back the whole window with
	// 8 Mbps asked of a link that delivered 0.48: straight down to fit.
	p, _, set := pacedTestBitrates(t)
	p.ack(0)
	start := time.Now()
	p.rtts = []sample{{start, float64(300 * time.Millisecond)}}
	p.tick(0, start)
	p.wrote(200_000, true) // past the budget: the next frame is held back
	p.ack(30_000)
	p.tick(1, start.Add(statsEvery))
	if got := (*set)[len(*set)-1]; got != testRates.floor {
		t.Fatalf("cut to %v, want the floor under 0.85 × 0.48 Mbps", *set)
	}
}

// slowTrips adds queuedSamples round trips of rtt to the window after from.
func slowTrips(p *pacer, from time.Time, rtt time.Duration) {
	for i := 1; i <= queuedSamples; i++ {
		p.rtts = append(p.rtts, sample{from.Add(time.Duration(i) * 100 * time.Millisecond), float64(rtt)})
	}
}

func TestPacerTakesAWiFiHiccupOnAStillScreenInStride(t *testing.T) {
	p, set, at := slowLink(t)
	// One slow round trip, a little sent: no cut at all.
	p.rtts = append(p.rtts, sample{at.Add(100 * time.Millisecond), float64(300 * time.Millisecond)})
	p.wrote(2_000, true)
	p.ack(2_000)
	p.tick(2, at.Add(statsEvery))
	if len(*set) != 1 {
		t.Fatalf("cut on one slow round trip: %v", *set)
	}
	// Queued for real on a still screen (300 ms against 40: deeply), but
	// never held back: the halving step, not a jump to the little that was
	// delivered.
	at = at.Add(cutEvery + statsEvery)
	slowTrips(p, at.Add(-statsEvery), 300*time.Millisecond)
	p.wrote(2_000, true)
	p.ack(p.sent)
	p.tick(2, at)
	if got := (*set)[len(*set)-1]; got != 4_000_000 {
		t.Fatalf("cut to %v", *set)
	}
}

func TestPacerResendsACreditTheEncoderNeverUsed(t *testing.T) {
	p, allowed := pacedTest(t)
	p.ack(0)
	if *allowed != 1 {
		t.Fatalf("allowed %d", *allowed)
	}
	at := time.Now()
	p.tick(0, at)
	// Under a second: still waiting for the frame.
	p.tick(0, at.Add(statsEvery))
	if *allowed != 1 {
		t.Fatalf("resent too soon: %d", *allowed)
	}
	// The encoder spent it on nothing: past a second, it goes again.
	p.tick(0, at.Add(reaskAfter+statsEvery))
	if *allowed != 2 {
		t.Fatalf("not resent: %d", *allowed)
	}
	// The frame comes: the next is allowed as usual, not twice.
	p.wrote(10_000, true)
	if *allowed != 3 {
		t.Fatalf("after the frame: %d", *allowed)
	}
}
