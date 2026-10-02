package emit

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/repogo/host/internal/device"
)

type frame struct {
	to      device.ID
	method  string
	payload string
}

type recorder struct {
	mu     sync.Mutex
	frames []frame
}

func (r *recorder) Send(to device.ID, method string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, frame{to, method, string(payload)})
	return nil
}

func (r *recorder) all() []frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]frame(nil), r.frames...)
}

type ping struct {
	N int `json:"n"`
}

func (ping) Method() string { return "test.ping" }

type snapshot struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	Done bool   `json:"done"`
	Stamp
}

func (snapshot) Method() string     { return "test.snapshot" }
func (s snapshot) StateKey() string { return s.Key }
func (s snapshot) Retain() bool     { return !s.Done }
func (s snapshot) Stamped(st Stamp) Stateful {
	s.Stamp = st
	return s
}

const (
	a = device.ID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b = device.ID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func TestPublishReachesOnlyTheRoom(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r1")
	e.Join(b, "r2")

	e.Publish("r1", snapshot{Key: "s", Text: "hi"}, nil)

	got := rec.all()
	if len(got) != 1 || got[0].to != a || got[0].method != "test.snapshot" {
		t.Fatalf("got %+v", got)
	}
}

func TestJoinReturnsStateWithoutSending(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)

	// Remembered with nobody watching.
	e.Publish("r", snapshot{Key: "s", Text: "hello"}, nil)
	if len(rec.all()) != 0 {
		t.Fatal("sent to nobody")
	}

	snap := e.Join(a, "r")
	if len(rec.all()) != 0 {
		t.Fatal("join sent a frame; the state belongs in the reply")
	}
	s, ok := snap.State["s"].(snapshot)
	if !ok || s.Text != "hello" || s.Revision != 1 || s.Epoch == "" || s.Epoch != snap.Epoch {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Revision != 1 {
		t.Errorf("watermark = %d, want the last revision taken", snap.Revision)
	}
}

// growing is a snapshot sent as what it added since `from`.
type growing struct {
	snapshot
	from int
}

func (g growing) Stamped(st Stamp) Stateful {
	g.Stamp = st
	return g
}

func (g growing) Delta() Event {
	g.Text = g.Text[g.from:]
	return g.snapshot
}

// A Delta event is sent as its smaller form, while the room keeps it whole
// for whoever joins next.
func TestDeltaSendsLessThanTheRoomKeeps(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r")
	e.Publish("r", growing{snapshot: snapshot{Key: "s", Text: "hello world"}, from: 5}, nil)

	var sent snapshot
	if err := json.Unmarshal([]byte(rec.all()[0].payload), &sent); err != nil || sent.Text != " world" || sent.Revision != 1 {
		t.Fatalf("sent %s", rec.all()[0].payload)
	}
	if kept := e.Join(b, "r").State["s"].(growing); kept.Text != "hello world" || kept.Revision != 1 {
		t.Errorf("room keeps %+v, want the whole text at revision 1", kept)
	}
}

func TestLatestStateWins(t *testing.T) {
	e := New(&recorder{}, nil)
	e.Publish("r", snapshot{Key: "s", Text: "one"}, nil)
	e.Publish("r", snapshot{Key: "s", Text: "two"}, nil)

	snap := e.Join(a, "r")
	if s := snap.State["s"].(snapshot); s.Text != "two" || s.Revision != 2 {
		t.Errorf("state = %+v, want two at revision 2", s)
	}
}

func TestPublishStampsEveryPayloadInOrder(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r")
	e.Publish("r", snapshot{Key: "s", Text: "one"}, nil)
	e.Publish("r", snapshot{Key: "other", Text: "x"}, nil)
	e.Publish("r", snapshot{Key: "s", Text: "one", Done: true}, nil)

	var revs []int64
	for _, f := range rec.all() {
		var s snapshot
		if err := json.Unmarshal([]byte(f.payload), &s); err != nil || s.Epoch == "" {
			t.Fatalf("payload %s", f.payload)
		}
		revs = append(revs, s.Revision)
	}
	if len(revs) != 3 || revs[0] != 1 || revs[1] != 2 || revs[2] != 3 {
		t.Fatalf("revisions %v", revs)
	}
	if New(rec, nil).epoch == e.epoch {
		t.Error("two emitters share an epoch")
	}
}

func TestNotRetainedClearsState(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Publish("r", snapshot{Key: "s", Text: "one"}, nil)
	e.Publish("r", snapshot{Key: "s", Text: "one", Done: true}, nil)

	snap := e.Join(a, "r")
	if len(snap.State) != 0 {
		t.Fatalf("state survived a non-retained event: %+v", snap.State)
	}
	// The clear took a revision, so a device holding the prompt can tell the
	// empty snapshot is newer than it.
	if snap.Revision != 2 {
		t.Errorf("watermark = %d, want 2", snap.Revision)
	}
}

func TestPublishPassLeavesStateAlone(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r")
	e.Publish("r", snapshot{Key: "s", Text: "current"}, nil)
	e.Publish("r", snapshot{Key: "s", Text: "stale", Done: true}, func(Event, bool) Verdict { return Pass })

	if s := e.Join(b, "r").State["s"].(snapshot); s.Text != "current" {
		t.Fatalf("state after pass: %+v", s)
	}
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("pass not delivered: %+v", got)
	}
}

