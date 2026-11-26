package tunnelgateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/tunnelwire"
)

// wsReadLimit caps one visitor frame; it must fit in a tunnelwire frame.
const wsReadLimit = 16 << 20

// writeVisitor writes one frame, closing a visitor that hasn't taken it within
// visitorWriteTimeout, so a socket that stops reading frees its place.
func writeVisitor(ctx context.Context, visitor *websocket.Conn, typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, visitorWriteTimeout)
	defer cancel()
	return visitor.Write(ctx, typ, data)
}

// serveWebSocket bridges a visitor's WebSocket to the host, which opens the
// matching socket on its local port.
func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request, row *Tunnel, h *Host) {
	id := newSessionID()
	protocols := subprotocols(r.Header.Get("Sec-WebSocket-Protocol"))
	visitor, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The local app checks Origin; it gets the visitor's in the header lines.
		InsecureSkipVerify: true,
		Subprotocols:       protocols,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	visitor.SetReadLimit(wsReadLimit)
	defer visitor.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	bridge, err := h.OpenWS(ctx, &tunnelwire.WSOpen{
		SessionID:    id,
		Target:       row.Target,
		Path:         r.URL.RequestURI(),
		HeaderLines:  append(flattenHeaders(r.Header), hostLine(r)),
		Subprotocols: protocols,
	})
	if err != nil {
		_ = visitor.Close(websocket.StatusTryAgainLater, "the computer serving this tunnel is unreachable")
		return
	}
	defer h.FinishWS(id)
	if bridge.Ack.CloseCode != 0 {
		_ = visitor.Close(websocket.StatusCode(bridge.Ack.CloseCode), bridge.Ack.CloseReason)
		return
	}

	// Host → visitor.
	written := make(chan struct{})
	go func() {
		defer close(written)
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-bridge.Closes:
				_ = visitor.Close(websocket.StatusCode(c.Code), c.Reason)
				return
			case f, ok := <-bridge.Frames:
				if !ok {
					_ = visitor.Close(websocket.StatusInternalError, "fell behind")
					return
				}
				typ := websocket.MessageText
				if f.Binary {
					typ = websocket.MessageBinary
				}
				if writeVisitor(ctx, visitor, typ, f.Data) != nil {
					return
				}
			}
		}
	}()

	// Visitor → host.
	for {
		typ, data, err := visitor.Read(ctx)
		if err != nil {
			code, reason := closeOf(err)
			_ = h.SendWSClose(&tunnelwire.WSClose{SessionID: id, Code: int(code), Reason: reason})
			break
		}
		if h.SendWSFrame(&tunnelwire.WSFrame{SessionID: id, Binary: typ == websocket.MessageBinary, Data: data}) != nil {
			_ = visitor.Close(websocket.StatusTryAgainLater, "the computer serving this tunnel disconnected")
			break
		}
	}
	cancel()
	<-written
}

func subprotocols(header string) []string {
	var out []string
	for _, p := range strings.Split(header, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func closeOf(err error) (websocket.StatusCode, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code, ce.Reason
	}
	return websocket.StatusGoingAway, ""
}
