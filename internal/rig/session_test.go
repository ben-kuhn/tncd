package rig

import (
	"bytes"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// fakeRadio is a stateful fake: it answers from records it holds and APPLIES the
// writes it receives. Scripting a fixed reply sequence would assert the requests
// tncd makes; holding state lets the tests assert what the radio ends up in,
// which is the thing that actually matters to an operator.
type fakeRadio struct {
	ch *fakeChannel

	mu       sync.Mutex
	settings []byte
	bss      []byte
	chans    map[byte][]byte
	// rejectWrite makes the named command answer with a non-zero status, for
	// testing that a best-effort restore carries on past a failure.
	rejectWrite map[benshi.Command]bool
	writes      map[benshi.Command]int

	done chan struct{}
}

func newFakeRadio(t *testing.T, settings, bss []byte, chans map[byte][]byte) *fakeRadio {
	t.Helper()
	fr := &fakeRadio{
		ch:          newFakeChannel(),
		settings:    append([]byte{}, settings...),
		bss:         append([]byte{}, bss...),
		chans:       map[byte][]byte{},
		rejectWrite: map[benshi.Command]bool{},
		writes:      map[benshi.Command]int{},
		done:        make(chan struct{}),
	}
	for id, rec := range chans {
		fr.chans[id] = append([]byte{}, rec...)
	}
	go fr.serve()
	return fr
}

func (fr *fakeRadio) stop() { close(fr.done) }

func (fr *fakeRadio) serve() {
	seen := 0
	for {
		select {
		case <-fr.done:
			return
		default:
		}
		w := fr.ch.writes()
		if len(w) <= seen {
			time.Sleep(time.Millisecond)
			continue
		}
		m, err := benshi.DecodeMessage(w[seen][4:])
		seen++
		if err != nil {
			continue
		}
		fr.handle(m)
	}
}

func (fr *fakeRadio) handle(m benshi.Message) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	ok := []byte{0x00}
	rejected := []byte{0x05} // INVALID_PARAMETER
	switch m.Command {
	case benshi.CmdReadSettings:
		fr.ch.reply(m.Command, append(ok, fr.settings...))
	case benshi.CmdWriteSettings:
		fr.writes[m.Command]++
		if fr.rejectWrite[m.Command] {
			fr.ch.reply(m.Command, rejected)
			return
		}
		fr.settings = append([]byte{}, m.Body...)
		fr.ch.reply(m.Command, ok)
	case benshi.CmdReadBSSSettings:
		fr.ch.reply(m.Command, append(ok, fr.bss...))
	case benshi.CmdWriteBSSSettings:
		fr.writes[m.Command]++
		if fr.rejectWrite[m.Command] {
			fr.ch.reply(m.Command, rejected)
			return
		}
		fr.bss = append([]byte{}, m.Body...)
		fr.ch.reply(m.Command, ok)
	case benshi.CmdReadRFCh:
		rec, found := fr.chans[m.Body[0]]
		if !found {
			fr.ch.reply(m.Command, rejected)
			return
		}
		fr.ch.reply(m.Command, append(ok, rec...))
	case benshi.CmdWriteRFCh:
		fr.writes[m.Command]++
		fr.chans[m.Body[0]] = append([]byte{}, m.Body...)
		fr.ch.reply(m.Command, []byte{0x00, m.Body[0]})
	default:
		fr.ch.reply(m.Command, rejected)
	}
}

func (fr *fakeRadio) currentSettings(t *testing.T) benshi.Settings {
	t.Helper()
	fr.mu.Lock()
	defer fr.mu.Unlock()
	set, err := benshi.DecodeSettings(append([]byte{0x00}, fr.settings...))
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	return set
}

func (fr *fakeRadio) settingsBytes() []byte {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]byte{}, fr.settings...)
}

func (fr *fakeRadio) aprsOn(t *testing.T) bool {
	t.Helper()
	fr.mu.Lock()
	defer fr.mu.Unlock()
	rec, err := benshi.ParseBSSRec(append([]byte{0x00}, fr.bss...))
	if err != nil {
		t.Fatalf("ParseBSSRec: %v", err)
	}
	return rec.APRSEnabled()
}

func (fr *fakeRadio) writeCount(cmd benshi.Command) int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.writes[cmd]
}

