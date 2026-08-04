package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	vegeta "github.com/tsenart/vegeta/lib"
)

// stage mirrors one entry of a k6 ramping-arrival-rate `stages` array:
// linearly ramp the rate to Target over Duration.
type stage struct {
	Duration string `json:"duration"`
	Target   int    `json:"target"`
}

func main() {
	adminURL := flag.String("admin-url", "", "bulk-provision endpoint, e.g. http://yasdb.internal:9091/__admin/bulk-provision (required)")
	targetURL := flag.String("target-url", "", "base URL streams are appended/read under, e.g. http://yasdb.internal:4437 (required)")
	streams := flag.Int("streams", 8, "stream pool size")
	op := flag.String("op", "append", "append (POST, payload-bytes long) or read (GET ?offset=-1, streams must already have data)")
	payloadBytes := flag.Int("payload-bytes", 64, "append body size; ignored for -op=read")
	startRate := flag.Float64("start-rate", 0, "requests/sec at t=0, before the first stage's ramp begins")
	stagesJSON := flag.String("stages", "", `JSON array of {"duration":"10s","target":600}, applied in order (required)`)
	maxWorkers := flag.Uint64("max-workers", 500, "vegeta attacker max concurrent workers")
	seedRecords := flag.Int("seed-records", 0, "for -op=read: records to seed into each stream during setup")
	flag.Parse()

	if *adminURL == "" || *targetURL == "" || *stagesJSON == "" {
		fmt.Fprintln(os.Stderr, "loadshape: -admin-url, -target-url, and -stages are required")
		flag.Usage()
		os.Exit(2)
	}

	var stages []stage
	if err := json.Unmarshal([]byte(*stagesJSON), &stages); err != nil {
		log.Fatalf("parse -stages: %v", err)
	}
	parsed := make([]parsedStage, len(stages))
	for i, s := range stages {
		d, err := time.ParseDuration(s.Duration)
		if err != nil {
			log.Fatalf("stage %d duration %q: %v", i, s.Duration, err)
		}
		parsed[i] = parsedStage{duration: d, target: float64(s.Target)}
	}

	prefix := fmt.Sprintf("/loadshape/%d/s", time.Now().UnixMilli())
	paths := provision(*adminURL, *targetURL, prefix, *streams, *op, *seedRecords)
	log.Printf("provisioned %d stream(s) under %s", len(paths), prefix)

	targeter := buildTargeter(*targetURL, paths, *op, *payloadBytes)
	pacer := &stagedPacer{startRate: *startRate, stages: parsed}

	attacker := vegeta.NewAttacker(vegeta.MaxWorkers(*maxWorkers))
	var metrics vegeta.Metrics
	for res := range attacker.Attack(targeter, pacer, pacer.total(), "loadshape") {
		metrics.Add(res)
	}
	metrics.Close()

	reporter := vegeta.NewTextReporter(&metrics)
	if err := reporter.Report(os.Stdout); err != nil {
		log.Fatalf("report: %v", err)
	}
}

// provision creates the stream pool via bulk-provision and, for -op=read,
// seeds each stream with seedRecords records so there's history to read.
func provision(adminURL, targetURL, prefix string, n int, op string, seedRecords int) []string {
	body, _ := json.Marshal(map[string]any{
		"pathPrefix":  prefix,
		"count":       n,
		"contentType": "text/plain",
	})
	resp, err := http.Post(adminURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("bulk-provision: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("bulk-provision: status %d", resp.StatusCode)
	}

	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	if op == "read" && seedRecords > 0 {
		seedStreams(targetURL, paths, seedRecords)
	}
	return paths
}

func seedStreams(targetURL string, paths []string, records int) {
	body := strings.Repeat("x", 64)
	for _, p := range paths {
		for i := 0; i < records; i++ {
			resp, err := http.Post(targetURL+p, "text/plain", strings.NewReader(body))
			if err != nil {
				log.Fatalf("seed %s: %v", p, err)
			}
			resp.Body.Close()
		}
	}
}

func buildTargeter(baseURL string, paths []string, op string, payloadBytes int) vegeta.Targeter {
	// n is incremented concurrently: vegeta calls the Targeter from every
	// attack worker goroutine. So it must be atomic, not a plain closure
	// variable.
	var n atomic.Uint64
	switch op {
	case "append":
		body := []byte(strings.Repeat("x", payloadBytes))
		return func(t *vegeta.Target) error {
			if t == nil {
				return vegeta.ErrNilTarget
			}
			path := paths[n.Add(1)%uint64(len(paths))]
			t.Method = http.MethodPost
			t.URL = baseURL + path
			t.Body = body
			t.Header = http.Header{"Content-Type": []string{"text/plain"}}
			return nil
		}
	case "read":
		return func(t *vegeta.Target) error {
			if t == nil {
				return vegeta.ErrNilTarget
			}
			path := paths[n.Add(1)%uint64(len(paths))]
			t.Method = http.MethodGet
			t.URL = baseURL + path + "?offset=-1"
			return nil
		}
	default:
		log.Fatalf("unknown -op %q (want append or read)", op)
		return nil
	}
}

type parsedStage struct {
	duration time.Duration
	target   float64
}

// stagedPacer linearly interpolates the request rate from the previous
// stage's end rate to each stage's own target, over its own duration. It
// drives vegeta's Pacer interface the same way k6's ramping-arrival-rate
// executor drives its stages.
type stagedPacer struct {
	startRate float64
	stages    []parsedStage
}

func (p *stagedPacer) total() time.Duration {
	var d time.Duration
	for _, s := range p.stages {
		d += s.duration
	}
	return d
}

// maxPaceWait caps the inter-request wait, so a near-zero (not just
// exactly zero) interpolated rate cannot produce a multi-minute sleep.
// Linear interpolation crosses arbitrarily small positive rates on its way
// to or from 0, and 1/rate blows up asymptotically right as rate
// approaches it. Capping the wait directly, instead of special-casing
// rate<=0, handles both the exact-zero and near-zero cases the same way.
const maxPaceWait = time.Second

func (p *stagedPacer) Pace(elapsed time.Duration, _ uint64) (time.Duration, bool) {
	from := p.startRate
	var t time.Duration
	for _, s := range p.stages {
		if elapsed < t+s.duration {
			frac := float64(elapsed-t) / float64(s.duration)
			rate := from + frac*(s.target-from)
			if rate <= 0 {
				return maxPaceWait, false
			}
			wait := time.Duration(float64(time.Second) / rate)
			if wait > maxPaceWait {
				wait = maxPaceWait
			}
			return wait, false
		}
		t += s.duration
		from = s.target
	}
	return 0, true
}
