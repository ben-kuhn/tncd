//go:build windows

package kiss

import (
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
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

// bluetoothTransport is a Windows Bluetooth SPP transport over Winsock RFCOMM.
type bluetoothTransport struct {
	cfg     BluetoothConfig
	mu      sync.Mutex
	fd      windows.Handle
	open    bool
	started bool // WSAStartup succeeded and needs WSACleanup
}

// NewBluetoothTransport returns a Windows Bluetooth SPP transport. It connects
// to cfg.BDAddr; the SPP service UUID drives SDP channel discovery, so
// cfg.Channel is informational only. The device must already be paired in
// Windows.
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

	fd, err := windows.Socket(windows.AF_BTH, windows.SOCK_STREAM, windows.BTHPROTO_RFCOMM)
	if err != nil {
		windows.WSACleanup()
		bt.started = false
		return fmt.Errorf("bluetooth: socket: %w", err)
	}

	sa := &windows.SockaddrBth{
		BtAddr:         addr,
		ServiceClassId: sppServiceClassID,
		Port:           0, // 0 + ServiceClassId => SDP channel lookup
	}
	if err := windows.Connect(fd, sa); err != nil {
		windows.Closesocket(fd)
		windows.WSACleanup()
		bt.started = false
		return fmt.Errorf("bluetooth: connect %s: %w", bt.cfg.BDAddr, err)
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
	tv := windows.Timeval{Sec: int32(btSendTimeout / time.Second)}
	if err := windows.SetsockoptTimeval(fd, windows.SOL_SOCKET, soSndTimeo, &tv); err != nil {
		log.Printf("bluetooth: SO_SNDTIMEO failed (%v); a stalled send may block indefinitely", err)
	}

	bt.fd = fd
	bt.open = true
	return nil
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
