package forward

import "github.com/repogo/host/internal/emit"

// Every event this package emits, declared for the catalog. A pipe belongs to
// the one device that opened it, so these are sent to that device and never
// broadcast.

func init() {
	emit.Register(Data{}, Closed{})
}

// Data is bytes the dev server wrote on an open pipe, in order.
type Data struct {
	PipeID string `json:"pipe_id"`
	Data   []byte `json:"data"`
}

func (Data) Method() string { return "forward.pipe_data" }

// Closed is the pipe ending, from either side, and why.
type Closed struct {
	PipeID string `json:"pipe_id"`
	Reason string `json:"reason"`
}

func (Closed) Method() string { return "forward.pipe_closed" }
