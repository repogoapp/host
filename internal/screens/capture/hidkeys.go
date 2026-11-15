package capture

// specialKeys are the HID usages (page 7) of the keysyms a phone keyboard
// sends for keys without a character: control keys, arrows and modifiers.
var specialKeys = map[uint32]int{
	0xff08: 0x2A, // BackSpace
	0xff09: 0x2B, // Tab
	0xff0d: 0x28, // Return
	0xff1b: 0x29, // Escape
	0xffff: 0x4C, // Delete forward
	0xff51: 0x50, // Left
	0xff52: 0x52, // Up
	0xff53: 0x4F, // Right
	0xff54: 0x51, // Down
	0xffe1: 0xE1, // Shift
	0xffe3: 0xE0, // Control
	0xffe9: 0xE2, // Option
	0xffeb: 0xE3, // Command
}

// usageFor is the HID key a keysym presses on a US keyboard, without the
// Shift a character may also need (the phone sends that as its own key).
func usageFor(keysym uint32) (int, bool) {
	if usage, ok := specialKeys[keysym]; ok {
		return usage, true
	}
	if keysym < 0x20 || keysym > 0x7e {
		return 0, false
	}
	usage, _, ok := keyFor(rune(keysym))
	return usage, ok
}
