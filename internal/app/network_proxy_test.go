package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHTTPProxyEmptyAllowlistDeniesHTTPAndCONNECT(t *testing.T) {
	handler := &networkProxyHandler{policy: networkPolicy{}}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "http://unlisted.example/artifact", nil),
		httptest.NewRequest(http.MethodConnect, "unlisted.example:443", nil),
	} {
		t.Run(request.Method, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("empty allowlist returned status %d, want 403", response.Code)
			}
		})
	}
}

func TestHTTPProxyEnforcesAllowlistAndOutboundHost(t *testing.T) {
	var receivedHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedHost = request.Host
		_, _ = io.WriteString(response, "allowed")
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	_, portText, _ := net.SplitHostPort(upstreamURL.Host)
	policy, err := parseNetworkPolicy([]string{"http://allowed.example:" + portText})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, upstreamURL.Host)
	}
	proxy, err := startNetworkProxyWithDial(policy, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxyURL, _ := url.Parse("http://" + proxy.listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	response, err := client.Get("http://allowed.example:" + portText + "/artifact")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "allowed" {
		t.Fatalf("allowed response: status=%d body=%q", response.StatusCode, body)
	}
	if receivedHost != "allowed.example:"+portText {
		t.Fatalf("upstream Host = %q", receivedHost)
	}
	raw, err := net.Dial("tcp", proxy.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(raw, "GET http://allowed.example:"+portText+"/artifact HTTP/1.1\r\nHost: denied-virtual-host.example\r\nConnection: close\r\n\r\n")
	rawResponse, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rawResponse.Body)
	rawResponse.Body.Close()
	raw.Close()
	if rawResponse.StatusCode != http.StatusOK || receivedHost != "allowed.example:"+portText {
		t.Fatalf("proxy preserved an untrusted Host header: status=%d host=%q", rawResponse.StatusCode, receivedHost)
	}

	response, err = client.Get("http://denied.example:" + portText + "/artifact")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("denied response status = %d", response.StatusCode)
	}
}

func TestHTTPSProxyRequiresMatchingTLSName(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, "secure")
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	_, portText, _ := net.SplitHostPort(upstreamURL.Host)
	policy, err := parseNetworkPolicy([]string{"https://allowed.example:" + portText})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, upstreamURL.Host)
	}
	proxy, err := startNetworkProxyWithDial(policy, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxyURL, _ := url.Parse("http://" + proxy.listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Test server certificate.
	}}
	response, err := client.Get("https://allowed.example:" + portText + "/artifact")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "secure" {
		t.Fatalf("HTTPS response: status=%d body=%q", response.StatusCode, body)
	}

	connection, err := net.Dial("tcp", proxy.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = io.WriteString(connection, "CONNECT allowed.example:"+portText+" HTTP/1.1\r\nHost: allowed.example:"+portText+"\r\n\r\n")
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, %v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	tlsConnection := tls.Client(&bufferedConnection{Conn: connection, reader: reader}, &tls.Config{ServerName: "other.example", InsecureSkipVerify: true})
	if err := tlsConnection.Handshake(); err == nil {
		t.Fatal("CONNECT accepted a TLS name different from its allowed authority")
	}
}

func TestReadTLSClientHello(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tlsClient := tls.Client(client, &tls.Config{ServerName: "Registry.Example.com", MinVersion: tls.VersionTLS12})
		_ = tlsClient.Handshake()
		_ = client.Close()
	}()
	_, name, encrypted, err := readTLSClientHello(bufio.NewReader(server))
	_ = server.Close()
	<-done
	if err != nil || encrypted || name != "registry.example.com" {
		t.Fatalf("ClientHello: name=%q encrypted=%v err=%v", name, encrypted, err)
	}
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *bufferedConnection) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

func dnsQuery(name string, recordType uint16) []byte {
	message := make([]byte, 12)
	binary.BigEndian.PutUint16(message[:2], 0x1234)
	binary.BigEndian.PutUint16(message[2:4], 0x0100)
	binary.BigEndian.PutUint16(message[4:6], 1)
	for _, label := range strings.Split(name, ".") {
		message = append(message, byte(len(label)))
		message = append(message, label...)
	}
	message = append(message, 0)
	message = binary.BigEndian.AppendUint16(message, recordType)
	message = binary.BigEndian.AppendUint16(message, 1)
	return message
}

