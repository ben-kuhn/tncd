// Package ports discovers serial (and, later, Bluetooth) devices and resolves
// stable device references to the concrete OS device path. A "usb:VID:PID" or
// "usb:VID:PID:SERIAL" reference survives Windows COM-port renumbering, since it
// is matched against the currently-attached USB serial ports at open time.
package ports

import (
	"fmt"
	"sort"
	"strings"

	"go.bug.st/serial/enumerator"
)

// Kind values for Port.
const (
	KindSerial    = "serial"
	KindBluetooth = "bluetooth"
)

// Port describes a device that can back a KISS TNC connection.
type Port struct {
	Ref    string `json:"ref"`              // stable value to write into config `device = ...`
	Label  string `json:"label"`            // human-readable label for the wizard/list
	Kind   string `json:"kind"`             // KindSerial | KindBluetooth
	Device string `json:"device"`           // current OS device path (COMx / /dev/ttyUSB0)
	VID    string `json:"vid,omitempty"`    // USB vendor id (USB serial only)
	PID    string `json:"pid,omitempty"`    // USB product id (USB serial only)
	Serial string `json:"serial,omitempty"` // USB serial number (when available)
}

// detailedPorts is the serial enumerator, indirected so tests can fake it.
var detailedPorts = func() ([]*enumerator.PortDetails, error) {
	return enumerator.GetDetailedPortsList()
}

// List returns all discoverable serial ports. USB ports get a "usb:VID:PID[:SERIAL]"
// Ref; other ports use their device path as the Ref. Results are sorted by
// device path for stable output.
func List() ([]Port, error) {
	ds, err := detailedPorts()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	out := make([]Port, 0, len(ds))
	for _, d := range ds {
		p := Port{Kind: KindSerial, Device: d.Name}
		if d.IsUSB && d.VID != "" && d.PID != "" {
			p.VID = strings.ToLower(d.VID)
			p.PID = strings.ToLower(d.PID)
			p.Serial = d.SerialNumber
			p.Ref = usbRef(p.VID, p.PID, p.Serial)
			name := d.Product
			if name == "" {
				name = "USB serial"
			}
			p.Label = fmt.Sprintf("USB: %s (%s)", name, d.Name)
		} else {
			p.Ref = d.Name
			p.Label = fmt.Sprintf("Serial: %s", d.Name)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

// usbRef builds a "usb:vid:pid[:serial]" reference.
func usbRef(vid, pid, serial string) string {
	if serial != "" {
		return fmt.Sprintf("usb:%s:%s:%s", vid, pid, serial)
	}
	return fmt.Sprintf("usb:%s:%s", vid, pid)
}

// Resolve turns a config device reference into a concrete OS device path.
// A "usb:VID:PID[:SERIAL]" reference is matched (case-insensitively) against
// currently-attached USB serial ports. Any other value — a bare COMx, a
// /dev/tty* path, etc. — is returned unchanged.
func Resolve(ref string) (string, error) {
	if !strings.HasPrefix(ref, "usb:") {
		return ref, nil
	}
	vid, pid, serial, err := parseUSBRef(ref)
	if err != nil {
		return "", err
	}
	ds, err := detailedPorts()
	if err != nil {
		return "", fmt.Errorf("enumerate serial ports: %w", err)
	}
	var matches []string
	for _, d := range ds {
		if !d.IsUSB {
			continue
		}
		if !strings.EqualFold(d.VID, vid) || !strings.EqualFold(d.PID, pid) {
			continue
		}
		if serial != "" && !strings.EqualFold(d.SerialNumber, serial) {
			continue
		}
		matches = append(matches, d.Name)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no USB serial port matching %s (is the device plugged in?)", ref)
	case 1:
		return matches[0], nil
	default:
		if serial == "" {
			return "", fmt.Errorf("multiple USB serial ports match %s (%s); add a serial number: usb:%s:%s:SERIAL",
				ref, strings.Join(matches, ", "), vid, pid)
		}
		return "", fmt.Errorf("multiple USB serial ports match %s (%s)", ref, strings.Join(matches, ", "))
	}
}

// parseUSBRef parses "usb:vid:pid" or "usb:vid:pid:serial". The serial number
// may itself contain colons, so only the first two colons are structural.
func parseUSBRef(ref string) (vid, pid, serial string, err error) {
	body := strings.TrimPrefix(ref, "usb:")
	parts := strings.SplitN(body, ":", 3)
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			break
		}
		return parts[0], parts[1], "", nil
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			break
		}
		return parts[0], parts[1], parts[2], nil
	}
	return "", "", "", fmt.Errorf("invalid usb device reference %q (want usb:VID:PID or usb:VID:PID:SERIAL)", ref)
}
