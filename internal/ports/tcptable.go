package ports

import "encoding/binary"

// tcpRow is one listening socket in a Windows TCP table: its port and the
// process that owns it.
type tcpRow struct{ port, pid int }

// The rows of GetExtendedTcpTable's TCP_TABLE_OWNER_PID_LISTENER tables, as
// laid out in memory: MIB_TCPROW_OWNER_PID (IPv4) and MIB_TCP6ROW_OWNER_PID.
const (
	tcp4RowSize = 24 // state, local addr, local port, remote addr, remote port, pid
	tcp6RowSize = 56 // local addr [16], scope, local port, remote addr [16], scope, remote port, state, pid
)

// parseTCPTable reads a MIB_TCPTABLE_OWNER_PID (or its IPv6 twin): a count,
// then the rows. Each port is in network byte order in its DWORD's low two
// bytes. A short table yields the rows it holds.
func parseTCPTable(buf []byte, v6 bool) []tcpRow {
	if len(buf) < 4 {
		return nil
	}
	size, portAt, pidAt := tcp4RowSize, 8, 20
	if v6 {
		size, portAt, pidAt = tcp6RowSize, 20, 52
	}
	n := int(binary.LittleEndian.Uint32(buf))
	rows := make([]tcpRow, 0, n)
	for i := 0; i < n; i++ {
		row := buf[4+i*size:]
		if len(row) < size {
			break
		}
		port := int(binary.BigEndian.Uint16(row[portAt:]))
		if port == 0 {
			continue
		}
		rows = append(rows, tcpRow{port: port, pid: int(binary.LittleEndian.Uint32(row[pidAt:]))})
	}
	return rows
}
