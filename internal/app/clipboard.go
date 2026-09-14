//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const clipboardLimit = 1 << 20
const clipboardInterval = 200 * time.Millisecond
const clipboardTimeout = 5 * time.Second

// This configuration stays in the host launcher, never in the sandbox environment.
type clipboardBridgeConfig struct {
	program     string
	environment []string
}

func prepareClipboard(mode string, tty bool, host hostContext) (string, *clipboardBridgeConfig, error) {
	if mode == "" {
		mode = "off"
	}
	if mode == "off" {
		return mode, nil, nil
	}
	if mode != "wayland" {
		return "", nil, fmt.Errorf("clipboard must be off or wayland")
	}
	if !tty {
		return "", nil, fmt.Errorf("--clipboard=wayland requires a PTY; use --tty=always or disable the bridge")
	}
	program, err := requireProgram("wl-copy")
	if err != nil {
		return "", nil, fmt.Errorf("clipboard: install host wl-clipboard: %w", err)
	}
	program, _, err = host.sources.resolveSource(program, false, true)
	if err != nil {
		return "", nil, fmt.Errorf("clipboard helper: %w", err)
	}
	if err = unix.Access(program, unix.X_OK); err != nil {
		return "", nil, fmt.Errorf("clipboard helper is not executable: %w", err)
	}
	display, _ := hostEnvValue(host.hostEnv, "WAYLAND_DISPLAY")
	runtime, _ := hostEnvValue(host.hostEnv, "XDG_RUNTIME_DIR")
	if display == "" {
		return "", nil, fmt.Errorf("clipboard: host WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(display) {
		if !filepath.IsAbs(runtime) {
			return "", nil, fmt.Errorf("clipboard: host XDG_RUNTIME_DIR must be absolute")
		}
		display = filepath.Join(runtime, display)
	}
	display, err = host.sources.resolvePath(display)
	if err != nil {
		return "", nil, fmt.Errorf("clipboard socket: %w", err)
	}
	info, err := os.Stat(display)
	if err != nil {
		return "", nil, fmt.Errorf("clipboard socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", nil, fmt.Errorf("clipboard: WAYLAND_DISPLAY does not name a Unix socket")
	}
	return mode, &clipboardBridgeConfig{program: program, environment: []string{
		"WAYLAND_DISPLAY=" + display, "XDG_RUNTIME_DIR=" + filepath.Dir(display),
		"PATH=/usr/bin:/bin", "TMPDIR=/tmp", "LANG=C.UTF-8",
	}}, nil
}

func (config *clipboardBridgeConfig) copy(ctx context.Context, data []byte) error {
	cmd := exec.CommandContext(ctx, config.program, "--type", "text/plain;charset=utf-8")
	cmd.Env = config.environment
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(data)
	// A successful wl-copy forks a clipboard owner. It must not inherit the PTY
	// or a stderr pipe, and must survive terminal and sandbox shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

type clipboardWorker struct {
	pending chan []byte
	done    chan struct{}
	cancel  context.CancelFunc
}

func newClipboardWorker(copy func(context.Context, []byte) error, warn func()) *clipboardWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &clipboardWorker{pending: make(chan []byte, 1), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(w.done)
		var next, lastWarning time.Time
		for data := range w.pending {
			if delay := time.Until(next); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			// Replace a request waiting for its rate-limit slot with the latest.
			select {
			case latest, ok := <-w.pending:
				if ok {
					data = latest
				}
			default:
			}
			if ctx.Err() != nil {
				return
			}
			next = time.Now().Add(clipboardInterval)
			attempt, stop := context.WithTimeout(ctx, clipboardTimeout)
			err := copy(attempt, data)
			stop()
			if err != nil && time.Since(lastWarning) >= clipboardTimeout {
				warn()
				lastWarning = time.Now()
			}
		}
	}()
	return w
}

// submit and close are called only by the relay goroutine.
func (w *clipboardWorker) submit(data []byte) {
	select {
	case w.pending <- data:
		return
	default:
	}
	select {
	case <-w.pending:
	default:
	}
	w.pending <- data
}

func (w *clipboardWorker) close() {
	close(w.pending)
	timer := time.NewTimer(clipboardTimeout)
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
		w.cancel()
		<-w.done
	}
	w.cancel()
}

const (
	clipGround = iota
	clipEscape
	clipPrefix
	clipData
	clipDataEscape
	clipOtherOSC
	clipOtherOSCEscape
	clipString
	clipStringEscape
)

// A bounded streaming parser, not a general terminal security filter. Other
// OSCs and opaque DCS/APC/PM/SOS strings retain their normal terminal semantics.
type clipboardParser struct {
	state    int
	prefix   []byte
	data     []byte
	rejected bool
	copy     func([]byte)
}

func (p *clipboardParser) appendData(c byte) {
	if p.rejected {
		return
	}
	// Selection/header space is bounded independently of decoded text size.
	if len(p.data) >= base64.StdEncoding.EncodedLen(clipboardLimit)+2 {
		p.rejected = true
		p.data = nil
		return
	}
	p.data = append(p.data, c)
}

func (p *clipboardParser) complete() {
	if !p.rejected {
		selection, payload, ok := strings.Cut(string(p.data), ";")
		if ok && (selection == "c" || selection == "") && !strings.ContainsAny(payload, "\r\n") {
			data, err := base64.StdEncoding.Strict().DecodeString(payload)
			if err == nil && len(data) <= clipboardLimit && utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
				p.copy(data)
			}
		}
	}
	p.data = nil
	p.rejected = false
	p.state = clipGround
}

