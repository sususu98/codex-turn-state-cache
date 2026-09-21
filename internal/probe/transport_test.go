package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSOCKS5HUsesDomainAndAuthentication(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	result := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			result <- "accept"
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		header := make([]byte, 2)
		if _, err = io.ReadFull(c, header); err != nil {
			result <- "greeting"
			return
		}
		methods := make([]byte, int(header[1]))
		io.ReadFull(c, methods)
		c.Write([]byte{5, 2})
		if _, err = io.ReadFull(c, header); err != nil {
			result <- "auth"
			return
		}
		user := make([]byte, int(header[1]))
		io.ReadFull(c, user)
		length := make([]byte, 1)
		io.ReadFull(c, length)
		pass := make([]byte, int(length[0]))
		io.ReadFull(c, pass)
		if string(user) != "user" || string(pass) != "pass" {
			result <- "credentials"
			return
		}
		c.Write([]byte{1, 0})
		connect := make([]byte, 4)
		if _, err = io.ReadFull(c, connect); err != nil || connect[3] != 3 {
			result <- "not remote DNS"
			return
		}
		io.ReadFull(c, length)
		domain := make([]byte, int(length[0]))
		io.ReadFull(c, domain)
		port := make([]byte, 2)
		io.ReadFull(c, port)
		result <- string(domain)
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // Refuse; no connection to upstream.
	}()
	rt, err := newTransport("socks5h://user:pass@" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if response, err := rt.RoundTrip(req); err == nil {
		response.Body.Close()
		t.Fatal("refused proxy succeeded")
	}
	select {
	case domain := <-result:
		if domain != "chatgpt.com" {
			t.Fatal(domain)
		}
	case <-ctx.Done():
		t.Fatal("no domain observed")
	}
}
func TestProxyStallHonorsCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		<-done
	}()
	rt, err := newTransport("socks5h://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	started := time.Now()
	if response, err := rt.RoundTrip(req); err == nil {
		response.Body.Close()
		t.Fatal("stalled proxy succeeded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled dialing still blocked")
	}
}
func TestInvalidProxyAndDestinationFailClosed(t *testing.T) {
	if _, err := newTransport("malformed proxy"); err == nil {
		t.Fatal("invalid route accepted")
	}
	if _, err := newTransport(""); err == nil {
		t.Fatal("implicit route accepted")
	}
	rt, err := newTransport("direct")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	if response, err := rt.RoundTrip(req); err == nil {
		response.Body.Close()
		t.Fatal("unapproved destination accepted")
	}
}
