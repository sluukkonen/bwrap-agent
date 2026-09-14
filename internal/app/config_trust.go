package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxProjectConfigSize = 1024 * 1024

type projectConfigTrust struct {
	Project string `json:"project"`
	SHA256  string `json:"sha256"`
}

func configDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// Validate even when project configuration is disabled: a sandbox must not be
// able to manufacture approvals for this or another project.
func projectTrustStore(project, gitCommon string, rwBind []string) (string, error) {
	base, err := stateBase()
	if err != nil {
		return "", err
	}
	lexical, err := filepath.Abs(filepath.Join(base, "trusted-projects"))
	if err != nil {
		return "", err
	}
	resolved, err := resolveState(lexical)
	if err != nil {
		return "", err
	}
	if err := validateInstanceStorePlacement(lexical, resolved, project, gitCommon, rwBind); err != nil {
		return "", fmt.Errorf("unsafe configuration trust store: %w", err)
	}
	instanceLexical, instanceResolved, err := managedInstancesPaths()
	if err != nil {
		return "", err
	}
	aliases, err := pathResolutionCandidates(lexical)
	if err != nil {
		return "", fmt.Errorf("resolve configuration trust store: %w", err)
	}
	// Protect against every instance, including ones already running. Check
	// intermediate symlinks too: writable state can retarget a trust-store alias
	// even when its current destination is outside managed instance storage.
	for _, instances := range []string{instanceLexical, instanceResolved} {
		if pathsOverlap(lexical, instances) || pathsOverlap(resolved, instances) {
			return "", fmt.Errorf("unsafe configuration trust store: overlaps managed instance store %s", instances)
		}
		for _, alias := range aliases {
			// Shared ancestor symlinks are harmless; only aliases within the
			// instance store can be controlled through writable instance state.
			if pathWithin(instances, alias) {
				return "", fmt.Errorf("unsafe configuration trust store: alias %s is within managed instance store %s", alias, instances)
			}
		}
	}
	return resolved, nil
}

func readProjectConfig(project string) (string, error) {
	fd, err := unix.Open(project, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	content, err := readSmallRegularFileAt(fd, projectConfigName, maxProjectConfigSize)
	if errors.Is(err, unix.ELOOP) {
		return "", fmt.Errorf("project configuration is a symlink: %s", filepath.Join(project, projectConfigName))
	}
	return content, err
}

func readProjectTrust(store, project string) (projectConfigTrust, error) {
	fd, err := unix.Open(store, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return projectConfigTrust{}, err
	}
	defer unix.Close(fd)
	content, err := readSmallRegularFileAt(fd, configDigest(project)+".json", 64*1024)
	if err != nil {
		return projectConfigTrust{}, err
	}
	var record projectConfigTrust
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		return record, fmt.Errorf("invalid configuration trust record: %w", err)
	}
	digest, err := hex.DecodeString(record.SHA256)
	if err != nil || len(digest) != sha256.Size || record.Project != project {
		return record, errors.New("invalid configuration trust record")
	}
	return record, nil
}

func loadTrustedProjectConfig(project string) (optionLayer, bool, error) {
	store, err := projectTrustStore(project, "", nil)
	if err != nil {
		return optionLayer{}, false, err
	}
	content, err := readProjectConfig(project)
	if errors.Is(err, os.ErrNotExist) {
		return optionLayer{}, false, nil
	}
	if err != nil {
		return optionLayer{}, false, err
	}
	record, err := readProjectTrust(store, project)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return optionLayer{}, false, fmt.Errorf("read configuration approval: %w", err)
	}
	if err != nil || record.SHA256 != configDigest(content) {
		return optionLayer{}, false, fmt.Errorf("project configuration is new, changed, or unapproved; review %q, then run bwrap-agent config trust in that project directory (or use --no-project-config to ignore it)", filepath.Join(project, projectConfigName))
	}
	// Hash and parse the same captured bytes, never reopen the path after approval.
	layer, err := decodeConfig(strings.NewReader(content), project)
	return layer, err == nil, err
}

func setProjectConfigTrust(directory string, trust bool, stdout io.Writer) error {
	project, err := resolveProjectDirectory(directory)
	if err != nil {
		return err
	}
	git, err := validatedGitMetadata(project)
	if err != nil {
		return err
	}
	store, err := projectTrustStore(project, git.ExternalCommon, nil)
	if err != nil {
		return err
	}
	name := configDigest(project) + ".json"
	if !trust {
		if err := unlinkStateFile(store, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Removed configuration approval for %q\n", project)
		return err
	}
	content, err := readProjectConfig(project)
	if err != nil {
		return err
	}
	if _, err := decodeConfig(strings.NewReader(content), project); err != nil {
		return err
	}
	record := projectConfigTrust{Project: project, SHA256: configDigest(content)}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := secureMkdir(store, 0o700); err != nil {
		return err
	}
	if _, err := writeStateFile(store, name, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Approved %q (SHA-256 %s)\n", filepath.Join(project, projectConfigName), record.SHA256)
	return err
}
