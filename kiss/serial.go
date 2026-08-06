package kiss

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"syscall"
	"time"

	goserial "go.bug.st/serial"
)

// SerialConfig holds the configuration for a serial KISS transport.
type SerialConfig struct {
	Device         string
	Baud           int
	Parity         string  // "N", "E", or "O"
	StopBits       float64 // 1, 1.5, or 2
	RTSCTS         bool
	InitString     string // e.g. "KISS ON\rRESTART\r" — literal backslash escapes
	InitDelay      time.Duration
	SendKISSExit   bool
	HostExitString string
	ExitDelay      time.Duration

	// Resolve, if set, maps Device (which may be a stable reference like
	// "usb:VID:PID") to the concrete OS device path to open. It is called on
	// every Open, so a device that moved to a different USB port is re-resolved
	// on reconnect. nil means open Device unchanged.
	Resolve func(ref string) (device string, err error)
}

// serialTransport implements Transport for a serial (RS-232/USB) KISS TNC.
//
// The rw field is populated by Open() from the real go.bug.st/serial port and
// is used for all Read/Write/Close calls. Tests bypass Open() by setting rw
// and probeWait directly on the struct.
// modemPort is the subset of goserial.Port used for modem control signals.
// Extracted as an interface so tests can inject a fake that returns ENOTTY.
type modemPort interface {
	SetDTR(dtr bool) error
	SetRTS(rts bool) error
	SetReadTimeout(t time.Duration) error
	// GetModemStatusBits queries the modem-status lines on the open handle. It
	// errors when the device has been removed (a USB unplug), which is how the
	// reader detects a disconnect on Windows — there Read returns a timeout, not
	// an error, when the device goes away.
	GetModemStatusBits() (*goserial.ModemStatusBits, error)
	Close() error
}

type serialTransport struct {
	cfg       SerialConfig
	rw        io.ReadWriteCloser
	flush     func() error
	probeWait time.Duration // default 1s; overridden to milliseconds in tests

	// openPort is called by Open() to open the underlying serial port.
	// Injected in tests to avoid requiring a real device.
	openPort func(device string, mode *goserial.Mode) (modemPort, error)

	// resolved is the concrete OS device path Open() opened (e.g. "COM4").
	resolved string
	// modem is the opened port, used for modem-status health checks that detect
	// device removal (see modemPort.GetModemStatusBits).
	modem modemPort
	// healthCheck is enabled at Open when the device supports modem status. It
	// stays off for devices that don't (e.g. a PTY / Direwolf pipe), which would
	// otherwise be falsely flagged as removed; those rely on read errors instead.
	healthCheck bool
	// healthInterval rate-limits the removal probe on the idle-read path.
	healthInterval time.Duration
	lastHealth     time.Time
}

// NewSerialTransport returns a Transport backed by a serial port.
func NewSerialTransport(cfg SerialConfig) Transport {
	return &serialTransport{
		cfg:            cfg,
		probeWait:      time.Second,
		healthInterval: time.Second,
	}
}

