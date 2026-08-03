package main

import (
	"strings"
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/ports"
)

func samplePorts() []ports.Port {
	return []ports.Port{
		{Ref: "COM1", Label: "Serial: COM1", Kind: "serial", Device: "COM1"},
		{Ref: "usb:0403:6001:A50285BI", Label: "USB: FT232R USB UART (COM3)", Kind: "serial",
			Device: "COM3", VID: "0403", PID: "6001", Serial: "A50285BI"},
	}
}

func TestFormatPortsTable(t *testing.T) {
	out, err := formatPorts(samplePorts(), false)
	if err != nil {
		t.Fatalf("formatPorts: %v", err)
	}
	if !strings.Contains(out, "usb:0403:6001:A50285BI") {
		t.Errorf("table missing usb ref:\n%s", out)
	}
	if !strings.Contains(out, "FT232R") {
		t.Errorf("table missing label:\n%s", out)
	}
	if !strings.Contains(out, "COM1") {
		t.Errorf("table missing plain port:\n%s", out)
	}
}

func TestFormatPortsJSON(t *testing.T) {
	out, err := formatPorts(samplePorts(), true)
	if err != nil {
		t.Fatalf("formatPorts: %v", err)
	}
	if !strings.Contains(out, `"ref": "usb:0403:6001:A50285BI"`) {
		t.Errorf("json missing ref field:\n%s", out)
	}
	if !strings.Contains(out, `"kind": "serial"`) {
		t.Errorf("json missing kind field:\n%s", out)
	}
}

func TestFormatPortsEmptyTable(t *testing.T) {
	out, err := formatPorts(nil, false)
	if err != nil {
		t.Fatalf("formatPorts: %v", err)
	}
	if !strings.Contains(out, "no serial ports found") {
		t.Errorf("want empty-table message, got:\n%s", out)
	}
}
