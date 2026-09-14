package app

import (
	"fmt"
	"reflect"
	"testing"
)

func TestOuterCommandRendering(t *testing.T) {
	for _, mode := range []string{"host", "none", "private"} {
		for _, podman := range []string{"", "/usr/bin/podman"} {
			t.Run(mode+"/"+podman, func(t *testing.T) {
				plan := LaunchPlan{outer: outerCommand{podman: podman}, Launcher: []string{"launcher", "--"}, Bwrap: []string{"bwrap", "command"}}
				want := []string{}
				if podman != "" {
					want = append(want, "/usr/bin/podman", "unshare")
				}
				if mode == "none" {
					plan.Bwrap = []string{"bwrap", "--unshare-net", "command"}
				}
				if mode == "private" {
					plan.outer.pasta = "/usr/bin/pasta"
					plan.ProxyGuestPort = proxyGuestPort
					plan.Ports = []PortMapping{{Protocol: "tcp", Host: 18080, Guest: 8080}, {Protocol: "udp", Host: 15432, Guest: 5432}}
					want = append(want, "/usr/bin/pasta", "--quiet", "--config-net", "--splice-only")
					if podman != "" {
						want = append(want, "--netns-only")
					}
					want = append(want, "--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", "65532:32123,53:32124", "--udp-ns", "53:32124", "--tcp-ports", "127.0.0.1/18080:8080", "--udp-ports", "127.0.0.1/15432:5432", "--")
				}
				want = append(want, "launcher", "--")
				want = append(want, plan.Bwrap...)
				got, err := plan.runtimeArgv(32123, 32124)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("runtime argv = %#v, %v; want %#v", got, err, want)
				}
				if mode != "private" {
					got, err = plan.runtimeArgv(0, 0)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("non-private argv = %#v, %v", got, err)
					}
				}
			})
		}
	}
}

func TestOuterCommandRuntimePortValidation(t *testing.T) {
	plan := LaunchPlan{outer: outerCommand{pasta: "pasta"}, ProxyGuestPort: proxyGuestPort}
	for _, ports := range [][2]int{{0, 1}, {-1, 1}, {65536, 1}, {1, 0}, {1, -1}, {1, 65536}} {
		if argv, err := plan.runtimeArgv(ports[0], ports[1]); err == nil || argv != nil {
			t.Fatalf("accepted invalid ports %v: %v, %v", ports, argv, err)
		}
	}
	got, err := plan.runtimeArgv(1, 65535)
	want := []string{"pasta", "--quiet", "--config-net", "--splice-only", "--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", "65532:1,53:65535", "--udp-ns", "53:65535", "--"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("boundary ports = %#v, %v", got, err)
	}
	plan.outer.pasta = ""
	if argv, err := plan.runtimeArgv(1, 65535); err == nil || argv != nil {
		t.Fatalf("accepted private plan without pasta: %v, %v", argv, err)
	}
}

func TestOuterCommandRenderingPreservesOpaqueArguments(t *testing.T) {
	literal := "65532:" + proxyPortPlaceholder + ",53:" + dnsPortPlaceholder
	plan := LaunchPlan{
		outer:          outerCommand{podman: "/tools/" + literal + "/podman", pasta: "/tools/" + literal + "/pasta"},
		ProxyGuestPort: proxyGuestPort,
		Launcher:       []string{"launcher", literal}, Bwrap: []string{"bwrap", "--setenv", "LITERAL", literal, "command", literal},
		Ports: []PortMapping{{Protocol: "tcp", Host: 12345, Guest: 80}},
	}
	want := []string{plan.outer.podman, "unshare", plan.outer.pasta, "--quiet", "--config-net", "--splice-only", "--netns-only", "--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", literal, "--udp-ns", "53:" + dnsPortPlaceholder, "--tcp-ports", "127.0.0.1/12345:80", "--", "launcher", literal, "bwrap", "--setenv", "LITERAL", literal, "command", literal}
	before := plan.Argv()
	if !reflect.DeepEqual(before, want) {
		t.Fatalf("descriptive argv = %#v; want %#v", before, want)
	}
	for _, ports := range [][2]int{{123, 456}, {321, 654}} {
		got, err := plan.runtimeArgv(ports[0], ports[1])
		if err != nil {
			t.Fatal(err)
		}
		runtimeWant := append([]string(nil), want...)
		runtimeWant[12] = fmt.Sprintf("65532:%d,53:%d", ports[0], ports[1])
		runtimeWant[14] = fmt.Sprintf("53:%d", ports[1])
		if !reflect.DeepEqual(got, runtimeWant) {
			t.Fatalf("runtime argv = %#v; want %#v", got, runtimeWant)
		}
		if !reflect.DeepEqual(plan.Argv(), before) {
			t.Fatal("runtime rendering mutated descriptive plan")
		}
		got[0] = "changed"
		got[len(got)-1] = "changed"
		if !reflect.DeepEqual(plan.Argv(), before) {
			t.Fatal("returned arguments alias the plan")
		}
	}
}
