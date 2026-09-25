package kiss

import (
	"log"
	"sync"
)

// Benshi/Gaia wire-framing constants, mirrored from benshi/frame.go (where
// they are unexported) rather than imported from that package.
//
// The demultiplexer needs byte-by-byte visibility into a frame that is only
// PARTIALLY accumulated -- specifically "how many more bytes belong to this
// frame, now that its 4-byte header has been read" -- so that a byte equal
// to 0xC0 inside a Gaia frame's payload is never mistaken for a KISS FEND.
// benshi.Decoder's Feed is deliberately a black box (whole frames in, whole
// frames out) and exposing that partial-frame state would serve no consumer
// but this one; a kiss -> benshi import for five small integers was judged
// not worth that dependency. benshi/frame.go remains the source of truth
// these constants must stay in sync with.
const (
	gaiaStart        = 0xFF
	gaiaVersion      = 0x01
	gaiaHeaderLen    = 4 // start, version, flags, length
	gaiaMsgHeaderLen = 4 // message header the length byte does not count
	gaiaFlagChecksum = 0x01
)

// demux is the single reader for a Port's transport once byte-level access
// must be shared between the KISS decoder and an optional rig-control
// consumer. It classifies each byte by its position in the stream -- never
// by which "socket" it arrived on, because on the radio this exists for
// (BTech UV-PRO and relatives) there is only one -- and routes whole frames
// by their leading bytes: 0xC0 (FEND) opens a KISS frame, 0xFF 0x01 opens a
// Gaia frame. This mirrors the wire behaviour bench-confirmed against a
// UV-PRO (2026-09-23 progress notes): two independent KISS frames and one
// Gaia reply, interleaved on a single RFCOMM link, distinguished only by
// their leading bytes.
//
// A demux owns one kiss.Decoder for the life of a Port. Feed is a drop-in
// replacement for that Decoder's own Feed in kiss.Port's readerLoop, so
// behaviour is unchanged when no rig consumer is ever attached -- see
// TestDemuxKISSOnlyPassthroughByteIdentical.
type demux struct {
	kissDec Decoder

	// inKISS is true while positioned between a KISS frame's own opening and
	// closing FEND. Each KISS frame is required to carry both delimiters
	// itself; see the design comment on Feed for why the decoder's "single
	// shared FEND between consecutive frames" compact form is not specially
	// handled here.
	inKISS bool

	// gaiaBuf accumulates a candidate Gaia frame; nil when idle. Bounded by
	// construction, not by a runtime check: the length byte is a single
	// uint8, so a fully committed candidate never exceeds gaiaHeaderLen +
	// 255 + gaiaMsgHeaderLen + 1 (checksum) = 264 bytes, regardless of how
	// much input is fed or how many times Feed is called.
	gaiaBuf   []byte
	gaiaTotal int // total length of gaiaBuf once its header is read; 0 until then

	mu   sync.Mutex
	ctrl chan []byte // non-nil while a control consumer is attached
}

// Feed processes p and returns any complete KISS frames now available,
// exactly like Decoder.Feed -- so it is a drop-in replacement in
// kiss.Port's readerLoop. Any complete Gaia frame found along the way is
// routed to the attached control consumer (deliverGaia), or discarded if
// none is attached.
//
// Design choice: a KISS frame is only entered by its OWN opening FEND here,
// and left by its own closing FEND. framing.go's Decoder additionally
// treats a FEND seen while already in-frame as closing the current frame AND
// opening the next one in the same byte (the "single shared FEND" compact
// form) -- correct for a KISS-only stream, but unsafe to assume here: the
// byte immediately after a shared FEND could just as well be the 0xFF of an
// interleaved Gaia frame, not more KISS payload. So if a peer ever emits the
// compact form immediately followed by something other than another FEND,
// this demux treats what follows as scan input (a new KISS start, a Gaia
// start, or noise) rather than a continuation of the previous KISS frame.
// The previous frame is then left incomplete and is silently dropped by the
// underlying Decoder the next time it sees a FEND -- never delivered as a
// corrupted frame. Degraded, not unsafe, and it self-resynchronizes on the
// next well-formed frame. This matches the wire behaviour actually observed
// on the bench (2026-09-23: "two independent KISS frames plus a Gaia
// reply" -- each frame carrying its own delimiters).
func (d *demux) Feed(p []byte) [][]byte {
	var frames [][]byte
	i := 0
	for i < len(p) {
		var fr [][]byte
		switch {
		case len(d.gaiaBuf) > 0:
			i = d.feedGaia(p, i)
		case d.inKISS:
			i, fr = d.feedKISSRun(p, i)
			frames = append(frames, fr...)
		default:
			i, fr = d.scan(p, i)
			frames = append(frames, fr...)
		}
	}
	return frames
}

