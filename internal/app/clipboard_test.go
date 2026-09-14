//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func clipboardSequence(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

func TestClipboardParser(t *testing.T) {
	for _, tc := range []struct {
		name, input, output string
		copies              []string
	}{
		{"text", "before" + clipboardSequence("hei 世界\n") + "after", "beforeafter", []string{"hei 世界\n"}},
		{"ST", "\x1b]52;;eA==\x1b\\", "", []string{"x"}},
		{"empty", clipboardSequence(""), "", []string{""}},
		{"query", "a\x1b]52;c;?\ab", "ab", nil},
		{"primary", "\x1b]52;p;eA==\a", "", nil},
		{"multiple selections", "\x1b]52;cp;eA==\a", "", nil},
		{"invalid", "\x1b]52;c;!!!!\a", "", nil},
		{"invalid escape", "\x1b]52;c;eA==\x1b\a", "", nil},
		{"noncanonical base64", "\x1b]52;c;eB==\a", "", nil},
		{"newline in encoding", "\x1b]52;c;eA==\n\a", "", nil},
		{"invalid utf8", clipboardSequence(string([]byte{0xff})), "", nil},
		{"nul", clipboardSequence("a\x00b"), "", nil},
		{"cancel", "\x1b]52;c;eA==\x18OK", "OK", nil},
		{"truncated", "before\x1b]52;c;eA==", "before", nil},
		{"escape tail", "before\x1b", "before\x1b", nil},
		{"prefix tail", "before\x1b]5", "before\x1b]5", nil},
		{"title", "\x1b]0;title\a", "\x1b]0;title\a", nil},
		{"colors", "\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m", nil},
		{"opaque title", "\x1b]0;" + clipboardSequence("x"), "\x1b]0;" + clipboardSequence("x"), nil},
		{"opaque DCS", "\x1bPtmux;\x1b" + clipboardSequence("x") + "\x1b\\", "\x1bPtmux;\x1b" + clipboardSequence("x") + "\x1b\\", nil},
		{"opaque APC", "\x1b_" + clipboardSequence("x") + "\x1b\\", "\x1b_" + clipboardSequence("x") + "\x1b\\", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for split := 0; split <= len(tc.input); split++ {
				var copies []string
				p := clipboardParser{copy: func(data []byte) { copies = append(copies, string(data)) }}
				got := append(p.feed([]byte(tc.input[:split])), p.feed([]byte(tc.input[split:]))...)
				got = append(got, p.finish()...)
				if string(got) != tc.output || !reflect.DeepEqual(copies, tc.copies) {
					t.Fatalf("split %d: output %q, copies %q", split, got, copies)
				}
			}
		})
	}
}

func TestClipboardParserLimitAndRecovery(t *testing.T) {
	for _, size := range []int{clipboardLimit, clipboardLimit + 1, clipboardLimit * 3} {
		var lengths []int
		p := clipboardParser{copy: func(data []byte) { lengths = append(lengths, len(data)) }}
		input := clipboardSequence(strings.Repeat("x", size)) + "visible" + clipboardSequence("ok")
		var output []byte
		for len(input) > 0 {
			n := min(4093, len(input))
			output = append(output, p.feed([]byte(input[:n]))...)
			input = input[n:]
			if len(p.data) > base64.StdEncoding.EncodedLen(clipboardLimit)+2 {
				t.Fatal("unbounded parser")
			}
		}
		want := []int{2}
		if size == clipboardLimit {
			want = []int{size, 2}
		}
		if string(output) != "visible" || !reflect.DeepEqual(lengths, want) {
			t.Fatalf("size %d: output %q, lengths %v", size, output, lengths)
		}
	}
}

func FuzzClipboardParser(f *testing.F) {
	for _, seed := range []string{clipboardSequence("hello"), "\x1b]52;c;?\a", "\x1bP" + clipboardSequence("x") + "\x1b\\", "\x1b]52;c;\x1b\a"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		parse := func(chunk int) ([]byte, []string) {
			var output []byte
			var copies []string
			p := clipboardParser{copy: func(data []byte) { copies = append(copies, string(data)) }}
			for offset := 0; offset < len(data); offset += chunk {
				output = append(output, p.feed(data[offset:min(offset+chunk, len(data))])...)
			}
			return append(output, p.finish()...), copies
		}
		a, ac := parse(max(1, len(data)))
		b, bc := parse(1)
		if !bytes.Equal(a, b) || !reflect.DeepEqual(ac, bc) {
			t.Fatal("fragmentation changes interpretation")
		}
	})
}

