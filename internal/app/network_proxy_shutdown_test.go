package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func shutdownTestProxy(t *testing.T, dial proxyDialer) *networkProxy {
	t.Helper()
	policy, err := parseNetworkPolicy([]string{"https://allowed.example"})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := startNetworkProxyWithDial(policy, dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

func shutdownTestClient(t *testing.T, proxy *networkProxy) net.Conn {
	t.Helper()
	client, err := net.DialTimeout("tcp", proxy.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = io.WriteString(client, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, err=%v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return client
}

func shutdownTestHello(t *testing.T) []byte {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tls.Client(client, &tls.Config{ServerName: "allowed.example"}).Handshake()
		_ = client.Close()
	}()
	_ = server.SetDeadline(time.Now().Add(time.Second))
	hello, _, _, err := readTLSClientHello(bufio.NewReader(server))
	_ = server.Close()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	return hello
}

func awaitProxyClose(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy shutdown did not complete")
	}
}

func TestHTTPProxyCloseDuringClientHello(t *testing.T) {
	proxy := shutdownTestProxy(t, func(context.Context, string, string) (net.Conn, error) {
		t.Error("dial called before ClientHello")
		return nil, net.ErrClosed
	})
	client := shutdownTestClient(t, proxy)
	done := make(chan error, 1)
	go func() { done <- proxy.Close() }()
	awaitProxyClose(t, done)
	var data [1]byte
	if _, err := client.Read(data[:]); err != io.EOF {
		t.Fatalf("client left open: %v", err)
	}
	if proxy.beginTunnel() {
		proxy.tunnels.Done()
		t.Fatal("shutdown accepted another tunnel")
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPProxyCloseWaitsForLateDial(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	upstream, peer := net.Pipe()
	defer peer.Close()
	defer upstream.Close()
	proxy := shutdownTestProxy(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		// Simulate a successful dial racing with cancellation.
		return upstream, nil
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	client := shutdownTestClient(t, proxy)
	if _, err := client.Write(shutdownTestHello(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dial was not entered")
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- proxy.Close() }()
	go func() { second <- proxy.Close() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel dial context")
	}
	for _, done := range []<-chan error{first, second} {
		select {
		case <-done:
			t.Fatal("Close returned while admitted dial was still running")
		default:
		}
	}
	unblock()
	awaitProxyClose(t, first)
	awaitProxyClose(t, second)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := peer.Read(data[:]); err != io.EOF {
		t.Fatalf("late dial connection escaped cleanup: %v", err)
	}
}

func TestHTTPProxyCloseDuringTunnelRelay(t *testing.T) {
	upstream, peer := net.Pipe()
	defer upstream.Close()
	defer peer.Close()
	proxy := shutdownTestProxy(t, func(context.Context, string, string) (net.Conn, error) { return upstream, nil })
	client := shutdownTestClient(t, proxy)
	hello := shutdownTestHello(t)
	if _, err := client.Write(hello); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(peer, make([]byte, len(hello))); err != nil {
		t.Fatal(err)
	}
	// Traffic in both directions confirms the handler reached its relay.
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(peer, make([]byte, len("request"))); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, len("response"))); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Close() }()
	awaitProxyClose(t, done)
	var data [1]byte
	for _, connection := range []net.Conn{client, peer} {
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := connection.Read(data[:]); err != io.EOF {
			t.Fatalf("relay endpoint remains open: %v", err)
		}
	}
}

func TestHTTPProxyRejectsLateConnectionRegistration(t *testing.T) {
	proxy := shutdownTestProxy(t, nil)
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()
	if proxy.track(connection) {
		t.Fatal("closed proxy accepted a connection")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := peer.Read(data[:]); err != io.EOF {
		t.Fatalf("rejected connection left open: %v", err)
	}
}
