package kiss

import (
	"errors"
	"io"
	"testing"
)

// fakeRWC is a minimal io.ReadWriteCloser stand-in so tests can assert
// ControlChannelFor returns exactly the value a transport hands back, without
// depending on any real transport's read/write semantics.
type fakeRWC struct{}

func (f *fakeRWC) Read(p []byte) (int, error)  { return 0, io.EOF }
func (f *fakeRWC) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeRWC) Close() error                { return nil }

// A transport with no control channel must be detectable without a type switch
// at every call site.
func TestControlChannelForReportsUnsupported(t *testing.T) {
	var tr Transport = &tcpTransport{}
	if _, err := ControlChannelFor(tr); !errors.Is(err, ErrNoControlChannel) {
		t.Errorf("err = %v, want ErrNoControlChannel", err)
	}
}

type fakeControlTransport struct {
	tcpTransport
	ch *fakeRWC
}

func (f *fakeControlTransport) ControlChannel() (io.ReadWriteCloser, error) { return f.ch, nil }

func TestControlChannelForReturnsChannel(t *testing.T) {
	f := &fakeControlTransport{ch: &fakeRWC{}}
	got, err := ControlChannelFor(f)
	if err != nil {
		t.Fatalf("ControlChannelFor: %v", err)
	}
	if got != f.ch {
		t.Error("returned channel is not the transport's channel")
	}
}