func (fr *fakeRadio) setSettings(b []byte) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.settings = append([]byte{}, b...)
}

// --- fixtures ---------------------------------------------------------------

// mustSettings builds a settings record from the captured UV-PRO one, with
// channel_a and dual watch set as asked.
func mustSettings(t *testing.T, channelA byte, dc benshi.DoubleChannel) []byte {
	t.Helper()
	raw, err := hex.DecodeString("5104a60618013ce0a300002000000878000000000000")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := benshi.ParseSettingsRec(append([]byte{0x00}, raw...))
	if err != nil {
		t.Fatal(err)
	}
	return rec.WithChannelA(channelA).WithDoubleChannel(dc).Bytes()
}

func mustBSSFixture(t *testing.T, aprs bool) []byte {
	t.Helper()
	raw, err := hex.DecodeString("001c801e00000000415052530000000000000000000000000000000000000000000000000000" +
		"2f5b4b5530484e00")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := benshi.ParseBSSRec(append([]byte{0x00}, raw...))
	if err != nil {
		t.Fatal(err)
	}
	return rec.WithAPRSEnabled(aprs).Bytes()
}

// namedRecord is a programmed memory; vfoRecord is the unnamed scratch record.
func sessionChans() map[byte][]byte {
	return map[byte][]byte{
		5:   append([]byte{}, fakeNamedRecord...),
		252: append([]byte{}, fakeVFORecord...),
		// 251 deliberately absent: an unprogrammed slot the radio refuses,
		// which findVFOChannel must skip rather than treat as fatal.
	}
}

// newSessionRig wires a rig to a fake radio in the given state.
func newSessionRig(t *testing.T, settings, bss []byte) (*Rig, *fakeRadio) {
	t.Helper()
	fr := newFakeRadio(t, settings, bss, sessionChans())
	r := New(fr.ch, 2*time.Second, 0)
	t.Cleanup(func() {
		r.Close()
		fr.stop()
	})
	return r, fr
}

// --- tests ------------------------------------------------------------------

// TestAcquireSessionFixesAllThreeAndReleaseRestoresThem is the whole feature on
// the worst realistic starting state: dual watch on, radio parked on a named
// memory, APRS beaconing.
func TestAcquireSessionFixesAllThreeAndReleaseRestoresThem(t *testing.T) {
	before := mustSettings(t, 5, benshi.DoubleChannelB)
	r, fr := newSessionRig(t, before, mustBSSFixture(t, true))

	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	got := fr.currentSettings(t)
	if got.DoubleChannel != benshi.DoubleChannelOff {
		t.Errorf("dual watch = %d after acquire, want off", got.DoubleChannel)
	}
	if got.ChannelA != 252 {
		t.Errorf("channel_a = %d after acquire, want the VFO record 252", got.ChannelA)
	}
	if fr.aprsOn(t) {
		t.Error("APRS still enabled after acquire")
	}
	if !r.SessionHeld() {
		t.Error("SessionHeld is false after a successful acquire")
	}

	if err := r.ReleaseSession(); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if !bytes.Equal(fr.settingsBytes(), before) {
		t.Errorf("settings not restored:\n got  %s\n want %s",
			hex.EncodeToString(fr.settingsBytes()), hex.EncodeToString(before))
	}
	if !fr.aprsOn(t) {
		t.Error("APRS not re-enabled after release")
	}
	if r.SessionHeld() {
		t.Error("SessionHeld is still true after release")
	}
}

// TestAcquireSessionLeavesAReadyRadioAlone: a radio already in VFO mode with
// dual watch off and APRS off must not be written to at all. Writing it anyway
// would be a pointless NVRAM write on every session, and would make restore
// think it had displaced something.
func TestAcquireSessionLeavesAReadyRadioAlone(t *testing.T) {
	r, fr := newSessionRig(t, mustSettings(t, 252, benshi.DoubleChannelOff), mustBSSFixture(t, false))

	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	if n := fr.writeCount(benshi.CmdWriteSettings); n != 0 {
		t.Errorf("wrote settings %d times to an already-ready radio, want 0", n)
	}
	if n := fr.writeCount(benshi.CmdWriteBSSSettings); n != 0 {
		t.Errorf("wrote BSS %d times to an already-ready radio, want 0", n)
	}
	if err := r.ReleaseSession(); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if n := fr.writeCount(benshi.CmdWriteSettings); n != 0 {
		t.Errorf("release wrote settings %d times having changed nothing, want 0", n)
	}
}

