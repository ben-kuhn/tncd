package l2

import "strings"

// ConnInfo is a read-only snapshot of one active connection, for the API.
type ConnInfo struct {
	Port       int      `json:"port"`
	Local      string   `json:"local"`
	Remote     string   `json:"remote"`
	Via        []string `json:"via"`
	State      string   `json:"state"` // lowercase: "connected" / "connecting"
	Incoming   bool     `json:"incoming"`
	Modulo     uint8    `json:"modulo"`
	SendSeq    uint8    `json:"send_seq"`
	RecvSeq    uint8    `json:"recv_seq"`
	Unacked    uint8    `json:"unacked"`
	SendQueue  int      `json:"send_queue"`
	T1Retries  int      `json:"t1_retries"`
	RemoteBusy bool     `json:"remote_busy"`
	SRTTms     int64    `json:"srtt_ms"`
	SREJ       bool     `json:"srej"`
}

// Snapshot returns info for every active (Connecting/Connected) connection.
// Must be called on the engine loop.
func (t *Table) Snapshot() []ConnInfo {
	out := make([]ConnInfo, 0, len(t.conns))
	for _, c := range t.conns {
		if c.State != Connecting && c.State != Connected {
			continue
		}
		via := append([]string(nil), c.Via...)
		out = append(out, ConnInfo{
			Port: c.Port, Local: c.Local, Remote: c.Remote, Via: via,
			State:    strings.ToLower(c.State.String()),
			Incoming: c.incoming, Modulo: c.modulo,
			SendSeq: c.sendSeq, RecvSeq: c.recvSeq, Unacked: c.unacked,
			SendQueue: len(c.outQueue), T1Retries: c.t1Polls,
			RemoteBusy: c.remoteBusy, SRTTms: c.srtt.Milliseconds(), SREJ: c.srejEnabled,
		})
	}
	return out
}
