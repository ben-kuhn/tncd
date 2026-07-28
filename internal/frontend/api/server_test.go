package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/ax25"
)

type fakeSender struct{ online bool }
func (fakeSender) Send([]byte)               {}
func (fakeSender) SendCommand(uint8, []byte) {}
func (f fakeSender) Online() bool            { return f.online }

func newBridge(t *testing.T, eng *engine.Engine) *bridge.Bridge {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := bridge.New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	bridge.InjectPorts(b, eng, params, []bridge.PortSender{fakeSender{online: true}})
	return b
}

func TestStatusEndpoint(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Version string              `json:"version"`
		Ports   []bridge.PortStatus `json:"ports"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if len(st.Ports) != 1 || !st.Ports[0].Online {
		t.Fatalf("status ports wrong: %+v", st)
	}
}

func TestEventsSSE(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give the handler a moment to register, then drive an RX frame on the loop.
	time.Sleep(50 * time.Millisecond)
	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0, Src: ax25.Address{Call: "A"}, Dst: ax25.Address{Call: "B"}, Info: []byte("hi")}
	eng.Do(func() { srv.OnRXFrame(0, f) })

	// Read one SSE event.
	// Read lines until the first complete event. The RX frame was pushed above;
	// the http client blocks on ReadString until the server flushes it. A
	// cancelable watchdog closes the body after 3s if nothing arrives, bounding
	// the test; it is stopped as soon as the event is captured.
	watchdog := time.AfterFunc(3*time.Second, func() { resp.Body.Close() })
	rd := bufio.NewReader(resp.Body)
	var evType, data string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "event: ") {
			evType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	watchdog.Stop()
	if evType != "rx" || !strings.Contains(data, `"from":"A"`) {
		t.Fatalf("SSE event wrong: type=%q data=%q", evType, data)
	}
}

// closeOnLoop closes the server on the engine loop (Close touches loop state).
func closeOnLoop(eng *engine.Engine, srv *Server) {
	done := make(chan struct{})
	eng.Do(func() { srv.Close(); close(done) })
	<-done
}

func TestServeUIEnabledServesRoot(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, marker := range []string{"tncd monitor", `id="ports"`, `id="connections"`, `id="events"`, "monitor.js"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("root page missing marker %q", marker)
		}
	}
	// /api still works with the UI on
	r2, err2 := http.Get("http://" + srv.Addr() + "/api/status")
	if err2 != nil {
		t.Fatal(err2)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("GET /api/status = %d, want 200", r2.StatusCode)
	}
}

func TestServeUIDisabledRootIs404(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET / with serve_ui=false = %d, want 404", resp.StatusCode)
	}
	r2, err2 := http.Get("http://" + srv.Addr() + "/api/status") // API still served
	if err2 != nil {
		t.Fatal(err2)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("GET /api/status = %d, want 200", r2.StatusCode)
	}
}

func TestUIEmbedHasIndex(t *testing.T) {
	if _, err := uiFS.ReadFile("ui/index.html"); err != nil {
		t.Fatalf("embedded ui/index.html missing: %v", err)
	}
}
