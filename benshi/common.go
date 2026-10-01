package benshi

import (
	"errors"
	"fmt"
)

// freqMask is the 30-bit frequency field shared by every frequency word in
// this protocol; the top 2 bits carry modulation.
const freqMask = 0x3FFFFFFF

// ErrShortBody reports a reply body too short for its command.
var ErrShortBody = errors.New("benshi: reply body too short")

// ErrRadioRejected reports a reply whose status byte is non-zero: the radio
// understood the command and refused it. Distinct from ErrShortBody, which
// means the reply was truncated -- conflating the two sent operators looking
// at the wrong thing entirely.
var ErrRadioRejected = errors.New("benshi: radio rejected the command")

// errRejected formats a non-zero reply status for a named command. Every
// command-specific decoder wraps ErrRadioRejected the same way, so the status
// code travels with the name of the command that produced it.
func errRejected(cmd string, status byte) error {
	return fmt.Errorf("%w (%s status %d)", ErrRadioRejected, cmd, status)
}
