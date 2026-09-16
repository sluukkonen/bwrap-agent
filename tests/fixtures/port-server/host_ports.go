package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func checkEcho(network, address string) error {
	conn, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	for _, message := range []string{"first message", "second message"} {
		if _, err = conn.Write([]byte(message)); err != nil {
			return err
		}
		reply := make([]byte, len(message))
		if _, err = io.ReadFull(conn, reply); err != nil {
			return err
		}
		if string(reply) != message {
			return fmt.Errorf("unexpected echo %q", reply)
		}
	}
	return nil
}

func hostPortsClient(args []string) error {
	hostPort, deniedPort := args[0], args[1]
	for _, protocol := range []string{"tcp", "udp"} {
		for _, port := range []string{hostPort, "19322"} {
			if err := checkEcho(protocol, net.JoinHostPort("127.0.0.1", port)); err != nil {
				return fmt.Errorf("%s port %s: %w", protocol, port, err)
			}
		}
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", deniedPort), time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("unrequested host port was reachable")
	}
	if err := checkEcho("udp", net.JoinHostPort("127.0.0.1", deniedPort)); err == nil {
		return fmt.Errorf("unrequested host UDP port was reachable")
	}
	// Existing proxy and DNS policy must still deny unlisted destinations.
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://host-ports-denied.invalid")
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		return fmt.Errorf("proxy returned %d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, "host-ports-denied.invalid"); err == nil {
		return fmt.Errorf("DNS allowed an unlisted name")
	}
	published, err := net.Listen("tcp4", "127.0.0.1:19324")
	if err != nil {
		return err
	}
	defer published.Close()
	go func() {
		conn, err := published.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}
	}()
	// Leave a live stream for namespace teardown to close.
	held, err := net.DialTimeout("tcp", "127.0.0.1:19322", 3*time.Second)
	if err != nil {
		return err
	}
	defer held.Close()
	fmt.Println("host-ports-ready")
	_, err = io.ReadFull(os.Stdin, make([]byte, 1))
	return err
}

func hostPortsIntegration(binary string) error {
	root, err := os.MkdirTemp("", "host-ports-integration.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	tcp, udp, err := hostPortListeners()
	if err != nil {
		return err
	}
	defer tcp.Close()
	defer udp.Close()
	hostPort := tcp.Addr().(*net.TCPAddr).Port
	denied, deniedUDP, err := hostPortListeners()
	if err != nil {
		return err
	}
	defer denied.Close()
	defer deniedUDP.Close()
	go echoDatagrams(deniedUDP)
	var active atomic.Int32
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			active.Add(1)
			go func() { defer active.Add(-1); defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	go echoDatagrams(udp)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	type session struct {
		cmd       *exec.Cmd
		input     io.WriteCloser
		published string
	}
	var sessions []session
	defer func() {
		cancel()
		for _, s := range sessions {
			_ = s.input.Close()
			if s.cmd.ProcessState == nil {
				_ = s.cmd.Wait()
			}
		}
	}()
	// Two simultaneous non-Podman sandboxes share guest ports; also exercise
	// the outer podman-unshare path without pulling any container images.
	for i, mode := range []string{"off", "off", "on"} {
		project := filepath.Join(root, strconv.Itoa(i))
		if err := os.Mkdir(project, 0700); err != nil {
			return err
		}
		available, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		published := strconv.Itoa(available.Addr().(*net.TCPAddr).Port)
		available.Close()
		port := strconv.Itoa(hostPort)
		args := []string{"run", "--project", project, "--instance", fmt.Sprintf("host-ports-%d", i), "--no-config", "--podman", mode, "--network", "private", "--tty", "never",
			"--host-port", port, "--host-port", port + "/udp", "--host-port", "19322:" + port, "--host-port", "19322:" + port + "/udp",
			"--publish", published + ":19324", self, "--host-ports-client", port, strconv.Itoa(denied.Addr().(*net.TCPAddr).Port)}
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Stderr = os.Stderr
		input, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			input.Close()
			return err
		}
		if err := cmd.Start(); err != nil {
			input.Close()
			output.Close()
			return err
		}
		ready := make(chan error, 1)
		sessions = append(sessions, session{cmd, input, published})
		go func() {
			scanner := bufio.NewScanner(output)
			for scanner.Scan() {
				if scanner.Text() == "host-ports-ready" {
					ready <- nil
					_, _ = io.Copy(io.Discard, output)
					return
				}
			}
			ready <- fmt.Errorf("sandbox did not become ready: %v", scanner.Err())
		}()
		// Wait until this publish listener is bound before choosing another
		// ephemeral host port, while keeping all ready sandboxes running.
		select {
		case err := <-ready:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, s := range sessions {
		if err := checkEcho("tcp", net.JoinHostPort("127.0.0.1", s.published)); err != nil {
			return fmt.Errorf("published port: %w", err)
		}
	}
	for _, s := range sessions {
		if _, err := s.input.Write([]byte("x")); err != nil {
			return err
		}
		s.input.Close()
	}
	for _, s := range sessions {
		if err := s.cmd.Wait(); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if active.Load() != 0 {
		return fmt.Errorf("forwarded streams survived sandbox exit")
	}
	// Mappings belong to a launch, not to the persistent instance.
	project := filepath.Join(root, "0")
	cmd := exec.CommandContext(ctx, binary, "run", "--project", project, "--instance", "host-ports-0", "--no-config", "--podman", "off", "--network", "private", "--tty", "never", self, "--host-ports-absent", strconv.Itoa(hostPort))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Println("host-ports-integration-ok")
	return nil
}

func hostPortsAbsent(port string) error {
	for _, target := range []string{port, "19322"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", target), time.Second)
		if err == nil {
			conn.Close()
			return fmt.Errorf("mapping survived relaunch: %s", target)
		}
		if err := checkEcho("udp", net.JoinHostPort("127.0.0.1", target)); err == nil {
			return fmt.Errorf("UDP mapping survived relaunch: %s", target)
		}
	}
	return nil
}

func runHostPortMode() (bool, error) {
	if len(os.Args) < 2 || !strings.HasPrefix(os.Args[1], "--host-ports-") {
		return false, nil
	}
	switch os.Args[1] {
	case "--host-ports-integration":
		if len(os.Args) == 3 {
			return true, hostPortsIntegration(os.Args[2])
		}
	case "--host-ports-client":
		if len(os.Args) == 4 {
			return true, hostPortsClient(os.Args[2:])
		}
	case "--host-ports-absent":
		if len(os.Args) == 3 {
			return true, hostPortsAbsent(os.Args[2])
		}
	}
	return true, fmt.Errorf("invalid host-port fixture arguments")
}

// Bind both protocols to one available port, excluding the fixture's guest
// endpoints and the sandbox's reserved endpoints. TCP and UDP allocators are
// independent, so a free TCP port can already be occupied by UDP.
func hostPortListeners() (net.Listener, net.PacketConn, error) {
	for attempt := 0; attempt < 32; attempt++ {
		tcp, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		if port == 53 || port == 65532 || port == 19322 || port == 19324 {
			tcp.Close()
			continue
		}
		udp, err := net.ListenPacket("udp4", tcp.Addr().String())
		if err == nil {
			return tcp, udp, nil
		}
		tcp.Close()
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("could not reserve host TCP/UDP fixture port")
}

func echoDatagrams(conn net.PacketConn) {
	buffer := make([]byte, 1024)
	for {
		n, peer, err := conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		_, _ = conn.WriteTo(buffer[:n], peer)
	}
}
