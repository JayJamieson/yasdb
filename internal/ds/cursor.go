package ds

import (
	"math/rand"
	"strconv"
	"time"
)

// Cursor collapsing parameters (protocol §10.1). Cursors are interval
// numbers counted from a fixed epoch. Echoing them as a query parameter
// gives CDNs a changing cache key, so waiters eventually break out of
// cached empty responses.
var cursorEpoch = time.Date(2024, 10, 9, 0, 0, 0, 0, time.UTC)

const cursorIntervalSecs = 20

// maxJitterIntervals is 3600s / 20s = 180 intervals (§10.1 monotonic progression).
const maxJitterIntervals = 3600 / cursorIntervalSecs

// computeCursor returns the cursor to send on a live response given the client's
// echoed cursor (may be ""). It never goes backwards: when the client's cursor
// is at or ahead of the current interval, it advances by random jitter so the
// cache key keeps changing.
func computeCursor(clientCursor string, now time.Time) string {
	cur := uint64(0)
	if d := now.Sub(cursorEpoch); d > 0 {
		cur = uint64(d.Seconds()) / cursorIntervalSecs
	}
	if clientCursor != "" {
		if c, err := strconv.ParseUint(clientCursor, 10, 64); err == nil && c >= cur {
			return strconv.FormatUint(c+1+uint64(rand.Intn(maxJitterIntervals)), 10)
		}
	}
	return strconv.FormatUint(cur, 10)
}
