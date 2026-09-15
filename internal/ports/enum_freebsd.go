//go:build freebsd

package ports

import (
	"os"
	"regexp"
	"sort"
)

// freebsdCallout matches FreeBSD callout serial devices: cuau0 (onboard UART),
// cuaU0 (USB serial). The callout (cua*) side is the one to open for outbound
// use; the matching tty* dial-in devices block on carrier detect, which is not
// what a TNC link wants.
//
// The trailing anchor excludes the .init and .lock clones FreeBSD creates for
// every port, which are settings templates rather than openable ports.
var freebsdCallout = regexp.MustCompile(`^cua[a-zA-Z]+[0-9]+$`)

// defaultDetailedPorts enumerates serial ports on FreeBSD by reading /dev.
//
// go.bug.st/serial cannot do this for us. Its detailed enumerator is an
// unimplemented stub on FreeBSD (returns PortEnumerationError, which is what
// `tncd ports` used to fail with), and its plain GetPortsList filters on
// `^(cu|tty)\..*` — macOS naming, which matches nothing on FreeBSD where the
// devices are cuau0/cuaU0 rather than cu.something.
//
// Ports therefore carry no USB metadata here, so usb:VID:PID references do not
// resolve on FreeBSD; use the concrete /dev/cua* path in the config (Resolve
// passes bare paths through unchanged), exactly as on macOS.
func defaultDetailedPorts() ([]portDetail, error) {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, err
	}
	var out []portDetail
	for _, e := range entries {
		if e.IsDir() || !freebsdCallout.MatchString(e.Name()) {
			continue
		}
		out = append(out, portDetail{Name: "/dev/" + e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
