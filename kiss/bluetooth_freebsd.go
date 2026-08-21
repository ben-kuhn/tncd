//go:build freebsd

package kiss

import (
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FreeBSD Bluetooth SPP transport over the native netgraph RFCOMM socket
// (AF_BLUETOOTH / BLUETOOTH_PROTO_RFCOMM). No BlueZ/D-Bus (Linux) or Winsock
// (Windows): FreeBSD exposes RFCOMM and L2CAP as raw sockets, so this mirrors
// the Windows raw-socket transport rather than the Linux D-Bus flow. The RFCOMM
// channel is discovered via SDP (see bluetooth_sdp.go) unless the [client]
// "channel" key pins it.
//
// Host prerequisites (FreeBSD-side): the Bluetooth stack must be up —
// `kldload ng_ubt` for a USB dongle, `service bluetooth start ubt0`,
// `service sdpd onestart` for SDP, and the device paired via hcsecd.
const (
	afBluetooth   = 36  // AF_BLUETOOTH (sys/socket.h)
	btProtoL2CAP  = 135 // BLUETOOTH_PROTO_L2CAP (ng_btsocket.h)
	btProtoRFCOMM = 136 // BLUETOOTH_PROTO_RFCOMM (ng_btsocket.h)
	sdpPSM        = 0x0001
)

// bluetoothTransport is a FreeBSD RFCOMM SPP transport.
type bluetoothTransport struct {
	cfg  BluetoothConfig
	mu   sync.Mutex
	fd   int
	open bool
}

// NewBluetoothTransport returns a FreeBSD Bluetooth SPP transport. It connects
// to cfg.BDAddr; the RFCOMM channel comes from SDP discovery unless cfg.Channel
// pins it. The device must already be paired (hcsecd).
func NewBluetoothTransport(cfg BluetoothConfig) Transport {
	return &bluetoothTransport{cfg: cfg, fd: -1}
}

func (bt *bluetoothTransport) Open() error {
	addr, err := parseBTAddrLE(bt.cfg.BDAddr)
	if err != nil {
		return err
	}

	// Channel: an explicit config value pins it; otherwise discover via SDP.
	channel := 0
	if s := strings.TrimSpace(bt.cfg.Channel); s != "" {
		ch, err := strconv.Atoi(s)
		if err != nil || ch < 1 || ch > 30 {
			return fmt.Errorf("bluetooth: invalid channel %q (want 1-30)", bt.cfg.Channel)
		}
		channel = ch
	} else {
		ch, err := sdpDiscoverSPPChannel(addr)
		if err != nil {
			return fmt.Errorf("bluetooth: SDP discovery for %s: %w", bt.cfg.BDAddr, err)
		}
		channel = ch
	}

	fd, err := unix.Socket(afBluetooth, unix.SOCK_STREAM, btProtoRFCOMM)
	if err != nil {
		return fmt.Errorf("bluetooth: RFCOMM socket: %w", err)
	}

	// struct sockaddr_rfcomm { u_char len; u_char family; bdaddr_t bdaddr[6];
	//                          u_int8_t channel; }  — 9 bytes, bdaddr LE.
	sa := make([]byte, 9)
	sa[0] = 9
	sa[1] = afBluetooth
	copy(sa[2:8], addr[:])
	sa[8] = byte(channel)
	if err := connectRaw(fd, sa); err != nil {
		unix.Close(fd)
		return fmt.Errorf("bluetooth: connect %s (channel %d): %w", bt.cfg.BDAddr, channel, err)
	}

	bt.mu.Lock()
	bt.fd = fd
	bt.open = true
	bt.mu.Unlock()
	return nil
}

func (bt *bluetoothTransport) Read(p []byte) (int, error) {
	n, err := unix.Read(bt.fd, p)
	if n == 0 && err == nil {
		return 0, io.EOF
	}
	return n, err
}

func (bt *bluetoothTransport) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return unix.Write(bt.fd, p)
}

func (bt *bluetoothTransport) Close() error {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.open {
		unix.Close(bt.fd)
		bt.fd = -1
		bt.open = false
	}
	return nil
}

func (bt *bluetoothTransport) EnterKISS() error { return nil }
func (bt *bluetoothTransport) ExitKISS()        {}

// connectRaw issues a blocking connect() with a hand-built sockaddr, retrying on
// EINTR. Go's runtime preemption (SIGURG) interrupts blocking syscalls, and the
// raw connect — unlike unix.Connect — doesn't retry on its own.
func connectRaw(fd int, sa []byte) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_CONNECT, uintptr(fd),
			uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa)))
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

// sdpDiscoverSPPChannel opens an L2CAP socket to the device's SDP PSM, asks for
// the SPP service's ProtocolDescriptorList, and returns the RFCOMM channel.
func sdpDiscoverSPPChannel(addr [6]byte) (int, error) {
	fd, err := unix.Socket(afBluetooth, unix.SOCK_SEQPACKET, btProtoL2CAP)
	if err != nil {
		return 0, fmt.Errorf("l2cap socket: %w", err)
	}
	defer unix.Close(fd)

	// struct sockaddr_l2cap { u_char len; u_char family; u_int16_t psm;
	//   bdaddr_t bdaddr[6]; u_int16_t cid; u_int8_t bdaddr_type; } — 14 bytes.
	sa := make([]byte, 14)
	sa[0] = 14
	sa[1] = afBluetooth
	binary.LittleEndian.PutUint16(sa[2:4], sdpPSM) // PSM is host-order
	copy(sa[4:10], addr[:])
	if err := connectRaw(fd, sa); err != nil {
		return 0, fmt.Errorf("connect SDP PSM: %w", err)
	}

	if _, err := unix.Write(fd, buildSSAReq()); err != nil {
		return 0, fmt.Errorf("sdp request write: %w", err)
	}
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return 0, fmt.Errorf("sdp response read: %w", err)
	}
	return parseRFCOMMChannel(buf[:n])
}