func TestPrivateDNSProxyFiltersQueriesAndAnswers(t *testing.T) {
	var lookedUp []string
	lookup := func(_ context.Context, network, host string) ([]netip.Addr, error) {
		lookedUp = append(lookedUp, network+" "+host)
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("203.0.113.8")}, nil
	}
	proxy := &dnsProxy{lookup: lookup}

	response := proxy.response(dnsQuery("Allowed.Example", 1), 512)
	if id := binary.BigEndian.Uint16(response[:2]); id != 0x1234 {
		t.Fatalf("DNS response ID = %#x", id)
	}
	if flags := binary.BigEndian.Uint16(response[2:4]); flags&0x000f != 0 || flags&0x8180 != 0x8180 {
		t.Fatalf("DNS response flags = %#x", flags)
	}
	if answers := binary.BigEndian.Uint16(response[6:8]); answers != 1 {
		t.Fatalf("DNS answer count = %d", answers)
	}
	if got := net.IP(response[len(response)-4:]).String(); got != "203.0.113.8" {
		t.Fatalf("DNS answer = %s", got)
	}
	if len(lookedUp) != 1 || lookedUp[0] != "ip allowed.example." {
		t.Fatalf("DNS lookups = %#v", lookedUp)
	}

	unlisted := proxy.response(dnsQuery("unlisted.example", 1), 512)
	if code := binary.BigEndian.Uint16(unlisted[2:4]) & 0x000f; code != 0 || len(lookedUp) != 2 || lookedUp[1] != "ip unlisted.example." {
		t.Fatalf("unlisted DNS response code = %d, lookups = %#v", code, lookedUp)
	}
	unsupported := proxy.response(dnsQuery("child.wild.example", 16), 512)
	if code := binary.BigEndian.Uint16(unsupported[2:4]) & 0x000f; code != 4 || len(lookedUp) != 2 {
		t.Fatalf("unsupported DNS response code = %d, lookups = %#v", code, lookedUp)
	}
}

func TestPrivateDNSProxyPreservesAbsoluteQuestion(t *testing.T) {
	for _, name := range []string{"Example.COM", "registry", "203.0.113.8"} {
		for _, kind := range []uint16{1, 28} {
			t.Run(fmt.Sprintf("%s/%d", name, kind), func(t *testing.T) {
				var lookedUp string
				proxy := &dnsProxy{lookup: func(_ context.Context, _, host string) ([]netip.Addr, error) {
					lookedUp = host
					return nil, nil
				}}
				query := dnsQuery(name, kind)
				response := proxy.response(query, 512)
				if want := strings.ToLower(name) + "."; lookedUp != want {
					t.Fatalf("upstream name = %q, want absolute name %q", lookedUp, want)
				}
				if !bytes.Equal(response[12:], query[12:]) {
					t.Fatal("response changed the original question")
				}
			})
		}
	}
}

func TestProxyLookupName(t *testing.T) {
	for input, want := range map[string]string{
		"example.com":  "example.com.",
		"example.com.": "example.com.",
		"registry":     "registry.",
		"203.0.113.8":  "203.0.113.8",
		"2001:db8::8":  "2001:db8::8",
	} {
		if got := proxyLookupName(input); got != want {
			t.Errorf("proxyLookupName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPrivateDNSProxyServesUDPAndTCP(t *testing.T) {
	lookup := func(_ context.Context, network, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2001:db8::8")}, nil
	}
	proxy, err := startDNSProxy(lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	query := dnsQuery("allowed.example", 28)

	udp, err := net.DialUDP("udp4", nil, proxy.udp.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_ = udp.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := udp.Write(query); err != nil {
		t.Fatal(err)
	}
	udpResponse := make([]byte, 512)
	length, err := udp.Read(udpResponse)
	if err != nil || binary.BigEndian.Uint16(udpResponse[6:8]) != 1 {
		t.Fatalf("UDP DNS response length=%d err=%v", length, err)
	}

	tcp, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	_ = tcp.SetDeadline(time.Now().Add(5 * time.Second))
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(query)))
	framed = append(framed, query...)
	if _, err := tcp.Write(framed); err != nil {
		t.Fatal(err)
	}
	lengthBytes := make([]byte, 2)
	if _, err := io.ReadFull(tcp, lengthBytes); err != nil {
		t.Fatal(err)
	}
	tcpResponse := make([]byte, binary.BigEndian.Uint16(lengthBytes))
	if _, err := io.ReadFull(tcp, tcpResponse); err != nil || binary.BigEndian.Uint16(tcpResponse[6:8]) != 1 {
		t.Fatalf("TCP DNS response err=%v", err)
	}
}

func TestPrivateDNSProxyMissingAddressFamily(t *testing.T) {
	for _, family := range []struct {
		name            string
		address         string
		present, absent uint16
	}{
		{"IPv4 only", "203.0.113.8", 1, 28},
		{"IPv6 only", "2001:db8::8", 28, 1},
	} {
		t.Run(family.name, func(t *testing.T) {
			proxy := &dnsProxy{lookup: func(_ context.Context, network, host string) ([]netip.Addr, error) {
				if network != "ip" {
					return nil, &net.DNSError{IsNotFound: true}
				}
				return []netip.Addr{netip.MustParseAddr(family.address)}, nil
			}}
			for _, record := range []struct{ kind, count uint16 }{{family.absent, 0}, {family.present, 1}} {
				response := proxy.response(dnsQuery("allowed.example", record.kind), 512)
				if code := binary.BigEndian.Uint16(response[2:4]) & 15; code != 0 {
					t.Fatalf("type %d returned rcode %d", record.kind, code)
				}
				if count := binary.BigEndian.Uint16(response[6:8]); count != record.count {
					t.Fatalf("type %d returned %d answers, want %d", record.kind, count, record.count)
				}
			}
		})
	}
	for _, failure := range []struct {
		name string
		err  error
		code uint16
	}{
		{"no address records", &net.DNSError{IsNotFound: true}, 0},
		{"timeout", &net.DNSError{IsTimeout: true}, 2},
		{"canceled", context.Canceled, 2},
	} {
		t.Run(failure.name, func(t *testing.T) {
			proxy := &dnsProxy{lookup: func(context.Context, string, string) ([]netip.Addr, error) { return nil, failure.err }}
			response := proxy.response(dnsQuery("allowed.example", 1), 512)
			if code := binary.BigEndian.Uint16(response[2:4]) & 15; code != failure.code || binary.BigEndian.Uint16(response[6:8]) != 0 {
				t.Fatalf("rcode = %d, want %d and zero answers", code, failure.code)
			}
		})
	}
}

func TestDNSListenerRetriesUDPCollision(t *testing.T) {
	attempts := 0
	var occupied *net.UDPConn
	var firstAddress string
	tcp, udp, err := listenDNS(func(network string, address *net.UDPAddr) (*net.UDPConn, error) {
		attempts++
		if attempts == 1 {
			var err error
			occupied, err = net.ListenUDP(network, address)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { occupied.Close() })
			firstAddress = address.String()
		}
		return net.ListenUDP(network, address)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	defer udp.Close()
	if attempts < 2 || tcp.Addr().(*net.TCPAddr).Port != udp.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("attempts=%d, TCP=%s, UDP=%s", attempts, tcp.Addr(), udp.LocalAddr())
	}
	released, err := net.Listen("tcp4", firstAddress)
	if err != nil {
		t.Fatalf("failed attempt leaked TCP listener: %v", err)
	}
	released.Close()
}

func TestDNSListenerStopsOnPermanentErrorOrRetryLimit(t *testing.T) {
	for _, failure := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"permission", syscall.EACCES, 1},
		{"resource", syscall.EMFILE, 1},
		{"collisions", syscall.EADDRINUSE, 16},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var addresses []string
			tcp, udp, err := listenDNS(func(_ string, address *net.UDPAddr) (*net.UDPConn, error) {
				addresses = append(addresses, address.String())
				return nil, &net.OpError{Op: "listen", Net: "udp4", Err: failure.err}
			})
			if tcp != nil || udp != nil || !errors.Is(err, failure.err) || len(addresses) != failure.attempts {
				t.Fatalf("listeners=%v/%v, error=%v, attempts=%d", tcp, udp, err, len(addresses))
			}
			for _, address := range addresses {
				listener, err := net.Listen("tcp4", address)
				if err != nil {
					t.Fatalf("leaked TCP listener %s: %v", address, err)
				}
				listener.Close()
			}
		})
	}
}

