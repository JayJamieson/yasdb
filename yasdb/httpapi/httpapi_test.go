package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JayJamieson/yasdb/yasdb"
	"github.com/JayJamieson/yasdb/yasdb/httpapi"
)

var dbCounter atomic.Uint64

func openTestDB(t *testing.T) *yasdb.DB {
	t.Helper()
	store, err := yasdb.OpenStore(fmt.Sprintf("yasdb-httpapi-test-%d", dbCounter.Add(1)), "memory:///", yasdb.StoreTuning{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, err := yasdb.Open(store, yasdb.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestHealth(t *testing.T) {
	h := httpapi.NewHandler(openTestDB(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/__health", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok\n" {
		t.Fatalf("GET /__health: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestStreamRoundTrip(t *testing.T) {
	h := httpapi.NewHandler(openTestDB(t))

	put := httptest.NewRequest("PUT", "/orders", nil)
	put.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, put)
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT /orders: code=%d body=%q", rr.Code, rr.Body.String())
	}

	post := httptest.NewRequest("POST", "/orders", strings.NewReader(`[{"id":1},{"id":2}]`))
	post.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /orders: code=%d body=%q", rr.Code, rr.Body.String())
	}

	get := httptest.NewRequest("GET", "/orders?offset=-1", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, get)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /orders: code=%d body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != `[{"id":1},{"id":2}]` {
		t.Fatalf("GET /orders body: got %q", got)
	}
}

func TestStateSnapshot(t *testing.T) {
	h := httpapi.NewHandler(openTestDB(t))

	put := httptest.NewRequest("PUT", "/chat", nil)
	put.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, put)
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT /chat: code=%d", rr.Code)
	}

	msg := `[{"type":"message","key":"m1","value":{"text":"hi"},"headers":{"operation":"insert"}}]`
	post := httptest.NewRequest("POST", "/chat", strings.NewReader(msg))
	post.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /chat: code=%d body=%q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/__state/chat", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /__state/chat: code=%d body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"m1"`) {
		t.Fatalf("GET /__state/chat body missing m1: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/__state/does-not-exist", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /__state/does-not-exist: code=%d, want 404", rr.Code)
	}
}

// TestMountOnExistingMux proves the handler mounts on a plain
// http.ServeMux like any other handler: at root, and under a prefix via
// http.StripPrefix, alongside the caller's own routes on the same mux.
func TestMountOnExistingMux(t *testing.T) {
	db := openTestDB(t)
	mux := http.NewServeMux()
	mux.Handle("/app/ping", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	}))
	mux.Handle("/streams/", http.StripPrefix("/streams", httpapi.NewHandler(db)))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/app/ping", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "pong" {
		t.Fatalf("GET /app/ping: code=%d body=%q", rr.Code, rr.Body.String())
	}

	put := httptest.NewRequest("PUT", "/streams/orders", nil)
	put.Header.Set("Content-Type", "text/plain")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, put)
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT /streams/orders: code=%d body=%q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/streams/__health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /streams/__health: code=%d", rr.Code)
	}
}
