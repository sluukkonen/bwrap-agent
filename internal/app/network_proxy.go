package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

type proxyDialer func(context.Context, string, string) (net.Conn, error)

type networkProxy struct {
	listener    net.Listener
	server      *http.Server
	transport   *http.Transport
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closeOnce   sync.Once
	done        chan struct{}
}

type networkProxyHandler struct {
	policy    networkPolicy
	dial      proxyDialer
	transport *http.Transport
	owner     *networkProxy
}

func startNetworkProxy(policy networkPolicy) (*networkProxy, error) {
	return startNetworkProxyWithDial(policy, restrictedDialContext)
}

func startNetworkProxyWithDial(policy networkPolicy, dial proxyDialer) (*networkProxy, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := &networkProxy{listener: listener, connections: map[net.Conn]struct{}{}, done: make(chan struct{})}
	proxy.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	proxy.server = &http.Server{
		Handler:           &networkProxyHandler{policy: policy, dial: dial, transport: proxy.transport, owner: proxy},
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		_ = proxy.server.Serve(listener)
		close(proxy.done)
	}()
	return proxy, nil
}

func (proxy *networkProxy) port() int {
	return proxy.listener.Addr().(*net.TCPAddr).Port
}

func (proxy *networkProxy) track(connection net.Conn) {
	proxy.mu.Lock()
	proxy.connections[connection] = struct{}{}
	proxy.mu.Unlock()
}

func (proxy *networkProxy) untrack(connection net.Conn) {
	proxy.mu.Lock()
	delete(proxy.connections, connection)
	proxy.mu.Unlock()
}

func (proxy *networkProxy) Close() error {
	if proxy == nil {
		return nil
	}
	var result error
	proxy.closeOnce.Do(func() {
		result = proxy.server.Close()
		proxy.transport.CloseIdleConnections()
		proxy.mu.Lock()
		for connection := range proxy.connections {
			_ = connection.Close()
		}
		proxy.mu.Unlock()
		<-proxy.done
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	})
	return result
}

func (handler *networkProxyHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		handler.connect(response, request)
		return
	}
	if strings.ToLower(request.URL.Scheme) != "http" || request.URL.Host == "" {
		http.Error(response, "bwrap-agent proxy permits absolute HTTP requests and HTTPS CONNECT only", http.StatusBadRequest)
		return
	}
	host, port, err := originTarget("http", request.URL.Host)
	if err != nil || !handler.policy.allows("http", host, port) {
		http.Error(response, "destination is not allowed by bwrap-agent", http.StatusForbidden)
		return
	}
	outbound := request.Clone(request.Context())
	outbound.RequestURI = ""
	outbound.Host = request.URL.Host
	outbound.Header = request.Header.Clone()
	removeHopHeaders(outbound.Header)
	outbound.Header.Del("Proxy-Authorization")
	received, err := handler.transport.RoundTrip(outbound)
	if err != nil {
		http.Error(response, "bwrap-agent proxy could not reach destination", http.StatusBadGateway)
		return
	}
	defer received.Body.Close()
	removeHopHeaders(received.Header)
	for name, values := range received.Header {
		for _, value := range values {
			response.Header().Add(name, value)
		}
	}
	response.WriteHeader(received.StatusCode)
	_, _ = io.Copy(response, received.Body)
}

