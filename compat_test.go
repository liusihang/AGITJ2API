package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCompatTask struct {
	ID                           int64
	Type, Code, Prompt, Duration string
}
type compatFixture struct {
	g                        *gateway
	image                    []byte
	mu                       sync.Mutex
	tasks                    map[int64]fakeCompatTask
	quotes, submits, uploads atomic.Int32
	pending, disconnect      atomic.Bool
	lastParams               map[string]json.RawMessage
	lastKey                  string
}

func setupCompat(t *testing.T) *compatFixture {
	t.Helper()
	f := &compatFixture{tasks: map[int64]fakeCompatTask{}}
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var imageBuffer bytes.Buffer
	if err := png.Encode(&imageBuffer, img); err != nil {
		t.Fatal(err)
	}
	f.image = imageBuffer.Bytes()
	schemas := map[string]string{
		"image-test":       `[{"key":"prompt","type":"text","required":true},{"key":"ratio","type":"select","default":"square","map":{"square":[1024,1024],"tiny":[2,2]}},{"key":"seed","type":"int","randomize":true,"min":0,"max":9999}]`,
		"edit-test":        `[{"key":"image","type":"image","required":true},{"key":"image_2","type":"image"},{"key":"prompt","type":"text","required":true},{"key":"seed","type":"int","randomize":true,"min":0,"max":9999}]`,
		"video-test":       `[{"key":"prompt","type":"text","required":true},{"key":"duration","type":"select","default":"5秒","map":{"5秒":[5],"10秒":[10]}},{"key":"seed","type":"int","default":42}]`,
		"video-image-test": `[{"key":"image","type":"image","required":true},{"key":"prompt","type":"text"},{"key":"duration","type":"select","default":"5秒","map":{"5秒":[5],"10秒":[10]}},{"key":"seed","type":"int","default":1}]`,
	}
	kinds := map[string]string{"image-test": "t2i", "edit-test": "i2i", "video-test": "t2v", "video-image-test": "i2v"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-token-one" {
			t.Error("upstream authorization missing or internal key leaked")
		}
		switch {
		case r.URL.Path == "/api/v1/chat/models":
			fmt.Fprint(w, `{"code":200,"data":[]}`)
		case r.URL.Path == "/api/v1/aigc/templates":
			items := make([]map[string]any, 0, len(schemas))
			for code, schema := range schemas {
				items = append(items, map[string]any{"code": code, "name": code, "type": kinds[code], "updated_at": "2026-11-13T20:00:00+08:00", "param_schema": schema})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": items})
		case r.URL.Path == "/api/v1/aigc/inputs":
			f.uploads.Add(1)
			file, header, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			data, _ := io.ReadAll(file)
			file.Close()
			if !bytes.Equal(data, f.image) || header.Header.Get("Content-Type") != "image/png" {
				t.Error("image upload changed")
			}
			fmt.Fprint(w, `{"code":200,"data":{"path":"uploaded/reference.png"}}`)
		case r.URL.Path == "/api/v1/aigc/quote":
			f.quotes.Add(1)
			var body generation
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			f.mu.Lock()
			f.lastParams = body.Params
			f.mu.Unlock()
			fmt.Fprint(w, `{"code":200,"data":{"token":"quote-token"}}`)
		case r.URL.Path == "/api/v1/aigc/tasks" && r.Method == "POST":
			f.submits.Add(1)
			var body generation
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.QuoteToken != "quote-token" || len(body.IdempotencyKey) != 36 {
				t.Error("quote or upstream UUID missing")
			}
			f.mu.Lock()
			f.lastKey = body.IdempotencyKey
			f.mu.Unlock()
			if f.disconnect.Load() {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			var prompt, duration string
			_ = json.Unmarshal(body.Params["prompt"], &prompt)
			_ = json.Unmarshal(body.Params["duration"], &duration)
			f.mu.Lock()
			ids := make([]int64, body.Count)
			for i := range ids {
				id := int64(len(f.tasks) + 101)
				f.tasks[id] = fakeCompatTask{id, kinds[body.TemplateCode], body.TemplateCode, prompt, duration}
				ids[i] = id
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"task_id": ids[0], "task_ids": ids}})
		case r.URL.Path == "/api/v1/aigc/media-ticket":
			fmt.Fprint(w, `{"code":200,"data":{"url":"/remote-home/image.png?ticket=renewed"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/aigc/tasks/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/aigc/tasks/"), "/")
			var id int64
			_, _ = fmt.Sscan(parts[0], &id)
			f.mu.Lock()
			task, ok := f.tasks[id]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"code":404,"message":"not found"}`)
				return
			}
			if len(parts) > 1 && parts[1] == "details" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"params": []any{map[string]any{"key": "prompt", "value": task.Prompt}, map[string]any{"key": "duration", "value": task.Duration}}}})
				return
			}
			video := task.Type == "t2v" || task.Type == "i2v"
			if len(parts) > 1 && parts[1] == "outputs" {
				if video {
					w.Header().Set("Content-Type", "video/mp4")
					if r.Header.Get("Range") == "bytes=0-3" {
						w.Header().Set("Content-Range", "bytes 0-3/8")
						w.WriteHeader(206)
						fmt.Fprint(w, "data")
						return
					}
					fmt.Fprint(w, "data1234")
					return
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(f.image)
				return
			}
			status := "succeeded"
			if f.pending.Load() {
				status = "running"
			}
			mimeType := "image/png"
			if video {
				mimeType = "video/mp4"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"id": task.ID, "type": task.Type, "template_code": task.Code, "status": status, "progress": 100, "created_at": "2026-11-13T20:00:00+08:00", "finished_at": "2026-11-13T20:01:00+08:00", "outputs": []any{map[string]any{"filename": "output", "mime": mimeType, "url": "/remote-home/image.png?ticket=old"}}}})
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
	g, err := newGateway(testKey, token, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.g = g
	return f
}

