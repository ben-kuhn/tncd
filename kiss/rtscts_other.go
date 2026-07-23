//go:build !linux

package kiss

import "fmt"

// applyRTSCTS is unsupported on non-Linux platforms. On Windows/macOS/BSD the
// underlying OS serial layer (or a virtual COM port) is expected to manage flow
// control; returns an error the caller logs as a non-fatal warning.
func applyRTSCTS(port modemPort) error {
	return fmt.Errorf("RTSCTS not supported on this platform")
}
