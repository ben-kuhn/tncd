package api

import (
	"encoding/json"
	"testing"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

func TestEncodeFrameUIHasBase64Data(t *testing.T) {
	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0,
		Src: ax25.Address{Call: "KU0HN"}, Dst: ax25.Address{Call: "CQ"},
		Via: []ax25.Address{{Call: "W0NE", SSID: 7}}, Info: []byte("hi")}
	b, _ := json.Marshal(encodeFrame(0, f))
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["type"] != "UI" || m["from"] != "KU0HN" || m["to"] != "CQ" || m["data"] != "aGk=" {
		t.Fatalf("UI encode wrong: %s", b)
	}
	if m["len"].(float64) != 2 {
		t.Fatalf("len wrong: %s", b)
	}
	via := m["via"].([]any)
	if len(via) != 1 || via[0] != "W0NE-7" {
		t.Fatalf("via wrong: %s", b)
	}
}

func TestEncodeFrameSFrameNoData(t *testing.T) {
	f := &ax25.Frame{Type: ax25.RR, Src: ax25.Address{Call: "A"}, Dst: ax25.Address{Call: "B"}}
	b, _ := json.Marshal(encodeFrame(0, f))
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["data"]; ok {
		t.Fatalf("S-frame must omit data: %s", b)
	}
	if _, ok := m["pid"]; ok {
		t.Fatalf("S-frame must omit pid: %s", b)
	}
}