// Open opens the serial port and asserts DTR/RTS.
//
// go.bug.st/serial v1.8.0 has no flow-control API and disables CRTSCTS on open.
// When cfg.RTSCTS is set, applyRTSCTS re-enables hardware flow control via a
// termios ioctl after the library's last termios write (SetReadTimeout).
func (s *serialTransport) Open() error {
	parity, err := parseParity(s.cfg.Parity)
	if err != nil {
		return fmt.Errorf("serial: %w", err)
	}
	stopBits, err := parseStopBits(s.cfg.StopBits)
	if err != nil {
		return fmt.Errorf("serial: %w", err)
	}

	mode := &goserial.Mode{
		BaudRate: s.cfg.Baud,
		DataBits: 8,
		Parity:   parity,
		StopBits: stopBits,
	}

	// Resolve a stable device reference (e.g. "usb:VID:PID") to the live OS
	// device path. Runs on every Open so a USB replug re-resolves on reconnect.
	device := s.cfg.Device
	if s.cfg.Resolve != nil {
		resolved, rerr := s.cfg.Resolve(device)
		if rerr != nil {
			return fmt.Errorf("serial: resolve %q: %w", device, rerr)
		}
		if resolved != device {
			log.Printf("serial: resolved %s -> %s", device, resolved)
		}
		device = resolved
	}

	// openPort is injected in tests; defaults to goserial.Open.
	openFn := s.openPort
	if openFn == nil {
		openFn = func(device string, mode *goserial.Mode) (modemPort, error) {
			return goserial.Open(device, mode)
		}
	}

	port, err := openFn(device, mode)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("serial: cannot open %s: port is busy (in use or exclusively locked) — free it and retry", device)
		}
		return fmt.Errorf("serial: open %s: %w", device, err)
	}

	// Assert DTR so the TNC knows the host is present.
	// DTR is never toggled on close: dropping DTR resets some TNCs (HUPCL lesson).
	// Non-fatal: PTY devices and some USB adapters do not support modem control
	// signals and return ENOTTY ("inappropriate ioctl for device"). The Python
	// reference implementation (kiss3/pyserial) never sets DTR explicitly, so
	// matching that behaviour: log a warning and continue.
	if err := port.SetDTR(true); err != nil {
		log.Printf("serial: SetDTR not supported on %s (non-fatal): %v", s.cfg.Device, err)
	}
	// RTS polarity depends on the interface. With hardware flow control the TNC
	// needs RTS asserted (host ready); CRTSCTS below then manages it. Without
	// flow control, hold RTS low — some interfaces (e.g. Digirig) wire RTS to
	// PTT, so asserting it on open would key the transmitter.
	rts := s.cfg.RTSCTS
	if err := port.SetRTS(rts); err != nil {
		log.Printf("serial: SetRTS not supported on %s (non-fatal): %v", s.cfg.Device, err)
	}

	// Set a 100ms read timeout so that probe reads return promptly when the TNC
	// is already in KISS mode and sends nothing after the probe CR.  Without a
	// timeout, Read blocks forever (the go.bug.st/serial default), which hangs
	// EnterKISS on TNCs that never leave KISS (e.g. Direwolf, Mobilinkd between
	// sessions).  100ms is long enough to read any real response; the 1s
	// probeWait sleep before each read gives the TNC its full response window.
	//
	// The same timeout applies to the readerLoop in port.go: Read returns
	// (0, nil) periodically on an idle line.  The loop already handles (0, nil)
	// correctly — it just loops back without treating it as an error — so no
	// wrapper is needed.
	if err := port.SetReadTimeout(100 * time.Millisecond); err != nil {
		_ = port.Close()
		return fmt.Errorf("serial: SetReadTimeout: %w", err)
	}

	// Enable hardware (RTS/CTS) flow control if requested. Must come after
	// SetReadTimeout (the library's last termios write) so it isn't clobbered.
	// Non-fatal: a device without flow-control lines still works without it.
	if s.cfg.RTSCTS {
		if err := applyRTSCTS(port); err != nil {
			log.Printf("serial: WARNING: could not enable RTSCTS on %s (non-fatal): %v", s.cfg.Device, err)
		} else {
			log.Printf("serial: hardware flow control (RTSCTS) enabled on %s", s.cfg.Device)
		}
	}

	s.rw = port.(io.ReadWriteCloser)
	if gp, ok := port.(goserial.Port); ok {
		// go.bug.st/serial Port.Drain() waits for all transmit bytes to be sent.
		// Use it as the flush function for the real port.
		s.flush = gp.Drain
	}
	s.resolved = device
	s.modem = port
	// Enable removal detection only for devices that answer a modem-status query.
	// A PTY / pipe (e.g. Direwolf) errors here and would be falsely flagged as
	// removed; those detect disconnect via read errors instead.
	if _, mserr := port.GetModemStatusBits(); mserr == nil {
		s.healthCheck = true
	}
	return nil
}

func (s *serialTransport) Read(b []byte) (int, error) {
	n, err := s.rw.Read(b)
	if err != nil || n > 0 {
		return n, err
	}
	// n == 0, err == nil: a read timeout on an idle line — but on Windows a
	// removed device also returns a timeout rather than an error. Rate-limited,
	// probe the open handle's modem status; if it errors the device is gone, so
	// surface it as a read error and let the reader take the port offline (the
	// bridge then reconnects and re-resolves a usb: reference).
	if s.healthCheck && time.Since(s.lastHealth) >= s.healthInterval {
		s.lastHealth = time.Now()
		if _, mserr := s.modem.GetModemStatusBits(); mserr != nil {
			return 0, fmt.Errorf("serial: %s appears removed: %w", s.resolved, mserr)
		}
	}
	return 0, nil
}

