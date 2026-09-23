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

// benshiControlServiceClassID is the Benshi rig-control service UUID
// (00001100-d102-11e1-9b23-00025b00a5a5), captured live off the radio's
// classic SDP records -- the same UUID the device advertises for its BLE
// GATT control service, just reachable over RFCOMM here instead of GATT.
// Passing it as the connect ServiceClassId resolves the RFCOMM channel via
// SDP exactly like sppServiceClassID does for the data link.
var benshiControlServiceClassID = windows.GUID{
	Data1: 0x00001100,
	Data2: 0xd102,
	Data3: 0x11e1,
	Data4: [8]byte{0x9b, 0x23, 0x00, 0x02, 0x5b, 0x00, 0xa5, 0xa5},
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

	// Deliberately NOT setting SO_SNDBUF=0 here.
	//
	// It used to be set, to stop Winsock copying each send into a stack-level
	// buffer that completes immediately: frames "sent" over ~20 minutes stayed
	// queued and then flushed at once when the radio next received something,
	// while tncd logged every one as transmitted. That symptom has since been
	// root-caused to the radio buffering frames in its own TX queue, which no
	// socket option can affect — so the setting was addressing a problem it
	// never had jurisdiction over.
	//
	// Meanwhile Winsock wants buffer sizes set before connect, and applying
	// SO_SNDBUF=0 to an already-connected RFCOMM socket left the send path in
	// a state where the first WSASend reached the air and every later one was
	// accepted, reported as fully sent, and silently discarded.
	//
	// SO_SNDTIMEO below still bounds a genuinely stuck send.

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
	// Send until the whole frame is gone.
	//
	// SO_SNDBUF=0 means Winsock does no buffering of its own: WSASend hands
	// bytes straight to the transport and is therefore free to accept fewer
	// than offered when the peer is slow to grant RFCOMM credits. io.Writer
	// requires that a short write be reported as an error, and callers rely on
	// it — kiss.port writes a whole KISS frame and checks only the error. So
	// returning (sent, nil) after a partial send truncated the frame mid-KISS:
	// the TNC dropped the fragment, the stream desynced, and nothing reached
	// the air while every counter still said "transmitted". Observed on a
	// UV-PRO as exactly one frame per SPP link; a Mobilinkd TNC4 drains fast
	// enough that it never short-wrote and so never showed the bug. Linux is
	// unaffected because os.File.Write already loops.
	var total int
	for total < len(p) {
		chunk := p[total:]
		buf := windows.WSABuf{Len: uint32(len(chunk)), Buf: &chunk[0]}
		var sent uint32
		if err := windows.WSASend(bt.fd, &buf, 1, &sent, 0, nil, nil); err != nil {
			// WSAETIMEDOUT here is SO_SNDTIMEO expiring, not a connect failure
			// — but Windows renders it as "the connected party did not
			// properly respond", which reads like one. Say which layer
			// actually stalled so the log points at the radio's TX path
			// instead of the link setup.
			if err == windows.WSAETIMEDOUT {
				return total, fmt.Errorf("bluetooth: TX stalled -- %s accepted %d of %d bytes within %s",
					bt.cfg.BDAddr, total, len(p), btSendTimeout)
			}
			return total, err
		}
		if sent == 0 {
			return total, fmt.Errorf("bluetooth: TX stalled -- %s accepted %d of %d bytes then stopped",
				bt.cfg.BDAddr, total, len(p))
		}
		if int(sent) < len(chunk) {
			// Rare and diagnostic: this is the condition that silently
			// corrupted frames before the loop existed.
			log.Printf("bluetooth: partial send to %s (%d of %d bytes, %d/%d of frame) -- continuing",
				bt.cfg.BDAddr, sent, len(chunk), total+int(sent), len(p))
		}
		total += int(sent)
	}
	return total, nil
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

// ControlChannel opens a second, independent RFCOMM connection to the Benshi
// rig-control service, alongside (not replacing) the KISS data socket. Unlike
// Linux -- where BlueZ resolves a Profile1 UUID to a channel on its own --
// raw Winsock RFCOMM always connects to a channel number. cfg.ControlChannel
// pins one directly, skipping a second SDP round trip to the radio;
// otherwise the control service's 128-bit UUID drives discovery the same way
// the SPP UUID does for the data link in Open.
func (bt *bluetoothTransport) ControlChannel() (io.ReadWriteCloser, error) {
	addr, err := parseBTAddr(bt.cfg.BDAddr)
	if err != nil {
		return nil, fmt.Errorf("bluetooth: %w", err)
	}

	sa := &windows.SockaddrBth{BtAddr: addr}
	route := "SDP lookup"
	if bt.cfg.ControlChannel != 0 {
		sa.Port = uint32(bt.cfg.ControlChannel)
		route = fmt.Sprintf("pinned control channel %d", bt.cfg.ControlChannel)
	} else {
		sa.ServiceClassId = benshiControlServiceClassID
	}

	fd, err := bt.dialControl(sa, route)
	if err != nil {
		// WSASERVICE_NOT_FOUND from an SDP-driven connect means the radio has
		// no matching record -- i.e. no control channel exists to find, which
		// is exactly what ErrNoControlChannel means to callers. Any other
		// failure (timeout, refused, ...) is a real error worth surfacing.
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == wsaServiceNotFound {
			return nil, ErrNoControlChannel
		}
		return nil, err
	}

	timeoutMS := int(btSendTimeout / time.Millisecond)
	if err := windows.SetsockoptInt(fd, windows.SOL_SOCKET, soSndTimeo, timeoutMS); err != nil {
		log.Printf("bluetooth: control channel SO_SNDTIMEO=%dms failed (%v); a stalled send may block indefinitely", timeoutMS, err)
	}

	return &bluetoothControlConn{fd: fd, bdaddr: bt.cfg.BDAddr}, nil
}

// dialControl makes a single connect() attempt for the control channel.
//
// Unlike dial (used for the KISS data link), this does not retry: the data
// link's retry cushion exists because losing it costs a full bridge backoff
// cycle with no packet traffic at all, while a control-channel connect
// failure just means one rigctl request answers with an error and the next
// one tries again. A single attempt also keeps the error unwrapped with %w
// (dial's retry loop renders the last error as prose via describeWSAError,
// which loses the underlying syscall.Errno that ControlChannel needs to
// detect WSASERVICE_NOT_FOUND).
func (bt *bluetoothTransport) dialControl(sa *windows.SockaddrBth, route string) (windows.Handle, error) {
	fd, err := windows.Socket(windows.AF_BTH, windows.SOCK_STREAM, windows.BTHPROTO_RFCOMM)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("bluetooth: control channel socket: %w", err)
	}
	if err := windows.Connect(fd, sa); err != nil {
		windows.Closesocket(fd)
		return windows.InvalidHandle, fmt.Errorf("bluetooth: control channel connect to %s via %s: %s: %w",
			bt.cfg.BDAddr, route, describeWSAError(err), err)
	}
	log.Printf("bluetooth: control channel connected to %s via %s", bt.cfg.BDAddr, route)
	return fd, nil
}

