package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type speechFixture struct {
	g                       *gateway
	recognize, submit, poll atomic.Int32
	queryFailures           atomic.Int32
	mu                      sync.Mutex
	last                    map[string]any
	fail                    atomic.Bool
	started, unblock        chan struct{}
}

func setupSpeech(t *testing.T) *speechFixture {
	t.Helper()
	f := &speechFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-token-one" {
			t.Error("upstream token missing or internal key leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/aigc/templates":
			fmt.Fprint(w, `{"code":200,"data":[{"code":"image-test","name":"image","type":"t2i","param_schema":"[]"}]}`)
		case "/api/v1/chat/models":
			fmt.Fprint(w, `{"code":200,"data":[{"id":162,"name":"Paraformer","task_mode":true,"model_capabilities":["audio_transcription"]},{"id":193,"name":"Whisper","model_capabilities":["audio_transcription"]},{"id":161,"name":"CosyVoice","model_capabilities":["text_to_speech"]}]}`)
		case "/api/v1/chat/services":
			id := 261
			if r.URL.Query().Get("model_asset_id") == "162" {
				id = 234
			}
			fmt.Fprintf(w, `{"code":200,"data":[{"id":%d}]}`, id)
		case "/api/v1/speech/recognize", "/api/v1/speech/tasks":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			f.mu.Lock()
			f.last = body
			f.mu.Unlock()
			if r.URL.Path == "/api/v1/speech/recognize" {
				f.recognize.Add(1)
				if f.started != nil {
					close(f.started)
					<-f.unblock
				}
				if f.fail.Load() {
					w.WriteHeader(500)
					fmt.Fprint(w, `{"code":500,"message":"upstream failure"}`)
					return
				}
				fmt.Fprint(w, `{"code":200,"data":{"text":"hello test","history_id":42}}`)
			} else {
				f.submit.Add(1)
				fmt.Fprint(w, `{"code":200,"data":{"task_id":"task-1","history_id":43}}`)
			}
		case "/api/v1/speech/tasks/query":
			f.poll.Add(1)
			if f.queryFailures.Swap(0) > 0 {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"code":500,"message":"temporary query failure"}`)
				return
			}
			fmt.Fprint(w, `{"code":200,"data":{"status":"succeeded","text":"你好测试","segments":[{"start":0.1,"end":1.2,"text":"你好测试"}]}}`)
		case "/api/v1/experience/history/42":
			fmt.Fprint(w, `{"code":200,"data":{"status":"succeeded","result":{"text":"hello test"}}}`)
		case "/api/v1/experience/history/43":
			fmt.Fprint(w, `{"code":200,"data":{"status":"succeeded","result":{"text":"你好测试","segments":[{"start":0.1,"end":1.2,"text":"你好测试"}]}}}`)
		default:
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("upstream-token-one"), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.g, err = newGateway(testKey, token, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func speechUpload(t *testing.T, g *gateway, key, filename string, audio []byte, values map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(audio); err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		if err = writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestSpeechModelsAppearAlongsideExistingModels(t *testing.T) {
	f := setupSpeech(t)
	w := compatJSON(f.g, "GET", "/v1/models", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"image-test"`) || !strings.Contains(w.Body.String(), `"id":"Whisper"`) || !strings.Contains(w.Body.String(), `"id":"Paraformer"`) || strings.Contains(w.Body.String(), "CosyVoice") {
		t.Fatalf("unexpected models: %d %s", w.Code, w.Body)
	}
	w = compatJSON(f.g, "GET", "/v1/models/whisper-1", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"whisper-1"`) || !strings.Contains(w.Body.String(), `"type":"stt"`) {
		t.Fatalf("alias lookup: %d %s", w.Code, w.Body)
	}
}

func TestSpeechSyncReplayDoesNotResubmit(t *testing.T) {
	f := setupSpeech(t)
	audio := []byte("RIFF sample audio")
	fields := map[string]string{"model": "whisper-1", "language": "zh"}
	w := speechUpload(t, f.g, "speech-key", "audio.wav", audio, fields)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"hello test"`) || w.Header().Get("Idempotency-Key") != "speech-key" {
		t.Fatalf("transcription: %d %s", w.Code, w.Body)
	}
	f.mu.Lock()
	got := f.last
	f.mu.Unlock()
	decoded, err := base64.StdEncoding.DecodeString(got["audio_base64"].(string))
	if err != nil || !bytes.Equal(decoded, audio) || got["service_id"] != float64(261) || got["language"] != "zh" || got["enable_punc"] != true {
		t.Fatalf("upstream request mismatch: %+v", got)
	}
	w = speechUpload(t, f.g, "speech-key", "audio.wav", audio, fields)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello test") || f.recognize.Load() != 1 {
		t.Fatalf("replay: %d %s, upstream calls %d", w.Code, w.Body, f.recognize.Load())
	}
	w = speechUpload(t, f.g, "speech-key", "audio.wav", []byte("other audio"), fields)
	if w.Code != 409 || f.recognize.Load() != 1 {
		t.Fatalf("conflict: %d %s", w.Code, w.Body)
	}
	path := recordPath(f.g.tokenFile, "speech-key")
	journal, err := os.ReadFile(path)
	if err != nil || bytes.Contains(journal, audio) || bytes.Contains(journal, []byte("hello test")) {
		t.Fatalf("journal contains content or cannot be read: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal permissions: %v %v", info, err)
	}
}

