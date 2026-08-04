package ds

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// adminStreamsPath serves a paginated listing of every stream: path, content
// type, closed status, record count, fork status, and live reader counts
// split by long-poll vs SSE.
const adminStreamsPath = "/__admin/streams"

const (
	defaultAdminPageLimit = 50
	maxAdminPageLimit     = 500
)

type adminStreamReaders struct {
	LongPoll int64 `json:"longPoll"`
	SSE      int64 `json:"sse"`
}

type adminStreamInfo struct {
	Path        string             `json:"path"`
	ContentType string             `json:"contentType"`
	Closed      bool               `json:"closed"`
	Records     uint64             `json:"records"`
	IsFork      bool               `json:"isFork"`
	Readers     adminStreamReaders `json:"readers"`
}

type adminStreamsResponse struct {
	Streams    []adminStreamInfo `json:"streams"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

func (s *Server) handleAdminStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	limit := defaultAdminPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		if n > maxAdminPageLimit {
			n = maxAdminPageLimit
		}
		limit = n
	}

	streams, nextCursor, err := s.listStreams(q.Get("cursor"), limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set(hContentType, "application/json")
	w.Header().Set(hCacheControl, "no-store")
	json.NewEncoder(w).Encode(adminStreamsResponse{Streams: streams, NextCursor: nextCursor})
}

// listStreams scans the ktStreamMeta keyspace in path order, starting just
// after cursor (empty cursor = from the beginning), and returns up to limit
// streams plus the cursor to resume from (empty once the scan is exhausted).
func (s *Server) listStreams(cursor string, limit int) ([]adminStreamInfo, string, error) {
	start := []byte{byte(ktStreamMeta)}
	if cursor != "" {
		// This is the smallest key strictly greater than metaKey(cursor). The
		// scan resumes after the last stream from the previous page, not on it.
		start = append(metaKey(cursor), 0x00)
	}
	end := prefixEnd([]byte{byte(ktStreamMeta)})

	it, err := s.store.Scan(start, end)
	if err != nil {
		return nil, "", err
	}
	defer it.Close()

	var out []adminStreamInfo
	var lastPath, nextCursor string
	for {
		k, v, ok, err := it.Next()
		if err != nil {
			return nil, "", err
		}
		if !ok {
			break
		}
		path := string(k[1:])
		if len(out) == limit {
			nextCursor = lastPath
			break
		}
		meta, err := unmarshalMeta(v)
		if err != nil {
			return nil, "", err
		}
		info, err := s.adminStreamInfo(path, meta)
		if err != nil {
			return nil, "", err
		}
		out = append(out, info)
		lastPath = path
	}
	return out, nextCursor, nil
}

// adminStreamInfo builds one listing entry. It prefers a resident streamer's
// live in-memory view (record count, closed status, per-mode reader counts),
// and falls back to durable state for dormant streams, which by definition
// have no parked readers.
func (s *Server) adminStreamInfo(path string, meta *StreamMeta) (adminStreamInfo, error) {
	info := adminStreamInfo{
		Path:        path,
		ContentType: meta.ContentType,
		IsFork:      meta.ForkedFrom != 0,
	}

	st, _ := s.registry.get(path)

	if st != nil {
		info.Records = st.tail.Load()
		info.Closed = st.closed.Load()
		info.Readers = adminStreamReaders{
			LongPoll: st.longPollReaders.Load(),
			SSE:      st.sseReaders.Load(),
		}
		return info, nil
	}

	tailSeq, err := s.loadTailSeq(meta.StreamID)
	if err != nil {
		return adminStreamInfo{}, err
	}
	info.Records = tailSeq
	info.Closed = meta.Closed
	return info, nil
}
