package collector

import (
	"reflect"
	"sort"
	"strings"
	"testing"
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
