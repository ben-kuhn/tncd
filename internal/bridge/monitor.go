// monitor.go — AGWPE monitor frame distribution.
// Mirrors tncd.py:1713-1720, 1722-1733, 1834-1842, 2118-2128.
package bridge

import (
	"fmt"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

// distributeMonitor sends monitor frames ('U', 'I', 'S') to every client
// that has monitoring enabled. Called from OnKISSFrame after l2.OnFrame.
// Mirrors tncd.py dispatch_ui / _dispatch_i / _dispatch_s monitor blocks.
func distributeMonitor(clients []Client, port int, f *ax25.Frame) {
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
		header := fmt.Sprintf("Fm %s To %s <UI pid=%02X Len=%d >[%s]\r",
			src, dst, pid, len(data), ts)
		payload := append([]byte(header), data...)
		for _, c := range clients {
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
		header := fmt.Sprintf("Fm %s To %s <I pid=%02X Len=%d >[%s]\r",
			src, dst, pid, len(data), ts)
		payload := append([]byte(header), data...)
		for _, c := range clients {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'I', pid, src, dst, payload)
			}
		}

	case f.Type.IsS():
		// S-frame names: RR, RNR, REJ
		name := f.Type.String()
		nr := f.NR
		payload := []byte(fmt.Sprintf("Fm %s To %s <%s R%d >[%s]\r",
			src, dst, name, nr, ts))
		for _, c := range clients {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'S', 0, src, dst, payload)
			}
		}
	}
}