// scan looks at one byte outside any open frame and decides what it starts.
func (d *demux) scan(p []byte, i int) (int, [][]byte) {
	switch p[i] {
	case FEND:
		d.inKISS = true
		return i + 1, d.kissDec.Feed(p[i : i+1])
	case gaiaStart:
		d.gaiaBuf = []byte{gaiaStart}
		return i + 1, nil
	default:
		// Neither a KISS delimiter nor a plausible Gaia start: stray noise,
		// dropped. It must NOT be handed to kissDec: framing.go's Decoder
		// never resets its inFrame flag once the first FEND is seen (only an
		// oversize drop does), so an out-of-band byte fed to it outside a
		// genuine FEND-delimited region would be silently absorbed as frame
		// content instead of ignored -- exactly the corruption this demux
		// exists to prevent.
		return i + 1, nil
	}
}

// feedKISSRun hands kissDec every byte up to and including the frame's
// closing FEND, or the rest of p if the frame continues past this call.
func (d *demux) feedKISSRun(p []byte, i int) (int, [][]byte) {
	j := i
	for j < len(p) {
		if p[j] == FEND {
			j++
			d.inKISS = false
			break
		}
		j++
	}
	return j, d.kissDec.Feed(p[i:j])
}

// feedGaia accumulates bytes into a candidate Gaia frame, delivering it
// (deliverGaia) once complete. Returns the index to resume scanning from.
//
// A false start (a byte after 0xFF that is not the version byte) resyncs by
// dropping only the 0xFF and returning the SAME index, so the rejected byte
// is re-examined fresh at scan -- it may itself be a FEND or a new 0xFF, and
// must not be silently swallowed along with the false start. This mirrors
// benshi.Decoder.Feed's own resync rule.
func (d *demux) feedGaia(p []byte, i int) int {
	for i < len(p) {
		b := p[i]
		switch {
		case len(d.gaiaBuf) == 1: // awaiting the version byte
			if b != gaiaVersion {
				d.gaiaBuf = nil
				return i // re-examine b at scan
			}
			d.gaiaBuf = append(d.gaiaBuf, b)
			i++
		case len(d.gaiaBuf) < gaiaHeaderLen: // flags, then length: always accepted
			d.gaiaBuf = append(d.gaiaBuf, b)
			i++
			if len(d.gaiaBuf) == gaiaHeaderLen {
				dataLen := int(d.gaiaBuf[3]) + gaiaMsgHeaderLen
				total := gaiaHeaderLen + dataLen
				if d.gaiaBuf[2]&gaiaFlagChecksum != 0 {
					total++
				}
				d.gaiaTotal = total
			}
		default:
			// Header committed: the frame's total length is now known, so
			// every remaining byte belongs to it unconditionally -- even one
			// equal to FEND or gaiaStart. This is what protects Gaia payload
			// data from being misread as a KISS delimiter.
			d.gaiaBuf = append(d.gaiaBuf, b)
			i++
			if len(d.gaiaBuf) >= d.gaiaTotal {
				d.deliverGaia(d.gaiaBuf)
				d.gaiaBuf = nil
				d.gaiaTotal = 0
				return i
			}
		}
	}
	return i
}

// deliverGaia routes a complete Gaia frame to the attached control consumer,
// or discards it if none is attached.
//
// The send is non-blocking. This method runs on Port's single reader
// goroutine -- the same one that feeds the KISS decoder -- so a full or
// absent rig consumer must never be able to stall a KISS read. Dropping a
// Gaia frame under backpressure is an acceptable, documented cost; delaying
// a KISS frame to avoid it is not.
func (d *demux) deliverGaia(frame []byte) {
	d.mu.Lock()
	ch := d.ctrl
	d.mu.Unlock()
	if ch == nil {
		return
	}
	cp := append([]byte(nil), frame...)
	select {
	case ch <- cp:
	default:
		log.Printf("kiss: rig control channel full, dropping Gaia frame")
	}
}

// attach registers ch as the sole destination for recognised Gaia frames.
// Returns false if a consumer is already attached -- a second concurrent
// consumer would only need to race the first for frames off of ch, so this
// demux allows only one at a time rather than letting that hazard resurface
// one layer up.
func (d *demux) attach(ch chan []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctrl != nil {
		return false
	}
	d.ctrl = ch
	return true
}

// detach removes ch if it is still the attached consumer. Comparing
// identity (rather than unconditionally nilling) avoids detaching a
// different, newer consumer if calls are ever reordered. It deliberately
// does not close ch: deliverGaia is the only sender and closing a channel
// from its receiver's Close() risks a send-on-closed-channel panic if a
// frame is in flight at the same moment.
func (d *demux) detach(ch chan []byte) {
	d.mu.Lock()
	if d.ctrl == ch {
		d.ctrl = nil
	}
	d.mu.Unlock()
}
