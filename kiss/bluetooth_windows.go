//go:build windows

package kiss

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// sppServiceClassID is the Bluetooth Serial Port Profile UUID
// {00001101-0000-1000-8000-00805F9B34FB}. Passing it as the connect
// ServiceClassId makes Windows resolve the RFCOMM channel via SDP — the
// equivalent of the Linux path's SDP auto-detect.
var sppServiceClassID = windows.GUID{
	Data1: 0x00001101,
	Data2: 0x0000,
	Data3: 0x1000,
	Data4: [8]byte{0x80, 0x00, 0x00, 0x80, 0x5F, 0x9B, 0x34, 0xFB},
}

// soSndTimeo is Winsock's SO_SNDTIMEO. x/sys/windows defines SO_RCVTIMEO and
// SO_SNDBUF but not this one.
const soSndTimeo = 0x1005

// btSendTimeout bounds a single unbuffered send on the SPP socket. It must be
// comfortably longer than a normal KISS write (microseconds) yet short enough
// that a wedged TX path is reported while the link is still worth reconnecting.
const btSendTimeout = 10 * time.Second

// Connect retry policy.
//
// BlueZ absorbs transient connect failures internally (InProgress,
// br-connection-busy) and only reports once it has genuinely given up, so the
// Linux path effectively retries for free. A raw Winsock connect() has no such
// cushion: one refused page surfaces immediately and, without this loop, costs
// a full bridge backoff cycle (5s growing to 60s) before the next attempt.
//
// Field observation this is sized against: a UV-PRO refused connect() with
// WSAENETUNREACH for ~95 minutes straight and then succeeded on an ordinary
// retry with nothing else changed. Short in-Open retries turn that class of
// transient refusal into a brief hiccup instead of minutes of dead air.
const (
	btConnectAttempts   = 3
	btConnectRetryDelay = 2 * time.Second
)

// wsaErrorNames renders the Winsock failures a Bluetooth connect actually
// produces. The default rendering is a prose sentence that is easy to misread
// — WSAETIMEDOUT in particular says "the connected party did not properly
// respond", which looks like an application-level timeout rather than a page
// timeout — and prose cannot be grepped or compared across runs. Logging
// NAME (number) keeps failures unambiguous and diffable.
var wsaErrorNames = map[syscall.Errno]string{
	windows.WSAENETUNREACH:  "WSAENETUNREACH",
	windows.WSAETIMEDOUT:    "WSAETIMEDOUT",
	windows.WSAECONNREFUSED: "WSAECONNREFUSED",
	windows.WSAEHOSTDOWN:    "WSAEHOSTDOWN",
	windows.WSAEHOSTUNREACH: "WSAEHOSTUNREACH",
	windows.WSAEINVAL:       "WSAEINVAL",
	windows.WSAEACCES:       "WSAEACCES",
	wsaServiceNotFound:      "WSASERVICE_NOT_FOUND",
}

// wsaServiceNotFound is WSASERVICE_NOT_FOUND, returned when an SDP service
// search finds no matching record on the remote device. x/sys/windows does not
// define it. It is the specific failure that pinning `channel` avoids.
const wsaServiceNotFound = syscall.Errno(10108)

// describeWSAError renders err as "NAME (number): prose", falling back to the
// bare error when it is not a Winsock errno.
func describeWSAError(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	if name, ok := wsaErrorNames[errno]; ok {
		return fmt.Sprintf("%s (%d): %v", name, uint32(errno), err)
	}
	return fmt.Sprintf("winsock error %d: %v", uint32(errno), err)
}

// bluetoothTransport is a Windows Bluetooth SPP transport over Winsock RFCOMM.
type bluetoothTransport struct {
	cfg     BluetoothConfig
	mu      sync.Mutex
	fd      windows.Handle
	open    bool
	started bool // WSAStartup succeeded and needs WSACleanup
}

