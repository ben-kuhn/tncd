package agwpe

import (
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
	last struct{ kind byte; from, to string; data []byte }
	n    int
}
func (c *capClient) SendAGWPE(_ uint8, kind byte, _ uint8, from, to string, data []byte) {
	c.n++; c.last.kind = kind; c.last.from = from; c.last.to = to; c.last.data = data
}
func (c *capClient) Monitoring() bool                { return c.mon }
func (c *capClient) RegisteredCalls() map[string]bool { return map[string]bool{} }
func (c *capClient) LastActivity() time.Time         { return time.Now() }
func (c *capClient) CloseTransport()                 {}

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