// TestAcquireSessionIsIdempotent: tncd supports several simultaneous AX.25
// connections per port but settings are per radio, so callers refcount and the
// second acquire must be a no-op rather than saving the already-displaced state
// as the "original" -- which would make restore put the packet settings back.
func TestAcquireSessionIsIdempotent(t *testing.T) {
	before := mustSettings(t, 5, benshi.DoubleChannelB)
	r, fr := newSessionRig(t, before, mustBSSFixture(t, true))

	for i := 0; i < 3; i++ {
		if err := r.AcquireSession(); err != nil {
			t.Fatalf("AcquireSession #%d: %v", i+1, err)
		}
	}
	if n := fr.writeCount(benshi.CmdWriteSettings); n != 1 {
		t.Errorf("wrote settings %d times for 3 acquires, want 1", n)
	}
	if err := r.ReleaseSession(); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if !bytes.Equal(fr.settingsBytes(), before) {
		t.Errorf("settings not restored after repeated acquire:\n got  %s\n want %s",
			hex.EncodeToString(fr.settingsBytes()), hex.EncodeToString(before))
	}
}

// TestReleaseSessionLeavesOperatorChangesAlone: flipping dual watch back on from
// the front panel mid-session is a normal thing to do in a car. Restore must not
// silently undo it -- but must still put back the field the operator did NOT
// touch.
func TestReleaseSessionLeavesOperatorChangesAlone(t *testing.T) {
	r, fr := newSessionRig(t, mustSettings(t, 5, benshi.DoubleChannelOff), mustBSSFixture(t, false))
	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	// Acquire moved channel_a 5 -> 252 and left dual watch alone (already off).
	// Now the operator turns dual watch on mid-session.
	cur, err := benshi.ParseSettingsRec(append([]byte{0x00}, fr.settingsBytes()...))
	if err != nil {
		t.Fatal(err)
	}
	fr.setSettings(cur.WithDoubleChannel(benshi.DoubleChannelA).Bytes())

	if err := r.ReleaseSession(); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	got := fr.currentSettings(t)
	if got.DoubleChannel != benshi.DoubleChannelA {
		t.Errorf("release undid the operator's dual watch change: %d, want A", got.DoubleChannel)
	}
	if got.ChannelA != 5 {
		t.Errorf("channel_a = %d after release, want the operator's memory 5", got.ChannelA)
	}
}

// TestReleaseSessionPreservesUnrelatedOperatorEdits: restore patches the record
// as it reads NOW rather than writing the saved snapshot back, so a setting the
// operator changed mid-session that tncd never managed must survive.
func TestReleaseSessionPreservesUnrelatedOperatorEdits(t *testing.T) {
	r, fr := newSessionRig(t, mustSettings(t, 5, benshi.DoubleChannelB), mustBSSFixture(t, false))
	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	// Change a byte tncd does not model, the way a front-panel squelch or
	// volume tweak would.
	edited := fr.settingsBytes()
	const unrelated = 4
	edited[unrelated] ^= 0xFF
	fr.setSettings(edited)
	want := edited[unrelated]

	if err := r.ReleaseSession(); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	after := fr.settingsBytes()
	if after[unrelated] != want {
		t.Errorf("release clobbered an unmanaged byte: got %#02x, want %#02x",
			after[unrelated], want)
	}
	if got := fr.currentSettings(t); got.DoubleChannel != benshi.DoubleChannelB || got.ChannelA != 5 {
		t.Errorf("managed fields not restored: double_channel=%d channel_a=%d, want B and 5",
			got.DoubleChannel, got.ChannelA)
	}
}