// NewBluetoothTransport returns a Windows Bluetooth SPP transport. It connects
// to cfg.BDAddr, using cfg.Channel as the RFCOMM channel when set and otherwise
// resolving one from the SPP service UUID via SDP. The device must already be
// paired in Windows.
func NewBluetoothTransport(cfg BluetoothConfig) Transport {
	return &bluetoothTransport{cfg: cfg, fd: windows.InvalidHandle}
}

func (bt *bluetoothTransport) Open() error {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	addr, err := parseBTAddr(bt.cfg.BDAddr)
	if err != nil {
		return fmt.Errorf("bluetooth: %w", err)
	}

	var wsad windows.WSAData
	if err := windows.WSAStartup(0x202, &wsad); err != nil { // MAKEWORD(2,2)
		return fmt.Errorf("bluetooth: WSAStartup: %w", err)
	}
	bt.started = true

	// Channel: an explicit config value pins it and skips the SDP lookup;
	// otherwise the SPP UUID drives discovery inside connect().
	channel, pinned, err := parseSPPChannel(bt.cfg.Channel)
	if err != nil {
		windows.WSACleanup()
		bt.started = false
		return err
	}
	sa := &windows.SockaddrBth{BtAddr: addr}
	route := "SDP lookup"
	if pinned {
		sa.Port = uint32(channel)
		route = fmt.Sprintf("pinned channel %d", channel)
	} else {
		sa.ServiceClassId = sppServiceClassID // Port 0 + UUID => SDP channel lookup
	}

	fd, err := bt.dial(sa, route)
	if err != nil {
		windows.WSACleanup()
		bt.started = false
		return err
	}

	// Make sends report reality instead of succeeding into a buffer that may
	// never drain.
	//
	// By default Winsock copies each send into a stack-level send buffer and
	// completes immediately, so WSASend returns success even when RFCOMM
	// flow control (credit starvation) means the bytes never reach the radio.
	// Observed on-air: frames "sent" over ~20 minutes stayed queued and then
	// all flushed at once the instant the radio transmitted something inbound
	// — while tncd had logged every one of them as transmitted.
	//
	// SO_SNDBUF=0 disables that intermediate buffering (each send goes
	// straight to the transport), and SO_SNDTIMEO bounds how long a send may
	// block. A stalled TX path then surfaces as a real write error, which
	// takes the port offline for a reconnect instead of silently swallowing
	// traffic. KISS frames are small and infrequent, so unbuffered sends cost
	// nothing here.
	if err := windows.SetsockoptInt(fd, windows.SOL_SOCKET, windows.SO_SNDBUF, 0); err != nil {
		log.Printf("bluetooth: SO_SNDBUF=0 failed (%v); sends may buffer silently", err)
	}
	// Winsock takes SO_SNDTIMEO as a DWORD of milliseconds, not the BSD
	// struct timeval. x/sys/windows has no timeval path to offer here —
	// SetsockoptTimeval is a stub that unconditionally returns EWINDOWS — so
	// setting it that way silently left every Windows build with no send
	// timeout at all. SetsockoptInt passes the DWORD the API actually wants.
	timeoutMS := int(btSendTimeout / time.Millisecond)
	if err := windows.SetsockoptInt(fd, windows.SOL_SOCKET, soSndTimeo, timeoutMS); err != nil {
		log.Printf("bluetooth: SO_SNDTIMEO=%dms failed (%v); a stalled send may block indefinitely", timeoutMS, err)
	}

	bt.fd = fd
	bt.open = true
	return nil
}