func clipboardTestConfig(t *testing.T) (hostContext, string) {
	t.Helper()
	root, host := testHostContext(t)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(bin, "wl-copy")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	socket := filepath.Join(root, "wayland-0")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	host.hostEnv = []string{"XDG_RUNTIME_DIR=" + root, "WAYLAND_DISPLAY=wayland-0", "LD_PRELOAD=untrusted", "TMPDIR=" + filepath.Join(filepath.Dir(host.state), "project")}
	return host, program
}

func TestPrepareClipboard(t *testing.T) {
	host, program := clipboardTestConfig(t)
	mode, config, err := prepareClipboard("wayland", true, host)
	if err != nil || mode != "wayland" || config.program != program {
		t.Fatalf("prepare: %s %#v %v", mode, config, err)
	}
	for _, value := range config.environment {
		if strings.HasPrefix(value, "LD_") || strings.Contains(value, filepath.Join(filepath.Dir(host.state), "project")) {
			t.Fatalf("unsafe environment %q", value)
		}
	}
	for _, mode := range []string{"", "off"} {
		if got, cfg, err := prepareClipboard(mode, false, hostContext{}); got != "off" || cfg != nil || err != nil {
			t.Fatalf("off: %q %v %v", got, cfg, err)
		}
	}
	for _, tc := range []struct {
		name, mode string
		tty        bool
		host       hostContext
	}{
		{"mode", "invalid", true, host}, {"pty", "wayland", false, host}, {"display", "wayland", true, hostContext{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := prepareClipboard(tc.mode, tc.tty, tc.host); err == nil {
				t.Fatal("accepted invalid prerequisites")
			}
		})
	}
	originalPolicy := host.sources
	host.sources = newHostSourcePolicy(instanceIdentity{Project: filepath.Join(filepath.Dir(host.state), "project"), State: host.state, RWBind: []string{filepath.Dir(program)}})
	if _, _, err := prepareClipboard("wayland", true, host); err == nil {
		t.Fatal("accepted writable helper")
	}
	host.sources = originalPolicy
	host.hostEnv = []string{"WAYLAND_DISPLAY=" + program}
	if _, _, err := prepareClipboard("wayland", true, host); err == nil {
		t.Fatal("accepted regular-file display")
	}
	if err := os.Remove(program); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareClipboard("wayland", true, host); err == nil {
		t.Fatal("accepted missing helper")
	}
}

