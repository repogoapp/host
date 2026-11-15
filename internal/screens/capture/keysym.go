package capture

// Keysym is the X11 keysym VNC types r with: Latin-1 is its own code,
// the control keys have their own, and the rest is Unicode with 0x01000000.
func Keysym(r rune) uint32 {
	switch r {
	case '\n':
		return 0xff0d // Return
	case '\t':
		return 0xff09 // Tab
	case '\b':
		return 0xff08 // BackSpace
	}
	if r >= 0x20 && r <= 0xff {
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}
