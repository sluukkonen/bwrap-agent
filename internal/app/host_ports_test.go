package app

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestHostPortParsing(t *testing.T) {
	for _, test := range []struct {
		input string
		want  PortMapping
	}{
		{"9222", PortMapping{"tcp", 9222, 9222}},
		{"15432:5432", PortMapping{"tcp", 5432, 15432}},
		{"15432:5432/udp", PortMapping{"udp", 5432, 15432}},
		{"1:65535/tcp", PortMapping{"tcp", 65535, 1}},
	} {
		got, err := parseHostPort(test.input)
		if err != nil || got != test.want {
			t.Errorf("parse %q = %#v, %v; want %#v", test.input, got, err, test.want)
		}
	}
	for _, value := range []string{"", "0", "0:80", "80:0", "65536", "1:65536", "-1", "+1", "1:+2", "1:2:3", ":80", "80:", "1-2", "localhost:80", "127.0.0.1/80", "80/sctp", "80/", "80/tcp/udp", " 80"} {
		if _, err := parseHostPort(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestHostPortResolution(t *testing.T) {
	got, err := resolveHostPorts([]string{"9222", "9222:9222/tcp", "9222/udp", "19322:9222"}, "private", nil)
	want := []PortMapping{{"tcp", 9222, 9222}, {"udp", 9222, 9222}, {"tcp", 9222, 19322}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %#v, %v", got, err)
	}
	for _, test := range []struct {
		name, network string
		values        []string
		published     []PortMapping
	}{
		{"host mode", "host", []string{"9222"}, nil},
		{"none mode", "none", []string{"9222"}, nil},
		{"conflict", "private", []string{"9222:80", "9222:81"}, nil},
		{"DNS TCP", "private", []string{"53:8053"}, nil},
		{"DNS UDP", "private", []string{"53:8053/udp"}, nil},
		{"proxy", "private", []string{"65532:80"}, nil},
		{"published", "private", []string{"9222"}, []PortMapping{{"tcp", 19222, 9222}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveHostPorts(test.values, test.network, test.published); err == nil {
				t.Fatal("accepted invalid mapping")
			}
		})
	}
	if _, err := resolveHostPorts([]string{"65532/udp", "9222/udp"}, "private", []PortMapping{{"tcp", 19222, 9222}}); err != nil {
		t.Fatal(err)
	}
}

func TestHostPortPlanAndRendering(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	opts := managedTestOptions(t.TempDir())
	opts.Network = "private"
	opts.HostPort = []string{"19222:9222", "19222:9222/tcp", "15353:5353/udp"}
	opts.Publish = []string{"19000:3000"}
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []PortMapping{{"tcp", 9222, 19222}, {"udp", 5353, 15353}}
	if !reflect.DeepEqual(plan.HostPorts, want) {
		t.Fatal(plan.HostPorts)
	}
	for _, podman := range []string{"", "/usr/bin/podman"} {
		plan.outer.podman = podman
		before := plan.Argv()
		runtime, err := plan.runtimeArgv(32123, 32124)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			argv     []string
			tcp, udp string
		}{
			{before, "65532:" + proxyPortPlaceholder + ",53:" + dnsPortPlaceholder + ",19222:9222", "53:" + dnsPortPlaceholder + ",15353:5353"},
			{runtime, "65532:32123,53:32124,19222:9222", "53:32124,15353:5353"},
		} {
			joined := strings.Join(test.argv, "\x00")
			for _, sequence := range []string{"--tcp-ns\x00" + test.tcp, "--udp-ns\x00" + test.udp, "--tcp-ports\x00none", "--udp-ports\x00none", "--tcp-ports\x00127.0.0.1/19000:3000"} {
				if !strings.Contains(joined, sequence) {
					t.Errorf("missing %q in %v", sequence, test.argv)
				}
			}
		}
		if !reflect.DeepEqual(plan.Argv(), before) {
			t.Fatal("runtime rendering mutated plan")
		}
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		HostPorts []PortMapping `json:"host_ports"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.HostPorts, want) {
		t.Fatal(output.String())
	}
	output.Reset()
	plan.HostPorts = nil
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"host_ports": []`) {
		t.Fatal(output.String())
	}
	opts.Publish = nil
	for _, mode := range []string{"host", "none"} {
		opts.Network = mode
		if _, err := BuildPlan(opts); err == nil || !strings.Contains(err.Error(), "--host-port requires") {
			t.Fatalf("%s mode: %v", mode, err)
		}
	}
}