func (p *clipboardParser) feed(input []byte) []byte {
	output := make([]byte, 0, len(input))
	for _, c := range input {
		switch p.state {
		case clipGround:
			if c == 0x1b {
				p.state = clipEscape
			} else {
				output = append(output, c)
			}
		case clipEscape:
			switch c {
			case ']':
				p.state = clipPrefix
				p.prefix = nil
			case 'P', '_', '^', 'X':
				output = append(output, 0x1b, c)
				p.state = clipString
			case 0x1b:
				output = append(output, 0x1b)
			default:
				output = append(output, 0x1b, c)
				p.state = clipGround
			}
		case clipPrefix:
			p.prefix = append(p.prefix, c)
			if bytes.Equal(p.prefix, []byte("52;")) {
				p.prefix = nil
				p.state = clipData
			} else if !bytes.HasPrefix([]byte("52;"), p.prefix) {
				output = append(output, 0x1b, ']')
				output = append(output, p.prefix...)
				p.prefix = nil
				p.state = clipOtherOSC
				if c == 7 || c == 0x18 || c == 0x1a {
					p.state = clipGround
				}
				if c == 0x1b {
					p.state = clipOtherOSCEscape
				}
			}
		case clipData, clipDataEscape:
			if c == 7 || p.state == clipDataEscape && c == '\\' {
				if c == 7 && p.state == clipDataEscape {
					p.appendData(0x1b)
				}
				p.complete()
			} else if c == 0x18 || c == 0x1a {
				p.data = nil
				p.rejected = false
				p.state = clipGround
			} else {
				if p.state == clipDataEscape {
					p.appendData(0x1b)
				}
				p.state = clipData
				if c == 0x1b {
					p.state = clipDataEscape
				} else {
					p.appendData(c)
				}
			}
		case clipOtherOSC, clipOtherOSCEscape, clipString, clipStringEscape:
			output = append(output, c)
			osc := p.state == clipOtherOSC || p.state == clipOtherOSCEscape
			escaped := p.state == clipOtherOSCEscape || p.state == clipStringEscape
			if escaped && c == '\\' || osc && c == 7 || c == 0x18 || c == 0x1a {
				p.state = clipGround
			} else if osc {
				p.state = clipOtherOSC
				if c == 0x1b {
					p.state = clipOtherOSCEscape
				}
			} else {
				p.state = clipString
				if c == 0x1b {
					p.state = clipStringEscape
				}
			}
		}
	}
	return output
}

func (p *clipboardParser) finish() []byte {
	var tail []byte
	if p.state == clipEscape {
		tail = []byte{0x1b}
	}
	if p.state == clipPrefix {
		tail = append([]byte{0x1b, ']'}, p.prefix...)
	}
	p.state = clipGround
	p.prefix = nil
	p.data = nil
	p.rejected = false
	return tail
}
