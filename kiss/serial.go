package kiss

import (
	"fmt"
	"io"
	"log"
	"strings"
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
}

// serialTransport implements Transport for a serial (RS-232/USB) KISS TNC.
//
// The rw field is populated by Open() from the real go.bug.st/serial port and
// is used for all Read/Write/Close calls. The serialPort field holds the same
// port as a goserial.Port so that Open() can call SetReadTimeout — the Port
// interface exposes SetReadTimeout but io.ReadWriteCloser does not.
// Tests bypass Open() by setting rw and probeWait directly on the struct;
// serialPort remains nil in tests (SetReadTimeout is not called on the fake).
type serialTransport struct {
	cfg        SerialConfig
	rw         io.ReadWriteCloser
	serialPort goserial.Port // same object as rw, kept for SetReadTimeout
	flush      func() error
	probeWait  time.Duration // default 1s; overridden to milliseconds in tests
}

// NewSerialTransport returns a Transport backed by a serial port.
func NewSerialTransport(cfg SerialConfig) Transport {
	return &serialTransport{
		cfg:       cfg,
		probeWait: time.Second,
	}
}

// Open opens the serial port and asserts DTR/RTS.
//
// NOTE: go.bug.st/serial v1.8.0 Mode has no RTSCTS field; the library
// unconditionally disables hardware flow control (CRTSCTS) during open.
// If cfg.RTSCTS is true, a warning is logged and the setting is not applied.
// This is a known limitation of go.bug.st/serial v1.8.0; tracked as a concern.
func (s *serialTransport) Open() error {
	if s.cfg.RTSCTS {
		log.Printf("serial: WARNING: RTSCTS=true requested for %s but "+
			"go.bug.st/serial v1.8.0 does not support hardware flow control "+
			"via Mode; RTSCTS will NOT be applied", s.cfg.Device)
	}

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

	port, err := goserial.Open(s.cfg.Device, mode)
	if err != nil {
		return fmt.Errorf("serial: open %s: %w", s.cfg.Device, err)
	}

	// Assert DTR so the TNC knows the host is present.
	// DTR is never toggled on close: dropping DTR resets some TNCs (HUPCL lesson).
	if err := port.SetDTR(true); err != nil {
		_ = port.Close()
		return fmt.Errorf("serial: SetDTR: %w", err)
	}
	// Hold RTS low: some interfaces (e.g. Digirig) wire RTS to PTT.
	if err := port.SetRTS(false); err != nil {
		_ = port.Close()
		return fmt.Errorf("serial: SetRTS: %w", err)
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

	s.rw = port
	s.serialPort = port
	// go.bug.st/serial Port.Drain() waits for all transmit bytes to be sent.
	// Use it as the flush function for the real port.
	s.flush = port.Drain
	return nil
}

func (s *serialTransport) Read(b []byte) (int, error) {
	return s.rw.Read(b)
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
// If the TNC is not in command mode (probe returns no text), the init is
// skipped (TNC already in KISS). After sending all lines, the probe is run
// again; if the TNC is still in command mode, an error is returned.
func (s *serialTransport) EnterKISS() error {
	if s.cfg.InitString == "" {
		return nil
	}

	if !s.tnc_in_command_mode() {
		// Already in KISS mode — nothing to do.
		return nil
	}

	// Split on the literal two-character sequence backslash-n (the escape for
	// newline in init strings), mirroring tncd.py:663.
	lines := strings.Split(s.cfg.InitString, `\n`)
	for _, line := range lines {
		cmd := resolveEscapes(line)
		log.Printf("serial: TNC init: %q", line)
		if _, err := s.rw.Write([]byte(cmd)); err != nil {
			return fmt.Errorf("serial: writing init line: %w", err)
		}
		time.Sleep(s.cfg.InitDelay)
	}

	// Verify the TNC left command mode (tncd.py:669–673).
	if s.tnc_in_command_mode() {
		return fmt.Errorf("serial: TNC still in command mode after init_string — " +
			"check that the init commands are correct for this TNC")
	}
	log.Printf("serial: TNC confirmed in KISS mode after init")
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

// tnc_in_command_mode probes the TNC to determine if it is in command mode,
// mirroring tncd.py:623–643.
//
// The probe works by flushing the input buffer, sending a bare CR, waiting
// probeWait (default 1s), then reading whatever is available. KISS framing
// bytes (0xC0) and NUL bytes are stripped. If the remaining bytes are all
// printable ASCII (0x20–0x7E) or CR/LF, and non-empty, the TNC is in command
// mode.
func (s *serialTransport) tnc_in_command_mode() bool {
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
		log.Printf("serial: TNC probe: no response, assuming KISS mode")
		return false
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
		log.Printf("serial: TNC probe: no command-mode response after strip, assuming KISS mode")
		return false
	}

	// All remaining bytes must be printable ASCII or CR/LF.
	for _, b := range filtered {
		if !((b >= 0x20 && b < 0x7F) || b == 0x0A || b == 0x0D) {
			log.Printf("serial: TNC probe: non-printable byte 0x%02x, assuming KISS mode", b)
			return false
		}
	}

	log.Printf("serial: TNC probe: command-mode response %q", filtered)
	return true
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
