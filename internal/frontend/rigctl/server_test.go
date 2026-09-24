package rigctl

import (
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

type fakeRig struct {
	hz      uint32
	setErr  error
	getErr  error
	ptt     bool
	pttErr  error
	setFreq uint32
}

func (f *fakeRig) SetFreq(hz uint32) error  { f.setFreq = hz; return f.setErr }
func (f *fakeRig) GetFreq() (uint32, error) { return f.hz, f.getErr }
func (f *fakeRig) SetPTT(on bool) error     { f.ptt = on; return f.pttErr }
func (f *fakeRig) GetPTT() (bool, error)    { return f.ptt, f.pttErr }

func TestHandleLineFrequencyCommands(t *testing.T) {
	cases := []struct {
		name string
		line string
		rig  *fakeRig
		want string
	}{
		{"get_freq returns a bare integer", `\get_freq`, &fakeRig{hz: 145030000}, "145030000"},
		{"set_freq succeeds", `\set_freq 145030000`, &fakeRig{}, "RPRT 0"},
		{"chk_vfo reports no VFO mode", `\chk_vfo`, &fakeRig{}, "CHKVFO 0"},
		{"dump_caps is a non-error ping", "dump_caps", &fakeRig{}, "RPRT 0"},
		{"unknown command", "zzz", &fakeRig{}, "RPRT -1"},
		{"set_freq with no argument", `\set_freq`, &fakeRig{}, "RPRT -1"},
		{"set_freq with junk", `\set_freq abc`, &fakeRig{}, "RPRT -1"},
		{"radio timeout", `\get_freq`, &fakeRig{getErr: rig.ErrTimeout}, "RPRT -5"},
		{"radio port closed", `\get_freq`, &fakeRig{getErr: rig.ErrClosed}, "RPRT -6"},
		{"radio malformed reply", `\get_freq`, &fakeRig{getErr: errOpaque}, "RPRT -8"},
		{"short mnemonic get_freq", "f", &fakeRig{hz: 145030000}, "145030000"},
		{"short mnemonic set_freq", "F 145030000", &fakeRig{}, "RPRT 0"},
		{"get_ptt off", `\get_ptt`, &fakeRig{ptt: false}, "0"},
		{"get_ptt on", "t", &fakeRig{ptt: true}, "1"},
		{"set_ptt is a stub", `\set_ptt 1`, &fakeRig{}, "RPRT -4"},
		{"blank line", "   ", &fakeRig{}, "RPRT -1"},
	}
	cfg := config.RigCtl{PTTTimeout: 30}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := handleLine(tc.line, tc.rig, cfg, &pttState{})
			if got != tc.want {
				t.Errorf("handleLine(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestSetFreqPassesThroughValue(t *testing.T) {
	f := &fakeRig{}
	handleLine(`\set_freq 145030000`, f, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if f.setFreq != 145030000 {
		t.Errorf("SetFreq got %d, want 145030000", f.setFreq)
	}
}

// An offline or relinking port must answer immediately, never hang.
func TestUnavailableRigAnswersIO(t *testing.T) {
	got := handleLine(`\get_freq`, nil, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if got != "RPRT -6" {
		t.Errorf("handleLine with nil rig = %q, want RPRT -6", got)
	}
}

// chk_vfo and dump_caps must answer even while the port is offline -- PAT
// uses dump_caps purely as a liveness ping and should not see it fail just
// because the radio itself is mid-relink.
func TestNoRigCommandsAnswerWithoutARig(t *testing.T) {
	cfg := config.RigCtl{PTTTimeout: 30}
	if got := handleLine(`\chk_vfo`, nil, cfg, &pttState{}); got != "CHKVFO 0" {
		t.Errorf("chk_vfo with nil rig = %q, want CHKVFO 0", got)
	}
	if got := handleLine("dump_caps", nil, cfg, &pttState{}); got != "RPRT 0" {
		t.Errorf("dump_caps with nil rig = %q, want RPRT 0", got)
	}
}

var errOpaque = errOpaqueType("boom")

type errOpaqueType string

func (e errOpaqueType) Error() string { return string(e) }
