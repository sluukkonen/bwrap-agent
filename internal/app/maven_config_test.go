package app

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

func TestMavenSettingsEncodings(t *testing.T) {
	for _, test := range []struct {
		name, declaration, password string
		codec                       encoding.Encoding
	}{
		{"latin1-ascii", `encoding="ISO-8859-1"`, "secret", charmap.ISO8859_1},
		{"latin1", `encoding='iso-8859-1'`, "café-£", charmap.ISO8859_1},
		{"windows1252", `encoding = "windows-1252"`, "€–café", charmap.Windows1252},
		{"utf16le", `encoding="UTF-16"`, "秘密🔒", unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)},
		{"utf16be", `encoding="UTF-16"`, "秘密🔒", unicode.UTF16(unicode.BigEndian, unicode.UseBOM)},
		{"utf16le-no-bom", `encoding="UTF-16LE"`, "秘密🔒", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, proxy := range []string{"", "<proxies/>", "<proxies><proxy><host>old.example</host></proxy></proxies>"} {
				// Non-ASCII text before and after the edit catches byte-offset drift.
				preserved := "<servers><server><password>" + test.password + "</password></server></servers>"
				original, err := test.codec.NewEncoder().Bytes([]byte(`<?xml version="1.0" ` + test.declaration + ` standalone="yes"?><settings>` + preserved + proxy + "<!--" + test.password + "--></settings>"))
				if err != nil {
					t.Fatal(err)
				}
				state := t.TempDir()
				if _, err := writeStateFile(state, "home/.m2/settings.xml", original, 0o600); err != nil {
					t.Fatal(err)
				}
				files, err := prepareMavenConfig(agentContext{state: state}, "private", false)
				if err != nil {
					t.Fatal(err)
				}
				generated, err := os.ReadFile(files[0].Source)
				if err != nil {
					t.Fatal(err)
				}
				if !utf8.Valid(generated) || !bytes.Contains(generated, []byte(`encoding="UTF-8"`)) || !bytes.Contains(generated, []byte(`standalone="yes"`)) || !bytes.Contains(generated, []byte(preserved)) || !bytes.Contains(generated, []byte("<!--"+test.password+"-->")) {
					t.Fatalf("incorrect encoding normalization: %q", generated)
				}
				var settings struct {
					Host string `xml:"proxies>proxy>host"`
				}
				if err := xml.Unmarshal(generated, &settings); err != nil || settings.Host != "127.0.0.1" {
					t.Fatalf("generated settings invalid: %v, %v", settings, err)
				}
				again, err := mavenProxySettings(generated)
				if err != nil || !bytes.Equal(again, generated) {
					t.Fatalf("normalization not stable: %v", err)
				}
				unchanged, err := os.ReadFile(files[0].Destination)
				if err != nil || !bytes.Equal(unchanged, original) {
					t.Fatalf("original encoding changed: %v", err)
				}
			}
		})
	}
}

