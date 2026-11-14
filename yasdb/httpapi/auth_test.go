package httpapi_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/JayJamieson/yasdb/yasdb/httpapi"
)

const testToken = "s3cr3t-shared-token"

// stateRecord is State Protocol shaped, so the same append satisfies both
// the plain read and the /__state snapshot.
const stateRecord = `[{"type":"message","key":"m1","value":{"text":"hi"},"headers":{"operation":"insert"}}]`

// exerciseEveryRoute drives every public route through h, applying
// authorize to each request before it is served.
func exerciseEveryRoute(t *testing.T, h http.Handler, authorize func(*http.Request)) {
	t.Helper()
	steps := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{"PUT", "/orders", "", http.StatusCreated},
		{"POST", "/orders", stateRecord, http.StatusNoContent},
		{"GET", "/orders?offset=-1", "", http.StatusOK},
		{"GET", "/__admin/streams", "", http.StatusOK},
		{"GET", "/__state/orders", "", http.StatusOK},
		{"DELETE", "/orders", "", http.StatusNoContent},
	}
	for _, s := range steps {
		var body io.Reader
		if s.body != "" {
			body = strings.NewReader(s.body)
		}
		req := httptest.NewRequest(s.method, s.path, body)
		req.Header.Set("Content-Type", "application/json")
		authorize(req)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != s.want {
			t.Fatalf("%s %s: code=%d, want %d; body=%q", s.method, s.path, rr.Code, s.want, rr.Body.String())
		}
	}
}

// dumpResponse renders a whole response as text, so two rejections can be
// compared for any difference an attacker could probe.
func dumpResponse(rr *httptest.ResponseRecorder) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", rr.Code)
	keys := make([]string, 0, len(rr.Header()))
	for k := range rr.Header() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %v\n", k, rr.Header()[k])
	}
	b.WriteString(rr.Body.String())
	return b.String()
}

func TestNoTokenConfiguredLeavesEveryRouteOpen(t *testing.T) {
	h := httpapi.RequireBearerToken("", httpapi.NewHandler(openTestDB(t)))
	exerciseEveryRoute(t, h, func(*http.Request) {})
}

func TestBearerTokenAdmitsEveryRoute(t *testing.T) {
	h := httpapi.RequireBearerToken(testToken, httpapi.NewHandler(openTestDB(t)))
	exerciseEveryRoute(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
	})
}

func TestRejectedCredentialsAreIndistinguishable(t *testing.T) {
	h := httpapi.RequireBearerToken(testToken, httpapi.NewHandler(openTestDB(t)))

	cases := map[string]func(*http.Request){
		"no header":   func(*http.Request) {},
		"wrong token": func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") },
		"wrong token of the same length": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", len(testToken)))
		},
		"wrong scheme": func(r *http.Request) { r.Header.Set("Authorization", "Basic "+testToken) },
		"empty bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"bare bearer":  func(r *http.Request) { r.Header.Set("Authorization", "Bearer") },
		"token in the wrong header": func(r *http.Request) {
			r.Header.Set("X-Auth-Token", testToken)
		},
	}

	var firstName, firstDump string
	for name, authorize := range cases {
		req := httptest.NewRequest("POST", "/orders", strings.NewReader(stateRecord))
		req.Header.Set("Content-Type", "application/json")
		authorize(req)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: code=%d, want 401; body=%q", name, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Fatalf("%s: WWW-Authenticate=%q, want %q", name, got, "Bearer")
		}
		dump := dumpResponse(rr)
		if firstName == "" {
			firstName, firstDump = name, dump
			continue
		}
		if dump != firstDump {
			t.Fatalf("%q and %q are distinguishable:\n%s\n--- vs ---\n%s", firstName, name, firstDump, dump)
		}
	}
}

func TestUnauthorizedResponseDoesNotLeakTheToken(t *testing.T) {
	h := httpapi.RequireBearerToken(testToken, httpapi.NewHandler(openTestDB(t)))

	req := httptest.NewRequest("POST", "/orders", strings.NewReader(stateRecord))
	req.Header.Set("Content-Type", "application/json")
	// A near miss that contains the real token, so an echo of what the
	// client sent fails this test too.
	req.Header.Set("Authorization", "Bearer "+testToken+"-not-quite")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rr.Code)
	}
	if dump := dumpResponse(rr); strings.Contains(dump, testToken) {
		t.Fatalf("401 response contains the configured token:\n%s", dump)
	}
}

// TestHealthStaysOpenSoFlyDoesNotRestartTheMachine pins the exemption that
// keeps the deployment alive. fly.toml's http_service check GETs /__health
// every 15s with no Authorization header. A 401 there marks the machine
// unhealthy and restarts it in a loop.
func TestHealthStaysOpenSoFlyDoesNotRestartTheMachine(t *testing.T) {
	h := httpapi.RequireBearerToken(testToken, httpapi.NewHandler(openTestDB(t)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/__health", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok\n" {
		t.Fatalf("GET /__health with no credentials: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

// TestPreflightStaysOpenSoBrowsersCanSendTheHeader pins the other
// exemption. A browser sends the CORS preflight without Authorization, so
// a 401 on OPTIONS stops the real request from ever being sent.
func TestPreflightStaysOpenSoBrowsersCanSendTheHeader(t *testing.T) {
	h := httpapi.RequireBearerToken(testToken, httpapi.NewHandler(openTestDB(t)))
	req := httptest.NewRequest("OPTIONS", "/orders", nil)
	req.Header.Set("Origin", "https://example.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS /orders with no credentials: code=%d body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "*" {
		t.Fatalf("Access-Control-Allow-Headers=%q, want %q so the real request may carry Authorization", got, "*")
	}
}
