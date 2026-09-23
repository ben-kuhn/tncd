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

// staleReplyExpiryMultiplier bounds how long an abandoned (timed-out)
// request's generation stays open for a late straggler before dispatch
// force-retires it and lets replySeq catch up on its own. A genuinely late
// reply is most plausible shortly after its own deadline -- the radio was
// probably just a bit slower than our budget that one time -- so a few
// multiples of the timeout comfortably covers that case without leaving a
// truly lost reply wedging every later request of the same command behind a
// gap that only a process restart would clear.
const staleReplyExpiryMultiplier = 3

// abandonedGen records a request that gave up without a reply, and when.
type abandonedGen struct {
	gen uint64
	at  time.Time
}

// Rig is a request/response session with one radio.
//
// One request is in flight at a time: the radio is a single serial endpoint and
// replies carry no correlation id, so concurrent requests could not be matched
// to their commands.
type Rig struct {
	ch      io.ReadWriteCloser
	timeout time.Duration

	reqMu sync.Mutex // serializes whole request/response exchanges

	mu      sync.Mutex
	waiting chan benshi.Message // non-nil while a request awaits its reply
	waitCmd benshi.Command
	// waitGen is the generation of the request currently occupying waiting.
	waitGen uint64
	// reqSeq counts requests issued so far; a new request's generation is
	// reqSeq after incrementing.
	reqSeq uint64
	// replySeq counts non-notification replies consumed so far, in FIFO
	// order. The link has no per-message correlation id, so this is the only
	// way to tell a late reply to an abandoned (timed-out) request apart from
	// the answer to whatever request is current: every reply retires the next
	// outstanding generation in order, whether or not anyone is still
	// waiting for it, so a straggler can never be mistaken for the answer to
	// a later request that happens to share the same command code.
	replySeq uint64
	// abandoned holds generations whose request timed out without ever
	// retiring, oldest first. A reply arriving inside its straggler window
	// still retires it normally (dispatch's replySeq advance below);
	// pruneExpiredLocked force-retires it once the window has elapsed, so a
	// genuinely lost reply self-heals instead of wedging replySeq forever.
	abandoned []abandonedGen

	cachedHz  uint32
	cachedOK  bool
	closed    bool
	closeErr  error
	closeOnce sync.Once
	closedCh  chan struct{} // closed by Close to unblock in-flight requests
}

// New starts a rig on ch. The reader goroutine runs until Close.
func New(ch io.ReadWriteCloser, timeout time.Duration) *Rig {
	r := &Rig{ch: ch, timeout: timeout, closedCh: make(chan struct{})}
	go r.readLoop()
	return r
}

// Close shuts the rig down and closes the underlying channel.
//
// closedCh is closed before the channel itself so any request blocked in its
// select wakes immediately with ErrClosed instead of waiting out its full
// timeout. closeErr is stored on the struct rather than a local var: sync.Once
// blocks concurrent callers until the winning call to f returns, which is
// exactly the happens-before guarantee needed to let every caller -- not just
// the one that ran f -- read the real result instead of a zero-value nil.
func (r *Rig) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.closedCh)
		r.closeErr = r.ch.Close()
	})
	return r.closeErr
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
	r.reqSeq++
	gen := r.reqSeq
	replyCh := make(chan benshi.Message, 1)
	r.waiting = replyCh
	r.waitCmd = cmd
	r.waitGen = gen
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		// Only clear waiting if it's still ours -- if we timed out, a later
		// request may already have installed its own generation here.
		if r.waitGen == gen {
			r.waiting = nil
		}
		r.mu.Unlock()
	}()

	msg := benshi.Message{Group: benshi.GroupBasic, Command: cmd, Body: body}
	frame := benshi.Frame{Flags: benshi.FlagNone, Data: msg.Bytes()}
	raw := frame.Bytes()
	if raw == nil {
		return nil, fmt.Errorf("rig: command %d produced an unencodable frame", cmd)
	}

	deadline := time.NewTimer(r.timeout)
	defer deadline.Stop()

	// io.ReadWriteCloser has no deadline method, and a wedged Bluetooth SPP
	// socket is a documented failure mode here (see CLAUDE.md) that accepts
	// writes without ever delivering them -- so Write itself must be bounded,
	// not just the wait for a reply. writeErr is buffered so this goroutine
	// can never block trying to report a result nobody is listening for any
	// more, whether the deadline fires first or the rig is closed underneath
	// it.
	writeErr := make(chan error, 1)
	go func() {
		_, err := r.ch.Write(raw)
		writeErr <- err
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			return nil, fmt.Errorf("rig: write: %w", err)
		}
	case <-deadline.C:
		// The write goroutine may still land on the wire after we give up on
		// it, so this generation gets the same straggler grace as a reply
		// timeout rather than being retired immediately.
		r.abandon(gen)
		return nil, ErrTimeout
	case <-r.closedCh:
		return nil, ErrClosed
	}

	select {
	case m := <-replyCh:
		return m.Body, nil
	case <-deadline.C:
		r.abandon(gen)
		return nil, ErrTimeout
	case <-r.closedCh:
		return nil, ErrClosed
	}
}

// abandon records that gen's request gave up without a reply. A reply for it
// may still be genuinely in flight, so it isn't force-retired immediately --
// see pruneExpiredLocked.
func (r *Rig) abandon(gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replySeq < gen {
		r.abandoned = append(r.abandoned, abandonedGen{gen: gen, at: time.Now()})
	}
}

// pruneExpiredLocked force-retires any abandoned generation whose straggler
// window has elapsed. Must be called with mu held, before dispatch's own
// replySeq advance so an incoming reply is matched against a gap that's
// already been caught up to reality.
func (r *Rig) pruneExpiredLocked() {
	for len(r.abandoned) > 0 {
		oldest := r.abandoned[0]
		if time.Since(oldest.at) < r.timeout*staleReplyExpiryMultiplier {
			break // still within the window; a straggler for it is plausible
		}
		if r.replySeq < oldest.gen {
			r.replySeq = oldest.gen
		}
		r.abandoned = r.abandoned[1:]
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
	// Force-retire any abandoned generation whose straggler window has
	// already elapsed before considering this reply, so a genuinely lost
	// reply doesn't leave replySeq permanently behind reqSeq.
	r.pruneExpiredLocked()
	// Retire the next outstanding generation unconditionally -- see the
	// replySeq field doc. This must happen even when nobody is currently
	// waiting (w == nil, or a later request already owns the slot), so a
	// straggler reply for an abandoned request is consumed here rather than
	// left to be misread as the answer to whatever request comes next.
	if r.replySeq < r.reqSeq {
		r.replySeq++
		if len(r.abandoned) > 0 && r.abandoned[0].gen == r.replySeq {
			r.abandoned = r.abandoned[1:] // this reply retired it directly
		}
	}
	gen := r.replySeq
	w, waitGen, cmd := r.waiting, r.waitGen, r.waitCmd
	r.mu.Unlock()
	if w != nil && gen == waitGen && m.Command == cmd {
		select {
		case w <- m:
		default:
		}
	}
}
