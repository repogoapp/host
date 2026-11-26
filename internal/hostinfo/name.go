package hostinfo

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
)

func init() { emit.Register(NameChanged{}) }

// NameMaxRunes bounds a host's name: room for "Jerrick's MacBook Pro (work)",
// short enough to fit a phone's environment row.
const NameMaxRunes = 64

// Named is a host's name as devices are told it: the one in use, and the
// machine's own it falls back to.
type Named struct {
	Name        string `json:"name"`
	DefaultName string `json:"default_name"`
}

// NameChanged is host.name_changed: every device hears a rename or a clear.
type NameChanged Named

func (NameChanged) Method() string { return "host.name_changed" }

// Rename saves name for every device, the last to arrive winning; "" goes
// back to the machine's own. It answers the name now in use.
func (s *Service) Rename(name string) (Named, error) {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > NameMaxRunes {
		return Named{}, errkind.New(errkind.Invalid, fmt.Sprintf("the name is over %d characters", NameMaxRunes))
	}
	// It reaches agents' prompts, terminals and pairing screens: no escapes,
	// no invisible or direction-changing characters.
	if strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return Named{}, errkind.New(errkind.Invalid, "the name has a character that can't be shown")
	}
	if name == s.d.DefaultName {
		name = ""
	}
	s.renaming.Lock()
	defer s.renaming.Unlock()
	if err := s.d.SaveName(name); err != nil {
		return Named{}, err
	}
	named := Named{Name: s.Name(), DefaultName: s.d.DefaultName}
	s.d.NameChanged(NameChanged(named))
	return named, nil
}