// dial makes up to btConnectAttempts connect() calls, returning the connected
// socket or the last failure.
//
// Every attempt is logged with its duration and a decoded Winsock error. That
// instrumentation is the point: connect failures here have been intermittent
// and self-resolving, so the question a log has to answer is "which call
// failed, how long did it block, and with exactly which errno" — for example
// WSASERVICE_NOT_FOUND (SDP found no SPP record, so pin `channel`) versus
// WSAETIMEDOUT (the radio never answered the page) versus WSAENETUNREACH.
// Without the numbers those cases are indistinguishable prose.
func (bt *bluetoothTransport) dial(sa *windows.SockaddrBth, route string) (windows.Handle, error) {
	var lastErr error
	started := time.Now()

	for attempt := 1; attempt <= btConnectAttempts; attempt++ {
		fd, err := windows.Socket(windows.AF_BTH, windows.SOCK_STREAM, windows.BTHPROTO_RFCOMM)
		if err != nil {
			return windows.InvalidHandle, fmt.Errorf("bluetooth: socket: %w", err)
		}

		attemptStart := time.Now()
		err = windows.Connect(fd, sa)
		elapsed := time.Since(attemptStart)

		if err == nil {
			log.Printf("bluetooth: connected to %s via %s on attempt %d/%d (%s, %s total)",
				bt.cfg.BDAddr, route, attempt, btConnectAttempts,
				elapsed.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
			return fd, nil
		}

		windows.Closesocket(fd)
		lastErr = err
		log.Printf("bluetooth: connect attempt %d/%d to %s via %s failed after %s -- %s",
			attempt, btConnectAttempts, bt.cfg.BDAddr, route,
			elapsed.Round(time.Millisecond), describeWSAError(err))

		if attempt < btConnectAttempts {
			time.Sleep(btConnectRetryDelay)
		}
	}

	return windows.InvalidHandle, fmt.Errorf("bluetooth: connect %s via %s: %d attempts over %s, last error %s",
		bt.cfg.BDAddr, route, btConnectAttempts,
		time.Since(started).Round(time.Millisecond), describeWSAError(lastErr))
}

func (bt *bluetoothTransport) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var recvd, flags uint32
	if err := windows.WSARecv(bt.fd, &buf, 1, &recvd, &flags, nil, nil); err != nil {
		return 0, err
	}
	if recvd == 0 {
		return 0, io.EOF
	}
	return int(recvd), nil
}

func (bt *bluetoothTransport) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var sent uint32
	if err := windows.WSASend(bt.fd, &buf, 1, &sent, 0, nil, nil); err != nil {
		// WSAETIMEDOUT here is SO_SNDTIMEO expiring, not a connect failure —
		// but Windows renders it as "the connected party did not properly
		// respond", which reads like one. Say which layer actually stalled so
		// the log points at the radio's TX path instead of the link setup.
		if err == windows.WSAETIMEDOUT {
			return 0, fmt.Errorf("bluetooth: TX stalled -- %s did not accept %d bytes within %s",
				bt.cfg.BDAddr, len(p), btSendTimeout)
		}
		return 0, err
	}
	return int(sent), nil
}

// Close closes the socket (unblocking any in-flight WSARecv in the reader
// goroutine) and releases the Winsock refcount.
func (bt *bluetoothTransport) Close() error {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.open {
		windows.Closesocket(bt.fd)
		bt.fd = windows.InvalidHandle
		bt.open = false
	}
	if bt.started {
		windows.WSACleanup()
		bt.started = false
	}
	return nil
}

func (bt *bluetoothTransport) EnterKISS() error { return nil }
func (bt *bluetoothTransport) ExitKISS()        {}

// parseBTAddr parses "AA:BB:CC:DD:EE:FF" (colons or dashes, any case, or no
// separators) into a BTH_ADDR: the 48-bit address in the low 6 bytes of a
// uint64, with AA as the most-significant octet.
func parseBTAddr(s string) (uint64, error) {
	h := strings.ReplaceAll(s, ":", "")
	h = strings.ReplaceAll(h, "-", "")
	if len(h) != 12 {
		return 0, fmt.Errorf("invalid Bluetooth address %q (want AA:BB:CC:DD:EE:FF)", s)
	}
	v, err := strconv.ParseUint(h, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Bluetooth address %q: %w", s, err)
	}
	return v, nil
}
