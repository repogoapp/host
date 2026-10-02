// Command e2e is a simulated phone that pairs and calls RPC entirely through
// the relay — the exact path the iOS app takes, with no shared network.
//
// It is the only thing that proves the whole chain at once: host dials out,
// relay routes, an unpaired device pairs over that route, and the host then
// authorizes it. Each piece has unit tests; this is the one that fails if they
// are individually right and jointly wrong.
//
//	go run ./bench/e2e -invite "<pair.begin invite payload>"
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/hostclient"
	"github.com/repogo/host/internal/jsonrpc"
)

func main() {
	inviteRaw := flag.String("invite", "", "invite payload from pair.begin")
	workers := flag.Int("workers", 4, "transcripts in flight at once; 0 skips transcripts")
	pageSize := flag.Int("page", 200, "events per transcript page")
	flag.Parse()
	if *inviteRaw == "" {
		fail("need -invite")
	}

	invite, err := device.DecodeInvite(*inviteRaw)
	must(err, "decode invite")
	fmt.Printf("invite     relay=%s group=%s expires_in=%v\n",
		invite.Address, invite.GroupID,
		time.Until(time.UnixMilli(invite.ExpiresAt)).Round(time.Second))

	identity, err := device.Generate()
	must(err, "generate identity")
	fmt.Printf("phone      %s\n", identity.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Every stage is timed: the point of running this against the real relay
	// is knowing what a cold phone waits for, not only that it gets there.
	var timings []string
	step := func(what string) func() {
		start := time.Now()
		return func() {
			timings = append(timings, fmt.Sprintf("%s=%v", what, time.Since(start).Round(time.Millisecond)))
		}
	}

	// 1. Attach to the relay. It only checks that we own this keypair; pair
	//    membership remains the host's decision. The host is addressed by its
	//    own id — the QR carried it, so there is nothing to negotiate.
	done := step("attach")
	c, err := hostclient.Dial(ctx, hostclient.Config{
		URL:        invite.Address,
		Identity:   identity,
		GroupID:    invite.GroupID,
		Role:       handshake.RoleClient,
		Platform:   "ios",
		Label:      "e2e phone",
		Host:       invite.InviterID,
		HostPublic: invite.InviterPublic,
	})
	must(err, "attach to relay")
	done()
	defer c.Close()
	fmt.Printf("attached   relay=%s\n", c.ServerVersion())

	// 2. An unpaired device must be refused everything except pairing. If this
	//    succeeds, a stranger who learned the host id owns the machine.
	if err := c.Call(ctx, "devices.list", nil, nil); err == nil {
		fail("SECURITY: an unpaired device listed the host's devices")
	} else {
		fmt.Printf("refused    devices.list before pairing (%v)\n", err)
	}

	// 3. Pair, over the relay, with no shared network anywhere in the path.
	var paired struct {
		HostID     string `json:"host_id"`
		HostPublic string `json:"host_public"`
		HostProof  string `json:"host_proof"`
	}
	done = step("pair")
	must(c.Call(ctx, "pair.complete", map[string]any{
		"device_id":  string(identity.ID),
		"public_key": base64.StdEncoding.EncodeToString(identity.Public),
		"label":      "e2e phone",
		"platform":   "ios",
		"proof":      base64.StdEncoding.EncodeToString(device.Proof(invite.Code, identity.Public)),
	}, &paired), "pair.complete")
	done()

	hostPub, err := base64.StdEncoding.DecodeString(paired.HostPublic)
	must(err, "decode host key")
	if paired.HostID != string(invite.InviterID) || !bytes.Equal(hostPub, invite.InviterPublic) {
		fail("pair response did not match the host identity pinned by the invite")
	}
	// The host's own proof: without checking it, we would have paired with
	// whatever answered rather than the machine whose screen we scanned.
	if base64.StdEncoding.EncodeToString(device.Proof(invite.Code, hostPub)) != paired.HostProof {
		fail("host proof did not verify")
	}
	fmt.Printf("paired     host=%s (proof verified)\n", paired.HostID)

	// 4. Now authorized.
	var list struct {
		Peers []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"peers"`
	}
	done = step("devices")
	must(c.Call(ctx, "devices.list", nil, &list), "devices.list after pairing")
	done()
	fmt.Printf("listed     %d peer(s):", len(list.Peers))
	for _, p := range list.Peers {
		fmt.Printf(" %s", p.Label)
	}
	fmt.Println()

	found := false
	for _, p := range list.Peers {
		if p.ID == string(identity.ID) {
			found = true
		}
	}
	if !found {
		fail("the phone paired but does not appear in the device list")
	}

	// 5. The cold start: projects and every chat row, each paged until `more`
	//    is false, both at once because the scopes are independent and a phone
	//    should pay one round trip, not two.
	type chatRow struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		EventCount int    `json:"event_count"`
		HostID     string `json:"host_id"`
	}
	type delta struct {
		Epoch  string            `json:"epoch"`
		Rev    int64             `json:"rev"`
		More   bool              `json:"more"`
		Upsert []json.RawMessage `json:"upsert"`
	}
	pull := func(family string) (last delta, rows []json.RawMessage, pages int) {
		for {
			var page delta
			pageDone := step(family + ".page1")
			must(c.Call(ctx, "sync.pull", map[string]any{
				"family": family, "epoch": last.Epoch, "since": last.Rev, "ids": []string{},
			}, &page), "sync.pull "+family)
			if pages == 0 {
				pageDone()
			}
			pages++
			rows = append(rows, page.Upsert...)
			last = page
			if !page.More {
				return
			}
		}
	}
	var (
		projectList struct {
			Projects []json.RawMessage `json:"projects"`
		}
		chatRaw    []json.RawMessage
		chatPages  int
		chatCursor delta
		cold       sync.WaitGroup
	)
	done = step("cold")
	cold.Add(2)
	go func() {
		defer cold.Done()
		must(c.Call(ctx, "project.list", map[string]any{}, &projectList), "project.list")
	}()
	go func() {
		defer cold.Done()
		chatCursor, chatRaw, chatPages = pull("chats")
	}()
	cold.Wait()
	done()

	for _, raw := range projectList.Projects {
		var p struct {
			Path   string `json:"path"`
			HostID string `json:"host_id"`
		}
		must(json.Unmarshal(raw, &p), "decode project")
		if p.HostID != paired.HostID {
			fail(fmt.Sprintf("project %s stamped with %q, not the paired host", p.Path, p.HostID))
		}
	}
	fmt.Printf("projects   %d row(s)\n", len(projectList.Projects))

	all := make([]chatRow, 0, len(chatRaw))
	for _, raw := range chatRaw {
		var ch chatRow
		must(json.Unmarshal(raw, &ch), "decode chat")
		if ch.HostID != paired.HostID {
			fail(fmt.Sprintf("chat %s stamped with %q, not the paired host", ch.ID, ch.HostID))
		}
		all = append(all, ch)
	}
	fmt.Printf("chats      %d row(s) in %d page(s), rev %d\n", len(all), chatPages, chatCursor.Rev)
	if len(all) == 0 {
		ok(timings, "paired, authorized, and projects synced, but the host has no chats cached yet.")
		return
	}

	// The same rows over chats.list, forty a page as the app scrolls: the
	//    lane the phone uses today, timed next to the mirror so each run
	//    says which is faster and by how much.
	done = step("chats.list")
	listPages, listRows, listCursor := 0, 0, ""
	for {
		var page struct {
			Chats      []chatRow `json:"chats"`
			NextCursor string    `json:"next_cursor"`
		}
		pageDone := step("chats.list.page1")
		must(c.Call(ctx, "chats.list", map[string]any{
			"limit": 40, "cursor": listCursor,
		}, &page), "chats.list")
		if listPages == 0 {
			pageDone()
		}
		listPages++
		listRows += len(page.Chats)
		listCursor = page.NextCursor
		if listCursor == "" || len(page.Chats) == 0 {
			break
		}
	}
	done()
	fmt.Printf("chats.list %d row(s) in %d page(s)\n", listRows, listPages)

	// A second mirror pull from the cursor just learned: what every launch
	//    after the first costs, which is the number the app actually lives on.
	done = step("chats.again")
	var again delta
	must(c.Call(ctx, "sync.pull", map[string]any{
		"family": "chats", "epoch": chatCursor.Epoch, "since": chatCursor.Rev, "ids": []string{},
	}, &again), "sync.pull chats again")
	done()
	fmt.Printf("chats.again %d row(s) changed since rev %d\n", len(again.Upsert), chatCursor.Rev)

	// 7. Every transcript, 200 events a page, a few chats in flight at once:
	//    what a full mirror would cost, staggered so one slow chat does not
	//    idle the link and a burst does not swamp the relay.
	if *workers == 0 {
		ok(timings, "paired, authorized, projects and chats synced, entirely over the relay.")
		return
	}
	done = step("messages")
	var (
		mu                sync.Mutex
		msgPages, msgRows int
		msgBytes, maxPage int
		shrunk            int
		expected          int
		work              = make(chan chatRow)
		wg                sync.WaitGroup
	)
	for _, ch := range all {
		expected += ch.EventCount
	}
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ch := range work {
				sinceIdx, pages, got, bytes := 0, 0, 0, 0
				limit := *pageSize
				var generation float64
				for {
					var page struct {
						Events     []json.RawMessage `json:"events"`
						Generation float64           `json:"generation"`
						EventCount int               `json:"event_count"`
						NextIdx    int               `json:"next_idx"`
					}
					err := c.Call(ctx, "chats.messages", map[string]any{
						"chat_id": ch.ID, "since_idx": sinceIdx, "limit": limit,
					}, &page)
					// A page of huge tool outputs can exceed one message; halve
					// and ask again, which is what a client would have to do
					// too, and count it so it shows.
					if err != nil && tooLarge(err) && limit > 10 {
						limit /= 2
						mu.Lock()
						shrunk++
						mu.Unlock()
						continue
					}
					must(err, "chats.messages "+ch.ID)
					size := 0
					for _, ev := range page.Events {
						size += len(ev)
					}
					bytes += size
					mu.Lock()
					if size > maxPage {
						maxPage = size
					}
					mu.Unlock()
					// A generation change mid-page means history was rebuilt
					// under us; a real client restarts from 0 rather than
					// appending onto stale rows.
					if pages > 0 && page.Generation != generation {
						fail("generation moved mid-sync — client must restart")
					}
					generation = page.Generation
					got += len(page.Events)
					pages++
					if page.NextIdx == sinceIdx || page.NextIdx >= page.EventCount {
						break
					}
					sinceIdx = page.NextIdx
				}
				mu.Lock()
				msgPages += pages
				msgRows += got
				msgBytes += bytes
				mu.Unlock()
			}
		}()
	}
	for _, ch := range all {
		work <- ch
	}
	close(work)
	wg.Wait()
	done()
	fmt.Printf("messages   %d/%d events, %.1f MB in %d page(s) across %d chat(s), %d in flight; largest page %.1f MB, %d page(s) shrunk after a timeout\n",
		msgRows, expected, float64(msgBytes)/1e6, msgPages, len(all), *workers, float64(maxPage)/1e6, shrunk)

	ok(timings, "paired, authorized, projects and chat history synced, entirely over the relay.")
}

// tooLarge is the host refusing a reply that would not fit one message.
func tooLarge(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeInternal && strings.Contains(rpcErr.Message, "exceeds limit")
}

func ok(timings []string, what string) {
	fmt.Printf("timing     %s\n\nOK — %s\n", strings.Join(timings, " "), what)
}

func must(err error, what string) {
	if err != nil {
		fail(what + ": " + err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "FAIL:", msg)
	os.Exit(1)
}