func TestMavenProxySettings(t *testing.T) {
	const preserved = `<!-- keep --><mirrors><mirror><id>corp</id><url>${env.REPO}</url><mirrorOf>*</mirrorOf></mirror></mirrors><servers><server><password>secret&amp;value</password><configuration><custom xmlns="urn:extension">value</custom></configuration></server></servers><profiles><profile><id>test</id></profile></profiles>`
	for _, input := range []string{
		defaultMavenSettings,
		"\xef\xbb\xbf<settings/>\r\n",
		"\xef\xbb\xbf<settings><proxies/></settings>\r\n",
		`<?xml version="1.0"?><settings xmlns="http://maven.apache.org/SETTINGS/1.2.0" />`,
		`<m:settings xmlns:m="http://maven.apache.org/SETTINGS/1.2.0"/>`,
		`<settings>` + preserved + `</settings>`,
		`<settings><proxies><proxy><host>old.example</host></proxy></proxies>` + preserved + `</settings>`,
		`<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0"><proxies/>` + preserved + `</settings>`,
		`<m:settings xmlns:m="http://maven.apache.org/SETTINGS/1.2.0"><m:proxies/></m:settings>`,
	} {
		t.Run(input, func(t *testing.T) {
			result, err := mavenProxySettings([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Proxies []struct {
					ID            string `xml:"id"`
					Protocol      string `xml:"protocol"`
					Host          string `xml:"host"`
					NonProxyHosts string `xml:"nonProxyHosts"`
					Active        bool   `xml:"active"`
					Port          int    `xml:"port"`
				} `xml:"proxies>proxy"`
			}
			if err := xml.Unmarshal(result, &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire.Proxies) != 1 {
				t.Fatalf("proxies = %#v", wire.Proxies)
			}
			proxy := wire.Proxies[0]
			if proxy.ID != "bwrap-agent" || !proxy.Active || proxy.Protocol != "http" || proxy.Host != "127.0.0.1" || proxy.Port != proxyGuestPort || proxy.NonProxyHosts != "localhost|127.0.0.1|::1|[::1]" {
				t.Fatalf("proxy = %#v", proxy)
			}
			if strings.Contains(input, preserved) && !bytes.Contains(result, []byte(preserved)) {
				t.Fatalf("unrelated XML changed: %s", result)
			}
			again, err := mavenProxySettings(result)
			if err != nil || !bytes.Equal(result, again) {
				t.Fatalf("regeneration changed settings: %s, %v", again, err)
			}
		})
	}
}

func TestMavenProxySettingsRejectsInvalidXML(t *testing.T) {
	for _, input := range []string{
		"", "<settings>", "<other/>", "<settings/><settings/>",
		"text<settings/>", "<settings/>text", "<settings><proxies/><proxies/></settings>",
		`<!DOCTYPE settings SYSTEM "file:///secret"><settings/>`,
		`<settings><password>&secret;</password></settings>`,
		`<?xml version="1.0" encoding="secret-unknown"?><settings/>`,
	} {
		t.Run(input, func(t *testing.T) {
			_, err := mavenProxySettings([]byte(input))
			if err == nil {
				t.Fatal("invalid XML accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked input: %v", err)
			}
		})
	}
}

func TestPrepareMavenConfigPreservesOriginal(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			state := t.TempDir()
			original := []byte(defaultMavenSettings)
			if existing {
				original = []byte(`<settings><proxies><proxy><host>old.example</host></proxy></proxies><offline>true</offline></settings>`)
				if _, err := writeStateFile(state, "home/.m2/settings.xml", original, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				files, err := prepareMavenConfig(agentContext{state: state}, "private", false)
				if err != nil {
					t.Fatal(err)
				}
				if len(files) != 1 || files[0].Permissions != "0600" {
					t.Fatalf("files = %#v", files)
				}
				got, err := os.ReadFile(files[0].Destination)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatalf("original changed: %q, %v", got, err)
				}
				info, err := os.Stat(files[0].Source)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("generated permissions: %v, %v", info, err)
				}
			}
		})
	}
}

func TestPrepareMavenConfigRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"home-symlink", "directory-symlink", "file-symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			state, outside := t.TempDir(), t.TempDir()
			var err error
			if kind == "home-symlink" {
				err = os.Symlink(outside, filepath.Join(state, "home"))
			} else {
				_, err = ensureStateDirectory(state, "home", 0o700)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "directory-symlink" {
					err = os.Symlink(outside, filepath.Join(state, "home/.m2"))
				} else {
					_, err = ensureStateDirectory(state, "home/.m2", 0o700)
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(state, "home/.m2/settings.xml")
					switch kind {
					case "file-symlink":
						err = os.Symlink(filepath.Join(outside, "missing.xml"), path)
					case "directory":
						err = os.Mkdir(path, 0o700)
					case "fifo":
						err = unix.Mkfifo(path, 0o600)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepareMavenConfig(agentContext{state: state}, "private", false); err == nil {
				t.Fatal("unsafe path accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside directory modified: %v, %v", entries, err)
			}
		})
	}
}

func TestMavenConfigLaunchModesAndHomeAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(t.TempDir(), "state:home"))
	for _, mode := range []string{"private", "private", "host", "none"} {
		plan, err := BuildPlan(Options{Project: ".", Instance: "maven", Network: mode, Podman: "off", TTY: "never", Command: []string{"/bin/true"}})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan.Bwrap, "\x00")
		if mode == "private" {
			passwd, err := os.ReadFile(filepath.Join(plan.State, "config/etc/passwd"))
			if err != nil || !bytes.Contains(passwd, []byte("root:x:0:0:root:"+sandboxPasswdHome+":/bin/sh")) {
				t.Fatalf("private pasta UID 0 home is not managed: %q, %v", passwd, err)
			}
		}
		for _, home := range []string{filepath.Join(plan.State, "home"), sandboxPasswdHome} {
			found := strings.Contains(joined, "\x00"+filepath.Join(home, ".m2/settings.xml"))
			if found != (mode == "private") {
				t.Fatalf("%s mode home %s mount = %v", mode, home, found)
			}
		}
		content, err := os.ReadFile(filepath.Join(plan.State, "home/.m2/settings.xml"))
		if err != nil || string(content) != defaultMavenSettings {
			t.Fatalf("persistent settings = %q, %v", content, err)
		}
	}
}
