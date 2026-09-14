package app

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
)

const defaultMavenSettings = "<settings/>\n"

var mavenXMLEncoding = regexp.MustCompile(`encoding[\t\r\n ]*=[\t\r\n ]*("[^"]*"|'[^']*')`)

// Normalize before computing XML byte offsets: CharsetReader on the editing
// decoder alone would leave offsets referring to a different byte stream.
func normalizeMavenSettings(content []byte) ([]byte, error) {
	var initial encoding.Encoding
	switch {
	case bytes.HasPrefix(content, []byte{0xfe, 0xff}), bytes.HasPrefix(content, []byte{0, '<', 0, '?'}):
		initial = unicode.UTF16(unicode.BigEndian, unicode.UseBOM)
	case bytes.HasPrefix(content, []byte{0xff, 0xfe}), bytes.HasPrefix(content, []byte{'<', 0, '?', 0}):
		initial = unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)
	}
	if initial != nil {
		var err error
		content, err = initial.NewDecoder().Bytes(content)
		if err != nil {
			return nil, errors.New("could not decode Maven settings encoding")
		}
	}
	offset := 0
	if bytes.HasPrefix(content, []byte{0xef, 0xbb, 0xbf}) {
		offset = 3
	}
	decoder := xml.NewDecoder(bytes.NewReader(content[offset:]))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		// Read just the declaration here; decode the entire document below.
		return input, nil
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("invalid or unsupported Maven settings XML encoding; use UTF-8 settings")
	}
	declaration, ok := token.(xml.ProcInst)
	if !ok || declaration.Target != "xml" {
		return content, nil
	}
	// encoding/xml does not recognize whitespace around the declaration's '='.
	// Read the quoted attribute ourselves so valid XML receives conversion too.
	var declared encoding.Encoding
	if match := mavenXMLEncoding.FindSubmatch(declaration.Inst); match != nil {
		name := string(match[1][1 : len(match[1])-1])
		if !strings.EqualFold(name, "UTF-8") {
			declared, err = ianaindex.IANA.Encoding(name)
			if err != nil || declared == nil {
				return nil, errors.New("unsupported Maven settings XML encoding; use UTF-8 settings")
			}
		}
	}
	if initial == nil && declared == nil {
		return content, nil
	}
	end := int(decoder.InputOffset()) + offset
	header := mavenXMLEncoding.ReplaceAll(content[:end], []byte(`encoding="UTF-8"`))
	body := content[end:]
	if initial == nil && declared != nil {
		body, err = declared.NewDecoder().Bytes(body)
		if err != nil {
			return nil, errors.New("could not decode Maven settings encoding")
		}
	}
	return append(header, body...), nil
}

// mavenProxySettings edits only the direct proxies element. Retaining the other
// XML avoids losing extension settings, namespace prefixes, or credentials.
func mavenProxySettings(content []byte) ([]byte, error) {
	content, err := normalizeMavenSettings(content)
	if err != nil {
		return nil, err
	}
	// Maven accepts a UTF-8 BOM; keep it while parsing offsets into the original.
	var offset int64
	if bytes.HasPrefix(content, []byte{0xef, 0xbb, 0xbf}) {
		offset = 3
	}
	decoder := xml.NewDecoder(bytes.NewReader(content[offset:]))
	depth, roots := 0, 0
	var root xml.Name
	var rootEnd, closingStart, closingEnd, proxyStart, proxyEnd int64
	proxyStart = -1
	for {
		start := decoder.InputOffset() + offset
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Parser errors can include input text; do not echo secrets.
			return nil, errors.New("malformed Maven settings XML")
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || token.Name.Local != "settings" {
					return nil, errors.New("Maven settings must contain one settings root element")
				}
				root, rootEnd = token.Name, decoder.InputOffset()+offset
			}
			if depth == 1 && token.Name.Local == "proxies" && token.Name.Space == root.Space {
				if proxyStart >= 0 {
					return nil, errors.New("Maven settings contain duplicate proxies sections")
				}
				proxyStart = start
			}
			depth++
		case xml.EndElement:
			depth--
			if depth == 1 && token.Name.Local == "proxies" && token.Name.Space == root.Space {
				proxyEnd = decoder.InputOffset() + offset
			}
			if depth == 0 {
				closingStart, closingEnd = start, decoder.InputOffset()+offset
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(token)) != 0 {
				return nil, errors.New("unexpected text outside Maven settings root")
			}
		case xml.Directive:
			return nil, errors.New("XML directives are not supported in Maven settings")
		}
	}
	if roots != 1 || depth != 0 {
		return nil, errors.New("Maven settings must contain one complete settings root element")
	}
	var namespace bytes.Buffer
	if err := xml.EscapeText(&namespace, []byte(root.Space)); err != nil {
		return nil, err
	}
	proxy := fmt.Sprintf(`<proxies xmlns="%s"><proxy><id>bwrap-agent</id><active>true</active><protocol>http</protocol><host>127.0.0.1</host><port>%d</port><nonProxyHosts>localhost|127.0.0.1|::1|[::1]</nonProxyHosts></proxy></proxies>`, namespace.String(), proxyGuestPort)
	if proxyStart >= 0 {
		return []byte(string(content[:proxyStart]) + proxy + string(content[proxyEnd:])), nil
	}
	if closingEnd == rootEnd {
		// Expand a self-closing root, preserving its original qualified name.
		opening := string(content[:rootEnd-2])
		nameStart := bytes.LastIndexByte(content[:rootEnd], '<') + 1
		name := strings.FieldsFunc(string(content[nameStart:rootEnd]), func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '/' || r == '>'
		})[0]
		return []byte(opening + ">" + proxy + "</" + name + ">" + string(content[rootEnd:])), nil
	}
	return []byte(string(content[:closingStart]) + proxy + string(content[closingStart:])), nil
}

