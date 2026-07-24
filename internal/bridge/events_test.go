package bridge

import (
	"testing"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

type recRaw struct{ ports []int; raws [][]byte }
func (r *recRaw) OnRawRX(port int, raw []byte) {
	r.ports = append(r.ports, port)
	r.raws = append(r.raws, append([]byte{}, raw...))
}

func TestRawRXSinkRegistration(t *testing.T) {
	b := &Bridge{}
	s := &recRaw{}
	b.RegisterRawRXSink(s)
	b.emitRawRX(2, []byte{0x01, 0x02})
	if len(s.ports) != 1 || s.ports[0] != 2 {
		t.Fatalf("after register: ports=%v want [2]", s.ports)
	}
	b.UnregisterRawRXSink(s)
	b.emitRawRX(2, []byte{0x03})
	if len(s.ports) != 1 {
		t.Fatalf("after unregister: got %d deliveries, want 1", len(s.ports))
	}
}

type recTx struct{ ports []int; frames []*ax25.Frame }
func (r *recTx) OnTXFrame(port int, f *ax25.Frame) { r.ports = append(r.ports, port); r.frames = append(r.frames, f) }

type recConn struct{ evs []ConnEvent }
func (r *recConn) OnConn(e ConnEvent) { r.evs = append(r.evs, e) }

func TestTxAndConnSinkRegistration(t *testing.T) {
	b := &Bridge{}
	tx := &recTx{}
	cn := &recConn{}
	b.RegisterTxFrameSink(tx)
	b.RegisterConnSink(cn)
	b.emitTXFrame(1, &ax25.Frame{Type: ax25.UI})
	b.emitConn(ConnEvent{Port: 1, State: "connected"})
	if len(tx.ports) != 1 || tx.ports[0] != 1 {
		t.Fatalf("tx sink not called: %v", tx.ports)
	}
	if len(cn.evs) != 1 || cn.evs[0].State != "connected" {
		t.Fatalf("conn sink not called: %v", cn.evs)
	}
	b.UnregisterTxFrameSink(tx)
	b.UnregisterConnSink(cn)
	b.emitTXFrame(1, &ax25.Frame{})
	b.emitConn(ConnEvent{})
	if len(tx.ports) != 1 || len(cn.evs) != 1 {
		t.Fatalf("unregister failed: tx=%d conn=%d", len(tx.ports), len(cn.evs))
	}
}

func TestSendToKISSEmitsTXFrame(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	tx := &recTx{}
	var b *Bridge
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.RegisterTxFrameSink(tx)
		b.SendToKISS(0, makeUIFrame("KU0HN", "CQ", []byte("hi")))
	})
	if len(tx.frames) != 1 || tx.frames[0].Type != ax25.UI || tx.frames[0].Src.String() != "KU0HN" {
		t.Fatalf("TX frame not emitted correctly: %+v", tx.frames)
	}
}
