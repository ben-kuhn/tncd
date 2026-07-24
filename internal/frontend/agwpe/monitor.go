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
