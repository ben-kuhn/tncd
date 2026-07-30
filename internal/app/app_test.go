package app_test

import (
	"net"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// TestRuntimeServesThenShutsDown verifies that New brings up the AGWPE
// listener (accepting connections) and that Shutdown both stops the engine
// loop (Wait returns) and closes the listener.
func TestRuntimeServesThenShutsDown(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{
			ListenHost: "127.0.0.1",
			ListenPort: 0, // ephemeral
			Callsign:   "TEST",
			MaxClients: 8,
		},
		AX25: config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name:        "Port 0",
			Type:        "tcp",
			Host:        "127.0.0.1",
			TCPPort:     1, // nothing listening; bridge connects async, won't block New
			OTABaudrate: 1200,
			AX25Version: 22,
			Reconnect:   false,
		}},
	}

	r, err := app.New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	addr := r.AGWPEAddr().String()

	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()

	// Listener is accepting while running.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial while running: %v", err)
	}
	c.Close()

	r.Shutdown()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Shutdown")
	}

	// Listener is closed after shutdown.
	if c2, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c2.Close()
		t.Fatal("listener still accepting after Shutdown")
	}
}