func TestPrivateDNSProxyReusesTCPConnection(t *testing.T) {
	proxy, err := startDNSProxy(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.8"), netip.MustParseAddr("2001:db8::8")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	connection, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	// Queue two frames together, then reuse the connection after their replies.
	for _, ids := range [][]uint16{{1, 2}, {3}} {
		var frames []byte
		for _, id := range ids {
			kind := uint16(1)
			if id == 2 {
				kind = 28
			}
			query := dnsQuery("allowed.example", kind)
			binary.BigEndian.PutUint16(query[:2], id)
			frames = binary.BigEndian.AppendUint16(frames, uint16(len(query)))
			frames = append(frames, query...)
		}
		if _, err := connection.Write(frames); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			var size [2]byte
			if _, err := io.ReadFull(connection, size[:]); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, binary.BigEndian.Uint16(size[:]))
			if _, err := io.ReadFull(connection, response); err != nil {
				t.Fatal(err)
			}
			if len(response) < 12 || binary.BigEndian.Uint16(response[:2]) != id || binary.BigEndian.Uint16(response[6:8]) != 1 {
				t.Fatalf("unexpected response for query %d: %x", id, response)
			}
		}
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	if _, err := connection.Read(buffer[:]); err == nil {
		t.Fatal("shutdown left reused TCP connection open")
	}
}

func TestPrivateDNSProxyCloseClosesIdleTCPClients(t *testing.T) {
	proxy, err := startDNSProxy(func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		proxy.Close()
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() { done <- proxy.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DNS proxy shutdown waited for an idle TCP client")
	}
}

func TestRuntimeProxyPortReplacement(t *testing.T) {
	plan := LaunchPlan{Outer: []string{
		"pasta", "--tcp-ns", strconv.Itoa(proxyGuestPort) + ":" + proxyPortPlaceholder + "," + strconv.Itoa(dnsGuestPort) + ":" + dnsPortPlaceholder,
		"--udp-ns", strconv.Itoa(dnsGuestPort) + ":" + dnsPortPlaceholder,
	}, ProxyGuestPort: proxyGuestPort}
	argv, err := plan.runtimeArgv(32123, 32124)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv, " "); !strings.Contains(got, strconv.Itoa(proxyGuestPort)+":32123") || !strings.Contains(got, strconv.Itoa(dnsGuestPort)+":32124") || strings.Contains(got, proxyPortPlaceholder) || strings.Contains(got, dnsPortPlaceholder) {
		t.Fatalf("runtime argv = %q", got)
	}
}
