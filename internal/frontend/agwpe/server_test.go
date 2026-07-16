package agwpe

import (
	"net"
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

// TestSendAfterCloseNoPanic verifies that SendAGWPE and sendFrame do not panic
// when called after the client's writeCh has been closed (teardown race — C1).
func TestSendAfterCloseNoPanic(t *testing.T) {
	// Create a minimal client with a real buffered channel.
	c := &client{
		eng:             engine.New(),
		writeCh:         make(chan []byte, 256),
		registeredCalls: make(map[string]bool),
	}
	// Use a connected pair so conn.RemoteAddr() doesn't panic.
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	c.conn = serverConn

	// Simulate run()'s teardown: set closed=true under mu, then close channel.
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	close(c.writeCh)

	// These must not panic, even with -race.
	c.SendAGWPE(0, 'R', 0, "", "", nil)
	c.SendAGWPE(0, 'G', 0, "", "", []byte("hello"))
	c.sendFrame(0, 'R', "", "", nil)
	c.sendFrame(0, 'G', "", "", []byte("world"))
}
