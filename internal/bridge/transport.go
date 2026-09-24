package bridge

import (
	"fmt"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/ports"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// BuildTransport is buildTransport exported for cmd/tncd's one-shot "rig"
// CLI, which needs to open a port's transport outside the running bridge.
// It is a thin wrapper, not a reimplementation, so the bridge stays the
// single source of truth for how a config.Port becomes a kiss.Transport --
// the CLI and the running bridge must never disagree about that.
func BuildTransport(pc config.Port) (kiss.Transport, error) {
	return buildTransport(pc)
}

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
			InitString:     pc.InitString,
			InitDelay:      time.Duration(pc.InitDelay * float64(time.Second)),
			SendKISSExit:   pc.SendKISSExit,
			HostExitString: pc.HostExitString,
			ExitDelay:      time.Duration(pc.ExitDelay * float64(time.Second)),
			Resolve:        ports.Resolve,
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
	case "ble":
		return kiss.NewBLETransport(kiss.BLEConfig{BDAddr: pc.BDAddr}), nil
	default:
		return nil, fmt.Errorf("unknown port type %q", pc.Type)
	}
}
