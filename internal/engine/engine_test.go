package engine

import (
	"testing"
	"time"
)

func TestFIFOOrder(t *testing.T) {
	e := New()
	var got []int
	for i := 0; i < 5; i++ {
		i := i
		e.Do(func() { got = append(got, i) })
	}
	e.Do(func() { e.Stop() })
	e.Run()
	for i, v := range got {
		if v != i {
			t.Fatalf("order = %v", got)
		}
	}
}

func TestDoFromLoopRunsAfterQueued(t *testing.T) {
	// The call_soon pattern: fn posted from inside the loop runs after
	// everything already queued.
	e := New()
	var got []string
	e.Do(func() {
		e.Do(func() { got = append(got, "deferred") })
	})
	e.Do(func() { got = append(got, "queued") })
	e.Do(func() { e.Stop() })
	e.Run()
	if len(got) != 2 || got[0] != "queued" || got[1] != "deferred" {
		t.Fatalf("got %v", got)
	}
}

func TestAfterFiresOnLoop(t *testing.T) {
	e := New()
	done := make(chan struct{})
	e.After(10*time.Millisecond, func() {
		close(done)
		e.Stop()
	})
	go e.Run()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timer never fired")
	}
}

func TestTimerCancel(t *testing.T) {
	e := New()
	fired := false
	tm := e.After(20*time.Millisecond, func() { fired = true })
	tm.Cancel()
	e.After(60*time.Millisecond, func() { e.Stop() })
	e.Run()
	if fired {
		t.Fatal("cancelled timer fired")
	}
}
