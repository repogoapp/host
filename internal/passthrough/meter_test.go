package passthrough

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
)

// fakeClock is a settable now for the meter.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func meteredRelay(lim Limits, log *slog.Logger) (*Relay, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Relay{Limits: lim, Log: log, clock: clock.now}, clock
}

const someHost = device.ID("0123456789abcdef0123456789abcdef")

func TestTheBurstPassesAtFullSpeedThenTheRateApplies(t *testing.T) {
	r, _ := meteredRelay(Limits{Burst: 32 << 20, Rate: 4 << 20}, nil)
	m := r.meterOf(someHost)
	if wait, err := r.take(someHost, m, 32<<20); err != nil || wait != 0 {
		t.Fatalf("the burst waited %v, %v", wait, err)
	}
	wait, err := r.take(someHost, m, 4<<20)
	if err != nil || wait < 990*time.Millisecond || wait > 1010*time.Millisecond {
		t.Fatalf("4 MB past the burst at 4 MB/s waits %v, %v; want about 1s", wait, err)
	}
}

func TestTheBucketRefillsOverTime(t *testing.T) {
	r, clock := meteredRelay(Limits{Burst: 8 << 20, Rate: 4 << 20}, nil)
	m := r.meterOf(someHost)
	r.take(someHost, m, 8<<20)
	clock.add(2 * time.Second)
	if wait, _ := r.take(someHost, m, 8<<20); wait != 0 {
		t.Fatalf("after 2s at 4 MB/s, 8 MB still waited %v", wait)
	}
}

func TestAHostsStreamsShareOneMeter(t *testing.T) {
	r, _ := meteredRelay(Limits{Burst: 1 << 20, Rate: 1 << 20}, nil)
	if r.meterOf(someHost) != r.meterOf(someHost) {
		t.Fatal("two streams of one host got separate meters")
	}
	if r.meterOf(someHost) == r.meterOf("ffffffffffffffffffffffffffffffff") {
		t.Fatal("two hosts share a meter")
	}
}

func TestTheDailyCapRefusesUntilMidnightUTC(t *testing.T) {
	r, clock := meteredRelay(Limits{DailyCap: 100}, nil)
	m := r.meterOf(someHost)
	for _, n := range []int{60, 60} {
		if _, err := r.take(someHost, m, n); err != nil {
			t.Fatalf("under the cap: %v", err)
		}
	}
	if _, err := r.take(someHost, m, 1); !errors.Is(err, errOverCap) {
		t.Fatalf("past the cap: %v, want errOverCap", err)
	}
	if !r.overCap(someHost) {
		t.Fatal("a capped host's new streams aren't refused")
	}
	clock.add(12 * time.Hour) // 00:00 UTC the next day
	if r.overCap(someHost) {
		t.Fatal("still refused after midnight UTC")
	}
	if _, err := r.take(someHost, m, 60); err != nil {
		t.Fatalf("the next day: %v", err)
	}
}

func TestTheDailyWarningIsLoggedOnceADay(t *testing.T) {
	var buf bytes.Buffer
	r, clock := meteredRelay(Limits{DailyWarn: 100}, slog.New(slog.NewTextHandler(&buf, nil)))
	m := r.meterOf(someHost)
	r.take(someHost, m, 150)
	r.take(someHost, m, 150)
	if got := strings.Count(buf.String(), "daily warning"); got != 1 {
		t.Fatalf("warned %d times in one day, want 1", got)
	}
	clock.add(24 * time.Hour)
	r.take(someHost, m, 150)
	if got := strings.Count(buf.String(), "daily warning"); got != 2 {
		t.Fatalf("warned %d times over two days, want 2", got)
	}
}

func TestYesterdaysMetersAreDropped(t *testing.T) {
	r, clock := meteredRelay(Limits{DailyCap: 100}, nil)
	r.meterOf(someHost)
	clock.add(24 * time.Hour)
	r.meterOf("ffffffffffffffffffffffffffffffff")
	r.mu.Lock()
	_, kept := r.meters[someHost]
	r.mu.Unlock()
	if kept {
		t.Fatal("a meter from an earlier day is still held")
	}
}

func TestAPhoneOverTheSpeedIsSlowedNotCut(t *testing.T) {
	var mu sync.Mutex
	var waits []time.Duration
	s := startWith(t, options{relay: func(r *Relay) {
		r.Limits = Limits{Burst: 1, Rate: 1 << 30}
		r.sleep = func(d time.Duration) {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
		}
	}})
	resp, err := s.phone(string(s.host.ID)+suffix, s.pin)
	if err != nil {
		t.Fatalf("a slowed stream failed: %v", err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(waits) == 0 {
		t.Fatal("past the burst, nothing was slowed")
	}
}

func TestACappedHostsPhonesAreRefused(t *testing.T) {
	s := startWith(t, options{relay: func(r *Relay) { r.Limits = Limits{DailyCap: 1 << 20} }})
	m := s.relay.meterOf(s.host.ID)
	s.relay.take(s.host.ID, m, 1<<20)
	if _, err := s.phone(string(s.host.ID)+suffix, s.pin); err == nil {
		t.Fatal("a host past its daily cap still got a phone through")
	}
}

func TestAnIdleStreamIsClosed(t *testing.T) {
	const idle = 200 * time.Millisecond
	s := startWith(t, options{relay: func(r *Relay) { r.Limits = Limits{Idle: idle} }})
	c := s.rawPhone(t)
	if err := get(c); err != nil {
		t.Fatalf("first request: %v", err)
	}
	// Send nothing and wait on the stream: the relay should end it.
	quiet := time.Now()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := c.r.ReadByte()
	if err == nil || isTimeout(err) {
		t.Fatalf("a stream idle past the limit is still open: %v", err)
	}
	if waited := time.Since(quiet); waited < idle {
		t.Fatalf("closed after %v, before the %v idle limit", waited, idle)
	}
}

func TestAnActiveStreamStaysOpen(t *testing.T) {
	const idle = 200 * time.Millisecond
	s := startWith(t, options{relay: func(r *Relay) { r.Limits = Limits{Idle: idle} }})
	c := s.rawPhone(t)
	// Keep the stream busy for several idle periods: it must not be cut.
	for start := time.Now(); time.Since(start) < 4*idle; {
		if err := get(c); err != nil {
			t.Fatalf("a busy stream was cut after %v: %v", time.Since(start), err)
		}
	}
}

// rawPhone opens one pinned TLS stream to the host through the relay, kept
// open across requests so idleness can be tested.
func (s *stack) rawPhone(t *testing.T) *bufConn {
	t.Helper()
	name := string(s.host.ID) + suffix
	raw, err := net.Dial("tcp", s.relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	pin := s.pin
	c := tls.Client(raw, &tls.Config{
		ServerName: name, MinVersion: tls.VersionTLS13, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if spki(tls.Certificate{Certificate: [][]byte{cs.PeerCertificates[0].Raw}}) != pin {
				return errors.New("certificate does not match the pin")
			}
			return nil
		},
	})
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &bufConn{Conn: c, r: bufio.NewReader(c)}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

// get sends one keep-alive request on c and reads the whole answer.
func get(c *bufConn) error {
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: host\r\n\r\n")); err != nil {
		return err
	}
	resp, err := http.ReadResponse(c.r, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if string(body) != secret {
		return errors.New("wrong answer")
	}
	return nil
}