func TestPublishJudgesTheCurrentState(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r")
	var seen []string
	judge := func(cur Event, ok bool) Verdict {
		if !ok {
			seen = append(seen, "")
			return Drop
		}
		seen = append(seen, cur.(snapshot).Text)
		return Keep
	}
	if e.Publish("r", snapshot{Key: "s", Text: "first"}, judge) {
		t.Fatal("dropped event reported sent")
	}
	e.Publish("r", snapshot{Key: "s", Text: "held"}, nil)
	if !e.Publish("r", snapshot{Key: "s", Text: "next"}, judge) {
		t.Fatal("kept event not sent")
	}
	if len(seen) != 2 || seen[0] != "" || seen[1] != "held" {
		t.Fatalf("judge saw %q", seen)
	}
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("frames %+v", got)
	}
	// A dropped event takes no revision.
	if snap := e.Join(b, "r"); snap.Revision != 2 {
		t.Errorf("watermark = %d, want 2", snap.Revision)
	}
}

func TestLeaveStopsDelivery(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r")
	e.Leave(a, "r")
	e.Publish("r", snapshot{Key: "s"}, nil)
	if len(rec.all()) != 0 {
		t.Fatal("delivered after leave")
	}
	if len(e.Members("r")) != 0 {
		t.Fatal("empty room not dropped")
	}
}

func TestLeaveAll(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	e.Join(a, "r1")
	e.Join(a, "r2")
	e.Join(b, "r1")
	e.LeaveAll(a)
	e.Publish("r1", snapshot{Key: "s"}, nil)
	e.Publish("r2", snapshot{Key: "s"}, nil)
	got := rec.all()
	if len(got) != 1 || got[0].to != b {
		t.Fatalf("got %+v", got)
	}
}

func TestToBypassesRooms(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	if err := e.To(a, snapshot{Key: "s", Text: "direct"}); err != nil {
		t.Fatal(err)
	}
	if snap := e.Join(b, ""); len(snap.State) != 0 || snap.Revision != 0 {
		t.Fatal("To must not record state")
	}
	if got := rec.all(); len(got) != 1 || got[0].to != a {
		t.Fatalf("got %+v", got)
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic on duplicate method")
		}
	}()
	c := map[string]Event{}
	register(c, ping{})
	register(c, ping{})
}

type arrayEvent struct {
	Items []string `json:"items" wire:"array"`
}

func (arrayEvent) Method() string           { return "test.arrays" }
func (arrayEvent) StateKey() string         { return "arrays" }
func (arrayEvent) Retain() bool             { return true }
func (e arrayEvent) Stamped(Stamp) Stateful { return e }

func TestEventNormalizesRequiredArrays(t *testing.T) {
	rec := &recorder{}
	e := New(rec, nil)
	if err := e.To(a, arrayEvent{}); err != nil {
		t.Fatal(err)
	}
	e.Join(a, "arrays")
	if !e.Publish("arrays", arrayEvent{}, nil) {
		t.Fatal("event not published")
	}
	frames := rec.all()
	if len(frames) != 2 {
		t.Fatalf("frames = %v", frames)
	}
	for _, f := range frames {
		if f.payload != `{"items":[]}` {
			t.Fatalf("payload = %s", f.payload)
		}
	}
}
