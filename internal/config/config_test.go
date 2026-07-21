package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tncd.ini")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad1xConfigUnchanged(t *testing.T) {
	// The shipped 1.x example config must load without error.
	cfg, err := Load("../../tncd.ini")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ListenPort != 8000 || cfg.Server.Callsign != "N0CALL" {
		t.Errorf("server = %+v", cfg.Server)
	}
	if len(cfg.Ports) != 1 || cfg.Ports[0].Type != "serial" ||
		cfg.Ports[0].Device != "/dev/ttyUSB0" {
		t.Errorf("ports = %+v", cfg.Ports)
	}
}

func TestDefaults(t *testing.T) {
	p := write(t, "[server]\n[client.0]\ntype = serial\ndevice = /dev/x\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	pt := cfg.Ports[0]
	if pt.SerialBaudrate != 9600 || pt.OTABaudrate != 1200 ||
		!pt.SendKISSExit || pt.InitDelay != 1.0 || pt.ExitDelay != 1.0 ||
		pt.Name != "Port 0" {
		t.Errorf("defaults wrong: %+v", pt)
	}
	if cfg.AX25.MaxWindow != 3 || cfg.AX25.N2Retry != 10 ||
		cfg.AX25.T3Timeout != 180 {
		t.Errorf("ax25 defaults wrong: %+v", cfg.AX25)
	}
}

func TestBareClientSectionMigrates(t *testing.T) {
	p := write(t, "[client]\ntype = tcp\nhost = h\nport = 8001\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ports) != 1 || cfg.Ports[0].Host != "h" {
		t.Errorf("migration failed: %+v", cfg.Ports)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct{ name, ini, wantSub string }{
		{"missing type", "[client.0]\ndevice=/dev/x\n", "type"},
		{"bad type", "[client.0]\ntype=carrier-pigeon\n", "carrier-pigeon"},
		{"serial no device", "[client.0]\ntype=serial\n", "device"},
		{"tcp no host", "[client.0]\ntype=tcp\nport=1\n", "host"},
		{"bt no bdaddr", "[client.0]\ntype=bluetooth\n", "bdaddr"},
		{"gap in ports",
			"[client.0]\ntype=serial\ndevice=/dev/x\n[client.2]\ntype=serial\ndevice=/dev/y\n",
			"contiguous"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.ini))
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err = %v, want containing %q", err, c.wantSub)
			}
		})
	}
}

func TestExampleLoads(t *testing.T) {
	p := write(t, Example())
	if _, err := Load(p); err != nil {
		t.Fatalf("Example() output does not load: %v", err)
	}
}

func TestAX25VersionParsing(t *testing.T) {
	p := write(t, `
[client.0]
type = serial
device = /dev/ttyUSB0
[client.1]
type = serial
device = /dev/ttyUSB1
ax25_version = 2.0
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ports[0].AX25Version != 22 {
		t.Errorf("port0 default = %d, want 22", cfg.Ports[0].AX25Version)
	}
	if cfg.Ports[1].AX25Version != 20 {
		t.Errorf("port1 = %d, want 20", cfg.Ports[1].AX25Version)
	}
}

func TestAX25VersionInvalid(t *testing.T) {
	p := write(t, `
[client.0]
type = serial
device = /dev/ttyUSB0
ax25_version = 3.0
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for ax25_version = 3.0")
	}
}

func TestSREJConfig(t *testing.T) {
	p := write(t, `
[client.0]
type = serial
device = /dev/ttyUSB0
[client.1]
type = serial
device = /dev/ttyUSB1
srej = off
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Ports[0].SREJ {
		t.Errorf("port0 SREJ = false, want true (default)")
	}
	if cfg.Ports[1].SREJ {
		t.Errorf("port1 SREJ = true, want false (srej=off)")
	}
}

func TestUppercaseKeysAccepted(t *testing.T) {
	p := write(t, "[client.0]\nType = serial\nDevice = /dev/x\nSerial_Baudrate = 1200\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Ports[0].Device != "/dev/x" || cfg.Ports[0].SerialBaudrate != 1200 {
		t.Errorf("port = %+v", cfg.Ports[0])
	}
}
