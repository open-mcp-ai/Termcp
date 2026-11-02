package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// serveSOCKS5 accepts connections and proxies them through the dialer function.
func serveSOCKS5(ctx context.Context, ln net.Listener, dialer func(target string) (net.Conn, error)) {
	defer ln.Close()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := ln.Accept()
		if err != nil {
			slog.Error("SOCKS5 accept failed", "err", err)
			return
		}
		go func() {
			defer conn.Close()
			// Register BEFORE the handshake read: a peer that connects and never
			// sends the SOCKS5 greeting would otherwise pin this goroutine and its
			// connection forever once the forward is deleted (ctx cancelled).
			stopGreeting := context.AfterFunc(ctx, func() { conn.Close() })
			defer stopGreeting()
			// Read SOCKS5 request without sending response yet
			target, err := socks5ReadRequest(conn)
			if err != nil {
				slog.Error("SOCKS5 read request failed", "err", err)
				return
			}
			// Dial target first
			remote, err := dialer(target)
			if err != nil {
				slog.Error("SOCKS5 dial failed", "target", target, "err", err)
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_, _ = conn.Write(socks5Reply(socks5ErrCode(err)))
				return
			}
			defer remote.Close()
			// Send success response AFTER dial succeeds
			if _, err := conn.Write(socks5Reply(0)); err != nil {
				slog.Error("SOCKS5 reply write failed", "err", err)
				return
			}
			// Close both sides when the forward is deleted (no lingering goroutine).
			stopOnCancel := context.AfterFunc(ctx, func() {
				conn.Close()
				remote.Close()
			})
			proxyBoth(conn, remote)
			stopOnCancel()
		}()
	}
}

func socks5ReadRequest(conn net.Conn) (string, error) {
	buf := make([]byte, 263)
	// Read auth methods
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return "", err
	}
	if buf[0] != 5 {
		return "", fmt.Errorf("not SOCKS5")
	}
	nmethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil {
		return "", err
	}
	// Reply: no auth
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	// Read request
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return "", err
	}
	if buf[1] != 1 {
		// RFC 1928: 0x07 = Command not supported. Send the reply so the caller
		// learns why it was refused, rather than dropping the connection with
		// no explanation.
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = conn.Write(socks5Reply(7))
		return "", fmt.Errorf("only CONNECT supported (got command %d)", buf[1])
	}
	var host string
	switch buf[3] {
	case 1: // IPv4
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return "", err
		}
		host = net.IP(buf[:4]).String()
	case 3: // Domain
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return "", err
		}
		l := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:l]); err != nil {
			return "", err
		}
		host = string(buf[:l])
	case 4: // IPv6
		if _, err := io.ReadFull(conn, buf[:16]); err != nil {
			return "", err
		}
		host = net.IP(buf[:16]).String()
	default:
		// RFC 1928: 0x08 = Address type not supported.
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = conn.Write(socks5Reply(8))
		return "", fmt.Errorf("unsupported address type %d", buf[3])
	}
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return "", err
	}
	port := int(buf[0])<<8 | int(buf[1])
	return net.JoinHostPort(host, fmt.Sprintf("%d", port)), nil
}

func socks5Reply(rep byte) []byte {
	// Always return IPv4 0.0.0.0:0 for simplicity
	return []byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0}
}

// socks5ErrCode translates a dial failure into RFC 1928's reply field so the caller
// gets an answer it can act on instead of general failure for everything. The codes
// that carry real information:
//
//	0x02 connection not allowed by ruleset
//	0x03 network unreachable
//	0x04 host unreachable
//	0x05 connection refused
//	0x06 TTL expired / timed out
//	0x07 command not supported
//	0x08 address type not supported
//
// The interesting half is SSH's: when the far end refuses to open the channel it
// answers with an OpenChannelError carrying a RejectionReason, which distinguishes
// "administratively prohibited" from "connection failed" -- two failures a caller
// must not confuse, because the first is permanent and the second is worth retrying.
func socks5ErrCode(err error) byte {
	if err == nil {
		return 0
	}
	var oce *ssh.OpenChannelError
	if errors.As(err, &oce) {
		switch oce.Reason {
		case ssh.Prohibited:
			return 0x02
		case ssh.ConnectionFailed:
			return 0x05
		case ssh.UnknownChannelType:
			return 0x07
		case ssh.ResourceShortage:
			return 0x01
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return 0x04
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return 0x05
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return 0x03
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return 0x04
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return 0x06
	}
	return 0x01
}