func (s *serialTransport) Write(b []byte) (int, error) {
	return s.rw.Write(b)
}

// Close closes the underlying serial port.
// DTR is NOT toggled before close: a DTR drop can reset some TNCs.
func (s *serialTransport) Close() error {
	if s.rw != nil {
		return s.rw.Close()
	}
	return nil
}

// EnterKISS sends the InitString command sequence to put the TNC into KISS
// mode, mirroring tncd.py:652–677.
//
// If InitString is empty, EnterKISS is a no-op (TNC assumed already in KISS).
// The probe is retried up to 3 times (spaced by InitDelay) to handle TNCs
// that are still rebooting after RESET. On command-mode detection, the init is
// sent and verified. On silence across all retries (ambiguous: already-KISS or
// wedged TNC), the init is sent anyway with a WARNING log. On non-command
// bytes (already-KISS echo), nothing is done.
func (s *serialTransport) EnterKISS() error {
	if s.cfg.InitString == "" {
		return nil
	}

	// Probe with retries: a TNC rebooting after RESET may not answer immediately.
	const probeAttempts = 3
	inCmd, sawBytes := false, false
	for i := 0; i < probeAttempts; i++ {
		c, saw := s.probeCommandMode()
		if saw {
			sawBytes = true
		}
		if c {
			inCmd = true
			break
		}
		if i < probeAttempts-1 {
			time.Sleep(s.cfg.InitDelay)
		}
	}

	if inCmd {
		if err := s.sendInit(); err != nil {
			return err
		}
		if c, _ := s.probeCommandMode(); c {
			return fmt.Errorf("serial: TNC still in command mode after init_string — " +
				"check that the init commands are correct for this TNC")
		}
		log.Printf("serial: TNC confirmed in KISS mode after init")
		return nil
	}

	if sawBytes {
		// Saw only KISS framing/non-printable bytes → already in KISS. Nothing to do.
		return nil
	}

	// Silence across all retries: ambiguous (already-KISS-but-silent, or a
	// wedged/mis-configured TNC). Send the init anyway — it is harmless if the
	// TNC is already in KISS (non-C0 bytes are ignored) and rescues one that is
	// stuck — then warn honestly rather than claiming success.
	if err := s.sendInit(); err != nil {
		return err
	}
	log.Printf("serial: WARNING: could not confirm KISS entry on %s (no probe response); "+
		"normal if the TNC was already in KISS, but if it does not transmit check "+
		"baud rate, flow control (rtscts), and cabling", s.cfg.Device)
	return nil
}

// sendInit writes the init_string command lines (split on the literal \n escape).
func (s *serialTransport) sendInit() error {
	lines := strings.Split(s.cfg.InitString, `\n`)
	for _, line := range lines {
		cmd := resolveEscapes(line)
		log.Printf("serial: TNC init: %q", line)
		if _, err := s.rw.Write([]byte(cmd)); err != nil {
			return fmt.Errorf("serial: writing init line: %w", err)
		}
		time.Sleep(s.cfg.InitDelay)
	}
	return nil
}

// ExitKISS sends the KISS exit byte sequence and optional host exit string,
// mirroring tncd.py:907–932.
//
// If SendKISSExit is true, the standard KISS exit (C0 FF C0) is written and
// flushed. If HostExitString is set, ExitDelay is slept, then each line of
// the host exit string is written with ExitDelay between lines.
func (s *serialTransport) ExitKISS() {
	if s.rw == nil {
		return
	}

	if s.cfg.SendKISSExit {
		log.Printf("serial: sending KISS exit (C0 FF C0)")
		_, _ = s.rw.Write([]byte{0xC0, 0xFF, 0xC0})
		if s.flush != nil {
			_ = s.flush()
		}
	}

	if s.cfg.HostExitString != "" {
		time.Sleep(s.cfg.ExitDelay)
		lines := strings.Split(s.cfg.HostExitString, `\n`)
		for _, line := range lines {
			cmd := resolveEscapes(line)
			log.Printf("serial: TNC host exit: %q", line)
			_, _ = s.rw.Write([]byte(cmd))
			time.Sleep(s.cfg.ExitDelay)
		}
	}
}

