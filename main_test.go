package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "test-internal-api-key-1234567890"
const templateFixture = `{"code":200,"data":[{"code":"example","param_schema":"[{\"key\":\"prompt\",\"type\":\"text\"},{\"key\":\"image\",\"type\":\"image\"},{\"key\":\"duration\",\"type\":\"select\",\"default\":\"5秒\"},{\"key\":\"seed\",\"type\":\"int\",\"randomize\":true,\"default\":42},{\"key\":\"locked_value\",\"type\":\"int\",\"locked\":true,\"default\":7},{\"key\":\"steps\",\"type\":\"int\",\"default\":8}]"}]}`

func setup(t *testing.T, handler http.HandlerFunc) (*gateway, string) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("upstream-token-one"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := newGateway(testKey, file, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	return g, file
}

func perform(g *gateway, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestAuthAndAllowlist(t *testing.T) {
	var calls atomic.Int32
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"code":200}`) })
	for _, key := range []string{"", testKey, "Bearer wrong"} {
		r := httptest.NewRequest("GET", "/v1/tasks", nil)
		r.Header.Set("Authorization", key)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("bad key: %d", w.Code)
		}
	}
	for _, path := range []string{"/api/v1/user/info", "/v1/tasks/1/../2", "/v1/tasks/1%2foutputs%2f0", "/v1/../../auth/sso/discover"} {
		if w := perform(g, "GET", path, ""); w.Code != 404 {
			t.Fatalf("route %s: %d", path, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized/disallowed requests reached upstream")
	}
	if w := perform(g, "GET", "/healthz", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestHotReloadAndExpiredToken(t *testing.T) {
	var received []string
	g, file := setup(t, func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"code":200,"data":[]}`)
	})
	perform(g, "GET", "/v1/tasks", "")
	_ = os.WriteFile(file, []byte(`{"authorization":"Bearer new-token"}`), 0600)
	perform(g, "GET", "/v1/tasks", "")
	if strings.Join(received, ",") != "Bearer upstream-token-one,Bearer new-token" {
		t.Fatal(received)
	}
	expired := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
	_ = os.WriteFile(file, []byte(expired), 0600)
	if w := perform(g, "GET", "/v1/tasks", ""); w.Code != 503 {
		t.Fatal(w.Code)
	}
	status := perform(g, "GET", "/v1/status", "")
	if !strings.Contains(status.Body.String(), `"token_ready":false`) {
		t.Fatal(status.Body.String())
	}
	if len(received) != 2 {
		t.Fatal("expired token sent upstream")
	}
}

func TestEstimateFiltersFullParamsAndPreservesNumbers(t *testing.T) {
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/aigc/templates" {
			fmt.Fprint(w, templateFixture)
			return
		}
		if r.URL.Path != "/api/v1/aigc/estimate" {
			t.Error(r.URL.Path)
		}
		var body generation
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Params) != 2 || string(body.Params["duration"]) != `"5秒"` || string(body.Params["steps"]) != "9007199254740993" {
			t.Errorf("params: %s", body.Params)
		}
		fmt.Fprint(w, `{"code":200,"data":{"total_points":3}}`)
	})
	w := perform(g, "POST", "/v1/estimate", `{"template_code":"example","params":{"prompt":"海面","image":"path.png","seed":99,"steps":9007199254740993}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"total_points":3`) {
		t.Fatal(w.Body.String())
	}
	w = perform(g, "GET", "/v1/workflows", "")
	if !strings.Contains(w.Body.String(), `"param_schema":[`) {
		t.Fatal(w.Body.String())
	}
}

