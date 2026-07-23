//go:build !linux

package kiss

import "fmt"

// applyRTSCTS is unsupported on non-Linux platforms. On Windows/macOS/BSD the
// underlying OS serial layer (or a virtual COM port) is expected to manage flow
// control; a hard error here lets the caller log a clear warning.
func applyRTSCTS(port modemPort) error {
	return fmt.Errorf("RTSCTS not supported on this platform")
}
