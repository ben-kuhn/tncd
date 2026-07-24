// events.go — the frontend subscriber bus. Sinks are invoked on the engine
// loop. Registration happens during setup (before engine.Run) or on the loop.
package bridge

import "github.com/ben-kuhn/tncd/v2/ax25"

// RawRXSink receives raw AX.25 bytes heard from the air (post echo-suppression),
// e.g. the KISS-over-TCP passthrough.
type RawRXSink interface {
	OnRawRX(port int, raw []byte)
}

// MonitorSink receives decoded received frames for monitoring, e.g. the AGWPE
// monitor and (later) the read-only API.
type MonitorSink interface {
	OnRXFrame(port int, f *ax25.Frame)
}

func (b *Bridge) RegisterRawRXSink(s RawRXSink)     { b.rawSinks = append(b.rawSinks, s) }
func (b *Bridge) RegisterMonitorSink(s MonitorSink) { b.monitorSinks = append(b.monitorSinks, s) }

func (b *Bridge) UnregisterRawRXSink(s RawRXSink) {
	for i, x := range b.rawSinks {
		if x == s {
			b.rawSinks = append(b.rawSinks[:i], b.rawSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) UnregisterMonitorSink(s MonitorSink) {
	for i, x := range b.monitorSinks {
		if x == s {
			b.monitorSinks = append(b.monitorSinks[:i], b.monitorSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) emitRawRX(port int, raw []byte) {
	for _, s := range b.rawSinks {
		s.OnRawRX(port, raw)
	}
}

func (b *Bridge) emitMonitor(port int, f *ax25.Frame) {
	for _, s := range b.monitorSinks {
		s.OnRXFrame(port, f)
	}
}

// TxFrameSink receives decoded frames tncd transmits (all TX: L2, AGWPE, kisstcp).
type TxFrameSink interface {
	OnTXFrame(port int, f *ax25.Frame)
}

// ConnSink receives connection lifecycle events.
type ConnSink interface {
	OnConn(e ConnEvent)
}

// ConnEvent is a connection lifecycle change. State is "connected" or "disconnected".
type ConnEvent struct {
	Port          int
	Local, Remote string
	State         string
	Incoming      bool // meaningful for "connected"
}

func (b *Bridge) RegisterTxFrameSink(s TxFrameSink) { b.txSinks = append(b.txSinks, s) }
func (b *Bridge) RegisterConnSink(s ConnSink)       { b.connSinks = append(b.connSinks, s) }

func (b *Bridge) UnregisterTxFrameSink(s TxFrameSink) {
	for i, x := range b.txSinks {
		if x == s {
			b.txSinks = append(b.txSinks[:i], b.txSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) UnregisterConnSink(s ConnSink) {
	for i, x := range b.connSinks {
		if x == s {
			b.connSinks = append(b.connSinks[:i], b.connSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) emitTXFrame(port int, f *ax25.Frame) {
	for _, s := range b.txSinks {
		s.OnTXFrame(port, f)
	}
}

func (b *Bridge) emitConn(e ConnEvent) {
	for _, s := range b.connSinks {
		s.OnConn(e)
	}
}
