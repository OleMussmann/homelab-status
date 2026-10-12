// Package collector implements metric scrapers for Node Exporter and Incus.
package collector

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// NixOSMetrics holds the translated, frontend-ready metrics for one NixOS machine.
type NixOSMetrics struct {
	UptimeHours      float64           `json:"uptime_hours"`
	CPULoad1m        float64           `json:"cpu_load_1m"`
	RAMUsedPercent   float64           `json:"ram_used_percent"`
	DiskUsedPercent  float64           `json:"disk_used_percent"`
	SMARTHealthy     bool              `json:"smart_healthy"`
	SMARTStatus      string            `json:"smart_status"`
	SMARTDisks       map[string]bool   `json:"smart_disks,omitempty"`
	FailedServices   []string          `json:"failed_services"`
	OOMKills         int64             `json:"oom_kills"`
	RebootRequired   bool              `json:"reboot_required"`
	NixOSGeneration  int64             `json:"nixos_generation"`
	Temperatures     map[string]float64 `json:"temperatures"`
	BorgLastBackup      *float64 `json:"borg_last_backup,omitempty"`
	BorgLastBackupHours *float64 `json:"borg_last_backup_hours,omitempty"`
}

// ScrapeNodeExporter fetches and parses metrics from a Prometheus Node Exporter endpoint.
func ScrapeNodeExporter(ctx context.Context, client *http.Client, url, username, password string) (*NixOSMetrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape returned status %d", resp.StatusCode)
	}

	return parseNodeExporterMetrics(resp.Body, resp.Header.Get("Content-Type"))
}

func parseNodeExporterMetrics(r io.Reader, contentType string) (*NixOSMetrics, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("parse prometheus text: %w", err)
	}

	m := &NixOSMetrics{
		SMARTHealthy: true, // default healthy until proven otherwise
		SMARTStatus:  "OK",
		Temperatures: make(map[string]float64),
	}

	m.UptimeHours = calcUptime(families)
	m.CPULoad1m = gaugeValue(families, "node_load1")
	m.RAMUsedPercent = calcRAMPercent(families)
	m.DiskUsedPercent = calcRootDiskPercent(families)
	smart := calcSMARTHealth(families)
	m.SMARTHealthy = smart.Healthy
	m.SMARTStatus = smart.Status
	m.SMARTDisks = smart.Disks
	m.FailedServices = calcFailedServices(families)
	m.OOMKills = int64(gaugeValue(families, "node_vmstat_oom_kill"))
	m.RebootRequired = calcRebootRequired(families)
	m.NixOSGeneration = int64(gaugeValue(families, "node_nixos_generation"))
	m.Temperatures = calcTemperatures(families)
	m.BorgLastBackup = calcBorgLastBackup(families)
	if m.BorgLastBackup != nil {
		hours := time.Since(time.Unix(int64(*m.BorgLastBackup), 0)).Hours()
		h := math.Round(hours*10) / 10
		m.BorgLastBackupHours = &h
	}

	return m, nil
}

func gaugeValue(families map[string]*dto.MetricFamily, name string) float64 {
	fam, ok := families[name]
	if !ok || len(fam.GetMetric()) == 0 {
		return 0
	}
	metric := fam.GetMetric()[0]
	if g := metric.GetGauge(); g != nil {
		return g.GetValue()
	}
	if u := metric.GetUntyped(); u != nil {
		return u.GetValue()
	}
	return 0
}

func calcUptime(families map[string]*dto.MetricFamily) float64 {
	now := gaugeValue(families, "node_time_seconds")
	boot := gaugeValue(families, "node_boot_time_seconds")
	if now == 0 || boot == 0 {
		return 0
	}
	hours := (now - boot) / 3600
	return math.Round(hours*10) / 10
}

func calcRAMPercent(families map[string]*dto.MetricFamily) float64 {
	total := gaugeValue(families, "node_memory_MemTotal_bytes")
	avail := gaugeValue(families, "node_memory_MemAvailable_bytes")
	if total == 0 {
		return 0
	}
	pct := ((total - avail) / total) * 100
	return math.Round(pct*10) / 10
}