func compatJSON(g *gateway, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestCompatModelListAndErrors(t *testing.T) {
	f := setupCompat(t)
	w := compatJSON(f.g, "GET", "/v1/models", "", "")
	var result struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Object != "list" || len(result.Data) != 4 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = compatJSON(f.g, "GET", "/v1/models/image-test", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"object":"model"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = compatJSON(f.g, "GET", "/v1/models/does-not-exist", "", ""); w.Code != 404 || !strings.Contains(w.Body.String(), `"error":`) {
		t.Fatal(w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	w = httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"authentication_error"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompatImageGenerationAndIdempotentReplay(t *testing.T) {
	f := setupCompat(t)
	body := `{"model":"image-test","prompt":"一只橘猫","n":2,"size":"2x2","seed":7}`
	key := "image-request-123"
	for i := 0; i < 2; i++ {
		w := compatJSON(f.g, "POST", "/v1/images/generations", body, key)
		var result struct {
			Created int64 `json:"created"`
			Data    []struct {
				Base64 string `json:"b64_json"`
			} `json:"data"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Created == 0 || len(result.Data) != 2 {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, v := range result.Data {
			data, err := base64.StdEncoding.DecodeString(v.Base64)
			if err != nil || !bytes.Equal(data, f.image) {
				t.Fatal("invalid image")
			}
		}
		if w.Header().Get("Idempotency-Key") != key {
			t.Fatal("key not returned")
		}
	}
	if f.submits.Load() != 1 || f.quotes.Load() != 1 {
		t.Fatal("replay created a second job", f.submits.Load(), f.quotes.Load())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.lastParams["ratio"]) != `"tiny"` || string(f.lastParams["seed"]) != "7" || len(f.lastKey) != 36 {
		t.Fatal("model parameters not mapped")
	}
}

func TestCompatIdempotencySurvivesRestartAndConcurrentRequests(t *testing.T) {
	f := setupCompat(t)
	body, key := `{"model":"image-test","prompt":"same request"}`, "persistent-key-123"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := compatJSON(f.g, "POST", "/v1/images/generations", body, key); w.Code != 200 {
				t.Error(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if f.submits.Load() != 1 {
		t.Fatal("concurrent requests submitted more than once", f.submits.Load())
	}
	restarted, err := newGateway(testKey, f.g.tokenFile, f.g.upstream.String())
	if err != nil {
		t.Fatal(err)
	}
	if w := compatJSON(restarted, "POST", "/v1/images/generations", body, key); w.Code != 200 || f.submits.Load() != 1 {
		t.Fatal("restart replay failed", w.Code, f.submits.Load())
	}
	info, err := os.Stat(recordPath(f.g.tokenFile, key))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe record permissions", err)
	}
}

func TestCompatImageMultipartEdit(t *testing.T) {
	f := setupCompat(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "edit-test")
	_ = writer.WriteField("prompt", "change background")
	part, err := writer.CreateFormFile("image[]", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(f.image)
	_ = writer.Close()
	r := httptest.NewRequest("POST", "/v1/images/edits", &body)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Idempotency-Key", "edit-request-123")
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 200 || f.uploads.Load() != 1 || f.submits.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.lastParams["image"]) != `"uploaded/reference.png"` {
		t.Fatal("uploaded path not mapped")
	}
}

func TestCompatMultipartUnsupportedFieldsAreNotIgnored(t *testing.T) {
	f := setupCompat(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "edit-test")
	_ = writer.WriteField("prompt", "test")
	_ = writer.WriteField("input_fidelity", "high")
	part, _ := writer.CreateFormFile("image", "test.png")
	_, _ = part.Write(f.image)
	_ = writer.Close()
	r := httptest.NewRequest("POST", "/v1/images/edits", &body)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 400 || f.quotes.Load() != 0 || f.submits.Load() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompatImageURLAndInvalidParameters(t *testing.T) {
	f := setupCompat(t)
	urlBody := `{"model":"image-test","prompt":"test","response_format":"url"}`
	w := compatJSON(f.g, "POST", "/v1/images/generations", urlBody, "url-request-123")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/remote-home/image.png?ticket=renewed") {
		t.Fatal(w.Code, w.Body.String())
	}
	before := f.submits.Load()
	for _, body := range []string{
		`{"model":"image-test","prompt":"test","size":"1536x1024"}`,
		`{"model":"image-test","prompt":"test","n":0}`,
		`{"model":"image-test","prompt":"test","stream":true}`,
		`{"model":"image-test","prompt":"test","quality":"high"}`,
		`{"model":"image-test","prompt":"test","response_format":"url","output_format":"jpeg"}`,
	} {
		w = compatJSON(f.g, "POST", "/v1/images/generations", body, "")
		if w.Code != 400 {
			t.Error(w.Code, w.Body.String())
		}
	}
	if f.submits.Load() != before {
		t.Fatal("invalid request submitted")
	}
	if _, failure := f.g.resolveReference(context.Background(), imageRef{URL: "https://127.0.0.1/example.png"}); failure == nil {
		t.Fatal("private reference URL allowed")
	}
}

func TestCompatPlatformOutputURLCanBeUsedAsReference(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/remote-home/output.png" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected output fetch")
		}
		_, _ = w.Write(data.Bytes())
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("upstream-token-one"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := newGateway(testKey, token, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	g.client.Transport = server.Client().Transport
	ref, failure := g.resolveReference(context.Background(), imageRef{URL: server.URL + "/remote-home/output.png?ticket=test"})
	if failure != nil || !bytes.Equal(ref.Data, data.Bytes()) {
		t.Fatal("signed output could not be read", failure)
	}
}

func TestCompatVideoCreateStatusAndRange(t *testing.T) {
	f := setupCompat(t)
	body := `{"model":"video-test","prompt":"海面","seconds":"10"}`
	w := compatJSON(f.g, "POST", "/v1/videos", body, "video-request-123")
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Object string `json:"object"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.ID != "video_101" || created.Status != "queued" || created.Object != "video" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = compatJSON(f.g, "GET", "/v1/videos/"+created.ID, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"completed"`) || !strings.Contains(w.Body.String(), `"seconds":"10"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/v1/videos/"+created.ID+"/content", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Range", "bytes=0-3")
	w = httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "data" || w.Header().Get("Content-Range") != "bytes 0-3/8" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	if w = compatJSON(f.g, "GET", "/v1/videos/"+created.ID+"/content?variant=thumbnail", "", ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = compatJSON(f.g, "POST", "/v1/videos", `{"model":"video-test","prompt":"x","seconds":"8"}`, ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = compatJSON(f.g, "POST", "/v1/videos", `{"model":"video-test","prompt":"x","size":"1280x720"}`, ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompatImageToVideoDataURL(t *testing.T) {
	f := setupCompat(t)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(f.image)
	body, _ := json.Marshal(map[string]any{"model": "video-image-test", "prompt": "move", "input_reference": map[string]string{"image_url": dataURL}})
	w := compatJSON(f.g, "POST", "/v1/videos", string(body), "image-video-key")
	if w.Code != 200 || f.uploads.Load() != 1 || f.submits.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompatTimeoutReplayAndUncertainSubmission(t *testing.T) {
	f := setupCompat(t)
	f.pending.Store(true)
	body := `{"model":"image-test","prompt":"wait"}`
	key := "same-image-key"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 504 || !strings.Contains(w.Body.String(), `"task_ids"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	f.pending.Store(false)
	w = compatJSON(f.g, "POST", "/v1/images/generations", body, key)
	if w.Code != 200 || f.submits.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), f.submits.Load())
	}
	w = compatJSON(f.g, "POST", "/v1/images/generations", `{"model":"image-test","prompt":"different"}`, key)
	if w.Code != 409 || f.submits.Load() != 1 {
		t.Fatal("idempotency conflict not caught")
	}
	f.disconnect.Store(true)
	w = compatJSON(f.g, "POST", "/v1/videos", `{"model":"video-test","prompt":"unknown"}`, "uncertain-key-123")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "submission_unknown") {
		t.Fatal(w.Code, w.Body.String())
	}
	f.disconnect.Store(false)
	w = compatJSON(f.g, "POST", "/v1/videos", `{"model":"video-test","prompt":"unknown"}`, "uncertain-key-123")
	if w.Code != 409 || f.submits.Load() != 2 {
		t.Fatal("uncertain submission retried", w.Code, f.submits.Load())
	}
}
