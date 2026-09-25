package benshi

import (
	"encoding/binary"
	"errors"
)

// CommandGroup selects which command namespace an id belongs to.
type CommandGroup uint16

const (
	GroupBasic    CommandGroup = 2
	GroupExtended CommandGroup = 10
)

// Command is a 15-bit command id within a group.
type Command uint16

// The v1 subset. The full protocol has roughly fifty basic commands; only
// those needed for status and VFO QSY are defined here.
//
// CmdWriteRFCh is the one command in this set that changes stored radio
// state, and it is here because on this hardware there is no other way to
// QSY. The radio has no scratch frequency register: each VFO is an INDEX
// into the channel table, and "frequency mode" is a VFO pointed at an
// unnamed record near the top of it.
//
// FREQ_MODE_SET_PAR (35) and FREQ_MODE_GET_STATUS (36) read like the right
// commands for this and are deliberately absent. They address a separate
// frequency-mode register that real UV-PRO firmware never promotes to the
// operating frequency: on 2026-09-24 the radio's dial was turned and channel
// record 252 followed it while FREQ_MODE_GET_STATUS went on reporting a
// stale value this code had written minutes earlier, with the radio
// transmitting on the channel record's frequency throughout. Defining them
// again would invite the same wrong turn.
//
// Callers must guard the write; see internal/rig's SetFreq, which refuses
// any record carrying a name so a real memory channel can never be the
// target.
const (
	CmdGetDevInfo        Command = 4
	CmdEventNotification Command = 9
	CmdReadSettings      Command = 10
	CmdReadRFCh          Command = 13
	CmdWriteRFCh         Command = 14
	CmdGetHTStatus       Command = 20
	CmdDoProgFunc        Command = 66
)

// ErrShortMessage reports a message too short to hold a header.
var ErrShortMessage = errors.New("benshi: message shorter than 4-byte header")

// replyBit is the top bit of the third/fourth header byte pair.
const replyBit = 0x8000

// Message is one protocol message: a 4-byte header and an opaque body.
//
// Bodies are left as bytes here; each command's body layout is decoded by the
// command-specific helpers, which keeps this type free of per-command
// knowledge.
type Message struct {
	Group   CommandGroup
	IsReply bool
	Command Command
	Body    []byte
}

// Bytes serializes the message: group big-endian, then the reply bit packed
// into the top bit of the 16-bit command word.
func (m Message) Bytes() []byte {
	out := make([]byte, msgHeaderLen, msgHeaderLen+len(m.Body))
	binary.BigEndian.PutUint16(out[0:2], uint16(m.Group))
	cmd := uint16(m.Command) &^ replyBit
	if m.IsReply {
		cmd |= replyBit
	}
	binary.BigEndian.PutUint16(out[2:4], cmd)
	return append(out, m.Body...)
}

// DecodeMessage parses a message from the Data field of a Frame.
func DecodeMessage(b []byte) (Message, error) {
	if len(b) < msgHeaderLen {
		return Message{}, ErrShortMessage
	}
	cmd := binary.BigEndian.Uint16(b[2:4])
	m := Message{
		Group:   CommandGroup(binary.BigEndian.Uint16(b[0:2])),
		IsReply: cmd&replyBit != 0,
		Command: Command(cmd &^ replyBit),
	}
	if len(b) > msgHeaderLen {
		m.Body = make([]byte, len(b)-msgHeaderLen)
		copy(m.Body, b[msgHeaderLen:])
	}
	return m, nil
}
