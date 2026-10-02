package syscmd

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOutputCarriesTheToolsWordsIntoItsError(t *testing.T) {
	out, err := Output(context.Background(), 5*time.Second, "sh", "-c", "echo ok")
	if err != nil || out != "ok\n" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	_, err = Output(context.Background(), 5*time.Second, "sh", "-c", "echo broken >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v, want the tool's stderr", err)
	}
}

func TestOutputStopsAtTheTimeout(t *testing.T) {
	start := time.Now()
	if _, err := Output(context.Background(), 50*time.Millisecond, "sleep", "10"); err == nil {
		t.Fatal("a command past its timeout succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not stop the command")
	}
}
