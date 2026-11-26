package clitool

import "testing"

// The phone gets enough of an address to tell accounts apart, never all of it.
func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alexandra@example.com": "ale•••a@example.com",
		"jerry@x.io":            "jer•••y@x.io",
		"jo@x.io":               "j•••@x.io",
		"anna@x.io":             "a•••@x.io",
		"élodie@x.fr":           "élo•••e@x.fr",
		"not-an-email":          "not•••l",
		"@x.io":                 "@x.io",
		"":                      "",
	} {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}
