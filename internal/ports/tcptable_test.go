package ports

import (
	"encoding/binary"
	"testing"
)

// A Windows TCP table reads as its rows' ports and owners, the port taken
// from network byte order, for IPv4 and IPv6, and a truncated table yields
// only the whole rows.
func TestParseTCPTable(t *testing.T) {
	row := func(size, portAt, pidAt, port, pid int) []byte {
		b := make([]byte, size)
		binary.BigEndian.PutUint16(b[portAt:], uint16(port))
		binary.LittleEndian.PutUint32(b[pidAt:], uint32(pid))
		return b
	}
	table := func(rows ...[]byte) []byte {
		b := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)))
		for _, r := range rows {
			b = append(b, r...)
		}
		return b
	}
	v4 := table(row(tcp4RowSize, 8, 20, 3000, 412), row(tcp4RowSize, 8, 20, 47444, 77))
	if got := parseTCPTable(v4, false); len(got) != 2 || got[0] != (tcpRow{3000, 412}) || got[1] != (tcpRow{47444, 77}) {
		t.Fatalf("IPv4: %+v", got)
	}
	v6 := table(row(tcp6RowSize, 20, 52, 5173, 9))
	if got := parseTCPTable(v6, true); len(got) != 1 || got[0] != (tcpRow{5173, 9}) {
		t.Fatalf("IPv6: %+v", got)
	}
	if got := parseTCPTable(v4[:4+tcp4RowSize+3], false); len(got) != 1 {
		t.Fatalf("truncated: %+v", got)
	}
	if got := parseTCPTable(nil, false); got != nil {
		t.Fatalf("empty: %+v", got)
	}
}
