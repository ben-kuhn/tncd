package benshi

import (
	"bytes"
	"testing"
)

func TestMessageBytesGolden(t *testing.T) {
	m := Message{Group: GroupBasic, IsReply: false, Command: CmdReadRFCh}
	got := m.Bytes()
	// group 2 big-endian, then reply bit clear | command 13.
	want := []byte{0x00, 0x02, 0x00, 0x0D}
	if !bytes.Equal(got, want) {
		t.Errorf("Bytes() = % X, want % X", got, want)
	}
}

func TestMessageReplyBitIsTopBit(t *testing.T) {
	m := Message{Group: GroupBasic, IsReply: true, Command: CmdReadRFCh}
	got := m.Bytes()
	want := []byte{0x00, 0x02, 0x80, 0x0D}
	if !bytes.Equal(got, want) {
		t.Errorf("Bytes() = % X, want % X", got, want)
	}
}

func TestDecodeMessageRoundTrip(t *testing.T) {
	in := Message{Group: GroupBasic, IsReply: true, Command: CmdGetHTStatus, Body: []byte{0x00, 0x11}}
	got, err := DecodeMessage(in.Bytes())
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if got.Group != in.Group || got.IsReply != in.IsReply || got.Command != in.Command {
		t.Errorf("header = %+v, want %+v", got, in)
	}
	if !bytes.Equal(got.Body, in.Body) {
		t.Errorf("Body = % X, want % X", got.Body, in.Body)
	}
}

func TestDecodeMessageRejectsShort(t *testing.T) {
	for _, b := range [][]byte{nil, {0x00}, {0x00, 0x02, 0x00}} {
		if _, err := DecodeMessage(b); err == nil {
			t.Errorf("DecodeMessage(% X) = nil error, want error", b)
		}
	}
}
