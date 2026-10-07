package forward

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
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
				conn.Write(socks5Reply(1)) // general failure
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
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { io.Copy(remote, conn); wg.Done() }()
			go func() { io.Copy(conn, remote); wg.Done() }()
			wg.Wait()
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
		return "", fmt.Errorf("only CONNECT supported")
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
