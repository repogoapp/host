package relay

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
)

func TestHelloFieldsAreBounded(t *testing.T) {
	for name, change := range map[string]func(*handshake.Hello){
		"group":    func(h *handshake.Hello) { h.GroupID = strings.Repeat("g", 129) },
		"label":    func(h *handshake.Hello) { h.Label = strings.Repeat("l", 257) },
		"platform": func(h *handshake.Hello) { h.Platform = strings.Repeat("p", 65) },
		"version":  func(h *handshake.Hello) { h.ClientVersion = strings.Repeat("v", 65) },
		"role":     func(h *handshake.Hello) { h.Role = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if _, refusal := h.connect("group", handshake.RoleClient, change); refusal == nil {
				t.Fatal("oversized or invalid hello was accepted")
			}
		})
	}
}

func TestHelloCannotUseTheRecordSizeLimit(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ws, _, err := h.dialRaw(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	// An unknown field must hit the wire limit even if decoding would ignore it.
	body := `{"jsonrpc":"2.0","id":"hello","method":"hello","params":{"padding":"` + strings.Repeat("x", maxHelloBytes) + `"}}`
	_ = ws.Write(ctx, websocket.MessageText, []byte(body))
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("oversized hello: %v, want message-too-big close", err)
	}
}

func TestReconnectChurnKeepsSpendingTheAddressBudget(t *testing.T) {
	s := newHarness(t).srv
	now := time.Now()
	for range connectionAttemptsPerMinute {
		if !s.admitIP("203.0.113.1", now) {
			t.Fatal("connection refused before its budget was spent")
		}
		s.releaseIP("203.0.113.1")
	}
	if s.admitIP("203.0.113.1", now.Add(time.Second)) {
		t.Fatal("closing sockets reset the connection-attempt budget")
	}
	if !s.admitIP("203.0.113.2", now) {
		t.Fatal("another address shared the attacker's budget")
	}
	s.releaseIP("203.0.113.2")
	if !s.admitIP("203.0.113.1", now.Add(time.Minute)) {
		t.Fatal("the address's budget did not recover")
	}
	s.releaseIP("203.0.113.1")
}

func TestRotatingAddressesCannotGrowTheLimiter(t *testing.T) {
	s := newHarness(t).srv
	now := time.Now()
	for i := range maxAttemptIPs {
		ip := fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
		if !s.admitIP(ip, now) {
			t.Fatal("address refused before the limiter filled")
		}
		s.releaseIP(ip)
	}
	if !s.admitIP("203.0.113.1", now) {
		t.Fatal("a full limiter locked out a new address")
	}
	s.releaseIP("203.0.113.1")
	if len(s.attempts) != maxAttemptIPs {
		t.Fatalf("limiter grew to %d addresses", len(s.attempts))
	}
	s.admitIP("203.0.113.1", now.Add(time.Minute))
	s.releaseIP("203.0.113.1")
	if len(s.attempts) != 1 {
		t.Fatal("expired address budgets were not reclaimed")
	}
}

func TestOneIPv6PrefixSharesABudget(t *testing.T) {
	a, b := subscriber(net.ParseIP("2001:db8:1:2::1")), subscriber(net.ParseIP("2001:db8:1:2:ffff::9"))
	if a != b {
		t.Fatalf("one /64 counted as %q and %q", a, b)
	}
	if subscriber(net.ParseIP("2001:db8:1:3::1")) == a {
		t.Fatal("another /64 shared the budget")
	}
	if got := subscriber(net.ParseIP("203.0.113.1")); got != "203.0.113.1" {
		t.Fatalf("IPv4 address became %q", got)
	}
}

func TestDeparturesStayBoundedUnderIdentityChurn(t *testing.T) {
	s := newHarness(t).srv
	for i := range maxDepartures + 1 {
		id := device.ID(fmt.Sprintf("%032x", i+1))
		c := &conn{id: id, group: "group", role: handshake.RoleClient}
		s.conns[id] = []*conn{c}
		s.routes[leg{from: id, to: "peer"}] = c
		s.detach(c)
	}
	if len(s.away) != maxDepartures {
		t.Fatalf("retained %d departures, want %d", len(s.away), maxDepartures)
	}
	newest := device.ID(fmt.Sprintf("%032x", maxDepartures+1))
	if _, kept := s.away[newest]; !kept {
		t.Fatal("a full cache refused the newest departure")
	}
	s.prunePresence(time.Now().Add(awayFor))
	if len(s.away) != 0 {
		t.Fatal("expired departures were not reclaimed")
	}
}

func TestLateJoinersCannotGrowPresenceWithoutBound(t *testing.T) {
	s := newHarness(t).srv
	target := device.ID("00000000000000000000000000000001")
	s.away[target] = departure{group: "group", at: time.Now()}
	for i := range maxUnreachable + 1 {
		from := &conn{srv: s, id: device.ID(fmt.Sprintf("%032x", i+2)), group: "group", out: make(chan []byte, 1)}
		s.unreachable(from, target)
	}
	if len(s.toldUnreachable) != maxUnreachable {
		t.Fatalf("retained %d notification entries", len(s.toldUnreachable))
	}
	if len(s.away[target].peers) != maxPresencePeers {
		t.Fatalf("retained %d peers", len(s.away[target].peers))
	}
	s.prunePresence(time.Now().Add(time.Minute))
	if len(s.toldUnreachable) != 0 {
		t.Fatal("expired notification entries were not reclaimed")
	}
}

func TestIncomingRoutesCannotGrowADeparturesPeerList(t *testing.T) {
	s := newHarness(t).srv
	id := device.ID("00000000000000000000000000000001")
	c := &conn{id: id, group: "group", role: handshake.RoleClient}
	s.conns[id] = []*conn{c}
	for i := range maxPresencePeers + 1 {
		peer := device.ID(fmt.Sprintf("%032x", i+2))
		s.routes[leg{from: peer, to: id}] = &conn{id: peer}
	}
	s.detach(c)
	if len(s.away[id].peers) != maxPresencePeers {
		t.Fatalf("retained %d peers", len(s.away[id].peers))
	}
}

func TestFullPresenceCacheStillForwardsTraffic(t *testing.T) {
	h := newHarness(t)
	h.srv.mu.Lock()
	for i := range maxDepartures {
		h.srv.away[device.ID(fmt.Sprintf("%032x", i+1))] = departure{group: "group", at: time.Now()}
	}
	h.srv.mu.Unlock()
	host, phone := talking(t, h)
	host.ws.Close(websocket.StatusNormalClosure, "")
	h.gone(host.id)
	// A full cache evicts the oldest entry, so the host gets its usual grace.
	method, p, err := phone.presence(hostGrace + 3*time.Second)
	if err != nil || method != PresenceGone || p.Device != host.id {
		t.Fatalf("full cache suppressed departure notice: %s %+v %v", method, p, err)
	}
	h.srv.mu.Lock()
	_, kept := h.srv.away[host.id]
	h.srv.mu.Unlock()
	if !kept {
		t.Fatal("full cache forgot the newest departure")
	}
	host, _ = h.connectAs(host.pub, host.priv, "group-a", handshake.RoleRuntime)
	phone.send(host.id, "hello again")
	if _, body, err := host.recv(3 * time.Second); err != nil || body != "hello again" {
		t.Fatalf("full cache blocked traffic: %q %v", body, err)
	}
}
