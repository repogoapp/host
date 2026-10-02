package store

// Change is what one committed write did to the chat list: rows that read
// differently now, and rows that are gone.
type Change struct {
	Changed []ChatID
	Removed []ChatID
}

// Notify sets the one listener told after each commit that moved the chat
// list, on the writer's goroutine. Set before the first write; a change made
// while no listener is set is not replayed.
func (s *Store) Notify(fn func(Change)) { s.onChange = fn }

func (s *Store) notify(c Change) {
	if s.onChange != nil && (len(c.Changed) > 0 || len(c.Removed) > 0) {
		s.onChange(c)
	}
}
