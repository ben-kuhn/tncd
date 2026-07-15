package bridge

import (
	"fmt"
	"strings"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// buildTransport constructs a kiss.Transport from a Port config entry.
// Mirrors the transport selection logic in tncd.py.
func buildTransport(pc config.Port) (kiss.Transport, error) {
	switch pc.Type {
	case "serial":
		return kiss.NewSerialTransport(kiss.SerialConfig{
			Device:         pc.Device,
			Baud:           pc.SerialBaudrate,
			Parity:         pc.Parity,
			StopBits:       pc.StopBits,
			RTSCTS:         pc.RTSCTS,
			InitString:     expandEscapes(pc.InitString),
			InitDelay:      time.Duration(pc.InitDelay * float64(time.Second)),
			SendKISSExit:   pc.SendKISSExit,
			HostExitString: expandEscapes(pc.HostExitString),
			ExitDelay:      time.Duration(pc.ExitDelay * float64(time.Second)),
		}), nil
	case "tcp":
		return kiss.NewTCPTransport(pc.Host, pc.TCPPort), nil
	case "bluetooth":
		return kiss.NewBluetoothTransport(kiss.BluetoothConfig{
			BDAddr:            pc.BDAddr,
			Channel:           pc.Channel,
			Reconnect:         pc.Reconnect,
			ReconnectDelay:    time.Duration(pc.ReconnectDelay * float64(time.Second)),
			ReconnectMaxDelay: time.Duration(pc.ReconnectMaxDelay * float64(time.Second)),
		}), nil
	default:
		return nil, fmt.Errorf("unknown port type %q", pc.Type)
	}
}

// expandEscapes converts literal \r and \n sequences to actual CR/LF bytes,
// mirroring tncd.py's init_string escape handling.
func expandEscapes(s string) string {
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\n`, "\n")
	return s
}
