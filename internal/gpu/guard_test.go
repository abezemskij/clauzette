package gpu

import "testing"

func TestParseMetricLine(t *testing.T) {
	cases := []struct {
		line string
		name string
		val  float64
		ok   bool
	}{
		{`DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-1",modelName="TITAN RTX"} 67`, "DCGM_FI_DEV_GPU_TEMP", 67, true},
		{`nvidia_smi_utilization_gpu_ratio{uuid="x"} 0.98 1700000000000`, "nvidia_smi_utilization_gpu_ratio", 0.98, true},
		{`plain_metric 12.5`, "plain_metric", 12.5, true},
		{`broken{label="x"`, "", 0, false},
		{`no_value{a="b"}`, "", 0, false},
	}
	for _, c := range cases {
		name, val, ok := parseMetricLine(c.line)
		if ok != c.ok || name != c.name || val != c.val {
			t.Errorf("parseMetricLine(%q) = %q, %v, %v; want %q, %v, %v", c.line, name, val, ok, c.name, c.val, c.ok)
		}
	}
}
