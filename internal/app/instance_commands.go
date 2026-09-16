package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

type instanceRecord struct {
	Name           string    `json:"name"`
	Project        string    `json:"project"`
	Status         string    `json:"status"`
	DiskUsageBytes uint64    `json:"disk_usage_bytes"`
	CreatedAt      time.Time `json:"created_at"`
	LastUsedAt     time.Time `json:"last_used_at"`
	StatePath      string    `json:"state_path"`
}

func instanceRecordFromSnapshot(snapshot instanceSnapshot) instanceRecord {
	return instanceRecord{
		Name: snapshot.metadata.Name, Project: snapshot.metadata.Project,
		Status: snapshot.status, DiskUsageBytes: snapshot.diskUsageBytes,
		CreatedAt: snapshot.metadata.CreatedAt, LastUsedAt: snapshot.metadata.LastUsedAt,
		StatePath: filepath.Join(snapshot.root, "state"),
	}
}

func humanBytes(bytes uint64) string {
	const unit = uint64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, suffix := range units {
		value /= 1024
		if value < 1024 || suffix == units[len(units)-1] {
			if value >= 10 {
				return fmt.Sprintf("%.0f %s", value, suffix)
			}
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

func tableField(value string) string {
	for _, char := range value {
		if unicode.IsControl(char) {
			return strconv.QuoteToGraphic(value)
		}
	}
	return value
}

func listInstances(asJSON bool, output io.Writer) error {
	snapshots, err := managedInstanceSnapshots()
	if err != nil {
		return err
	}
	records := make([]instanceRecord, 0, len(snapshots))
	for _, snapshot := range snapshots {
		records = append(records, instanceRecordFromSnapshot(snapshot))
	}
	return writeInstanceList(records, asJSON, output)
}

func writeInstanceList(records []instanceRecord, asJSON bool, output io.Writer) error {
	if asJSON {
		if records == nil {
			records = []instanceRecord{}
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(records)
	}
	if len(records) == 0 {
		_, err := fmt.Fprintln(output, "No instances.")
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tSTATUS\tDISK USAGE\tLAST USED\tPROJECT"); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", record.Name, record.Status,
			humanBytes(record.DiskUsageBytes), record.LastUsedAt.Local().Format("2006-01-02 15:04"), tableField(record.Project)); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func confirmedDeletion(name string, snapshot instanceSnapshot, yes, interactive bool, input io.Reader, prompt io.Writer) (bool, error) {
	if yes {
		return true, nil
	}
	if !interactive {
		return false, errors.New("deletion requires an interactive terminal or --yes")
	}
	if _, err := fmt.Fprintf(prompt, "Delete instance %q for project %q (%s)? [y/N] ", name, snapshot.metadata.Project, humanBytes(snapshot.diskUsageBytes)); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func deleteInstance(name string, yes, interactive bool, input io.Reader, output, prompt io.Writer) error {
	if _, err := safeName(name); err != nil {
		return err
	}
	store, found, err := managedInstancesDirectory(false)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("instance not found: %s", name)
	}
	snapshot, err := inspectManagedInstance(store, name)
	if errors.Is(err, errManagedInstanceNotFound) {
		removed, cleanupErr := retryPendingInstanceDeletion(store, name)
		if cleanupErr != nil {
			return cleanupErr
		}
		if removed > 0 {
			_, writeErr := fmt.Fprintf(output, "Deleted pending instance data for %q.\n", name)
			return writeErr
		}
	}
	if err != nil {
		return err
	}
	if snapshot.status == "running" {
		return fmt.Errorf("instance %s is running", name)
	}
	confirmed, err := confirmedDeletion(name, snapshot, yes, interactive, input, prompt)
	if err != nil {
		return err
	}
	if !confirmed {
		_, err := fmt.Fprintln(output, "Not deleted.")
		return err
	}
	if err := deleteManagedInstance(name, snapshot.metadata); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Deleted instance %q.\n", name)
	return err
}
