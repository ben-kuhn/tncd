package bridge

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// reconnFakeTransport is a kiss.Transport whose Open count is observable and
// whose Read blocks until Close, so a started port stays "online" until torn
// down. Reused across reconnect attempts (one shared readCh).
type reconnFakeTransport struct {
	opens  int32
	once   sync.Once
	readCh chan struct{}
}

func newReconnFake() *reconnFakeTransport {
	return &reconnFakeTransport{readCh: make(chan struct{})}
}

func (f *reconnFakeTransport) Open() error                    { atomic.AddInt32(&f.opens, 1); return nil }
func (f *reconnFakeTransport) EnterKISS() error               { return nil }
func (f *reconnFakeTransport) ExitKISS()                      {}
func (f *reconnFakeTransport) Write(p []byte) (int, error)    { return len(p), nil }
func (f *reconnFakeTransport) Read(p []byte) (int, error)     { <-f.readCh; return 0, io.EOF }
func (f *reconnFakeTransport) Close() error {
	f.once.Do(func() { close(f.readCh) })
	return nil
}
func (f *reconnFakeTransport) openCount() int { return int(atomic.LoadInt32(&f.opens)) }

func waitFor(t *testing.T, cond func() bool, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func reconnCfg(reconnect bool) *config.Config {
	return &config.Config{
		Server: config.Server{ListenHost: "127.0.0.1", ListenPort: 0, Callsign: "TEST", MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name: "Port 0", Type: "serial", Device: "/dev/fake",
			OTABaudrate: 1200, AX25Version: 22,
			Reconnect: reconnect, ReconnectDelay: 0.01, ReconnectMaxDelay: 0.05,
		}},
	}
}

// TestSerialPortReconnectsAfterOffline proves a serial port with reconnect=true
// reopens after going offline (previously only bluetooth ports reconnected).
func TestSerialPortReconnectsAfterOffline(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(true))
	ft := newReconnFake()
	b.newTransport = func(config.Port) (kiss.Transport, error) { return ft, nil }

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, func() bool { return ft.openCount() >= 1 }, 2*time.Second, "initial open")

	// Simulate the reader detecting a disconnect.
	onLoop(t, eng, func() { b.portWentOffline(0) })

	waitFor(t, func() bool { return ft.openCount() >= 2 }, 2*time.Second, "reconnect open")
}

// TestPortNoReconnectWhenDisabled proves reconnect=false opts out: no reopen
// after going offline.
func TestPortNoReconnectWhenDisabled(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(false))
	ft := newReconnFake()
	b.newTransport = func(config.Port) (kiss.Transport, error) { return ft, nil }

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, func() bool { return ft.openCount() >= 1 }, 2*time.Second, "initial open")

	onLoop(t, eng, func() { b.portWentOffline(0) })

	// Give any (erroneous) reconnect time to fire; opens must stay at 1.
	time.Sleep(200 * time.Millisecond)
	if got := ft.openCount(); got != 1 {
		t.Fatalf("reconnect happened despite reconnect=false: open count = %d", got)
	}
}