func TestQuoteThenSubmit(t *testing.T) {
	var paths []string
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/aigc/templates":
			fmt.Fprint(w, templateFixture)
		case "/api/v1/aigc/quote", "/api/v1/aigc/tasks":
			var body generation
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Count != 1 || string(body.Params["seed"]) != "42" {
				t.Errorf("body: %+v", body)
			}
			if strings.HasSuffix(r.URL.Path, "/quote") {
				if body.IdempotencyKey != "" {
					t.Error("key in quote")
				}
				fmt.Fprint(w, `{"code":200,"data":{"token":"fresh-quote"}}`)
			} else {
				if body.IdempotencyKey != "saved-key-123" || body.QuoteToken != "fresh-quote" {
					t.Errorf("body: %+v", body)
				}
				fmt.Fprint(w, `{"code":200,"data":{"task_id":123,"task_ids":[123]}}`)
			}
		}
	})
	w := perform(g, "POST", "/v1/generations", `{"template_code":"example","params":{"prompt":"海面"},"idempotency_key":"saved-key-123"}`)
	if w.Code != 200 || w.Header().Get("Idempotency-Key") != "saved-key-123" {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Join(paths, ",") != "/api/v1/aigc/templates,/api/v1/aigc/quote,/api/v1/aigc/tasks" {
		t.Fatal(paths)
	}
	if w := perform(g, "POST", "/v1/generations", `{"template_code":"example"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestInvalidGenerationStopsBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, body := range []string{
		`null`,
		`{"template_code":"example","count":0,"idempotency_key":"saved-key-123"}`,
		`{"template_code":"example","count":-1,"idempotency_key":"saved-key-123"}`,
		`{"template_code":"example","unexpected":true,"idempotency_key":"saved-key-123"}`,
		`{"template_code":"example","quote_token":"caller-quote","idempotency_key":"saved-key-123"}`,
		`{"template_code":"example","idempotency_key":"saved-key-123"} {}`,
	} {
		if w := perform(g, "POST", "/v1/generations", body); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/generations", strings.NewReader(`{"template_code":"example","idempotency_key":"saved-key-123"}`))
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Idempotency-Key", "different-saved-key")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 400 || calls.Load() != 0 {
		t.Fatal(w.Code, calls.Load())
	}
}

func TestQuoteFailureDoesNotSubmit(t *testing.T) {
	var calls atomic.Int32
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/templates") {
			fmt.Fprint(w, templateFixture)
			return
		}
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":429,"error_code":"LIMITED","request_id":"abc","retryable":true}`)
	})
	w := perform(g, "POST", "/v1/generations", `{"template_code":"example","idempotency_key":"saved-key-123"}`)
	if w.Code != 429 || calls.Load() != 2 || w.Header().Get("Retry-After") != "17" || !strings.Contains(w.Body.String(), `"request_id":"abc"`) {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
}

func TestUploadRangeAndUpstreamErrors(t *testing.T) {
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-token-one" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("credential isolation")
		}
		if strings.HasSuffix(r.URL.Path, "/inputs") {
			data, _ := io.ReadAll(r.Body)
			if string(data) != "multipart-body" || r.Header.Get("Content-Type") != "multipart/form-data; boundary=abc" {
				t.Error("upload body changed")
			}
			fmt.Fprint(w, `{"code":200,"data":{"path":"test.png"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"code":409,"request_id":"already-done"}`)
			return
		}
		if r.Header.Get("Range") != "bytes=0-3" {
			t.Error("Range lost")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Set-Cookie", "upstream-secret=x")
		w.WriteHeader(206)
		fmt.Fprint(w, "test")
	})
	for _, tc := range []struct {
		method, path, body, content string
		status                      int
	}{
		{"POST", "/v1/inputs", "multipart-body", "multipart/form-data; boundary=abc", 200},
		{"GET", "/api/v1/aigc/tasks/123/outputs/0", "", "", 206},
		{"POST", "/v1/tasks/123/cancel", "", "", 409},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+testKey)
		r.Header.Set("Range", "bytes=0-3")
		r.Header.Set("Cookie", "internal-cookie=secret")
		r.Header.Set("X-Api-Key", testKey)
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Set-Cookie") != "" {
			t.Fatal(w.Code, w.Body.String())
		}
		if tc.status == 206 && (w.Body.String() != "test" || w.Header().Get("Content-Range") != "bytes 0-3/100") {
			t.Fatal(w.Body.String(), w.Header())
		}
	}
}

func TestSubmissionDisconnectIsNotRetried(t *testing.T) {
	var submissions atomic.Int32
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/templates"):
			fmt.Fprint(w, templateFixture)
		case strings.HasSuffix(r.URL.Path, "/quote"):
			fmt.Fprint(w, `{"code":200,"data":{"token":"fresh"}}`)
		default:
			submissions.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}
	})
	w := perform(g, "POST", "/v1/generations", `{"template_code":"example","idempotency_key":"saved-key-123"}`)
	if w.Code != 502 || submissions.Load() != 1 {
		t.Fatal(w.Code, submissions.Load())
	}
}

func TestReadConnectionEOFIsRetriedOnce(t *testing.T) {
	for _, path := range []string{"/v1/workflows", "/v1/tasks"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				fmt.Fprint(w, `{"code":200,"data":[]}`)
			})
			w := perform(g, "GET", path, "")
			if w.Code != 200 || calls.Load() != 2 {
				t.Fatal(w.Code, calls.Load())
			}
		})
	}
}

func TestInitializeDoesNotOverwriteKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "key")
	if err := initializeKey(path); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := initializeKey(path); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	stat, _ := os.Stat(path)
	if string(first) != string(second) || stat.Mode().Perm() != 0600 {
		t.Fatal("key changed or permissions wrong")
	}
}
