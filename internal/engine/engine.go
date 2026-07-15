// Package engine provides an asyncio-equivalent serialised event loop.
// Everything stateful in tncd runs on one Engine; Do ≡ call_soon_threadsafe,
// After ≡ call_later.
package engine

import (
	"sync"
	"time"
)

// Clock lets downstream packages (e.g. l2) inject fake time in unit tests.
type Clock interface {
	Now() time.Time
	After(d time.Duration, fn func()) *Timer
}

// Timer wraps a time.AfterFunc timer so that the posted fn can be cancelled.
// If Cancel is called after the timer has already fired, the fn may still
// execute on the loop — callers must guard against that with their own state
// (mirror of conn.t1_handle = None in tncd.py).
type Timer struct {
	t          *time.Timer
	mu         sync.Mutex
	fired      bool
	cancel     bool
	cancelHook func() // optional; called under mu when Cancel() runs (test seam)
}

// Cancel prevents the timer callback from running. If the timer has already
// fired and the fn is queued, it will be silently dropped.
func (tm *Timer) Cancel() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.cancel = true
	if tm.t != nil {
		tm.t.Stop()
	}
	if tm.cancelHook != nil {
		tm.cancelHook()
	}
}

// NewManualTimer returns a *Timer that has no real time.Timer behind it.
// Calling Cancel() sets *cancelled to true. This is a test seam used by
// fakeClock in ax25/l2 tests so that l2 code can hold *Timer values under
// a fake clock without a real time.AfterFunc.
func NewManualTimer(cancelled *bool) *Timer {
	tm := &Timer{
		cancelHook: func() { *cancelled = true },
	}
	return tm
}

// Engine is a serialised event loop. All funcs posted via Do or After run
// sequentially on the single goroutine that calls Run.
type Engine struct {
	mu    sync.Mutex
	queue []func()
	wake  chan struct{} // cap-1; signals the loop that work is available
	quit  chan struct{} // closed by Stop to drain-and-exit
	stop  bool          // set under mu; causes Run to exit after draining
}

// New returns a ready-to-use Engine. Call Run (in a goroutine or directly) to
// start processing posted funcs.
func New() *Engine {
	return &Engine{
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
	}
}

// Do posts fn to the loop's FIFO queue. It never blocks, even when called from
// inside the loop itself (the call_soon_threadsafe / call_soon pattern).
func (e *Engine) Do(fn func()) {
	e.mu.Lock()
	e.queue = append(e.queue, fn)
	e.mu.Unlock()
	// Non-blocking send: if a wake signal is already pending, skip.
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Stop asks Run to exit after draining the funcs currently queued. It is safe
// to call from inside or outside the loop.
func (e *Engine) Stop() {
	e.Do(func() {
		e.mu.Lock()
		e.stop = true
		e.mu.Unlock()
	})
}

// Run processes funcs posted via Do and After in FIFO order, blocking until
// Stop is called. It must be called from exactly one goroutine.
func (e *Engine) Run() {
	for {
		// Drain all currently-queued funcs in one batch.
		e.mu.Lock()
		batch := e.queue
		e.queue = nil
		shouldStop := e.stop
		e.mu.Unlock()

		for _, fn := range batch {
			fn()
		}

		// Check stop after running the batch (Stop sets the flag via a posted fn,
		// so it will have been executed in the batch above).
		e.mu.Lock()
		shouldStop = e.stop
		e.mu.Unlock()
		if shouldStop {
			return
		}

		// Wait for more work.
		<-e.wake
	}
}

// After schedules fn to run on the loop after duration d. Returns a *Timer
// that can be cancelled before it fires.
func (e *Engine) After(d time.Duration, fn func()) *Timer {
	tm := &Timer{}
	tm.t = time.AfterFunc(d, func() {
		tm.mu.Lock()
		cancelled := tm.cancel
		tm.fired = true
		tm.mu.Unlock()
		if cancelled {
			return
		}
		e.Do(fn)
	})
	return tm
}

// Now returns the current wall-clock time. Satisfies the Clock interface.
func (e *Engine) Now() time.Time {
	return time.Now()
}