// probeCommandMode probes the TNC. Returns (inCmd, sawBytes): inCmd is true if
// the response is printable command-mode text; sawBytes is true if any bytes
// were read at all (used to tell silence from a KISS-mode echo).
//
// The probe works by flushing the input buffer, sending a bare CR, waiting
// probeWait (default 1s), then reading whatever is available. KISS framing
// bytes (0xC0) and NUL bytes are stripped. If the remaining bytes are all
// printable ASCII (0x20–0x7E) or CR/LF, and non-empty, the TNC is in command
// mode.
func (s *serialTransport) probeCommandMode() (inCmd, sawBytes bool) {
	// Drain any pending input by reading with a short timeout.
	// We use a fixed-size read of up to 256 bytes; the fake serial's Read
	// returns 0,io.EOF on empty — that is fine.
	drain := make([]byte, 256)
	for {
		n, _ := s.rw.Read(drain)
		if n == 0 {
			break
		}
	}

	// Send probe CR.
	_, _ = s.rw.Write([]byte("\r"))

	// Wait for TNC to respond.
	time.Sleep(s.probeWait)

	// Read whatever is available.
	buf := make([]byte, 1024)
	n, _ := s.rw.Read(buf)
	resp := buf[:n]

	log.Printf("serial: TNC probe raw response: %q", resp)

	if len(resp) == 0 {
		return false, false
	}

	// Strip KISS FENDs (0xC0) and NUL bytes — a TNC already in KISS mode may
	// echo framing bytes but not printable ASCII (tncd.py:638).
	filtered := resp[:0]
	for _, b := range resp {
		if b != 0xC0 && b != 0x00 {
			filtered = append(filtered, b)
		}
	}
	// Strip leading/trailing whitespace-equivalent (bytes.TrimSpace includes CR/LF).
	// Python does .strip() which strips whitespace including CR/LF.
	filtered = trimSpace(filtered)

	if len(filtered) == 0 {
		return false, true // saw only KISS framing/NULs → already in KISS
	}

	// All remaining bytes must be printable ASCII or CR/LF.
	for _, b := range filtered {
		if !((b >= 0x20 && b < 0x7F) || b == 0x0A || b == 0x0D) {
			return false, true // non-printable → treat as KISS
		}
	}

	log.Printf("serial: TNC probe: command-mode response %q", filtered)
	return true, true
}

// resolveEscapes converts literal two-character backslash escape sequences
// within a single line to their byte values, mirroring tncd.py:664 and :927.
// Only \r → CR and \n → LF are handled (matching the Python implementation).
func resolveEscapes(s string) string {
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\n`, "\n")
	return s
}

// trimSpace removes leading and trailing bytes that Python's str.strip()
// would remove: space (0x20), tab (0x09), CR (0x0D), LF (0x0A), form feed
// (0x0C), vertical tab (0x0B).
func trimSpace(b []byte) []byte {
	isSpace := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'
	}
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

// parseParity converts the config string ("N", "E", "O") to go.bug.st/serial
// Parity values.
func parseParity(p string) (goserial.Parity, error) {
	switch strings.ToUpper(p) {
	case "", "N":
		return goserial.NoParity, nil
	case "E":
		return goserial.EvenParity, nil
	case "O":
		return goserial.OddParity, nil
	default:
		return goserial.NoParity, fmt.Errorf("unknown parity %q (use N, E, or O)", p)
	}
}

// parseStopBits converts the config float64 (1, 1.5, 2) to go.bug.st/serial
// StopBits values.
func parseStopBits(sb float64) (goserial.StopBits, error) {
	switch sb {
	case 0, 1:
		return goserial.OneStopBit, nil
	case 1.5:
		return goserial.OnePointFiveStopBits, nil
	case 2:
		return goserial.TwoStopBits, nil
	default:
		return goserial.OneStopBit, fmt.Errorf("unknown stop bits %v (use 1, 1.5, or 2)", sb)
	}
}
