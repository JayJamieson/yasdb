package ds

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

// WriteMetrics writes yasdb's own request-level counters to w, in
// Prometheus text exposition format
// (https://prometheus.io/docs/instrumenting/exposition_formats/). When the
// storage backend implements MetricsProvider (storage.go), it also writes
// every metric the storage engine has registered. Backends without
// MetricsProvider (e.g. pwal, or the in-memory fakeStore the test suite
// uses by default) still get the yasdb-level counters below; they just do
// not contribute engine-level series.
func (s *Server) WriteMetrics(w io.Writer) error {
	writeYasdbMetrics(w, s)
	if provider, ok := s.store.(MetricsProvider); ok {
		writePrometheusMetrics(w, provider.MetricsSnapshot())
	}
	return nil
}

// writeYasdbMetrics emits counters tracked in Go, not by the storage engine.
// This makes throughput queryable via Prometheus rate() over an exact time
// window. A load test's own end-of-run summary cannot do this: its printed
// "/s" rate divides by the whole test wall-clock (setup, run, and
// teardown), not just the timed scenario, and is silently wrong whenever
// those are not small next to each other (see BENCHMARKS.md).
func writeYasdbMetrics(w io.Writer, s *Server) {
	fmt.Fprintf(w, "# HELP yasdb_append_requests_total Total successful append HTTP requests (POST with a 2xx response).\n")
	fmt.Fprintf(w, "# TYPE yasdb_append_requests_total counter\n")
	fmt.Fprintf(w, "yasdb_append_requests_total %d\n", s.appendRequests.Load())

	fmt.Fprintf(w, "# HELP yasdb_append_records_total Total records durably appended across all streams (a single POST may append more than one record).\n")
	fmt.Fprintf(w, "# TYPE yasdb_append_records_total counter\n")
	fmt.Fprintf(w, "yasdb_append_records_total %d\n", s.appendRecords.Load())
}

// metricGroup collects every label-variant sample of one metric name, so
// each gets a single "# HELP"/"# TYPE" pair (per the exposition format) even
// though SlateDB reports one Metric per label combination.
type metricGroup struct {
	promName    string
	description string
	kind        string // Prometheus TYPE: counter, gauge, or histogram
	samples     []slatedb.Metric
}

func writePrometheusMetrics(w io.Writer, metrics []slatedb.Metric) {
	groups := make(map[string]*metricGroup)
	var order []string
	for _, m := range metrics {
		name := sanitizeMetricName(m.Name)
		g, ok := groups[name]
		if !ok {
			g = &metricGroup{promName: name, kind: prometheusKind(m.Value)}
			groups[name] = g
			order = append(order, name)
		}
		if g.description == "" {
			g.description = m.Description
		}
		g.samples = append(g.samples, m)
	}
	sort.Strings(order)

	for _, name := range order {
		g := groups[name]
		if g.description != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", g.promName, escapeHelp(g.description))
		}
		fmt.Fprintf(w, "# TYPE %s %s\n", g.promName, g.kind)
		sort.Slice(g.samples, func(i, j int) bool {
			return labelString(g.samples[i].Labels) < labelString(g.samples[j].Labels)
		})
		for _, m := range g.samples {
			writeSample(w, g.promName, m)
		}
	}
}

// prometheusKind maps a SlateDB metric value onto the closest Prometheus
// metric type. Prometheus has no dedicated up/down-counter type, so that
// case maps to gauge: its exposition is indistinguishable from a plain
// gauge.
func prometheusKind(v slatedb.MetricValue) string {
	switch v.(type) {
	case slatedb.MetricValueCounter:
		return "counter"
	case slatedb.MetricValueHistogram:
		return "histogram"
	default:
		return "gauge"
	}
}

func writeSample(w io.Writer, name string, m slatedb.Metric) {
	switch v := m.Value.(type) {
	case slatedb.MetricValueCounter:
		fmt.Fprintf(w, "%s%s %d\n", name, formatLabels(m.Labels, nil), v.Field0)
	case slatedb.MetricValueGauge:
		fmt.Fprintf(w, "%s%s %d\n", name, formatLabels(m.Labels, nil), v.Field0)
	case slatedb.MetricValueUpDownCounter:
		fmt.Fprintf(w, "%s%s %d\n", name, formatLabels(m.Labels, nil), v.Field0)
	case slatedb.MetricValueHistogram:
		writeHistogram(w, name, m.Labels, v.Field0)
	}
}

// writeHistogram expands one SlateDB histogram value into Prometheus's
// cumulative _bucket/_sum/_count series. SlateDB's BucketCounts are
// per-bucket exclusive: one more entry than Boundaries, with the last entry
// being the overflow bucket above the last boundary. Prometheus buckets are
// cumulative counts up to and including "le", so this function runs a
// prefix sum.
func writeHistogram(w io.Writer, name string, labels []slatedb.MetricLabel, h slatedb.HistogramMetricValue) {
	var cumulative uint64
	for i, boundary := range h.Boundaries {
		if i < len(h.BucketCounts) {
			cumulative += h.BucketCounts[i]
		}
		le := []slatedb.MetricLabel{{Key: "le", Value: formatFloat(boundary)}}
		fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(labels, le), cumulative)
	}
	le := []slatedb.MetricLabel{{Key: "le", Value: "+Inf"}}
	fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(labels, le), h.Count)
	fmt.Fprintf(w, "%s_sum%s %s\n", name, formatLabels(labels, nil), formatFloat(h.Sum))
	fmt.Fprintf(w, "%s_count%s %d\n", name, formatLabels(labels, nil), h.Count)
}

// formatLabels renders labels ++ extra (e.g. a histogram's "le") as a
// Prometheus label-set suffix: "" when empty, otherwise "{k=\"v\",...}".
func formatLabels(labels, extra []slatedb.MetricLabel) string {
	if len(labels) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	writeLabelList(&b, labels)
	if len(labels) > 0 && len(extra) > 0 {
		b.WriteByte(',')
	}
	writeLabelList(&b, extra)
	b.WriteByte('}')
	return b.String()
}

func writeLabelList(b *strings.Builder, labels []slatedb.MetricLabel) {
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(sanitizeLabelName(l.Key))
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.Value))
		b.WriteByte('"')
	}
}

// labelString is formatLabels without braces. It is used only as a stable
// sort key, to order a metric's label-variant samples deterministically.
func labelString(labels []slatedb.MetricLabel) string {
	var b strings.Builder
	writeLabelList(&b, labels)
	return b.String()
}

func sanitizeMetricName(name string) string { return sanitizeIdent(name, true) }
func sanitizeLabelName(name string) string  { return sanitizeIdent(name, false) }

// sanitizeIdent maps s onto the Prometheus identifier charset
// ([a-zA-Z_:][a-zA-Z0-9_:]*, or without ':' for label names). SlateDB
// metric names use '.' as a namespace separator (e.g.
// "slatedb.db.request_count"), which is not legal in a Prometheus name. So
// every disallowed byte, and any leading digit, becomes '_'.
func sanitizeIdent(s string, allowColon bool) string {
	if s == "" {
		return "_"
	}
	b := []byte(s)
	for i, c := range b {
		valid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(allowColon && c == ':') || (i > 0 && c >= '0' && c <= '9')
		if !valid {
			b[i] = '_'
		}
	}
	return string(b)
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
