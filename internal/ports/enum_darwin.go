//go:build darwin

package ports

import "go.bug.st/serial"

// defaultDetailedPorts on macOS lists device paths only: go.bug.st/serial's
// detailed enumerator (VID/PID) requires cgo/IOKit, which tncd's pure-Go build
// excludes. Ports therefore have no USB metadata, so usb:VID:PID references do
// not resolve on macOS — use the concrete /dev/cu.* path in the config (Resolve
// passes bare paths through unchanged).
func defaultDetailedPorts() ([]portDetail, error) {
	names, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	out := make([]portDetail, 0, len(names))
	for _, n := range names {
		out = append(out, portDetail{Name: n})
	}
	return out, nil
}
