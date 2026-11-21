//go:build !(darwin && cgo) && !(windows && cgo)

package desktop

func locked() bool                        { return false }
func trusted() bool                       { return false }
func scrollEdge() (int, [4]float64, bool) { return -1, [4]float64{}, false }
func read() ([]Space, int, []rawWindow)   { return nil, 0, nil }
func focusOf(int) *Focus                  { return nil }
func press(int, string) error             { return nil }
func pressMenu(int, string) error         { return nil }
func icon(string, int) []byte             { return nil }
func bundleAt(string) (App, bool)         { return App{}, false }
func quit(string)                         {}
func raise(int, uint32)                   {}
func pidOf(uint32) int                    { return 0 }
func watch()                              {}
func swipe(int, float64, float64)         {}
