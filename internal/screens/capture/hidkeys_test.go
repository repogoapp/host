package capture

import "testing"

func TestUsageFor(t *testing.T) {
	for keysym, want := range map[uint32]int{0xff08: 0x2A, 0xff51: 0x50, 'a': 0x04, 'A': 0x04, '1': 0x1E, '!': 0x1E, 0xffeb: 0xE3} {
		if got, ok := usageFor(keysym); !ok || got != want {
			t.Errorf("usageFor(%#x) = %#x, %v; want %#x", keysym, got, ok, want)
		}
	}
	if _, ok := usageFor(0x01000000 | 0x1F600); ok {
		t.Error("an emoji has no key")
	}
}
