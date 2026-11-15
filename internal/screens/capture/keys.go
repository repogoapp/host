package capture

// HID usages (page 7) of the keys typing presses on its own.
const (
	leftShift  = 0xE1
	commandKey = 0xE3
	vKey       = 0x19
)

// unshifted and shifted map the punctuation keys of a US keyboard to their
// HID usages (page 7).
var (
	unshifted = map[rune]int{
		'\n': 0x28, '\t': 0x2B, ' ': 0x2C, '-': 0x2D, '=': 0x2E, '[': 0x2F, ']': 0x30, '\\': 0x31,
		';': 0x33, '\'': 0x34, '`': 0x35, ',': 0x36, '.': 0x37, '/': 0x38, '\b': 0x2A,
	}
	shifted = map[rune]int{
		'!': 0x1E, '@': 0x1F, '#': 0x20, '$': 0x21, '%': 0x22, '^': 0x23, '&': 0x24, '*': 0x25,
		'(': 0x26, ')': 0x27, '_': 0x2D, '+': 0x2E, '{': 0x2F, '}': 0x30, '|': 0x31, ':': 0x33,
		'"': 0x34, '~': 0x35, '<': 0x36, '>': 0x37, '?': 0x38,
	}
)

// keyFor is the key that types r on a US keyboard, and whether it needs Shift.
func keyFor(r rune) (usage int, shift, ok bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return 0x04 + int(r-'a'), false, true
	case r >= 'A' && r <= 'Z':
		return 0x04 + int(r-'A'), true, true
	case r >= '1' && r <= '9':
		return 0x1E + int(r-'1'), false, true
	case r == '0':
		return 0x27, false, true
	}
	if u, ok := unshifted[r]; ok {
		return u, false, true
	}
	if u, ok := shifted[r]; ok {
		return u, true, true
	}
	return 0, false, false
}
