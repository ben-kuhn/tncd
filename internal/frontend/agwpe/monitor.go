// monitor.go — AGWPE monitor frame distribution (moved from bridge/monitor.go).
// Registered as a bridge.MonitorSink; output bytes are unchanged.
package agwpe

import (
	"fmt"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
)

type monitorSink struct{ b *bridge.Bridge }

// NewMonitorSink returns a MonitorSink that formats decoded RX frames as AGWPE
// 'U'/'I'/'S' monitor frames and delivers them to monitoring AGWPE clients.
func NewMonitorSink(b *bridge.Bridge) bridge.MonitorSink { return &monitorSink{b: b} }

type rawSink struct{ b *bridge.Bridge }

// NewRawSink returns a RawRXSink that delivers every frame heard on the air to
// AGWPE clients that enabled raw mode with 'k', as an AGWPE 'K' frame.
//
// This is the mode Xastir uses — it sends 'k' and never 'm', so without this it
// connects successfully and then receives nothing at all.
func NewRawSink(b *bridge.Bridge) bridge.RawRXSink { return &rawSink{b: b} }

// OnRawRX forwards one raw AX.25 frame to every client in raw mode.
//
// The data field is the port number in the high nibble of a leading byte,
// followed by the unmodified AX.25 frame. That leading byte is part of the
// protocol, not padding: Dire Wolf writes `chan << 4` there, and Xastir decodes
// the AX.25 header starting one byte past the 36-byte AGWPE header. Omitting it
// shifts the whole frame by one and every client fails to decode.
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

func (m *monitorSink) OnRXFrame(port int, f *ax25.Frame) {
	src := f.Src.String()
	dst := f.Dst.String()
	ts := time.Now().Format("15:04:05")

	switch {
	case f.Type == ax25.UI:
		data := f.Info
		if data == nil {
			data = []byte{}
		}
		pid := f.PID
		header := fmt.Sprintf("Fm %s To %s <UI pid=%02X Len=%d >[%s]\r", src, dst, pid, len(data), ts)
		payload := append([]byte(header), data...)
		for _, c := range m.b.Clients() {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'U', pid, src, dst, payload)
			}
		}
	case f.Type.IsI():
		data := f.Info
		if data == nil {
			data = []byte{}
		}
		pid := f.PID
		header := fmt.Sprintf("Fm %s To %s <I pid=%02X Len=%d >[%s]\r", src, dst, pid, len(data), ts)
		payload := append([]byte(header), data...)
		for _, c := range m.b.Clients() {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'I', pid, src, dst, payload)
			}
		}
	case f.Type.IsS():
		name := f.Type.String()
		nr := f.NR
		payload := []byte(fmt.Sprintf("Fm %s To %s <%s R%d >[%s]\r", src, dst, name, nr, ts))
		for _, c := range m.b.Clients() {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'S', 0, src, dst, payload)
			}
		}
	}
}
