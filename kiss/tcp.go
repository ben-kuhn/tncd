package kiss

import (
	"net"
	"strconv"
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

func (t *tcpTransport) Open() error {
	conn, err := net.Dial("tcp", net.JoinHostPort(t.host, strconv.Itoa(t.port)))
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