func TestClipboardHelperInputAndFailure(t *testing.T) {
	host, program := clipboardTestConfig(t)
	_, cfg, err := prepareClipboard("wayland", true, host)
	if err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(filepath.Dir(program), "result")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$#\" \"$1\" \"$2\" \"${LD_PRELOAD-unset}\" > %q\n/bin/cat >> %q\n", result, result)
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	text := "--shell-option\n世界\n$(false)\n"
	if err := cfg.copy(context.Background(), []byte(text)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "2\n--type\ntext/plain;charset=utf-8\nunset\n"+text {
		t.Fatalf("helper data %q", got)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cfg.copy(context.Background(), nil); err == nil {
		t.Fatal("ignored exit failure")
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\n/bin/sleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := cfg.copy(ctx, []byte("x")); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout: %v after %v", err, time.Since(start))
	}
}

func TestClipboardWorkerCoalescesWithoutBlocking(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls []string
	var times []time.Time
	var mu sync.Mutex
	w := newClipboardWorker(func(ctx context.Context, data []byte) error {
		mu.Lock()
		calls = append(calls, string(data))
		times = append(times, time.Now())
		n := len(calls)
		mu.Unlock()
		if n == 1 {
			close(started)
			<-release
		}
		return nil
	}, func() { t.Error("unexpected failure") })
	w.submit([]byte("first"))
	<-started
	for i := 0; i < 1000; i++ {
		w.submit([]byte(fmt.Sprint(i)))
	}
	close(release)
	w.close()
	if !reflect.DeepEqual(calls, []string{"first", "999"}) {
		t.Fatalf("calls %v", calls)
	}
	if times[1].Sub(times[0]) < clipboardInterval {
		t.Fatalf("rate limit: %v", times)
	}
}

func TestClipboardWorkerWarningsAndCancellation(t *testing.T) {
	warnings := 0
	w := newClipboardWorker(func(ctx context.Context, data []byte) error { return errors.New("failed") }, func() { warnings++ })
	w.submit([]byte("x"))
	w.close()
	if warnings != 1 {
		t.Fatalf("warnings %d", warnings)
	}
	started := make(chan struct{})
	w = newClipboardWorker(func(ctx context.Context, data []byte) error { close(started); <-ctx.Done(); return ctx.Err() }, func() {})
	w.submit([]byte("x"))
	<-started
	w.cancel()
	w.close()
}

func TestClipboardConfigurationAndJSON(t *testing.T) {
	wayland, off := "wayland", "off"
	for _, tc := range []struct {
		layers []optionLayer
		cli    *string
		want   string
	}{
		{nil, nil, "off"}, {[]optionLayer{{clipboard: &wayland}}, nil, "wayland"},
		{[]optionLayer{{clipboard: &wayland}, {clipboard: &off}}, nil, "off"},
		{[]optionLayer{{clipboard: &wayland}}, &off, "off"},
	} {
		opts, err := mergeOptions(cliOptions{Clipboard: tc.cli}, t.TempDir(), tc.layers, nil, nil)
		if err != nil || opts.Clipboard != tc.want {
			t.Fatalf("merge %q %v", opts.Clipboard, err)
		}
	}
	for _, value := range []string{"off", "wayland", "invalid"} {
		_, err := makeConfigLayer(fileConfig{Clipboard: &value}, t.TempDir())
		if (err != nil) != (value == "invalid") {
			t.Fatalf("validation %s: %v", value, err)
		}
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, LaunchPlan{Clipboard: "wayland"}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result["clipboard"] != "wayland" {
		t.Fatalf("JSON %s: %v", output.String(), err)
	}
}

func TestClipboardCLIAndFileConfiguration(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeTestFile(t, filepath.Join(project, projectConfigName), "clipboard = \"wayland\"\n")
	trustTestConfig(t, project)
	for _, tc := range []struct {
		extra []string
		want  string
	}{
		{nil, "wayland"}, {[]string{"--clipboard", "off"}, "off"},
		{[]string{"--no-config"}, "off"}, {[]string{"--no-config", "--clipboard", "wayland"}, "wayland"},
	} {
		args := append([]string{"run", "--project", project}, tc.extra...)
		args = append(args, "/bin/true")
		var out, stderr bytes.Buffer
		opts, code, err := parseOptions(args, &out, &stderr)
		if err != nil || code != 0 || opts.Clipboard != tc.want {
			t.Fatalf("args %v: %q, code %d, %v", args, opts.Clipboard, code, err)
		}
	}
	var out, stderr bytes.Buffer
	if _, _, err := parseOptions([]string{"run", "--no-config", "--clipboard", "invalid", "/bin/true"}, &out, &stderr); err == nil {
		t.Fatal("accepted invalid CLI mode")
	}
}

func TestClipboardPlanDoesNotExposeDesktop(t *testing.T) {
	originalPath := os.Getenv("PATH")
	host, program := clipboardTestConfig(t)
	t.Setenv("PATH", filepath.Dir(program)+":"+originalPath)
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, entry := range host.hostEnv[:2] {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	opts := controlTestOptions(filepath.Join(filepath.Dir(host.state), "project"), "clipboard-plan")
	opts.NoAgentConfig = true
	opts.TTY = "always"
	before, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.Clipboard = "wayland"
	after, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Bwrap, after.Bwrap) || !reflect.DeepEqual(before.LaunchEnv, after.LaunchEnv) {
		t.Fatal("clipboard changed sandbox mounts or environment")
	}
	if after.clipboardBridge == nil || after.Clipboard != "wayland" {
		t.Fatal("bridge missing from plan")
	}
	opts.Env = []string{"WAYLAND_DISPLAY=/sandbox/override", "XDG_RUNTIME_DIR=/sandbox/runtime"}
	redirected, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.clipboardBridge, redirected.clipboardBridge) {
		t.Fatal("sandbox env redirected host helper")
	}
}

func TestClipboardRejectsWritableSymlinkHops(t *testing.T) {
	host, program := clipboardTestConfig(t)
	link := filepath.Join(filepath.Dir(host.state), "project", "helper")
	if err := os.Symlink(program, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(program, program+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(program+".real", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(link, program); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareClipboard("wayland", true, host); err == nil {
		t.Fatal("accepted helper through project symlink")
	}
	if err := os.Remove(program); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(program+".real", program); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(filepath.Dir(filepath.Dir(program)), "wayland-0")
	socketLink := filepath.Join(filepath.Dir(host.state), "project", "display")
	if err := os.Symlink(socket, socketLink); err != nil {
		t.Fatal(err)
	}
	host.hostEnv = []string{"WAYLAND_DISPLAY=" + socketLink}
	if _, _, err := prepareClipboard("wayland", true, host); err == nil {
		t.Fatal("accepted socket through project symlink")
	}
	host.hostEnv = []string{"WAYLAND_DISPLAY=" + socket}
	if _, _, err := prepareClipboard("wayland", true, host); err != nil {
		t.Fatalf("absolute display: %v", err)
	}
}

func TestClipboardPTYEndToEnd(t *testing.T) {
	host, program := clipboardTestConfig(t)
	_, cfg, err := prepareClipboard("wayland", true, host)
	if err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(filepath.Dir(program), "copied")
	if err := os.WriteFile(program, []byte(fmt.Sprintf("#!/bin/sh\n/bin/cat > %q\n", result)), 0o700); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	sequence := clipboardSequence("selected text")
	for _, enabled := range []bool{false, true} {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var bridge *clipboardBridgeConfig
		if enabled {
			bridge = cfg
		}
		status := runWithPTYClipboard([]string{"/usr/bin/printf", "%s", "before" + sequence + "after"}, nil, input, write, make(chan os.Signal), bridge)
		write.Close()
		data, err := io.ReadAll(read)
		read.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "beforeafter"
		if !enabled {
			want = "before" + sequence + "after"
		}
		if status != 0 || string(data) != want {
			t.Fatalf("enabled %v: status %d output %q", enabled, status, data)
		}
		if !enabled {
			if _, err := os.Stat(result); !os.IsNotExist(err) {
				t.Fatal("disabled bridge invoked helper")
			}
		}
	}
	data, err := os.ReadFile(result)
	if err != nil || string(data) != "selected text" {
		t.Fatalf("copied %q: %v", data, err)
	}
}

func TestClipboardBackgroundOwnerSurvivesPTYExit(t *testing.T) {
	host, program := clipboardTestConfig(t)
	_, cfg, err := prepareClipboard("wayland", true, host)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(program)
	release, result := filepath.Join(root, "release"), filepath.Join(root, "owner-result")
	// Model wl-copy's short-lived parent and background selection owner. The
	// owner has no inherited terminal descriptors and responds only after exit.
	script := fmt.Sprintf("#!/bin/sh\n/bin/cat >/dev/null\n(\n for i in 1 2 3 4 5 6 7 8 9 10; do\n  if [ -f %q ]; then printf served > %q; exit 0; fi\n  /bin/sleep 0.1\n done\n) </dev/null >/dev/null 2>&1 &\nexit 0\n", release, result)
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	status := runWithPTYClipboard([]string{"/usr/bin/printf", "%s", clipboardSequence("x")}, nil, input, output, make(chan os.Signal), cfg)
	if status != 0 {
		t.Fatalf("status %d", status)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(result); err == nil && string(data) == "served" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background owner did not survive PTY exit")
}
