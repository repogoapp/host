package screens

import "testing"

func TestMeterReportsNothingBeforeTheFirstFrame(t *testing.T) {
	if _, ok := newMeter(newPacer()).report(); ok {
		t.Fatal("reported before anything was sent")
	}
}

func TestMeterReportsTheWindowAndWhatIsInFlight(t *testing.T) {
	p := newPacer()
	m := newMeter(p)
	p.wrote(1000, true)
	p.wrote(500, false)
	m.video(0)
	p.ack(600)
	stats, ok := m.report()
	if !ok || stats.SentBytes != 1500 || stats.InFlightBytes != 900 || stats.Frames != 1 {
		t.Fatalf("stats = %+v, %v", stats, ok)
	}
	if stats.EncodeMS != nil {
		t.Fatalf("an unstamped frame gave an encode time: %v", *stats.EncodeMS)
	}
	if stats, _ := m.report(); stats.Frames != 0 {
		t.Fatalf("second window = %+v", stats)
	}
}

func TestMeterTimesFramesOnTheHostClock(t *testing.T) {
	now := uptimeUS()
	if now == 0 {
		t.Skip("no uptime clock here")
	}
	p := newPacer()
	m := newMeter(p)
	p.wrote(1, true)
	m.video(uint64(now - 4000)) // 4 ms ago
	m.video(uint64(now - 60e6)) // a minute ago: another clock, left out
	stats, _ := m.report()
	if stats.EncodeMS == nil || *stats.EncodeMS < 4 || *stats.EncodeMS > 100 {
		t.Fatalf("encode = %v", stats.EncodeMS)
	}
	if stats.Frames != 2 {
		t.Fatalf("frames = %d", stats.Frames)
	}
}
