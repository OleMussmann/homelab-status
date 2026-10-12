package collector

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func failedServices(t *testing.T, text string) []string {
	t.Helper()
	m, err := parseNodeExporterMetrics(strings.NewReader(text), "")
	if err != nil {
		t.Fatalf("parseNodeExporterMetrics failed: %v", err)
	}
	if m.FailedServices == nil {
		t.Fatal("FailedServices is nil, want an empty slice")
	}
	got := append([]string{}, m.FailedServices...)
	sort.Strings(got)
	return got
}

func TestFailedServices(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "nothing failed",
			text: `# TYPE node_systemd_unit_state gauge
node_systemd_unit_state{name="a.service",state="active",type="simple"} 1
node_systemd_unit_state{name="a.service",state="failed",type="simple"} 0
`,
			want: []string{},
		},
		{
			name: "failed state",
			text: `# TYPE node_systemd_unit_state gauge
node_systemd_unit_state{name="a.service",state="failed",type="simple"} 1
node_systemd_unit_state{name="b.service",state="failed",type="simple"} 0
`,
			want: []string{"a.service"},
		},
		{
			name: "failed state was reset, last run still failed",
			text: `# TYPE node_systemd_unit_state gauge
node_systemd_unit_state{name="job.service",state="failed",type="oneshot"} 0
node_systemd_unit_state{name="job.service",state="inactive",type="oneshot"} 1
# TYPE node_systemd_unit_last_run_failed gauge
node_systemd_unit_last_run_failed{name="job.service"} 125
`,
			want: []string{"job.service"},
		},
		{
			name: "both sources name the unit once",
			text: `# TYPE node_systemd_unit_state gauge
node_systemd_unit_state{name="job.service",state="failed",type="oneshot"} 1
node_systemd_unit_state{name="other.service",state="failed",type="simple"} 1
# TYPE node_systemd_unit_last_run_failed gauge
node_systemd_unit_last_run_failed{name="job.service"} 1
`,
			want: []string{"job.service", "other.service"},
		},
		{
			name: "host without the textfile metric",
			text: `# TYPE node_load1 gauge
node_load1 0.5
`,
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failedServices(t, tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FailedServices = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIncusExportLast(t *testing.T) {
	parse := func(text string) *NixOSMetrics {
		t.Helper()
		m, err := parseNodeExporterMetrics(strings.NewReader(text), "")
		if err != nil {
			t.Fatalf("parseNodeExporterMetrics failed: %v", err)
		}
		return m
	}

	t.Run("absent", func(t *testing.T) {
		m := parse("node_load1 0.5\n")
		if m.IncusExportLast != nil || m.IncusExportLastHours != nil {
			t.Errorf("want nil export fields, got %v / %v", m.IncusExportLast, m.IncusExportLastHours)
		}
	})

	t.Run("never exported", func(t *testing.T) {
		m := parse("node_incus_export_last_timestamp_seconds 0\n")
		if m.IncusExportLast != nil || m.IncusExportLastHours != nil {
			t.Errorf("want nil export fields, got %v / %v", m.IncusExportLast, m.IncusExportLastHours)
		}
	})

	t.Run("three hours ago", func(t *testing.T) {
		ts := time.Now().Add(-3 * time.Hour).Unix()
		m := parse(fmt.Sprintf("node_incus_export_last_timestamp_seconds %d\n", ts))
		if m.IncusExportLast == nil || int64(*m.IncusExportLast) != ts {
			t.Fatalf("IncusExportLast = %v, want %d", m.IncusExportLast, ts)
		}
		if h := *m.IncusExportLastHours; h < 2.9 || h > 3.1 {
			t.Errorf("IncusExportLastHours = %v, want about 3", h)
		}
	})
}
