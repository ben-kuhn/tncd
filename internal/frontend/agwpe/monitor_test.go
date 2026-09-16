package agwpe

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

type capClient struct {
	mon  bool
	raw  bool
	last struct {
		kind     byte
		from, to string
		data     []byte
	}
	n int
}

func (c *capClient) SendAGWPE(_ uint8, kind byte, _ uint8, from, to string, data []byte) {
	c.n++
	c.last.kind = kind
	c.last.from = from
	c.last.to = to
	c.last.data = data
}
func (c *capClient) Monitoring() bool                 { return c.mon }
func (c *capClient) RawMode() bool                    { return c.raw }
func (c *capClient) RegisteredCalls() map[string]bool { return map[string]bool{} }
func (c *capClient) LastActivity() time.Time          { return time.Now() }
func (c *capClient) CloseTransport()                  {}

func TestMonitorSinkUIFormat(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})
	cc := &capClient{mon: true}
	done := make(chan struct{})
	eng.Do(func() { b.AddClient(cc); close(done) })
	<-done

	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0,
		Src: ax25.Address{Call: "KU0HN"}, Dst: ax25.Address{Call: "CQ"}, Info: []byte("hi")}
	sink := NewMonitorSink(b)
	d2 := make(chan struct{})
	eng.Do(func() { sink.OnRXFrame(0, f); close(d2) })
	<-d2

	if cc.n != 1 || cc.last.kind != 'U' {
		t.Fatalf("got n=%d kind=%c, want 1 'U'", cc.n, cc.last.kind)
	}
	if !strings.HasPrefix(string(cc.last.data), "Fm KU0HN To CQ <UI pid=F0 Len=2 >[") {
		t.Fatalf("bad UI header: %q", cc.last.data)
	}
	if !strings.HasSuffix(string(cc.last.data), "hi") {
		t.Fatalf("UI payload missing data: %q", cc.last.data)
	}
}

// TestMonitorSinkNonMonitoringClientReceivesNothing verifies that a client
// with Monitoring()==false is skipped: only the monitoring client receives the
// frame, and the non-monitoring client receives nothing.
func TestMonitorSinkNonMonitoringClientReceivesNothing(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})
	monClient := &capClient{mon: true}
	nonMonClient := &capClient{mon: false}
	done := make(chan struct{})
	eng.Do(func() {
		b.AddClient(monClient)
		b.AddClient(nonMonClient)
		close(done)
	})
	<-done

	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0,
		Src: ax25.Address{Call: "KU0HN"}, Dst: ax25.Address{Call: "CQ"}, Info: []byte("hi")}
	sink := NewMonitorSink(b)
	d2 := make(chan struct{})
	eng.Do(func() { sink.OnRXFrame(0, f); close(d2) })
	<-d2

	if monClient.n != 1 {
		t.Fatalf("monitoring client: got n=%d, want 1", monClient.n)
	}
	if nonMonClient.n != 0 {
		t.Fatalf("non-monitoring client: got n=%d, want 0", nonMonClient.n)
	}
}

// Xastir enables raw mode with 'k' and never sends 'm'. Before raw mode was
// implemented it connected successfully and then received nothing at all
// (issue #2), so these tests pin both the routing and the wire format.

func TestRawSinkSendsKFrameOnlyToRawClients(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})

	rawClient := &capClient{raw: true}
	monClient := &capClient{mon: true} // monitoring only: must NOT get 'K'
	plainClient := &capClient{}
	done := make(chan struct{})
	eng.Do(func() {
		b.AddClient(rawClient)
		b.AddClient(monClient)
		b.AddClient(plainClient)
		close(done)
	})
	<-done

	frame := []byte{0x9c, 0x94, 0x6e, 0xa0, 0x40, 0x40, 0xe0, // dst NJ7P
		0x96, 0xaa, 0x60, 0x90, 0x9c, 0x40, 0x61, // src KU0HN
		0x03, 0xf0, 'h', 'i'}
	d2 := make(chan struct{})
	sink := NewRawSink(b)
	eng.Do(func() { sink.OnRawRX(0, frame); close(d2) })
	<-d2

	if rawClient.n != 1 || rawClient.last.kind != 'K' {
		t.Fatalf("raw client: got n=%d kind=%c, want 1 'K'", rawClient.n, rawClient.last.kind)
	}
	if monClient.n != 0 {
		t.Errorf("monitoring-only client got %d raw frames, want 0", monClient.n)
	}
	if plainClient.n != 0 {
		t.Errorf("plain client got %d raw frames, want 0", plainClient.n)
	}
}

func TestRawSinkKFrameWireFormat(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})
	cc := &capClient{raw: true}
	done := make(chan struct{})
	eng.Do(func() { b.AddClient(cc); close(done) })
	<-done

	frame := []byte{0x9c, 0x94, 0x6e, 0xa0, 0x40, 0x40, 0xe0,
		0x96, 0xaa, 0x60, 0x90, 0x9c, 0x40, 0x61,
		0x03, 0xf0, 'h', 'i'}
	d2 := make(chan struct{})
	sink := NewRawSink(b)
	eng.Do(func() { sink.OnRawRX(2, frame); close(d2) })
	<-d2

	got := cc.last.data
	// The leading byte is protocol, not padding: Dire Wolf writes chan<<4 and
	// Xastir decodes AX.25 from one byte past the 36-byte header. Dropping it
	// shifts the frame and every client fails to decode.
	if len(got) != len(frame)+1 {
		t.Fatalf("data len = %d, want %d (frame + 1 leading byte)", len(got), len(frame)+1)
	}
	if got[0] != 2<<4 {
		t.Errorf("leading byte = 0x%02x, want 0x%02x (port 2 << 4)", got[0], 2<<4)
	}
	if !bytes.Equal(got[1:], frame) {
		t.Errorf("frame body altered:\n got %x\nwant %x", got[1:], frame)
	}
}

func TestRawSinkIgnoresEmptyFrame(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})
	cc := &capClient{raw: true}
	done := make(chan struct{})
	eng.Do(func() { b.AddClient(cc); close(done) })
	<-done

	d2 := make(chan struct{})
	sink := NewRawSink(b)
	eng.Do(func() { sink.OnRawRX(0, nil); sink.OnRawRX(0, []byte{}); close(d2) })
	<-d2

	if cc.n != 0 {
		t.Errorf("empty frames produced %d sends, want 0", cc.n)
	}
}
