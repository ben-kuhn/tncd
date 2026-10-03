package kiss

import (
	"io"
	"net"
	"testing"
	"time"
)

func intp(v int) *int { return &v }

func TestPacerDisabledByDefault(t *testing.T) {
	pc := newPacer(Params{})
	if pc.enabled() {
		t.Fatal("zero-value Params must not pace")
	}
	// A disabled pacer must never delay, however much it is handed.
	now := time.Now()
	for i := 0; i < 100; i++ {
		if d := pc.delayFor(300, now); d != 0 {
			t.Fatalf("frame %d: disabled pacer returned delay %v", i, d)
		}
	}
}

func TestPacerAirTime(t *testing.T) {
	// 1200 baud, no TXDELAY: 120 bytes = 960 bits = 800ms.
	pc := newPacer(Params{OTABaud: 1200})
	if got := pc.airTime(120); got < 790*time.Millisecond || got > 810*time.Millisecond {
		t.Errorf("airTime(120 bytes @1200) = %v, want ~800ms", got)
	}
	// 9600 baud is 8x faster.
	fast := newPacer(Params{OTABaud: 9600})
	if got := fast.airTime(120); got < 95*time.Millisecond || got > 105*time.Millisecond {
		t.Errorf("airTime(120 bytes @9600) = %v, want ~100ms", got)
	}
	// TXDELAY is charged per transmission: 30 units = 300ms.
	withDelay := newPacer(Params{OTABaud: 1200, TXDelay: intp(30)})
	if got := withDelay.airTime(120) - pc.airTime(120); got < 295*time.Millisecond || got > 305*time.Millisecond {
		t.Errorf("TXDELAY contributed %v, want ~300ms", got)
	}
}

// The first frames onto an idle channel must go out immediately -- pacing caps
// the backlog, it does not add latency to a link that is keeping up.
func TestPacerIdleChannelDoesNotDelay(t *testing.T) {
	pc := newPacer(Params{OTABaud: 1200})
	now := time.Now()
	if d := pc.delayFor(120, now); d != 0 {
		t.Fatalf("first frame delayed by %v, want 0", d)
	}
	// Still within the one-frame lead budget.
	if d := pc.delayFor(120, now); d != 0 {
		t.Fatalf("second frame delayed by %v, want 0 (inside lead budget)", d)
	}
}

// The point of the change: a burst cannot be handed over faster than the
// channel drains, so the backlog at the TNC stays bounded.
func TestPacerBoundsOutstandingAirTime(t *testing.T) {
	pc := newPacer(Params{OTABaud: 1200})
	lead := pc.maxLead()
	now := time.Now()

	// Hand over a long burst of full-size frames as fast as possible,
	// honouring each returned delay by advancing the clock.
	for i := 0; i < 20; i++ {
		wait := pc.delayFor(maxPacedFrameBytes, now)
		now = now.Add(wait)
		// At the moment of the write, the outstanding air time may never
		// exceed the lead budget by more than the frame we are about to add.
		if outstanding := pc.avail.Sub(now); outstanding > lead+pc.airTime(maxPacedFrameBytes) {
			t.Fatalf("frame %d: %v of air time outstanding, budget is %v",
				i, outstanding, lead)
		}
	}
}

// Without pacing, 20 full-size frames at 1200 baud would be handed over in
// microseconds. With it, the elapsed time must approach the real air time.
func TestPacerBurstTakesAirTime(t *testing.T) {
	pc := newPacer(Params{OTABaud: 1200})
	start := time.Now()
	now := start
	const frames = 20
	for i := 0; i < frames; i++ {
		now = now.Add(pc.delayFor(maxPacedFrameBytes, now))
	}
	elapsed := now.Sub(start)
	air := time.Duration(frames) * pc.airTime(maxPacedFrameBytes)
	// Allow the one-frame lead budget plus the final frame still in flight.
	if elapsed < air-2*pc.airTime(maxPacedFrameBytes)-pc.maxLead() {
		t.Fatalf("burst of %d frames handed over in %v, but needs ~%v of air time",
			frames, elapsed, air)
	}
}

// End-to-end through writerLoop: a paced port must not dump a burst into the
// transport at once, and an unpaced port must.
func TestPortPacingSpreadsBurst(t *testing.T) {
	// 1200 baud: each 120-byte frame is ~820ms of air and the lead budget is
	// one full-size frame (~1.84s), so a five-frame burst must be held back.
	const burst = 5
	paced := func(ota int) time.Duration {
		a, b := net.Pipe()
		p := NewPort(0, &pipeTransport{c: a}, Params{OTABaud: ota},
			func(RXFrame) {}, func(int) {})
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		defer p.Close()

		payload := make([]byte, 120)
		start := time.Now()
		for i := 0; i < burst; i++ {
			p.Send(payload)
		}
		// Drain until every KISS frame has been read off the wire. Count
		// delimiters, not bytes: a KISS frame is longer than its payload, so
		// a byte count is satisfied before the last frame arrives. Each frame
		// is bracketed by a leading and trailing FEND.
		buf := make([]byte, 4096)
		var delims int
		b.SetReadDeadline(time.Now().Add(10 * time.Second))
		for delims < 2*burst {
			n, err := b.Read(buf)
			for _, c := range buf[:n] {
				if c == FEND {
					delims++
				}
			}
			if err != nil && err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			if n == 0 {
				break
			}
		}
		return time.Since(start)
	}

	unpacedElapsed := paced(0)
	if unpacedElapsed > 2*time.Second {
		t.Fatalf("unpaced burst took %v, expected it to be prompt", unpacedElapsed)
	}

	pacedElapsed := paced(1200)
	// The first two frames go immediately (inside the lead budget); the rest
	// must wait. Expect hundreds of milliseconds, not microseconds.
	if pacedElapsed < 200*time.Millisecond {
		t.Fatalf("paced burst took %v, expected pacing to hold frames back", pacedElapsed)
	}
	if pacedElapsed <= unpacedElapsed {
		t.Fatalf("paced burst (%v) was not slower than unpaced (%v)",
			pacedElapsed, unpacedElapsed)
	}
}

// Teardown must not wait out a pacing delay.
func TestPortPacingAbortsOnClose(t *testing.T) {
	a, _ := net.Pipe()
	// 50 baud: a single frame is many seconds of "air time", so the writer
	// loop is certain to be sitting in a pacing delay.
	p := NewPort(0, &pipeTransport{c: a}, Params{OTABaud: 50},
		func(RXFrame) {}, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 120)
	for i := 0; i < 8; i++ {
		p.Send(payload)
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a pacing delay")
	}
}
