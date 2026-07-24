package bridge

import "testing"

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
