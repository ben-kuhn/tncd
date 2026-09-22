package kiss

import (
	"net"
	"strconv"
	"time"
)

type tcpTransport struct {
	host string
	port int
	conn net.Conn
}

// NewTCPTransport returns a Transport that connects via TCP.
func NewTCPTransport(host string, port int) Transport {
	return &tcpTransport{host: host, port: port}
}

// tcpDialTimeout bounds a TCP dial to a remote TNC. Without it a black-holed
// host can block Open for minutes (the OS connect timeout), holding the
// reconnect goroutine and starving the backoff chain. 10s is comfortably above
// any LAN/WAN connect the TNC requires.
const tcpDialTimeout = 10 * time.Second

func (t *tcpTransport) Open() error {
	d := net.Dialer{Timeout: tcpDialTimeout}
	conn, err := d.Dial("tcp", net.JoinHostPort(t.host, strconv.Itoa(t.port)))
	if err != nil {
		return err
	}
	t.conn = conn
	return nil
}

func (t *tcpTransport) Read(b []byte) (int, error) {
	return t.conn.Read(b)
}

func (t *tcpTransport) Write(b []byte) (int, error) {
	return t.conn.Write(b)
}

func (t *tcpTransport) Close() error {
	if t.conn != nil {
		return t.conn.Close()
	}
	return nil
}

// EnterKISS is a no-op for TCP transports.
func (t *tcpTransport) EnterKISS() error { return nil }

// ExitKISS is a no-op for TCP transports.
func (t *tcpTransport) ExitKISS() {}