func prepareMavenConfig(context agentContext, generated, network string, inherit bool) ([]injectedFile, error) {
	state := context.state
	// This path can contain inherited host credentials. Never leave an old copy
	// visible when inheritance is disabled, the host file disappears, or a
	// different network mode no longer needs a generated overlay.
	if err := unlinkStateFile(state, "config/maven/settings.xml"); err != nil {
		return nil, fmt.Errorf("remove previous generated Maven settings: %w", err)
	}
	var source, securitySource string
	var content, securityContent []byte
	if inherit {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate Maven user configuration: %w", err)
		}
		source, content, err = readHostMavenFile(context, filepath.Join(home, ".m2", "settings.xml"))
		if err != nil {
			return nil, err
		}
		if source != "" {
			securitySource, securityContent, err = readHostMavenFile(context, filepath.Join(home, ".m2", "settings-security.xml"))
			if err != nil {
				return nil, err
			}
		}
	}
	if source == "" {
		if network != "private" {
			return nil, nil
		}
		parent, err := openStateDirectory(state, "home/.m2", 0o700)
		if err != nil {
			return nil, err
		}
		content, err = readMavenSettings(parent)
		unix.Close(parent)
		if errors.Is(err, unix.ENOENT) {
			content = []byte(defaultMavenSettings)
		} else if err != nil {
			return nil, fmt.Errorf("read instance .m2/settings.xml: %w", err)
		}
	}
	// Validate the selected settings in every mode, but expose their original
	// bytes in host/none mode so Maven keeps its original proxy configuration.
	privateSettings, err := mavenProxySettings(content)
	if err != nil {
		selected := source
		if selected == "" {
			selected = filepath.Join(state, "home/.m2/settings.xml")
		}
		return nil, fmt.Errorf("parse Maven settings %s: %w", selected, err)
	}
	if network == "private" {
		content = privateSettings
	}
	source, err = writeStateFile(generated, "maven/settings.xml", content, 0o600)
	if err != nil {
		return nil, err
	}
	if securitySource != "" {
		securitySource, err = writeStateFile(generated, "maven/settings-security.xml", securityContent, 0o600)
		if err != nil {
			return nil, err
		}
	}
	files, err := mavenFileMounts(state, source, "settings.xml", defaultMavenSettings)
	if err != nil {
		return nil, err
	}
	if securitySource != "" {
		securityFiles, err := mavenFileMounts(state, securitySource, "settings-security.xml", "<settingsSecurity/>\n")
		if err != nil {
			return nil, err
		}
		files = append(files, securityFiles...)
	}
	return files, nil
}

func readHostMavenFile(context agentContext, path string) (string, []byte, error) {
	source, found, err := resolveAgentSource(context, path, false, false)
	if err != nil {
		return "", nil, fmt.Errorf("resolve host Maven configuration %s: %w", path, err)
	}
	if !found {
		return "", nil, nil
	}
	file, err := openInjectedFile(source)
	if err != nil {
		return "", nil, fmt.Errorf("open host Maven configuration %s: %w", path, err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return "", nil, fmt.Errorf("read host Maven configuration %s: %w", path, err)
	}
	return source, content, nil
}

func mavenFileMounts(state, source, name, placeholder string) ([]injectedFile, error) {
	destination := filepath.Join(state, "home", ".m2", name)
	if err := validateAgentStateMountpoint(state, destination, source); err != nil {
		return nil, err
	}
	parent, err := openStateDirectory(state, "home/.m2", 0o700)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		// A valid, credential-free mountpoint keeps subsequent launches usable
		// when host inheritance is disabled or a host source is removed.
		if _, err := writeStateFile(state, filepath.Join("home/.m2", name), []byte(placeholder), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	files := []injectedFile{{Source: source, Destination: destination}}
	if home, aliased := accountHome(state); aliased {
		files = append(files, injectedFile{Source: source, Destination: filepath.Join(home, ".m2", name)})
	}
	return files, nil
}

func readMavenSettings(parent int) ([]byte, error) {
	// NONBLOCK lets us reject a FIFO without waiting for a writer.
	fd, err := unix.Openat(parent, "settings.xml", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "settings.xml")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Maven settings must be a regular file")
	}
	return io.ReadAll(file)
}
