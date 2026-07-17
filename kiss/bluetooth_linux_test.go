//go:build linux

package kiss

import (
	"errors"
	"sync"
	"testing"
)

// TestEnsureProfileRetrySeam verifies the retry behaviour of ensureProfile
// without requiring a real D-Bus / BlueZ connection.
//
// Call table:
//
//	call 1 → register() fails  → ensureProfile returns error,
//	          profileRegistered stays false
//	call 2 → register() succeeds → ensureProfile returns nil,
//	          profileRegistered becomes true
//	call 3 → register() must NOT be called again (already registered)
func TestEnsureProfileRetrySeam(t *testing.T) {
	// Reset package-level state so this test is hermetic.
	profileMu.Lock()
	profileRegistered = false
	profileMu.Unlock()
	t.Cleanup(func() {
		profileMu.Lock()
		profileRegistered = false
		profileMu.Unlock()
	})

	var callCount int
	var mu sync.Mutex

	type testCase struct {
		name      string
		returnErr error
		wantErr   bool
		wantCalls int // cumulative after this call
		wantReg   bool
	}
	cases := []testCase{
		{
			name:      "first call fails",
			returnErr: errors.New("UUID already registered"),
			wantErr:   true,
			wantCalls: 1,
			wantReg:   false,
		},
		{
			name:      "second call retries and succeeds",
			returnErr: nil,
			wantErr:   false,
			wantCalls: 2,
			wantReg:   true,
		},
		{
			name:      "third call skips register (already registered)",
			returnErr: errors.New("should never be called"),
			wantErr:   false,
			wantCalls: 2, // count must not increment
			wantReg:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturedErr := tc.returnErr // capture for closure
			err := ensureProfile(func() error {
				mu.Lock()
				callCount++
				mu.Unlock()
				return capturedErr
			})

			if tc.wantErr && err == nil {
				t.Errorf("wanted error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("wanted nil error, got %v", err)
			}

			mu.Lock()
			got := callCount
			mu.Unlock()
			if got != tc.wantCalls {
				t.Errorf("register call count = %d, want %d", got, tc.wantCalls)
			}

			profileMu.Lock()
			reg := profileRegistered
			profileMu.Unlock()
			if reg != tc.wantReg {
				t.Errorf("profileRegistered = %v, want %v", reg, tc.wantReg)
			}
		})
	}
}
