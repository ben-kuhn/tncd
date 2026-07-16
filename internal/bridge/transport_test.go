package bridge

import (
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// TestBuildTransportDoesNotExpandEscapes verifies that buildTransport accepts
// a raw init string with literal backslash escapes and creates a transport
// without error. The serial layer (EnterKISS/ExitKISS) resolves escapes
// per-line — pre-expanding here would corrupt multi-line sequences (I3).
func TestBuildTransportDoesNotExpandEscapes(t *testing.T) {
	pc := config.Port{
		Type:           "serial",
		Device:         "/dev/null",
		SerialBaudrate: 9600,
		InitString:     `KISS ON\rRESTART\r`,
		HostExitString: `KISS OFF\r`,
	}
	_, err := buildTransport(pc)
	if err != nil {
		t.Fatalf("buildTransport returned error: %v", err)
	}
}
