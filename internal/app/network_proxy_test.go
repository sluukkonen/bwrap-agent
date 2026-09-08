package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

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

func TestRuntimeProxyPortReplacement(t *testing.T) {
	plan := LaunchPlan{Outer: []string{"pasta", "--tcp-ns", strconv.Itoa(proxyGuestPort) + ":" + proxyPortPlaceholder}, ProxyGuestPort: proxyGuestPort}
	argv, err := plan.runtimeArgv(32123)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv, " "); !strings.Contains(got, strconv.Itoa(proxyGuestPort)+":32123") || strings.Contains(got, proxyPortPlaceholder) {
		t.Fatalf("runtime argv = %q", got)
	}
}