// bluetoothControlConn is the rig-control RFCOMM socket. It has its own fd
// and lifecycle, entirely independent of bluetoothTransport's KISS data
// socket -- the two links are opened, read, written, and closed separately,
// which is the point of a *second* channel.
type bluetoothControlConn struct {
	fd     windows.Handle
	bdaddr string
}

func (c *bluetoothControlConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var recvd, flags uint32
	if err := windows.WSARecv(c.fd, &buf, 1, &recvd, &flags, nil, nil); err != nil {
		return 0, err
	}
	if recvd == 0 {
		return 0, io.EOF
	}
	return int(recvd), nil
}

// Write loops until the whole buffer is sent, for the same reason
// bluetoothTransport.Write does: a short write reported as success would
// silently truncate a command to the radio.
func (c *bluetoothControlConn) Write(p []byte) (int, error) {
	var total int
	for total < len(p) {
		chunk := p[total:]
		buf := windows.WSABuf{Len: uint32(len(chunk)), Buf: &chunk[0]}
		var sent uint32
		if err := windows.WSASend(c.fd, &buf, 1, &sent, 0, nil, nil); err != nil {
			if err == windows.WSAETIMEDOUT {
				return total, fmt.Errorf("bluetooth: control channel TX stalled -- %s accepted %d of %d bytes within %s",
					c.bdaddr, total, len(p), btSendTimeout)
			}
			return total, err
		}
		if sent == 0 {
			return total, fmt.Errorf("bluetooth: control channel TX stalled -- %s accepted %d of %d bytes then stopped",
				c.bdaddr, total, len(p))
		}
		total += int(sent)
	}
	return total, nil
}

func (c *bluetoothControlConn) Close() error {
	if c.fd != windows.InvalidHandle {
		windows.Closesocket(c.fd)
		c.fd = windows.InvalidHandle
	}
	return nil
}

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