func TestSpeechAsyncAndTextResponse(t *testing.T) {
	f := setupSpeech(t)
	fields := map[string]string{"model": "Paraformer", "language": "zh", "response_format": "text"}
	w := speechUpload(t, f.g, "async-key", "audio.mp3", []byte("audio"), fields)
	if w.Code != 200 || w.Body.String() != "你好测试" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("transcription: %d %s", w.Code, w.Body)
	}
	f.mu.Lock()
	got := f.last
	f.mu.Unlock()
	if got["service_id"] != float64(234) || got["kind"] != "stt" || got["language"] != "中文" {
		t.Fatalf("upstream request mismatch: %+v", got)
	}
	w = speechUpload(t, f.g, "async-key", "audio.mp3", []byte("audio"), fields)
	if w.Code != 200 || w.Body.String() != "你好测试" || f.submit.Load() != 1 || f.poll.Load() != 1 {
		t.Fatalf("async replay: %d %s submit=%d poll=%d", w.Code, w.Body, f.submit.Load(), f.poll.Load())
	}
}

func TestSpeechUnknownSubmissionNeverAutomaticallyRepeats(t *testing.T) {
	f := setupSpeech(t)
	f.fail.Store(true)
	fields := map[string]string{"model": "Whisper"}
	w := speechUpload(t, f.g, "unknown-key", "audio.wav", []byte("audio"), fields)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "submission_unknown") {
		t.Fatalf("upstream failure: %d %s", w.Code, w.Body)
	}
	w = speechUpload(t, f.g, "unknown-key", "audio.wav", []byte("audio"), fields)
	if w.Code != 409 || f.recognize.Load() != 1 {
		t.Fatalf("unsafe replay: %d %s calls=%d", w.Code, w.Body, f.recognize.Load())
	}
}

func TestSpeechAsyncResumesAfterQueryFailure(t *testing.T) {
	f := setupSpeech(t)
	f.queryFailures.Store(1)
	fields := map[string]string{"model": "Paraformer"}
	w := speechUpload(t, f.g, "resume-key", "audio.wav", []byte("audio"), fields)
	if w.Code != 500 || f.submit.Load() != 1 {
		t.Fatalf("initial query: %d %s submit=%d", w.Code, w.Body, f.submit.Load())
	}
	w = speechUpload(t, f.g, "resume-key", "audio.wav", []byte("audio"), fields)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "你好测试") || f.submit.Load() != 1 || f.poll.Load() != 2 {
		t.Fatalf("resume: %d %s submit=%d poll=%d", w.Code, w.Body, f.submit.Load(), f.poll.Load())
	}
}

func TestSpeechConcurrentSameKeySubmitsOnce(t *testing.T) {
	f := setupSpeech(t)
	f.started, f.unblock = make(chan struct{}), make(chan struct{})
	fields := map[string]string{"model": "Whisper"}
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() { completed <- speechUpload(t, f.g, "concurrent-key", "audio.wav", []byte("audio"), fields) }()
	<-f.started
	w := speechUpload(t, f.g, "concurrent-key", "audio.wav", []byte("audio"), fields)
	if w.Code != 409 {
		t.Errorf("concurrent request: %d %s", w.Code, w.Body)
	}
	close(f.unblock)
	if w = <-completed; w.Code != 200 || f.recognize.Load() != 1 {
		t.Fatalf("first request: %d %s calls=%d", w.Code, w.Body, f.recognize.Load())
	}
}

func TestSpeechRejectsUnsupportedOptionsBeforeSubmitting(t *testing.T) {
	f := setupSpeech(t)
	tests := []struct {
		name, filename string
		fields         map[string]string
	}{
		{"unsupported file", "audio.webm", nil},
		{"prompt", "audio.wav", map[string]string{"prompt": "technical terms"}},
		{"stream", "audio.wav", map[string]string{"stream": "true"}},
		{"format", "audio.wav", map[string]string{"response_format": "verbose_json"}},
		{"unknown field", "audio.wav", map[string]string{"timestamps": "word"}},
		{"Paraformer language", "audio.wav", map[string]string{"model": "Paraformer", "language": "en"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := speechUpload(t, f.g, "", tc.filename, []byte("audio"), tc.fields)
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"error":`) {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
		})
	}
	if f.recognize.Load() != 0 || f.submit.Load() != 0 {
		t.Fatal("invalid request reached upstream inference")
	}
}
