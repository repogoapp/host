package desktop

// Edge is where the vertical scroll under the cursor rests.
type Edge int

const (
	// EdgeUnknown is no word on it: nothing there says, or not yet.
	EdgeUnknown Edge = iota - 1
	// EdgeNone is between the ends.
	EdgeNone
	EdgeTop
	EdgeBottom
)

// ScrollReader reads the vertical scroll under the cursor after scrolls,
// from the nearest scroll area with a bar. A Chrome page reports no
// position, so it keeps what sat at the page's middle: scrolled toward an
// end and unmoved, the page is at that end. An app that draws its own
// scrolling reports neither, and reads as EdgeUnknown.
type ScrollReader struct {
	anchor [4]float64
	has    bool
}

// Edge reads the scroll, after scrolls heading up (positive) or down (negative)
// since the last read; heading 0 when none came.
func (r *ScrollReader) Edge(heading int) Edge {
	edge, anchor, web := scrollEdge()
	if edge >= 0 {
		r.has = false
		return Edge(edge)
	}
	if !web {
		r.has = false
		return EdgeUnknown
	}
	last, had := r.anchor, r.has
	r.anchor, r.has = anchor, true
	switch {
	case !had:
		return EdgeUnknown
	case anchor != last:
		return EdgeNone
	case heading > 0:
		return EdgeTop
	case heading < 0:
		return EdgeBottom
	}
	return EdgeUnknown
}

// Forget drops what the last read kept, as when the cursor moves elsewhere.
func (r *ScrollReader) Forget() { r.has = false }
