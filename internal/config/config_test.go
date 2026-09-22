package config

import (
	"fmt"
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

func TestAPIServeUIDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n\n[api]\nenabled=true\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.API.ServeUI {
		t.Fatal("serve_ui should default to true when [api] present")
	}
}

func TestAPIServeUIExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n\n[api]\nenabled=true\nserve_ui=false\n"), 0o644)
	cfg, _ := Load(path)
	if cfg.API.ServeUI {
		t.Fatal("serve_ui=false must disable the UI")
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

func TestKISSTCPSectionParsed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte(`
[client.0]
type = serial
device = /dev/null

[kisstcp]
enabled = true
listen_port = 8010
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.KISSTCP.Enabled || cfg.KISSTCP.ListenPort != 8010 {
		t.Fatalf("KISSTCP = %+v, want enabled + port 8010", cfg.KISSTCP)
	}
	if cfg.KISSTCP.ListenHost != "127.0.0.1" || cfg.KISSTCP.MaxClients != 16 {
		t.Fatalf("defaults wrong: %+v", cfg.KISSTCP)
	}
}

func TestKISSTCPAbsentDisabled(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype = serial\ndevice = /dev/null\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KISSTCP.Enabled {
		t.Fatal("KISSTCP should default disabled when section absent")
	}
}

func TestAPISectionParsed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n\n[api]\nenabled=true\nlisten_port=9002\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.API.Enabled || cfg.API.ListenPort != 9002 || cfg.API.ListenHost != "127.0.0.1" || cfg.API.MaxClients != 16 {
		t.Fatalf("API cfg wrong: %+v", cfg.API)
	}
}

func TestAPIAbsentDisabled(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n"), 0o644)
	cfg, _ := Load(path)
	if cfg.API.Enabled {
		t.Fatal("API should default disabled")
	}
}

func TestAllowedSubnets(t *testing.T) {
	ini := "[server]\n" +
		"allowed_subnets = 192.168.1.0/24, 10.0.0.1\n\n" +
		"[kisstcp]\n" +
		"enabled = true\n" +
		"idle_timeout = 60\n" +
		"allowed_subnets = ::1\n\n" +
		"[api]\n" +
		"enabled = true\n\n" +
		"[client.0]\ntype = serial\ndevice = /dev/x\n"
	cfg, err := Load(write(t, ini))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.AllowedSubnets.Enabled() {
		t.Error("server AllowedSubnets should be enabled")
	}
	if !cfg.KISSTCP.AllowedSubnets.Enabled() {
		t.Error("kisstcp AllowedSubnets should be enabled")
	}
	if cfg.API.AllowedSubnets.Enabled() {
		t.Error("api AllowedSubnets should default to disabled (allow all)")
	}
	if cfg.KISSTCP.IdleTimeout != 60 {
		t.Errorf("kisstcp idle_timeout = %d, want 60", cfg.KISSTCP.IdleTimeout)
	}
}

func TestAllowedSubnetsInvalid(t *testing.T) {
	ini := "[server]\nallowed_subnets = not-a-cidr\n\n[client.0]\ntype = serial\ndevice = /dev/x\n"
	if _, err := Load(write(t, ini)); err == nil {
		t.Fatal("invalid allowed_subnets should fail config load")
	}
}

func TestKISSTCPIdleTimeoutDefault(t *testing.T) {
	p := write(t, "[client.0]\ntype = serial\ndevice = /dev/x\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KISSTCP.IdleTimeout != 300 {
		t.Errorf("kisstcp idle_timeout default = %d, want 300", cfg.KISSTCP.IdleTimeout)
	}
}

func TestRXWedgeTimeoutDefault(t *testing.T) {
	// Bluetooth defaults the read-side watchdog on (20s); serial/tcp off (0);
	// an explicit value (including 0 to disable) always wins.
	bt, err := Load(write(t, "[client.0]\ntype=bluetooth\nbdaddr=00:11:22:33:44:55\n"))
	if err != nil {
		t.Fatal(err)
	}
	if bt.Ports[0].RXWedgeTimeout != 20 {
		t.Errorf("bluetooth default RXWedgeTimeout = %d, want 20", bt.Ports[0].RXWedgeTimeout)
	}

	ser, err := Load(write(t, "[client.0]\ntype=serial\ndevice=/dev/ttyUSB0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ser.Ports[0].RXWedgeTimeout != 0 {
		t.Errorf("serial default RXWedgeTimeout = %d, want 0", ser.Ports[0].RXWedgeTimeout)
	}

	ov, err := Load(write(t, "[client.0]\ntype=bluetooth\nbdaddr=00:11:22:33:44:55\nrx_wedge_timeout=0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ov.Ports[0].RXWedgeTimeout != 0 {
		t.Errorf("explicit rx_wedge_timeout=0 = %d, want 0 (disabled)", ov.Ports[0].RXWedgeTimeout)
	}
}

func TestN2RetryClamped(t *testing.T) {
	// F-new-6: n2_retry <= 0 makes a connect give up after the first T1 with no
	// warning; it must be clamped to >= 1 at load.
	cfg, err := Load(write(t, "[ax25]\nn2_retry = 0\n\n[client.0]\ntype = serial\ndevice = /dev/x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AX25.N2Retry != 1 {
		t.Errorf("N2Retry = %d, want 1 (clamped)", cfg.AX25.N2Retry)
	}
}

func TestSerialParityStopBitsValidated(t *testing.T) {
	// F-new-6: a parity/stopbits typo must be a load (check) error, not an
	// endless reconnect loop once the serial port fails to Open.
	cases := []struct{ ini, want string }{
		{"parity = Q", "parity"},
		{"stopbits = 3", "stopbits"},
	}
	for _, c := range cases {
		ini := "[client.0]\ntype = serial\ndevice = /dev/x\n" + c.ini + "\n"
		if _, err := Load(write(t, ini)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("parity/stopbits %q: err = %v, want containing %q", c.ini, err, c.want)
		}
	}
	// Valid values still load.
	if _, err := Load(write(t, "[client.0]\ntype = serial\ndevice = /dev/x\nparity = E\nstopbits = 1.5\n")); err != nil {
		t.Errorf("valid parity/stopbits rejected: %v", err)
	}
}

func TestPortCountCapped(t *testing.T) {
	// F-new-6: >16 ports wraps the KISS-TCP/raw-'K' port nibble. Reject at load.
	var b strings.Builder
	for i := 0; i <= maxPorts; i++ {
		fmt.Fprintf(&b, "[client.%d]\ntype = serial\ndevice = /dev/x%d\n", i, i)
	}
	if _, err := Load(write(t, b.String())); err == nil || !strings.Contains(err.Error(), "too many ports") {
		t.Errorf("err = %v, want too-many-ports error", err)
	}
}

// TestBLEPortType: type=ble is accepted and, like classic bluetooth, requires
// a bdaddr. Both address the same radio; they differ only in transport.
func TestBLEPortType(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ble.ini")
	if err := os.WriteFile(good, []byte(`
[server]
callsign = N0CALL

[client.0]
type = ble
bdaddr = AA:BB:CC:DD:EE:FF
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(good)
	if err != nil {
		t.Fatalf("type=ble rejected: %v", err)
	}
	if cfg.Ports[0].Type != "ble" {
		t.Fatalf("Type = %q, want ble", cfg.Ports[0].Type)
	}
	if cfg.Ports[0].BDAddr != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("BDAddr = %q", cfg.Ports[0].BDAddr)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid ble port: %v", err)
	}

	bad := filepath.Join(dir, "ble-nobdaddr.ini")
	if err := os.WriteFile(bad, []byte(`
[server]
callsign = N0CALL

[client.0]
type = ble
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("type=ble without bdaddr was accepted")
	}
}

// KISS timing parameters are single bytes on the wire; an out-of-range value
// would silently wrap (tx_delay = 300 -> 44), so it is a load error.
func TestKISSParamOutOfRangeRejected(t *testing.T) {
	for _, kv := range []string{"tx_delay = 300", "persistence = -1", "slot_time = 256", "tx_tail = 1000", "full_duplex = 2"} {
		p := write(t, "[client.0]\ntype = serial\ndevice = /dev/x\n[kiss.0]\n"+kv+"\n")
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "kiss.0") {
			t.Errorf("%q: err = %v, want a [kiss.0] range error", kv, err)
		}
	}
	p := write(t, "[client.0]\ntype = serial\ndevice = /dev/x\n[kiss.0]\ntx_delay = 255\npersistence = 0\nfull_duplex = 1\n")
	if _, err := Load(p); err != nil {
		t.Fatalf("in-range params rejected: %v", err)
	}
}

// [api] allowed_hosts lists the Host header values the API answers to.
func TestAPIAllowedHosts(t *testing.T) {
	p := write(t, "[api]\nenabled = true\nallowed_hosts = tncd.example.org, shack-pi:8002\n[client.0]\ntype = serial\ndevice = /dev/x\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tncd.example.org", "shack-pi:8002"}
	if fmt.Sprint(cfg.API.AllowedHosts) != fmt.Sprint(want) {
		t.Fatalf("AllowedHosts = %q, want %q", cfg.API.AllowedHosts, want)
	}
}
