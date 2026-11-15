package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeSpeechRoutes(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/chat/models?model_type=speech", ""},
		{"GET", "/api/v1/chat/services?model_asset_id=161", ""},
		{"POST", "/api/v1/speech/tasks", `{"kind":"tts","voice_type":"可爱女生","text":"你好"}`},
		{"POST", "/api/v1/speech/tasks/query", `{"kind":"tts","task_id":"speech-1"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != tc.method || r.URL.RequestURI() != tc.path || string(body) != tc.body {
					t.Errorf("forwarded request changed: %s %s %s", r.Method, r.URL.RequestURI(), body)
				}
				if r.Header.Get("Authorization") != "Bearer upstream-token-one" || r.Header.Get("Cookie") != "" {
					t.Error("credentials were not isolated")
				}
				fmt.Fprint(w, `{"code":200,"data":{"task_id":"speech-1"}}`)
			})
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+testKey)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Cookie", "private-cookie=not-forwarded")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"task_id":"speech-1"`) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeSpeechRejectsOtherRoutesAndDoesNotRetrySubmission(t *testing.T) {
	var calls atomic.Int32
	g, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/api/v1/chat/models"},
		{"POST", "/api/v1/chat/services"},
		{"GET", "/api/v1/speech/tasks"},
		{"POST", "/api/v1/speech/arbitrary"},
		{"GET", "/api/v1/chat/models%2fprivate"},
		{"GET", "/api/v1/user/info"},
	} {
		if w := perform(g, tc.method, tc.path, ""); w.Code != 404 {
			t.Fatalf("unexpected allowed route %s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("disallowed route reached upstream")
	}
	w := perform(g, "POST", "/api/v1/speech/tasks", `{"kind":"tts"}`)
	if w.Code != 502 || calls.Load() != 1 {
		t.Fatal(w.Code, calls.Load())
	}
}
