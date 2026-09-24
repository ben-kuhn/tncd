package kiss

import (
	"errors"
	"io"
)

// ErrNoControlChannel reports a transport that cannot carry rig control.
var ErrNoControlChannel = errors.New("kiss: transport has no rig control channel")

// ErrControlChannelInUse reports that a Port's control channel has already
// been claimed by another caller. Only one rig-control consumer may be
// attached to a Port at a time: a second concurrent reader on the same
// demultiplexed stream would race the first for Gaia frames -- the exact
// hazard the demultiplexer in demux.go exists to prevent one layer down.
var ErrControlChannelInUse = errors.New("kiss: control channel already attached")

// ControlCapable is implemented by transports that can carry a rig-control
// channel alongside KISS data.
//
// The channel is byte-oriented on purpose: this package knows Bluetooth, the
// benshi package knows the protocol, and neither needs the other's details.
type ControlCapable interface {
	ControlChannel() (io.ReadWriteCloser, error)
}

// ControlChannelFor returns tr's rig-control channel, or ErrNoControlChannel if
// the transport does not support one.
func ControlChannelFor(tr Transport) (io.ReadWriteCloser, error) {
	cc, ok := tr.(ControlCapable)
	if !ok {
		return nil, ErrNoControlChannel
	}
	return cc.ControlChannel()
}
