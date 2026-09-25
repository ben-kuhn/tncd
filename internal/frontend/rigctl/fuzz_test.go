package rigctl

import (
	"strings"
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// FuzzHandleLine covers the rigctl command parser. Its input is a line from
// an arbitrary TCP client, and CLAUDE.md requires a fuzz target for every
// parser of untrusted bytes -- network-sourced as much as radio-sourced.
//
// The invariant is the protocol contract PAT depends on: every command
// produces exactly one single-line response, never a panic and never a
// multi-line reply that would desynchronise the client's read loop.
func FuzzHandleLine(f *testing.F) {
	for _, seed := range []string{
		"f", "F 145030000", "m", "v", "t", "T 1", "T 0",
		`\chk_vfo`, `\get_freq`, `\set_freq 145670000`, `\dump_state`,
		"q", "", "   ", "F", "F notanumber", "F -1",
		"F 99999999999999999999", `\set_freq`, "T", "T 2", "\x00\xff",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
		got := handleLine(line, &fakeRig{}, cfg, &pttState{})
		if strings.Contains(got, "\n") {
			t.Fatalf("handleLine(%q) returned a multi-line response %q -- "+
				"the client reads one line per command and would desynchronise", line, got)
		}
	})
}
