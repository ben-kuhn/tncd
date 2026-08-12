//go:build windows

package ports

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// bluetoothDevices enumerates cached/paired Bluetooth devices via WSALookupService
// in the NS_BTH namespace, returning them as Kind=bluetooth Ports whose Device is
// the MAC address (what goes in the config's bdaddr). No administrator rights are
// required. Modeled on x/sys/windows' TestWSALookupService.
func bluetoothDevices() []Port {
	var wsad windows.WSAData
	if err := windows.WSAStartup(0x202, &wsad); err != nil { // MAKEWORD(2,2)
		return nil
	}
	defer windows.WSACleanup()

	flags := uint32(windows.LUP_CONTAINERS | windows.LUP_RETURN_NAME | windows.LUP_RETURN_ADDR)

	var qs windows.WSAQUERYSET
	qs.NameSpace = windows.NS_BTH
	qs.Size = uint32(unsafe.Sizeof(qs))

	var handle windows.Handle
	if err := windows.WSALookupServiceBegin(&qs, flags, &handle); err != nil {
		return nil // e.g. WSASERVICE_NOT_FOUND when there is no radio/cache
	}
	defer windows.WSALookupServiceEnd(handle)

	n := int32(unsafe.Sizeof(windows.WSAQUERYSET{}))
	buf := make([]byte, n)
	var out []Port
	for {
		q := (*windows.WSAQUERYSET)(unsafe.Pointer(&buf[0]))
		switch err := windows.WSALookupServiceNext(handle, flags, &n, q); err {
		case windows.WSA_E_NO_MORE, windows.WSAENOMORE:
			return out
		case windows.WSAEFAULT:
			buf = make([]byte, n) // buffer too small — grow and retry the same item
		case nil:
			name := windows.UTF16PtrToString(q.ServiceInstanceName)
			mac := ""
			if q.NumberOfCsAddrs > 0 && q.SaBuffer != nil && q.SaBuffer.RemoteAddr.Sockaddr != nil {
				bth := (*windows.RawSockaddrBth)(unsafe.Pointer(q.SaBuffer.RemoteAddr.Sockaddr))
				addr := *(*uint64)(unsafe.Pointer(&bth.BtAddr[0]))
				mac = formatBTMAC(addr)
			}
			if mac == "" {
				continue
			}
			label := name
			if label == "" {
				label = mac
			}
			out = append(out, Port{
				Ref:    mac,
				Label:  "Bluetooth: " + label,
				Kind:   KindBluetooth,
				Device: mac,
			})
		default:
			return out // unexpected error — return what we have
		}
	}
}

// formatBTMAC renders a BTH_ADDR (48-bit address in the low 6 bytes of a uint64,
// most-significant octet first) as "AA:BB:CC:DD:EE:FF".
func formatBTMAC(addr uint64) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
		byte(addr>>40), byte(addr>>32), byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr))
}
