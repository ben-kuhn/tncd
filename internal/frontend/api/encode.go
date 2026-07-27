package api

import (
	"encoding/base64"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

// frameEvent is the JSON body of an rx/tx SSE event.
type frameEvent struct {
	Port int      `json:"port"`
	From string   `json:"from"`
	To   string   `json:"to"`
	Type string   `json:"type"`
	PID  *uint8   `json:"pid,omitempty"`
	Len  int      `json:"len"`
	Via  []string `json:"via"`
	Data string   `json:"data,omitempty"`
}

// encodeFrame turns a decoded AX.25 frame into an rx/tx event body. PID and
// base64 Data are included only for info-bearing frames (I, UI).
func encodeFrame(port int, f *ax25.Frame) frameEvent {
	via := make([]string, 0, len(f.Via))
	for _, a := range f.Via {
		via = append(via, a.String())
	}
	ev := frameEvent{
		Port: port, From: f.Src.String(), To: f.Dst.String(),
		Type: f.Type.String(), Len: len(f.Info), Via: via,
	}
	if f.Type == ax25.I || f.Type == ax25.UI {
		pid := f.PID
		ev.PID = &pid
		ev.Data = base64.StdEncoding.EncodeToString(f.Info)
	}
	return ev
}

type connectEvent struct {
	Port     int    `json:"port"`
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	Incoming bool   `json:"incoming"`
}

type disconnectEvent struct {
	Port   int    `json:"port"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
}
