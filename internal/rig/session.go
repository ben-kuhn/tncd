package rig

import (
	"errors"
	"fmt"
	"log"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// A radio someone also uses for voice has two features on by default that
// actively break packet, and one mode that makes a QSY impossible:
//
//   - DUAL WATCH time-slices the one receiver between two frequencies, so
//     traffic on the other side eats part or all of an inbound frame. Harmful
//     whichever side is selected, so the answer is off rather than "pick a
//     side".
//   - APRS / position beaconing transmits on the radio's own schedule, which
//     collides with a transfer in progress.
//   - A radio parked on a NAMED MEMORY cannot be tuned at all: SetFreq refuses,
//     correctly, because tuning it would overwrite the operator's memory.
//
// AcquireSession fixes all three for the duration of a session and
// ReleaseSession puts them back. This is deliberately NOT done at startup:
// people run tncd idle for hours -- on a tablet in a car -- while using the
// radio for voice, and grabbing these settings then would break the radio for
// all the time no session exists.
//
// Writes reach NVRAM with no STORE_SETTINGS (measured 2026-10-01: the radio was
// left displaced and power-cycled, and came back up displaced). So there is no
// power-cycle safety net, restore is best-effort by explicit decision, and
// restore must run on every exit path the process can still act on.

// ErrNoVFOChannel reports that no usable VFO scratch record could be found, so
// a radio on a memory channel cannot be moved off it.
var ErrNoVFOChannel = errors.New("rig: no unnamed VFO channel record found")

// session records what AcquireSession displaced, and what it put there.
//
// Both halves are needed. The original is what to restore; what tncd WROTE is
// how restore tells "the operator has not touched this" from "the operator
// changed it back mid-session", which in a car is a perfectly normal thing to
// do and must not be silently undone.
type session struct {
	held bool

	origDoubleChannel benshi.DoubleChannel
	setDoubleChannel  bool

	origChannelA byte
	wantChannelA byte
	setChannelA  bool

	origAPRS bool
	setAPRS  bool
}

// AcquireSession puts the radio into a state where packet works, saving what it
// changed. It is idempotent: the second and later calls of an overlapping set of
// sessions do nothing, so callers can refcount on the 0->1 transition without
// coordinating with this package.
func (r *Rig) AcquireSession() error {
	r.mu.Lock()
	held := r.sess.held
	r.mu.Unlock()
	if held {
		return nil
	}

	rec, err := r.readSettings()
	if err != nil {
		return err
	}
	set := rec.Settings()
	var s session
	want := rec

	// Dual watch off first, so channel_a is unambiguously the active VFO
	// before its record is examined below.
	if set.DoubleChannel != benshi.DoubleChannelOff {
		s.origDoubleChannel = set.DoubleChannel
		s.setDoubleChannel = true
		want = want.WithDoubleChannel(benshi.DoubleChannelOff)
		log.Printf("rig: turning dual watch off for this session (was %s) -- "+
			"it shares the receiver between two frequencies and drops packets",
			dualWatchSideName(set.DoubleChannel))
	}

	// Then move off a memory channel if that is where the operator left it.
	// "Is this a memory" is decided by the same guards SetFreq applies, so
	// anything this leaves in place is something SetFreq will accept.
	switch usable, err := r.channelIsTunable(set.ChannelA); {
	case err != nil:
		return err
	case !usable:
		vfo, err := r.findVFOChannel()
		if err != nil {
			return err
		}
		s.origChannelA = set.ChannelA
		s.wantChannelA = vfo
		s.setChannelA = true
		want = want.WithChannelA(vfo)
		log.Printf("rig: switching the A VFO from channel %d to the VFO record %d for this session",
			set.ChannelA, vfo)
	}

	if !want.Equal(rec) {
		if err := r.writeSettings(want); err != nil {
			return err
		}
	}

	// APRS lives in a different record, so it is a second write whether or
	// not the first one happened.
	bss, err := r.readBSS()
	if err != nil {
		// The settings write already happened; roll it back rather than
		// leave the radio half-configured by a session that never started.
		r.mu.Lock()
		r.sess = s
		r.sess.held = true
		r.mu.Unlock()
		return errors.Join(err, r.ReleaseSession())
	}
	if bss.APRSEnabled() {
		s.origAPRS = true
		s.setAPRS = true
		if err := r.writeBSS(bss.WithAPRSEnabled(false)); err != nil {
			r.mu.Lock()
			r.sess = s
			r.sess.held = true
			r.mu.Unlock()
			return errors.Join(err, r.ReleaseSession())
		}
		log.Printf("rig: disabling APRS beaconing for this session -- " +
			"it transmits on its own schedule and collides with a transfer")
	}

	s.held = true
	r.mu.Lock()
	r.sess = s
	r.mu.Unlock()
	return nil
}

// ReleaseSession puts back everything AcquireSession changed, plus the channel
// record SetFreq displaced, and reports whether anything was restored.
//
// Each field is restored ONLY if the radio still holds the value tncd wrote.
// The operator may have flipped dual watch back on from the front panel
// mid-session; blindly writing a snapshot back would silently undo them.
// Restoring field by field onto the record as it reads NOW, rather than writing
// the saved record wholesale, is the same principle one level down: an
// unrelated setting they changed during the session survives.
//
// Every step runs even if an earlier one failed -- restore is best-effort, and
// there is no power-cycle safety net, so a failure on the dual-watch write is no
// reason to also abandon the operator's memory channel. Errors are joined.
func (r *Rig) ReleaseSession() error {
	r.mu.Lock()
	s := r.sess
	r.mu.Unlock()

	// The channel-record restore is independent of the settings record and is
	// wanted even for a session that only ever QSYed.
	_, tdErr := r.Teardown()
	errs := []error{tdErr}

	if s.held {
		errs = append(errs, r.restoreSettings(s), r.restoreBSS(s))
		r.mu.Lock()
		r.sess = session{}
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

// SessionHeld reports whether managed settings are currently displaced. Callers
// use it to decide whether a shutdown path has anything to put back.
func (r *Rig) SessionHeld() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sess.held
}

func (r *Rig) restoreSettings(s session) error {
	if !s.setDoubleChannel && !s.setChannelA {
		return nil
	}
	cur, err := r.readSettings()
	if err != nil {
		return err
	}
	set := cur.Settings()
	want := cur
	if s.setDoubleChannel {
		if set.DoubleChannel != benshi.DoubleChannelOff {
			log.Printf("rig: leaving dual watch as the operator set it (%s) rather than restoring %s",
				dualWatchSideName(set.DoubleChannel), dualWatchSideName(s.origDoubleChannel))
		} else {
			want = want.WithDoubleChannel(s.origDoubleChannel)
		}
	}
	if s.setChannelA {
		if set.ChannelA != s.wantChannelA {
			log.Printf("rig: leaving the A VFO on channel %d as the operator set it rather than restoring %d",
				set.ChannelA, s.origChannelA)
		} else {
			want = want.WithChannelA(s.origChannelA)
		}
	}
	if want.Equal(cur) {
		return nil
	}
	return r.writeSettings(want)
}

func (r *Rig) restoreBSS(s session) error {
	if !s.setAPRS {
		return nil
	}
	cur, err := r.readBSS()
	if err != nil {
		return err
	}
	if cur.APRSEnabled() {
		// The operator turned it back on themselves.
		return nil
	}
	return r.writeBSS(cur.WithAPRSEnabled(s.origAPRS))
}

// channelIsTunable reports whether SetFreq would accept the record at id, using
// the same three guards it applies: a name means a programmed memory, an id
// below the VFO floor means a memory bank entry, and a split means a repeater
// memory whatever its id or name.
func (r *Rig) channelIsTunable(id byte) (bool, error) {
	ch, err := r.readChannel(id)
	if err != nil {
		return false, err
	}
	return ch.Name() == "" && int(ch.ID()) >= r.vfoMin && ch.TXFreqHz() == ch.RXFreqHz(), nil
}

// findVFOChannel locates the unnamed scratch record the radio uses for frequency
// mode, by scanning from the VFO floor upward and taking the first record SetFreq
// would accept.
//
// Discovering it beats hardcoding 252: the floor is already configurable because
// other Benshi variants are not confirmed to number their VFOs the same way, and
// a record found by SetFreq's own criteria is one SetFreq will then accept. A
// record the radio refuses to read is skipped rather than treated as fatal --
// unprogrammed slots in this range are expected.
func (r *Rig) findVFOChannel() (byte, error) {
	for id := r.vfoMin; id <= 255; id++ {
		ok, err := r.channelIsTunable(byte(id))
		if err != nil {
			if errors.Is(err, benshi.ErrRadioRejected) {
				continue
			}
			return 0, err
		}
		if ok {
			return byte(id), nil
		}
	}
	return 0, fmt.Errorf("%w at or above channel %d -- every record there is named, split, "+
		"or unreadable, so the radio cannot be moved off its memory channel; "+
		"set vfo_channel_min for this port if its VFO lives elsewhere",
		ErrNoVFOChannel, r.vfoMin)
}

func (r *Rig) readSettings() (benshi.SettingsRec, error) {
	body, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		return benshi.SettingsRec{}, err
	}
	return benshi.ParseSettingsRec(body)
}

func (r *Rig) writeSettings(rec benshi.SettingsRec) error {
	body, err := r.request(benshi.CmdWriteSettings, rec.Bytes())
	if err != nil {
		return err
	}
	if len(body) < 1 || body[0] != 0 {
		return fmt.Errorf("%w (WRITE_SETTINGS status %d)", benshi.ErrRadioRejected, status(body))
	}
	return nil
}

func (r *Rig) readBSS() (benshi.BSSRec, error) {
	body, err := r.request(benshi.CmdReadBSSSettings, nil)
	if err != nil {
		return benshi.BSSRec{}, err
	}
	return benshi.ParseBSSRec(body)
}

func (r *Rig) writeBSS(rec benshi.BSSRec) error {
	body, err := r.request(benshi.CmdWriteBSSSettings, rec.Bytes())
	if err != nil {
		return err
	}
	if len(body) < 1 || body[0] != 0 {
		return fmt.Errorf("%w (WRITE_BSS_SETTINGS status %d)", benshi.ErrRadioRejected, status(body))
	}
	return nil
}

// readChannel reads one channel record.
func (r *Rig) readChannel(id byte) (benshi.RFCh, error) {
	body, err := r.request(benshi.CmdReadRFCh, []byte{id})
	if err != nil {
		return benshi.RFCh{}, err
	}
	if len(body) < 1 || body[0] != 0 {
		return benshi.RFCh{}, fmt.Errorf("%w (READ_RF_CH channel %d status %d)",
			benshi.ErrRadioRejected, id, status(body))
	}
	return benshi.ParseRFCh(body[1:])
}

// status is a reply's status byte, or 255 for a reply too short to have one, so
// an error message never indexes an empty slice.
func status(body []byte) byte {
	if len(body) < 1 {
		return 255
	}
	return body[0]
}
