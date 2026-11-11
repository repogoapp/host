package chats

import (
	"errors"
	"sync"
	"testing"
)

func TestOnceRunsARepeatedSendOnce(t *testing.T) {
	s := newSentTurns()
	runs := 0
	send := func() (SendResult, error) { runs++; return SendResult{TurnID: "t1"}, nil }
	first, _ := once(s, "phone", "send-1", send)
	again, _ := once(s, "phone", "send-1", send)
	if runs != 1 || first.TurnID != "t1" || again.TurnID != "t1" {
		t.Fatalf("runs=%d first=%q again=%q", runs, first.TurnID, again.TurnID)
	}
	if _, _ = once(s, "other", "send-1", send); runs != 2 {
		t.Fatalf("another device's id ran %d times, want 2", runs)
	}
	if _, _ = once(s, "phone", "", send); runs != 3 {
		t.Fatalf("an empty id should always run, ran %d", runs)
	}
}

func TestOnceForgetsARefusal(t *testing.T) {
	s := newSentTurns()
	runs := 0
	fail := func() (SendResult, error) { runs++; return SendResult{}, errors.New("no") }
	_, _ = once(s, "phone", "send-1", fail)
	_, _ = once(s, "phone", "send-1", fail)
	if runs != 2 {
		t.Fatalf("a refused send should run again, ran %d", runs)
	}
}

func TestOnceRepeatWaitsForTheFirst(t *testing.T) {
	s := newSentTurns()
	release := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = once(s, "phone", "send-1", func() (SendResult, error) {
			close(started)
			<-release
			return SendResult{TurnID: "t1"}, nil
		})
	}()
	<-started
	done := make(chan string)
	go func() {
		r, _ := once(s, "phone", "send-1", func() (SendResult, error) { return SendResult{TurnID: "t2"}, nil })
		done <- r.TurnID
	}()
	close(release)
	if got := <-done; got != "t1" {
		t.Fatalf("repeat got %q, want t1", got)
	}
	wg.Wait()
}
