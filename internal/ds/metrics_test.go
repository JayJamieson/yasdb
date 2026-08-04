package ds

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

func TestSanitizeIdent(t *testing.T) {
	cases := []struct {
		in         string
		allowColon bool
		want       string
	}{
		{"slatedb.db.request_count", true, "slatedb_db_request_count"},
		{"op", false, "op"},
		{"9lives", true, "_lives"},
		{"", true, "_"},
		{"a:b", true, "a:b"},
		{"a:b", false, "a_b"},
	}
	for _, tc := range cases {
		if got := sanitizeIdent(tc.in, tc.allowColon); got != tc.want {
			t.Errorf("sanitizeIdent(%q, %v) = %q, want %q", tc.in, tc.allowColon, got, tc.want)
		}
	}
}

func TestWritePrometheusMetricsCounterAndGauge(t *testing.T) {
	metrics := []slatedb.Metric{
		{
			Name: "slatedb.db.request_count", Description: "DB requests",
			Labels: []slatedb.MetricLabel{{Key: "op", Value: "get"}},
			Value:  slatedb.MetricValueCounter{Field0: 42},
		},
		{
			Name: "slatedb.db.request_count", Description: "DB requests",
			Labels: []slatedb.MetricLabel{{Key: "op", Value: "scan"}},
			Value:  slatedb.MetricValueCounter{Field0: 1},
		},
		{
			Name: "slatedb.db.l0_sst_count", Description: "",
			Value: slatedb.MetricValueGauge{Field0: 3},
		},
	}

	var buf strings.Builder
	writePrometheusMetrics(&buf, metrics)
	out := buf.String()

	wantLines := []string{
		"# HELP slatedb_db_request_count DB requests",
		"# TYPE slatedb_db_request_count counter",
		`slatedb_db_request_count{op="get"} 42`,
		`slatedb_db_request_count{op="scan"} 1`,
		"# TYPE slatedb_db_l0_sst_count gauge",
		"slatedb_db_l0_sst_count 3",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("output missing line %q; full output:\n%s", want, out)
		}
	}
	// A metric with no description gets no # HELP line.
	if strings.Contains(out, "# HELP slatedb_db_l0_sst_count") {
		t.Errorf("unexpected # HELP for a metric with empty description:\n%s", out)
	}
}

func TestWritePrometheusMetricsHistogramCumulative(t *testing.T) {
	metrics := []slatedb.Metric{
		{
			Name:   "slatedb.object_store.request_duration_seconds",
			Labels: []slatedb.MetricLabel{{Key: "api", Value: "get"}},
			Value: slatedb.MetricValueHistogram{Field0: slatedb.HistogramMetricValue{
				Count:        7,
				Sum:          0.5,
				Boundaries:   []float64{0.001, 0.01, 0.1},
				BucketCounts: []uint64{2, 3, 1, 1}, // exclusive: last is the overflow bucket
			}},
		},
	}

	var buf strings.Builder
	writePrometheusMetrics(&buf, metrics)
	out := buf.String()

	// Prometheus buckets are cumulative, not exclusive.
	wantLines := []string{
		`slatedb_object_store_request_duration_seconds_bucket{api="get",le="0.001"} 2`,
		`slatedb_object_store_request_duration_seconds_bucket{api="get",le="0.01"} 5`,
		`slatedb_object_store_request_duration_seconds_bucket{api="get",le="0.1"} 6`,
		`slatedb_object_store_request_duration_seconds_bucket{api="get",le="+Inf"} 7`,
		`slatedb_object_store_request_duration_seconds_sum{api="get"} 0.5`,
		`slatedb_object_store_request_duration_seconds_count{api="get"} 7`,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("output missing line %q; full output:\n%s", want, out)
		}
	}
}

func TestWritePrometheusMetricsLabelEscaping(t *testing.T) {
	metrics := []slatedb.Metric{
		{
			Name:   "slatedb.test.metric",
			Labels: []slatedb.MetricLabel{{Key: "path", Value: `weird"value\with` + "\n" + `newline`}},
			Value:  slatedb.MetricValueCounter{Field0: 1},
		},
	}
	var buf strings.Builder
	writePrometheusMetrics(&buf, metrics)
	out := buf.String()
	want := `slatedb_test_metric{path="weird\"value\\with\nnewline"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("output missing escaped line %q; full output:\n%s", want, out)
	}
}

func TestWriteMetricsUnsupportedBackend(t *testing.T) {
	// fakeStore doesn't implement MetricsProvider, so WriteMetrics should still
	// succeed and emit yasdb's own counters, just no engine-level series.
	srv, err := NewServer(newFakeStore(), Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	var buf strings.Builder
	if err := srv.WriteMetrics(&buf); err != nil {
		t.Fatalf("WriteMetrics() error = %v, want nil", err)
	}
	out := buf.String()
	for _, want := range []string{
		"# TYPE yasdb_append_requests_total counter",
		"yasdb_append_requests_total 0",
		"# TYPE yasdb_append_records_total counter",
		"yasdb_append_records_total 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing line %q; full output:\n%s", want, out)
		}
	}
}

func TestWriteMetricsCountsAppends(t *testing.T) {
	srv, err := NewServer(newFakeStore(), Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv)
	defer ts.Close()

	mustDo(t, http.MethodPut, ts.URL+"/s", "text/plain", nil)
	mustDo(t, http.MethodPost, ts.URL+"/s", "text/plain", strings.NewReader("hello"))
	mustDo(t, http.MethodPost, ts.URL+"/s", "text/plain", strings.NewReader("world"))

	var buf strings.Builder
	if err := srv.WriteMetrics(&buf); err != nil {
		t.Fatalf("WriteMetrics() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"yasdb_append_requests_total 2",
		"yasdb_append_records_total 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing line %q; full output:\n%s", want, out)
		}
	}
}

func mustDo(t *testing.T, method, url, contentType string, body io.Reader) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: status %d", method, url, resp.StatusCode)
	}
}
