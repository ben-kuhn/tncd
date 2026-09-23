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
// those needed for read-only status and VFO-mode QSY are defined here, so that
// no channel-writing or NVRAM-persisting command is reachable from this code.
const (
	CmdGetDevInfo        Command = 4
	CmdEventNotification Command = 9
	CmdReadSettings      Command = 10
	CmdReadRFCh          Command = 13
	CmdGetHTStatus       Command = 20
	CmdFreqModeSetPar    Command = 35
	CmdFreqModeGetStatus Command = 36
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
