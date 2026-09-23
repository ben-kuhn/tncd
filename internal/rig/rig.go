// Package rig drives a Benshi-protocol radio over a control channel.
//
// Everything here runs OFF the engine goroutine. A Benshi command is a
// round-trip to the radio with a timeout, and the engine owns all L2 state, so
// blocking it would stall AX.25 on every port for the duration.
package rig

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// ErrTimeout reports a radio that did not answer within the configured timeout.
var ErrTimeout = errors.New("rig: radio did not reply in time")

// ErrClosed reports use of a rig whose channel has been closed.
var ErrClosed = errors.New("rig: control channel closed")

// Rig is a request/response session with one radio.
//
// One request is in flight at a time: the radio is a single serial endpoint and
// replies carry no correlation id, so concurrent requests could not be matched
// to their commands.
type Rig struct {
	ch      io.ReadWriteCloser
	timeout time.Duration

	reqMu sync.Mutex // serializes whole request/response exchanges

	mu        sync.Mutex
	waiting   chan benshi.Message // non-nil while a request awaits its reply
	waitCmd   benshi.Command
	cachedHz  uint32
	cachedOK  bool
	closed    bool
	closeOnce sync.Once
}

// New starts a rig on ch. The reader goroutine runs until Close.
func New(ch io.ReadWriteCloser, timeout time.Duration) *Rig {
	r := &Rig{ch: ch, timeout: timeout}
	go r.readLoop()
	return r
}

// Close shuts the rig down and closes the underlying channel.
func (r *Rig) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		err = r.ch.Close()
	})
	return err
}

// CachedFreq returns the last frequency pushed by the radio, if any.
func (r *Rig) CachedFreq() (uint32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cachedHz, r.cachedOK
}

// SetFreq enters frequency (VFO) mode and tunes rx = tx = hz.
//
// v1 is simplex only, which is what Winlink RMS gateways need.
func (r *Rig) SetFreq(hz uint32) error {
	p := benshi.FreqModeParams{
		RXFreqHz: hz,
		TXFreqHz: hz,
		RXMod:    benshi.ModFM,
		TXMod:    benshi.ModFM,
		Step:     benshi.DefaultStep,
	}
	_, err := r.request(benshi.CmdFreqModeSetPar, p.Payload())
	return err
}

// GetFreq reads the current frequency from the radio.
//
// A radio sitting on a stored channel answers FREQ_MODE_GET_STATUS with a
// failure status rather than a frequency, since it is not in VFO mode. In that
// case fall back to reading the active channel directly instead of surfacing
// the error -- QSY only ever writes frequency mode, but readback must work
// whichever mode the radio happens to be in.
func (r *Rig) GetFreq() (uint32, error) {
	body, err := r.request(benshi.CmdFreqModeGetStatus, nil)
	if err != nil {
		return 0, err
	}
	hz, err := benshi.DecodeFreqModeStatus(body)
	if err == nil {
		return hz, nil
	}
	id, cerr := r.currChannel()
	if cerr != nil {
		return 0, err
	}
	return r.channelFreq(id)
}

// Teardown drops the radio out of frequency mode, restoring its channel state.
func (r *Rig) Teardown() error {
	_, err := r.request(benshi.CmdFreqModeSetPar, benshi.TeardownPayload())
	return err
}

// htStatusTXBit is is_in_tx within the first Status byte. Status packs, MSB
// first: is_power_on, is_in_tx, is_sq, is_in_rx, double_channel(2), is_scan,
// is_radio -- so is_in_tx is bit 6.
const htStatusTXBit = 0x40

// GetPTT reports whether the radio is currently transmitting.
func (r *Rig) GetPTT() (bool, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return false, err
	}
	if len(body) < 2 || body[0] != 0 {
		return false, benshi.ErrShortBody
	}
	return body[1]&htStatusTXBit != 0, nil
}

// currChannel returns the radio's currently selected channel id from
// GET_HT_STATUS. curr_ch_id_lower is the high nibble of the second Status byte.
func (r *Rig) currChannel() (byte, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return 0, err
	}
	if len(body) < 3 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	return body[2] >> 4, nil
}

// channelFreq reads a stored channel's RX frequency. READ_RF_CH is read-only;
// its writing counterpart is deliberately absent from this package.
func (r *Rig) channelFreq(id byte) (uint32, error) {
	body, err := r.request(benshi.CmdReadRFCh, []byte{id})
	if err != nil {
		return 0, err
	}
	// body: reply status, channel_id, tx word (mod<<30|freq), rx word.
	if len(body) < 10 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	return binary.BigEndian.Uint32(body[6:10]) & 0x3FFFFFFF, nil
}

// Probe confirms the radio answers the protocol at all, so callers can fail
// with a clear message rather than a timeout on every later command.
func (r *Rig) Probe() error {
	body, err := r.request(benshi.CmdGetDevInfo, nil)
	if err != nil {
		return err
	}
	if len(body) < 1 || body[0] != 0 {
		return fmt.Errorf("rig: radio rejected GET_DEV_INFO")
	}
	return nil
}

// request sends one command and waits for its reply.
func (r *Rig) request(cmd benshi.Command, body []byte) ([]byte, error) {
	r.reqMu.Lock()
	defer r.reqMu.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	replyCh := make(chan benshi.Message, 1)
	r.waiting = replyCh
	r.waitCmd = cmd
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.waiting = nil
		r.mu.Unlock()
	}()

	msg := benshi.Message{Group: benshi.GroupBasic, Command: cmd, Body: body}
	frame := benshi.Frame{Flags: benshi.FlagNone, Data: msg.Bytes()}
	raw := frame.Bytes()
	if raw == nil {
		return nil, fmt.Errorf("rig: command %d produced an unencodable frame", cmd)
	}
	if _, err := r.ch.Write(raw); err != nil {
		return nil, fmt.Errorf("rig: write: %w", err)
	}

	select {
	case m := <-replyCh:
		return m.Body, nil
	case <-time.After(r.timeout):
		return nil, ErrTimeout
	}
}

// readLoop decodes frames from the radio, routing replies to a waiting request
// and folding notifications into the cache.
func (r *Rig) readLoop() {
	dec := benshi.NewDecoder()
	buf := make([]byte, 512)
	for {
		n, err := r.ch.Read(buf)
		if n > 0 {
			frames, derr := dec.Feed(buf[:n])
			if derr == nil {
				for _, f := range frames {
					r.dispatch(f)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *Rig) dispatch(f benshi.Frame) {
	m, err := benshi.DecodeMessage(f.Data)
	if err != nil {
		return
	}
	if m.Command == benshi.CmdEventNotification {
		if st, err := benshi.DecodeFreqModeNotification(m.Body); err == nil {
			r.mu.Lock()
			if st.Active {
				r.cachedHz, r.cachedOK = st.RXFreqHz, true
			} else {
				r.cachedOK = false
			}
			r.mu.Unlock()
		}
		return
	}
	r.mu.Lock()
	w, cmd := r.waiting, r.waitCmd
	r.mu.Unlock()
	if w != nil && m.Command == cmd {
		select {
		case w <- m:
		default:
		}
	}
}