func (handler *networkProxyHandler) connect(response http.ResponseWriter, request *http.Request) {
	host, port, err := originTarget("https", request.Host)
	if err != nil || !handler.policy.allows("https", host, port) {
		http.Error(response, "destination is not allowed by bwrap-agent", http.StatusForbidden)
		return
	}
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		http.Error(response, "CONNECT is unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	handler.owner.track(client)
	defer func() {
		handler.owner.untrack(client)
		_ = client.Close()
	}()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffered.Flush() != nil {
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	prefix, serverName, encryptedHello, err := readTLSClientHello(buffered.Reader)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil || encryptedHello || !connectServerNameMatches(host, serverName) {
		return
	}
	upstream, err := handler.dial(request.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return
	}
	handler.owner.track(upstream)
	defer func() {
		handler.owner.untrack(upstream)
		_ = upstream.Close()
	}()
	if _, err := upstream.Write(prefix); err != nil {
		return
	}
	relayTunnel(client, buffered.Reader, upstream)
}

func connectServerNameMatches(host, serverName string) bool {
	serverName = strings.ToLower(strings.TrimSuffix(serverName, "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return serverName == "" || serverName == host
	}
	return serverName == host
}

func relayTunnel(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if closer, ok := client.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

func removeHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

func restrictedDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("network %q is not permitted", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var failures []error
	dialer := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	for _, candidate := range addresses {
		candidate = candidate.Unmap()
		if !safeProxyDestination(candidate) {
			continue
		}
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		failures = append(failures, dialErr)
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return nil, fmt.Errorf("destination %s resolves only to protected local addresses", host)
}

func safeProxyDestination(address netip.Addr) bool {
	return address.IsValid() && !address.IsUnspecified() && !address.IsLoopback() && !address.IsMulticast() && !address.IsLinkLocalUnicast() && !address.IsLinkLocalMulticast()
}

func readTLSClientHello(reader *bufio.Reader) ([]byte, string, bool, error) {
	const maximumHello = 128 * 1024
	var raw, handshake bytes.Buffer
	for raw.Len() < maximumHello {
		header := make([]byte, 5)
		if _, err := io.ReadFull(reader, header); err != nil {
			return nil, "", false, err
		}
		if header[0] != 22 {
			return nil, "", false, errors.New("CONNECT stream did not begin with a TLS handshake")
		}
		length := int(header[3])<<8 | int(header[4])
		if length == 0 || length > 18432 || raw.Len()+5+length > maximumHello {
			return nil, "", false, errors.New("invalid TLS record length")
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, "", false, err
		}
		raw.Write(header)
		raw.Write(payload)
		handshake.Write(payload)
		if handshake.Len() < 4 {
			continue
		}
		message := handshake.Bytes()
		if message[0] != 1 {
			return nil, "", false, errors.New("first TLS handshake message is not ClientHello")
		}
		messageLength := int(message[1])<<16 | int(message[2])<<8 | int(message[3])
		if messageLength > maximumHello-4 {
			return nil, "", false, errors.New("TLS ClientHello is too large")
		}
		if len(message) < 4+messageLength {
			continue
		}
		name, encrypted, err := parseTLSClientHello(message[4 : 4+messageLength])
		return raw.Bytes(), name, encrypted, err
	}
	return nil, "", false, errors.New("TLS ClientHello is too large")
}

func parseTLSClientHello(hello []byte) (string, bool, error) {
	if len(hello) < 35 {
		return "", false, errors.New("truncated TLS ClientHello")
	}
	offset := 34
	sessionLength := int(hello[offset])
	offset++
	if offset+sessionLength+2 > len(hello) {
		return "", false, errors.New("truncated TLS session identifier")
	}
	offset += sessionLength
	cipherLength := int(hello[offset])<<8 | int(hello[offset+1])
	offset += 2
	if cipherLength == 0 || offset+cipherLength+1 > len(hello) {
		return "", false, errors.New("truncated TLS cipher suites")
	}
	offset += cipherLength
	compressionLength := int(hello[offset])
	offset++
	if offset+compressionLength == len(hello) {
		return "", false, nil
	}
	if offset+compressionLength+2 > len(hello) {
		return "", false, errors.New("truncated TLS compression methods")
	}
	offset += compressionLength
	extensionsLength := int(hello[offset])<<8 | int(hello[offset+1])
	offset += 2
	if extensionsLength != len(hello)-offset {
		return "", false, errors.New("invalid TLS extensions length")
	}
	var serverName string
	var encrypted bool
	for offset < len(hello) {
		if offset+4 > len(hello) {
			return "", false, errors.New("truncated TLS extension")
		}
		typeCode := int(hello[offset])<<8 | int(hello[offset+1])
		length := int(hello[offset+2])<<8 | int(hello[offset+3])
		offset += 4
		if offset+length > len(hello) {
			return "", false, errors.New("truncated TLS extension data")
		}
		data := hello[offset : offset+length]
		offset += length
		if typeCode == 0xfe0d {
			encrypted = true
		}
		if typeCode != 0 || serverName != "" {
			continue
		}
		name, err := parseTLSServerName(data)
		if err != nil {
			return "", false, err
		}
		serverName = name
	}
	return serverName, encrypted, nil
}

func parseTLSServerName(data []byte) (string, error) {
	if len(data) < 2 || int(data[0])<<8|int(data[1]) != len(data)-2 {
		return "", errors.New("invalid TLS server-name extension")
	}
	for offset := 2; offset < len(data); {
		if offset+3 > len(data) {
			return "", errors.New("truncated TLS server name")
		}
		nameType := data[offset]
		length := int(data[offset+1])<<8 | int(data[offset+2])
		offset += 3
		if offset+length > len(data) {
			return "", errors.New("truncated TLS server name")
		}
		if nameType == 0 {
			name := strings.ToLower(strings.TrimSuffix(string(data[offset:offset+length]), "."))
			if err := validateNetworkHostname(name); err != nil {
				return "", err
			}
			return name, nil
		}
		offset += length
	}
	return "", nil
}