// TestReleaseSessionCarriesOnPastAFailure: restore is best-effort by explicit
// decision and there is no power-cycle safety net, so a failed settings write is
// no reason to also abandon the APRS bit. Both must be attempted and the error
// must still surface.
func TestReleaseSessionCarriesOnPastAFailure(t *testing.T) {
	r, fr := newSessionRig(t, mustSettings(t, 5, benshi.DoubleChannelB), mustBSSFixture(t, true))
	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	fr.mu.Lock()
	fr.rejectWrite[benshi.CmdWriteSettings] = true
	fr.mu.Unlock()

	err := r.ReleaseSession()
	if err == nil {
		t.Fatal("ReleaseSession reported success despite a rejected settings write")
	}
	if !errors.Is(err, benshi.ErrRadioRejected) {
		t.Errorf("error = %v, want it to wrap ErrRadioRejected", err)
	}
	if !fr.aprsOn(t) {
		t.Error("APRS was not restored after the settings write failed -- restore gave up early")
	}
}

// TestAcquireSessionRefusesWhenNoVFORecordExists: without a scratch record there
// is nowhere to move a radio parked on a memory, and guessing would mean writing
// a programmed channel. It must refuse, and must not have changed anything.
func TestAcquireSessionRefusesWhenNoVFORecordExists(t *testing.T) {
	fr := newFakeRadio(t, mustSettings(t, 5, benshi.DoubleChannelOff), mustBSSFixture(t, false),
		map[byte][]byte{5: append([]byte{}, fakeNamedRecord...)})
	r := New(fr.ch, 2*time.Second, 0)
	t.Cleanup(func() { r.Close(); fr.stop() })

	err := r.AcquireSession()
	if !errors.Is(err, ErrNoVFOChannel) {
		t.Fatalf("AcquireSession: err = %v, want ErrNoVFOChannel", err)
	}
	if n := fr.writeCount(benshi.CmdWriteSettings); n != 0 {
		t.Errorf("wrote settings %d times before refusing, want 0", n)
	}
	if r.SessionHeld() {
		t.Error("SessionHeld is true after a refused acquire")
	}
}

// TestAcquireSessionThenSetFreqSucceedsOnAMemoryRadio is the point of managing
// channel_a at all: a QSY that SetFreq refuses outright beforehand must work
// once the session has moved the radio to its VFO.
func TestAcquireSessionThenSetFreqSucceedsOnAMemoryRadio(t *testing.T) {
	r, fr := newSessionRig(t, mustSettings(t, 5, benshi.DoubleChannelOff), mustBSSFixture(t, false))

	if err := r.SetFreq(145030000); !errors.Is(err, ErrChannelMode) {
		t.Fatalf("SetFreq before acquire: err = %v, want ErrChannelMode", err)
	}
	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	if err := r.SetFreq(145030000); err != nil {
		t.Fatalf("SetFreq after acquire: %v", err)
	}
	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000", hz)
	}

	if err := r.ReleaseSession(); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	// Back on the memory, and the VFO record it borrowed is back to the
	// frequency the operator had left there.
	if got := fr.currentSettings(t); got.ChannelA != 5 {
		t.Errorf("channel_a = %d after release, want 5", got.ChannelA)
	}
	fr.mu.Lock()
	vfo := append([]byte{}, fr.chans[252]...)
	fr.mu.Unlock()
	if !bytes.Equal(vfo, fakeVFORecord) {
		t.Errorf("VFO record not restored:\n got  %s\n want %s",
			hex.EncodeToString(vfo), hex.EncodeToString(fakeVFORecord))
	}
}

// TestAcquireSessionOrdersTheModeSwitchBeforeAnyQSY: SetFreq rewrites whatever
// record channel_a points at, so if a QSY could land between reading the old
// pointer and writing the new one it would overwrite the operator's memory --
// exactly what the name guard exists to prevent. Asserted by checking no
// WRITE_RF_CH reaches a named record across an acquire.
func TestAcquireSessionOrdersTheModeSwitchBeforeAnyQSY(t *testing.T) {
	r, fr := newSessionRig(t, mustSettings(t, 5, benshi.DoubleChannelOff), mustBSSFixture(t, false))
	if err := r.AcquireSession(); err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	if err := r.SetFreq(145030000); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}
	fr.mu.Lock()
	mem := append([]byte{}, fr.chans[5]...)
	fr.mu.Unlock()
	if !bytes.Equal(mem, fakeNamedRecord) {
		t.Errorf("the operator's memory channel was written:\n got  %s\n want %s",
			hex.EncodeToString(mem), hex.EncodeToString(fakeNamedRecord))
	}
}