func calcRootDiskPercent(families map[string]*dto.MetricFamily) float64 {
	sizeFam := families["node_filesystem_size_bytes"]
	availFam := families["node_filesystem_avail_bytes"]
	if sizeFam == nil || availFam == nil {
		return 0
	}

	// Build maps of mountpoint -> size/avail bytes.
	type fsInfo struct {
		size, avail float64
		fstype      string
	}
	filesystems := make(map[string]fsInfo)

	for _, m := range sizeFam.GetMetric() {
		mp := labelValue(m, "mountpoint")
		fstype := labelValue(m, "fstype")
		if m.GetGauge() == nil || mp == "" {
			continue
		}
		// Skip pseudo filesystems.
		if fstype == "tmpfs" || fstype == "devtmpfs" || fstype == "overlay" ||
			fstype == "ramfs" || fstype == "fuse.lxcfs" {
			continue
		}
		filesystems[mp] = fsInfo{size: m.GetGauge().GetValue(), fstype: fstype}
	}
	for _, m := range availFam.GetMetric() {
		mp := labelValue(m, "mountpoint")
		if m.GetGauge() == nil || mp == "" {
			continue
		}
		if fs, ok := filesystems[mp]; ok {
			fs.avail = m.GetGauge().GetValue()
			filesystems[mp] = fs
		}
	}

	// For ZFS: all datasets in a pool share the same free space, so
	// individual dataset usage is misleading (especially "/" which may
	// be nearly empty on impermanence setups). Use the largest dataset
	// by size, which best represents the pool capacity.
	var bestSize, bestAvail float64
	isZFS := false
	for _, fs := range filesystems {
		if fs.fstype == "zfs" {
			isZFS = true
			break
		}
	}

	if isZFS {
		// Pick the dataset with the largest reported size.
		for _, fs := range filesystems {
			if fs.fstype == "zfs" && fs.size > bestSize {
				bestSize = fs.size
				bestAvail = fs.avail
			}
		}
	} else {
		// Non-ZFS: prefer the root mount, fall back to largest filesystem.
		for mp, fs := range filesystems {
			if mp == "/" {
				bestSize = fs.size
				bestAvail = fs.avail
				break
			}
			if fs.size > bestSize {
				bestSize = fs.size
				bestAvail = fs.avail
			}
		}
	}

	if bestSize == 0 {
		return 0
	}
	pct := ((bestSize - bestAvail) / bestSize) * 100
	return math.Round(pct*10) / 10
}

// smartResult holds per-disk SMART health and aggregate status.
type smartResult struct {
	Healthy bool
	Status  string
	Disks   map[string]bool // disk path -> healthy
}

func calcSMARTHealth(families map[string]*dto.MetricFamily) smartResult {
	// textfile metric: node_smart_healthy{disk="/dev/sda"} 1 = healthy, 0 = failing
	fam, ok := families["node_smart_healthy"]
	if !ok {
		return smartResult{Healthy: true, Status: "OK"} // no SMART data means we assume healthy
	}

	disks := make(map[string]bool)
	allHealthy := true

	for _, m := range fam.GetMetric() {
		disk := labelValue(m, "disk")
		if m.GetGauge() != nil {
			healthy := m.GetGauge().GetValue() != 0
			if disk != "" {
				disks[disk] = healthy
			}
			if !healthy {
				allHealthy = false
			}
		}
	}

	status := "OK"
	if !allHealthy {
		status = "FAILING"
	}

	return smartResult{
		Healthy: allHealthy,
		Status:  status,
		Disks:   disks,
	}
}

// calcFailedServices lists units that are in the failed state or whose last
// run exited with an error. The second source exists because the failed state
// does not last: `switch-to-configuration` resets it, the exit status of the
// last run stays until the unit runs again.
func calcFailedServices(families map[string]*dto.MetricFamily) []string {
	failed := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			failed = append(failed, name)
		}
	}

	// systemd collector: node_systemd_unit_state{name="...",state="failed"} 1
	if fam, ok := families["node_systemd_unit_state"]; ok {
		for _, m := range fam.GetMetric() {
			if labelValue(m, "state") == "failed" && m.GetGauge() != nil && m.GetGauge().GetValue() == 1 {
				add(labelValue(m, "name"))
			}
		}
	}

	// textfile metric: node_systemd_unit_last_run_failed{name="..."} <exit status>
	if fam, ok := families["node_systemd_unit_last_run_failed"]; ok {
		for _, m := range fam.GetMetric() {
			if m.GetGauge() != nil && m.GetGauge().GetValue() != 0 {
				add(labelValue(m, "name"))
			}
		}
	}

	return failed
}

func calcRebootRequired(families map[string]*dto.MetricFamily) bool {
	// textfile metric: node_reboot_required 1 = reboot needed
	v := gaugeValue(families, "node_reboot_required")
	return v == 1
}

func calcTemperatures(families map[string]*dto.MetricFamily) map[string]float64 {
	temps := make(map[string]float64)
	fam, ok := families["node_hwmon_temp_celsius"]
	if !ok {
		return temps
	}
	for _, m := range fam.GetMetric() {
		if m.GetGauge() == nil {
			continue
		}
		chip := labelValue(m, "chip")
		sensor := labelValue(m, "sensor")
		key := chip
		if sensor != "" {
			key = chip + "_" + sensor
		}
		if key == "" {
			continue
		}
		temps[key] = math.Round(m.GetGauge().GetValue()*10) / 10
	}
	return temps
}

func calcBorgLastBackup(families map[string]*dto.MetricFamily) *float64 {
	// textfile metric: node_borg_last_backup_timestamp_seconds
	fam, ok := families["node_borg_last_backup_timestamp_seconds"]
	if !ok || len(fam.GetMetric()) == 0 {
		return nil
	}

	var oldestValid float64 = -1

	for _, metric := range fam.GetMetric() {
		var val float64
		if g := metric.GetGauge(); g != nil {
			val = g.GetValue()
		} else if u := metric.GetUntyped(); u != nil {
			val = u.GetValue()
		}
		
		// A value of 0 means the script failed to query the repository.
		if val > 0 {
			if oldestValid == -1 || val < oldestValid {
				oldestValid = val
			}
		}
	}

	if oldestValid == -1 {
		return nil
	}

	return &oldestValid
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}


