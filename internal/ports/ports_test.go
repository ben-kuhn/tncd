package ports

import (
	"strings"
	"testing"

	"go.bug.st/serial/enumerator"
)

func withFakePorts(t *testing.T, ds []*enumerator.PortDetails) {
	t.Helper()
	orig := detailedPorts
	detailedPorts = func() ([]*enumerator.PortDetails, error) { return ds, nil }
	t.Cleanup(func() { detailedPorts = orig })
}

func TestListUSBAndPlainPorts(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM3", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A50285BI", Product: "FT232R USB UART"},
		{Name: "COM1", IsUSB: false},
	})
	ps, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("want 2 ports, got %d", len(ps))
	}
	// Sorted by Device: COM1 before COM3.
	if ps[0].Device != "COM1" || ps[0].Ref != "COM1" || ps[0].Kind != KindSerial {
		t.Errorf("plain port wrong: %+v", ps[0])
	}
	if ps[1].Ref != "usb:0403:6001:A50285BI" {
		t.Errorf("usb ref = %q, want usb:0403:6001:A50285BI", ps[1].Ref)
	}
	if !strings.Contains(ps[1].Label, "FT232R") || !strings.Contains(ps[1].Label, "COM3") {
		t.Errorf("usb label = %q, want it to mention product + COM3", ps[1].Label)
	}
}

func TestListUSBNoSerialNumber(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM5", IsUSB: true, VID: "10C4", PID: "EA60", Product: "CP2102"},
	})
	ps, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ps[0].Ref != "usb:10c4:ea60" {
		t.Errorf("ref = %q, want usb:10c4:ea60 (lowercased, no serial)", ps[0].Ref)
	}
}

func TestResolvePassthrough(t *testing.T) {
	// No enumerator call needed for bare device paths.
	for _, dev := range []string{"COM3", "/dev/ttyUSB0", "/dev/serial/by-id/usb-FTDI"} {
		got, err := Resolve(dev)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", dev, err)
		}
		if got != dev {
			t.Errorf("Resolve(%q) = %q, want passthrough", dev, got)
		}
	}
}

func TestResolveUSBSingleMatch(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A50285BI"},
		{Name: "COM2", IsUSB: false},
	})
	got, err := Resolve("usb:0403:6001")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "COM7" {
		t.Errorf("got %q, want COM7", got)
	}
}

func TestResolveUSBNoMatch(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "1234", PID: "5678"},
	})
	if _, err := Resolve("usb:0403:6001"); err == nil {
		t.Fatal("want error for no match, got nil")
	}
}

func TestResolveUSBAmbiguousWantsSerial(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "AAAA"},
		{Name: "COM8", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "BBBB"},
	})
	_, err := Resolve("usb:0403:6001")
	if err == nil {
		t.Fatal("want ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "serial number") {
		t.Errorf("error should suggest adding a serial number, got: %v", err)
	}
}

func TestResolveUSBWithSerialDisambiguates(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "AAAA"},
		{Name: "COM8", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "BBBB"},
	})
	got, err := Resolve("usb:0403:6001:bbbb") // case-insensitive
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "COM8" {
		t.Errorf("got %q, want COM8", got)
	}
}

func TestResolveBadRef(t *testing.T) {
	if _, err := Resolve("usb:0403"); err == nil {
		t.Fatal("want error for malformed ref, got nil")
	}
}
