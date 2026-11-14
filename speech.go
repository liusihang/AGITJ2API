package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxSpeechFile = 30 << 20

type speechModel struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"model_capabilities"`
	TaskMode     bool     `json:"task_mode"`
	UpdatedAt    string   `json:"updated_at"`
}

func (g *gateway) speechModels(w http.ResponseWriter, r *http.Request) ([]speechModel, *compatError) {
	data, failure := g.compatCall(w, r, "GET", "/chat/models?model_type=speech", nil)
	if failure != nil {
		// Older installations may not expose speech models at all.
		if failure.status == http.StatusNotFound {
			return nil, nil
		}
		return nil, failure
	}
	var models []speechModel
	if json.Unmarshal(data, &models) != nil {
		return nil, &compatError{status: 502, code: "invalid_models", message: "上游语音模型列表格式不符"}
	}
	result := make([]speechModel, 0, len(models))
	for _, model := range models {
		if model.ID <= 0 || model.Name == "" {
			continue
		}
		for _, capability := range model.Capabilities {
			if capability == "audio_transcription" {
				result = append(result, model)
				break
			}
		}
	}
	return result, nil
}

func findSpeechModel(models []speechModel, name string) (speechModel, bool) {
	if strings.EqualFold(name, "whisper-1") {
		name = "Whisper"
	}
	for _, model := range models {
		if strings.EqualFold(model.Name, name) {
			return model, true
		}
	}
	return speechModel{}, false
}

func speechModelObject(model speechModel) map[string]any {
	created := int64(0)
	if value, err := time.Parse(time.RFC3339Nano, model.UpdatedAt); err == nil {
		created = value.Unix()
	}
	return map[string]any{"id": model.Name, "object": "model", "created": created, "owned_by": "tongji", "name": model.Name, "type": "stt"}
}

type speechRequest struct {
	Model, Language, Format, Filename string
	Audio                             []byte
}

func parseSpeechRequest(w http.ResponseWriter, r *http.Request) (speechRequest, *compatError) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "multipart/form-data" {
		return speechRequest{}, bad("body", "请使用 multipart/form-data 上传音频文件")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechFile+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return speechRequest{}, &compatError{status: 413, code: "file_too_large", message: "音频文件不能超过 30 MiB", param: "file"}
		}
		return speechRequest{}, bad("body", "multipart 请求无效或超过大小限制")
	}
	defer r.MultipartForm.RemoveAll()
	allowed := map[string]bool{"model": true, "language": true, "response_format": true, "prompt": true, "temperature": true, "stream": true}
	for key, values := range r.MultipartForm.Value {
		if !allowed[key] {
			return speechRequest{}, bad(key, "该字段暂不受支持")
		}
		if len(values) != 1 {
			return speechRequest{}, bad(key, "字段不能重复")
		}
	}
	for key := range r.MultipartForm.File {
		if key != "file" {
			return speechRequest{}, bad(key, "该文件字段暂不受支持")
		}
	}
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		return speechRequest{}, bad("file", "请上传一个音频文件")
	}
	filename := filepath.Base(files[0].Filename)
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".wav" && ext != ".mp3" {
		return speechRequest{}, bad("file", "同济语音识别当前支持 WAV 和 MP3 文件")
	}
	file, err := files[0].Open()
	if err != nil {
		return speechRequest{}, bad("file", "无法读取音频文件")
	}
	defer file.Close()
	audio, err := io.ReadAll(io.LimitReader(file, maxSpeechFile+1))
	if err != nil || len(audio) == 0 {
		return speechRequest{}, bad("file", "音频文件为空或读取失败")
	}
	if len(audio) > maxSpeechFile {
		return speechRequest{}, &compatError{status: 413, code: "file_too_large", message: "音频文件不能超过 30 MiB", param: "file"}
	}
	if r.FormValue("prompt") != "" {
		return speechRequest{}, bad("prompt", "同济语音识别暂不支持提示词")
	}
	if value := r.FormValue("temperature"); value != "" {
		temperature, err := strconv.ParseFloat(value, 64)
		if err != nil || temperature != 0 {
			return speechRequest{}, bad("temperature", "同济语音识别暂不支持调节 temperature")
		}
	}
	if value := r.FormValue("stream"); value != "" && value != "false" {
		return speechRequest{}, bad("stream", "暂不支持流式语音识别")
	}
	format := r.FormValue("response_format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "text" {
		return speechRequest{}, bad("response_format", "支持 json 或 text")
	}
	model := r.FormValue("model")
	if model == "" {
		model = "Whisper"
	}
	language := strings.TrimSpace(r.FormValue("language"))
	if len(language) > 32 {
		return speechRequest{}, bad("language", "语言代码过长")
	}
	return speechRequest{Model: model, Language: language, Format: format, Filename: filename, Audio: audio}, nil
}

type speechRecord struct {
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	ServiceID   int64  `json:"service_id,omitempty"`
	TaskID      string `json:"speech_task_id,omitempty"`
	HistoryID   int64  `json:"history_id,omitempty"`
}

func speechFingerprint(request speechRequest) string {
	digest := sha256.Sum256(request.Audio)
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(struct {
		Kind, Model, Language, Format, Filename, Digest string
	}{"speech", request.Model, request.Language, request.Format, request.Filename, hex.EncodeToString(digest[:])})
	return hex.EncodeToString(h.Sum(nil))
}

func saveSpeechRecord(path string, record speechRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicWrite(path, string(data)+"\n")
}

type speechResult struct {
	Text     string          `json:"text"`
	Segments json.RawMessage `json:"segments"`
}

func writeSpeechResult(w http.ResponseWriter, result speechResult, format string) {
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, result.Text)
		return
	}
	data := map[string]any{"text": result.Text}
	if len(result.Segments) > 0 && string(result.Segments) != "null" {
		data["segments"] = result.Segments
	}
	writeJSON(w, 200, data)
}

func (g *gateway) speechTranscription(w http.ResponseWriter, r *http.Request) {
	request, failure := parseSpeechRequest(w, r)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	models, failure := g.speechModels(w, r)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	model, found := findSpeechModel(models, request.Model)
	if !found {
		writeCompatError(w, &compatError{status: 404, code: "model_not_found", message: "未找到语音识别模型，请查询 /v1/models", param: "model"})
		return
	}
	request.Model = model.Name
	if strings.EqualFold(model.Name, "Paraformer") {
		switch strings.ToLower(request.Language) {
		case "", "zh", "zh-cn", "中文":
			request.Language = "中文"
		default:
			writeCompatError(w, bad("language", "该 Paraformer 服务仅配置中文识别"))
			return
		}
	}
	key, failure := compatKey(r.Header.Get("Idempotency-Key"))
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	w.Header().Set("Idempotency-Key", key)
	path := recordPath(g.tokenFile, key)
	signature := speechFingerprint(request)

	// Record before the upstream POST; concurrent retries cannot start a second request.
	g.compatMu.Lock()
	var record speechRecord
	data, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &record) != nil || record.Fingerprint == "" {
			g.compatMu.Unlock()
			writeCompatError(w, &compatError{status: 500, code: "invalid_journal", message: "本地提交记录损坏，请检查平台识别历史"})
			return
		}
		if record.Fingerprint != signature {
			g.compatMu.Unlock()
			writeCompatError(w, &compatError{status: 409, code: "idempotency_conflict", message: "同一幂等键对应了不同请求"})
			return
		}
		if record.State != "accepted" && record.State != "completed" {
			g.compatMu.Unlock()
			writeCompatError(w, &compatError{status: 409, code: "submission_unknown", message: "此前识别请求的提交结果尚未确认，请检查平台识别历史"})
			return
		}
		g.compatMu.Unlock()
		g.resumeSpeech(w, r, path, record, request.Format)
		return
	}
	if !os.IsNotExist(err) {
		g.compatMu.Unlock()
		writeCompatError(w, &compatError{status: 500, code: "journal_unavailable", message: "无法读取本地提交记录"})
		return
	}
	record = speechRecord{Fingerprint: signature, State: "submitting"}
	if err := saveSpeechRecord(path, record); err != nil {
		g.compatMu.Unlock()
		writeCompatError(w, &compatError{status: 500, code: "journal_unavailable", message: "无法保存提交记录，识别任务未提交"})
		return
	}
	g.compatMu.Unlock()

	services, failure := g.compatCall(w, r, "GET", fmt.Sprintf("/chat/services?model_asset_id=%d", model.ID), nil)
	if failure != nil {
		g.removeSpeechRecord(path)
		writeCompatError(w, failure)
		return
	}
	var available []struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(services, &available) != nil || len(available) == 0 || available[0].ID <= 0 {
		g.removeSpeechRecord(path)
		writeCompatError(w, &compatError{status: 503, code: "model_unavailable", message: "该模型当前没有可用服务"})
		return
	}
	record.ServiceID = available[0].ID
	body := map[string]any{"service_id": record.ServiceID, "audio_base64": base64.StdEncoding.EncodeToString(request.Audio), "file_name": request.Filename}
	if request.Language != "" {
		body["language"] = request.Language
	}
	endpoint := "/speech/recognize"
	if model.TaskMode {
		endpoint = "/speech/tasks"
		body["kind"] = "stt"
	} else {
		body["enable_itn"], body["enable_punc"], body["enable_ddc"] = true, true, true
	}
	data, failure = g.compatCall(w, r, "POST", endpoint, body)
	if failure != nil {
		if failure.status == 400 || failure.status == 401 || failure.status == 403 || failure.status == 404 || failure.status == 422 {
			g.removeSpeechRecord(path)
		} else {
			failure.code = "submission_unknown"
			failure.message = "识别请求结果未知，请检查平台识别历史，避免重复提交"
		}
		writeCompatError(w, failure)
		return
	}
	var response struct {
		speechResult
		TaskID    string `json:"task_id"`
		HistoryID int64  `json:"history_id"`
	}
	if json.Unmarshal(data, &response) != nil || response.HistoryID <= 0 || (model.TaskMode && response.TaskID == "") {
		writeCompatError(w, &compatError{status: 502, code: "submission_unknown", message: "上游识别响应不完整，请检查平台识别历史"})
		return
	}
	record.TaskID, record.HistoryID = response.TaskID, response.HistoryID
	record.State = "completed"
	if model.TaskMode {
		record.State = "accepted"
	}
	if err := saveSpeechRecord(path, record); err != nil {
		writeCompatError(w, &compatError{status: 500, code: "journal_unavailable", message: "识别已提交但本地记录保存失败，请检查平台识别历史"})
		return
	}
	if !model.TaskMode {
		writeSpeechResult(w, response.speechResult, request.Format)
		return
	}
	g.resumeSpeech(w, r, path, record, request.Format)
}

func (g *gateway) removeSpeechRecord(path string) {
	g.compatMu.Lock()
	defer g.compatMu.Unlock()
	_ = os.Remove(path)
}

func (g *gateway) resumeSpeech(w http.ResponseWriter, r *http.Request, path string, record speechRecord, format string) {
	if record.HistoryID <= 0 {
		writeCompatError(w, &compatError{status: 500, code: "invalid_journal", message: "本地识别记录不完整，请检查平台识别历史"})
		return
	}
	if record.State == "completed" {
		data, failure := g.compatCall(w, r, "GET", "/experience/history/"+strconv.FormatInt(record.HistoryID, 10), nil)
		if failure != nil {
			writeCompatError(w, failure)
			return
		}
		var history struct {
			Status string       `json:"status"`
			Result speechResult `json:"result"`
		}
		if json.Unmarshal(data, &history) != nil || history.Status != "succeeded" {
			writeCompatError(w, &compatError{status: 502, code: "history_unavailable", message: "无法从平台识别历史恢复结果"})
			return
		}
		writeSpeechResult(w, history.Result, format)
		return
	}
	if record.ServiceID <= 0 || record.TaskID == "" {
		writeCompatError(w, &compatError{status: 500, code: "invalid_journal", message: "本地识别任务记录不完整"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	for {
		data, failure := g.compatCall(w, r, "POST", "/speech/tasks/query", map[string]any{"service_id": record.ServiceID, "task_id": record.TaskID, "kind": "stt", "history_id": record.HistoryID})
		if failure != nil {
			writeCompatError(w, failure)
			return
		}
		var response struct {
			speechResult
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(data, &response) != nil {
			writeCompatError(w, &compatError{status: 502, code: "invalid_task", message: "上游语音任务状态格式不符"})
			return
		}
		switch response.Status {
		case "succeeded":
			record.State = "completed"
			if err := saveSpeechRecord(path, record); err != nil {
				writeCompatError(w, &compatError{status: 500, code: "journal_unavailable", message: "识别已完成但本地记录保存失败，请检查平台识别历史"})
				return
			}
			writeSpeechResult(w, response.speechResult, format)
			return
		case "failed", "expired", "cancelled", "canceled":
			message := response.Error
			if message == "" {
				message = "语音识别任务失败，请检查平台识别历史"
			}
			writeCompatError(w, &compatError{status: 502, code: "transcription_failed", message: message})
			return
		}
		if !waitContext(ctx, 3*time.Second) {
			if r.Context().Err() == context.Canceled {
				return
			}
			writeCompatError(w, &compatError{status: 504, code: "transcription_timeout", message: "语音任务仍在处理；可用相同幂等键恢复，或查看平台识别历史"})
			return
		}
	}
}
