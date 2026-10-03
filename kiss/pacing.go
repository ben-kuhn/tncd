package kiss

import "time"

// TX pacing: bound how far ahead of the air we hand frames to the transport.
//
// A KISS TNC accepts frames as fast as the link will take them and queues them
// internally, so a full window of I-frames lands in the TNC in milliseconds
// and then takes seconds to actually transmit. tncd's "tx" counter means "we
// handed it to the transport", not "it went on the air". That gap is where a
// class of hard failures lives:
//
//   - A Bluetooth radio that buffers internally (the UV-PRO holds frames for
//     ~2 minutes) accepts the whole burst, and nothing below the application
//     layer can see that it never transmitted.
//   - Retransmissions compound it: L2 can queue a retransmit of a frame the
//     TNC has not even started sending yet.
//   - Serial write-pattern corruption at high baud (observed on the TS-2000 at
//     57600) is a symptom of handing a device more than it can absorb.
//
// Pacing hands over at roughly the rate the channel can drain, keeping at most
// maxLead of air time outstanding at the TNC. The TNC always has work queued,
// so throughput is unaffected on a healthy link, but tncd can no longer dump
// 11 seconds of audio into a radio in 0.4 seconds.
//
// This is deliberately an estimate, not an accounting of the actual air time.
// The exact figure would need the bit-stuffed on-air length, the TNC's
// channel-access outcome (p-persistence, slot time, DCD), and the remote's
// behaviour -- none of which the host can observe. The estimate only has to be
// the right order of magnitude to cap the backlog, and erring slightly fast
// (under-estimating air time) is the safe direction: it degrades toward
// today's unpaced behaviour rather than throttling a good link.

// pacer tracks when the transport is expected to have drained everything
// handed to it so far. The zero value paces nothing.
type pacer struct {
	baud    int           // on-air bits per second; <=0 disables
	txDelay time.Duration // per-transmission preamble (KISS TXDELAY)

	// avail is when everything handed over so far should be off the air.
	// Zero (or in the past) means the channel is idle as far as we know.
	avail time.Time
}

// maxPacedFrameBytes is the largest frame used to size the lead budget: a
// 256-byte AX.25 payload plus worst-case address and control overhead.
const maxPacedFrameBytes = 256 + 20

// newPacer builds a pacer from a port's Params. TXDELAY is in units of 10ms,
// per the KISS spec.
func newPacer(p Params) pacer {
	pc := pacer{baud: p.OTABaud}
	if p.TXDelay != nil && *p.TXDelay > 0 {
		pc.txDelay = time.Duration(*p.TXDelay) * 10 * time.Millisecond
	}
	return pc
}

// enabled reports whether this pacer will ever delay a frame.
func (pc *pacer) enabled() bool { return pc.baud > 0 }

// airTime estimates how long nBytes takes to transmit, including the
// per-transmission preamble. Under pacing each frame tends to become its own
// transmission, so charging TXDELAY per frame is self-consistent rather than
// pessimistic.
func (pc *pacer) airTime(nBytes int) time.Duration {
	if pc.baud <= 0 {
		return 0
	}
	bits := float64(nBytes) * 8
	return pc.txDelay + time.Duration(bits/float64(pc.baud)*float64(time.Second))
}

// maxLead is how far ahead of the channel we allow ourselves to be: one
// full-size frame of air time. That is enough that the TNC never runs dry
// while it finishes the frame it is sending, and no more.
func (pc *pacer) maxLead() time.Duration {
	return pc.airTime(maxPacedFrameBytes)
}

// delayFor returns how long to wait before handing over a frame of nBytes,
// and must be called immediately before the write. It charges the frame
// against the air-time budget, so every call must be followed by an attempt
// to write that frame.
func (pc *pacer) delayFor(nBytes int, now time.Time) time.Duration {
	if !pc.enabled() {
		return 0
	}
	air := pc.airTime(nBytes)
	// An idle channel: nothing outstanding, so start the clock from now.
	if pc.avail.Before(now) {
		pc.avail = now
	}
	var wait time.Duration
	if lead := pc.avail.Sub(now); lead > pc.maxLead() {
		wait = lead - pc.maxLead()
	}
	pc.avail = pc.avail.Add(air)
	return wait
}
