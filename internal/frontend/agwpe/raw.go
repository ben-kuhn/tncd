// raw.go — AGWPE raw AX.25 frame distribution.
// Registered as a bridge.RawRXSink.
//
// Kept separate from monitor.go deliberately: that file formats decoded frames
// as human-readable 'U'/'I'/'S' monitor text, while this one ships the
// undecoded bytes to clients that decode for themselves. File split follows
// larsks' approach in PR #3.
package agwpe

import (
	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
)

type rawSink struct{ b *bridge.Bridge }

// NewRawSink returns a RawRXSink that delivers every frame heard on the air to
// AGWPE clients that enabled raw mode with 'k', as an AGWPE 'K' frame.
//
// This is the mode Xastir uses — it sends 'k' and never 'm', so without this it
// connects successfully and then receives nothing at all (issue #2).
func NewRawSink(b *bridge.Bridge) bridge.RawRXSink { return &rawSink{b: b} }

// OnRawRX forwards one raw AX.25 frame to every client in raw mode.
//
// The data field is the port number in the high nibble of a leading byte,
// followed by the unmodified AX.25 frame. That leading byte is part of the
// protocol, not padding: Dire Wolf writes `chan << 4` there (a hardcoded 0
// until its 1.8), and Xastir decodes the AX.25 header starting one byte past
// the 36-byte AGWPE header. Omitting it shifts the whole frame by one and every
// client fails to decode; hardcoding 0 works only while the port is 0.
func (m *rawSink) OnRawRX(port int, raw []byte) {
	if len(raw) == 0 {
		return
	}
	data := make([]byte, 0, len(raw)+1)
	data = append(data, byte(port)<<4)
	data = append(data, raw...)

	// Callsigns are decoded for the header fields where possible; clients read
	// the frame itself from the data field, so a malformed frame is still worth
	// delivering with the fields left empty rather than dropped here.
	var src, dst string
	if f, err := ax25.Parse(raw); err == nil {
		src, dst = f.Src.String(), f.Dst.String()
	}

	for _, c := range m.b.Clients() {
		if c.RawMode() {
			c.SendAGWPE(uint8(port), 'K', 0, src, dst, data)
		}
	}
}
