// Package vsock dials into a Firecracker microVM's vsock channel via the
// per-VM Unix-domain socket that Firecracker exposes on the host.
//
// Firecracker does not register guest CIDs with the host's vhost_vsock
// subsystem; instead it multiplexes vsock connections over a UDS using a
// small text protocol:
//
//	host  → fc:   CONNECT <port>\n
//	fc    → host: OK <host_side_port>\n
//	(then the stream is a transparent pipe to the guest's vsock port)
//
// This package implements the host side of that handshake.
package vsock

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Dial connects to a guest vsock port via Firecracker's UDS proxy.
// The returned net.Conn is a transparent stream to whatever is listening
// on (guest CID, port) inside the VM. Caller closes it.
func Dial(udsPath string, port int) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("vsock dial %s: %w", udsPath, err)
	}

	// Bound the handshake — a hung Firecracker shouldn't wedge callers.
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT %d: %w", port, err)
	}

	// Firecracker replies with "OK <host_port>\n" on success, or closes the
	// socket / returns nothing on failure. Read byte-by-byte so we don't
	// accidentally buffer past the newline and lose application bytes that
	// the caller will read next.
	reply, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock read OK: %w", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: unexpected reply %q", reply)
	}

	// Clear the deadline now that the handshake is done — the application
	// stream may be long-lived (streaming exec output).
	conn.SetDeadline(time.Time{})
	return conn, nil
}

func readLine(r io.Reader) (string, error) {
	var (
		buf [1]byte
		out strings.Builder
	)
	for {
		_, err := r.Read(buf[:])
		if err != nil {
			return out.String(), err
		}
		if buf[0] == '\n' {
			return strings.TrimRight(out.String(), "\r"), nil
		}
		out.WriteByte(buf[0])
		if out.Len() > 256 {
			return out.String(), fmt.Errorf("handshake line too long")
		}
	}
}
