package bridge

import (
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// TestRigForOutOfRangePort covers RigFor's bounds check: a negative index and
// an index past the configured (but not yet started) port slice must both
// error rather than index out of range.
func TestRigForOutOfRangePort(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(false))
	go eng.Run()
	t.Cleanup(func() { eng.Stop() })

	onLoop(t, eng, func() {
		if r, err := b.RigFor(-1); err == nil {
			t.Errorf("RigFor(-1) = %v, %v; want a nil rig and an error", r, err)
		}
		if r, err := b.RigFor(0); err == nil {
			t.Errorf("RigFor(0) before Start = %v, %v; want a nil rig and an error (no ports configured yet)", r, err)
		}
	})
}

// TestRigForOfflinePortReturnsError proves a port that has never come online
// (b.ports[0] is still the offline sentinel, not a *kiss.Port) yields an
// error and no rig -- this is the case internal/app's rigForPort relies on to
// answer a rigctl client with RPRT -6 instead of blocking on a radio that
// isn't there.
func TestRigForOfflinePortReturnsError(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(false))
	// Open blocks forever: the port never reports online, so b.ports[0]
	// stays the offline sentinel for the life of the test.
	f := newGateFake(true)
	b.newTransport = gateFactory(t, f)

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, f.openEntered, 2*time.Second, "dial entered")

	onLoop(t, eng, func() {
		r, err := b.RigFor(0)
		if err == nil {
			t.Error("RigFor on an offline port: want error, got nil")
		}
		if r != nil {
			t.Errorf("RigFor on an offline port: want nil rig, got %v", r)
		}
	})
}

// TestRigForCachesRigForSameLink proves RigFor does not build a fresh
// *rig.Rig (and therefore a fresh control-channel attachment) on every call:
// kiss.Port.ControlChannel allows only one attached consumer at a time, so a
// second, uncached attempt while the first rig.Rig is still alive would fail
// with ErrControlChannelInUse instead of quietly reusing it.
func TestRigForCachesRigForSameLink(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(false))
	f := newReconnFake()
	b.newTransport = func(config.Port) (kiss.Transport, error) { return f, nil }

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, func() bool { return f.openCount() >= 1 }, 2*time.Second, "initial open")
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "online")

	var first, second *rig.Rig
	onLoop(t, eng, func() {
		var err error
		first, err = b.RigFor(0)
		if err != nil {
			t.Fatalf("RigFor (1st call): %v", err)
		}
		second, err = b.RigFor(0)
		if err != nil {
			t.Fatalf("RigFor (2nd call): %v", err)
		}
	})
	if first == nil {
		t.Fatal("RigFor returned a nil rig for an online port")
	}
	if first != second {
		t.Error("RigFor built a second rig for the same live link instead of reusing the cached one")
	}
}

// TestRigForInvalidatesCacheAcrossReconnect proves a reconnect (a fresh
// *kiss.Port replacing the old one -- see connectPort) invalidates the
// cached rig rather than handing back one still bound to the dead link's
// control channel: RigFor must return a different *rig.Rig after the port
// comes back up on a new transport, and the old one must be closed (further
// calls fail with rig.ErrClosed) rather than left to dangle.
func TestRigForInvalidatesCacheAcrossReconnect(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(true))

	f1 := newGateFake(false) // initial link: opens immediately
	f2 := newGateFake(false) // reconnect: opens immediately
	b.newTransport = gateFactory(t, f1, f2)

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, f1.openEntered, 2*time.Second, "initial open")
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "initial online")

	var firstRig *rig.Rig
	onLoop(t, eng, func() {
		var err error
		firstRig, err = b.RigFor(0)
		if err != nil {
			t.Fatalf("RigFor (first link): %v", err)
		}
	})

	// Drop the link; reconnect=true rearms a dial onto f2.
	f1.Close()
	waitFor(t, f2.openEntered, 2*time.Second, "reconnect open")
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "reconnect online")

	var secondRig *rig.Rig
	onLoop(t, eng, func() {
		var err error
		secondRig, err = b.RigFor(0)
		if err != nil {
			t.Fatalf("RigFor (second link): %v", err)
		}
	})

	if firstRig == secondRig {
		t.Fatal("RigFor returned the same *rig.Rig across a reconnect -- it would be bound to a dead control channel")
	}
	if err := firstRig.SetFreq(145030000); err != rig.ErrClosed {
		t.Errorf("stale rig SetFreq error = %v, want rig.ErrClosed (RigFor should have closed it on invalidation)", err)
	}
}
