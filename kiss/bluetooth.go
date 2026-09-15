package kiss

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BluetoothConfig holds the configuration for a Bluetooth SPP KISS transport.
// It is shared by all platform implementations (Linux BlueZ, Windows Winsock
// RFCOMM, and the unsupported-platform stub).
type BluetoothConfig struct {
	BDAddr string
	// Channel pins the RFCOMM server channel; empty means discover it. It is
	// honoured on Windows and FreeBSD, which connect to a channel number and
	// otherwise have to resolve one via SDP first. Linux ignores it: BlueZ is
	// asked for the SPP profile UUID and does its own resolution.
	Channel           string
	Reconnect         bool
	ReconnectDelay    time.Duration
	ReconnectMaxDelay time.Duration
}

// parseSPPChannel interprets the optional `channel` config value.
//
// An empty value means "resolve the channel via SDP" (pinned=false). A set
// value pins the RFCOMM server channel and skips discovery, which matters
// because SDP resolution is a second round-trip to the radio during connect
// and therefore a second thing that can fail. Radios publish a stable channel
// (Mobilinkd TNC4 = 1, TNC3 = 6), so pinning is the reliable path when the
// number is known.
//
// Valid channels are 1-30 per the RFCOMM spec.
func parseSPPChannel(s string) (channel int, pinned bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, nil
	}
	ch, err := strconv.Atoi(s)
	if err != nil || ch < 1 || ch > 30 {
		return 0, false, fmt.Errorf("bluetooth: invalid channel %q (want 1-30)", s)
	}
	return ch, true, nil
}
