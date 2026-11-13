package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxReference = 20 << 20

type compatError struct {
	status               int
	code, message, param string
	taskIDs              []int64
}

func writeCompatError(w http.ResponseWriter, e *compatError) {
	kind := "invalid_request_error"
	if e.status == 401 {
		kind = "authentication_error"
	} else if e.status == 429 {
		kind = "rate_limit_error"
	} else if e.status >= 500 {
		kind = "api_error"
	}
	var param any
	if e.param != "" {
		param = e.param
	}
	detail := map[string]any{"message": e.message, "type": kind, "param": param, "code": e.code}
	if len(e.taskIDs) > 0 {
		detail["task_ids"] = e.taskIDs
	}
	writeJSON(w, e.status, map[string]any{"error": detail})
}

func bad(param, message string) *compatError {
	return &compatError{status: 400, code: "invalid_request", message: message, param: param}
}

func isCompatPath(path string) bool {
	return path == "/v1/models" || strings.HasPrefix(path, "/v1/models/") ||
		strings.HasPrefix(path, "/v1/images/") || path == "/v1/videos" || strings.HasPrefix(path, "/v1/videos/")
}

func (g *gateway) compat(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawPath != "" {
		writeCompatError(w, bad("path", "URL 路径不合法"))
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		g.compatModels(w, r, "")
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		g.compatModels(w, r, strings.TrimPrefix(r.URL.Path, "/v1/models/"))
	case r.Method == "POST" && r.URL.Path == "/v1/images/generations":
		g.compatCreate(w, r, "image")
	case r.Method == "POST" && r.URL.Path == "/v1/images/edits":
		g.compatCreate(w, r, "edit")
	case r.Method == "POST" && r.URL.Path == "/v1/videos":
		g.compatCreate(w, r, "video")
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/videos/"):
		g.compatVideo(w, r)
	default:
		writeCompatError(w, &compatError{status: 404, code: "not_found", message: "接口不存在或请求方法不支持"})
	}
}

func (g *gateway) compatCall(w http.ResponseWriter, r *http.Request, method, path string, body any) (json.RawMessage, *compatError) {
	status, raw, err := g.call(w, r, method, path, body)
	if err != nil {
		return nil, &compatError{status: 502, code: "upstream_unavailable", message: "上游连接失败；请核对任务列表后再重试"}
	}
	var response struct {
		Code      int             `json:"code"`
		Message   string          `json:"message"`
		ErrorCode string          `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return nil, &compatError{status: 502, code: "invalid_upstream_response", message: "上游未返回有效 JSON"}
	}
	if status < 200 || status >= 300 || response.Code != 200 {
		if status >= 200 && status < 300 {
			status = response.Code
			if status < 400 || status > 599 {
				status = 502
			}
		}
		message, code := response.Message, response.ErrorCode
		if message == "" {
			message = "上游请求失败"
		}
		if code == "" {
			code = "upstream_error"
		}
		return nil, &compatError{status: status, code: code, message: message}
	}
	return response.Data, nil
}

type compatParam struct {
	Key, Type         string
	Default           json.RawMessage            `json:"default"`
	Map               map[string]json.RawMessage `json:"map"`
	Options           []string                   `json:"options"`
	Min, Max          json.RawMessage
	Randomize, Locked bool
}

type compatWorkflow struct {
	Code      string        `json:"code"`
	Name      string        `json:"name"`
	Type      string        `json:"type"`
	UpdatedAt string        `json:"updated_at"`
	Schema    []compatParam `json:"-"`
}

func (g *gateway) compatWorkflows(w http.ResponseWriter, r *http.Request) ([]compatWorkflow, *compatError) {
	data, failure := g.compatCall(w, r, "GET", "/aigc/templates", nil)
	if failure != nil {
		return nil, failure
	}
	var raw []struct {
		Code      string          `json:"code"`
		Name      string          `json:"name"`
		Type      string          `json:"type"`
		UpdatedAt string          `json:"updated_at"`
		Schema    json.RawMessage `json:"param_schema"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil, &compatError{status: 502, code: "invalid_templates", message: "上游工作流清单格式不符"}
	}
	result := make([]compatWorkflow, 0, len(raw))
	for _, item := range raw {
		schema := item.Schema
		var text string
		if json.Unmarshal(schema, &text) == nil {
			schema = []byte(text)
		}
		var fields []compatParam
		if json.Unmarshal(schema, &fields) != nil {
			return nil, &compatError{status: 502, code: "invalid_templates", message: "上游工作流参数格式不符"}
		}
		result = append(result, compatWorkflow{Code: item.Code, Name: item.Name, Type: item.Type, UpdatedAt: item.UpdatedAt, Schema: fields})
	}
	return result, nil
}

func workflowByCode(items []compatWorkflow, code string) (compatWorkflow, bool) {
	for _, item := range items {
		if item.Code == code {
			return item, true
		}
	}
	return compatWorkflow{}, false
}

func modelObject(item compatWorkflow) map[string]any {
	created := int64(0)
	if value, err := time.Parse(time.RFC3339Nano, item.UpdatedAt); err == nil {
		created = value.Unix()
	}
	return map[string]any{"id": item.Code, "object": "model", "created": created, "owned_by": "tongji", "name": item.Name, "type": item.Type}
}

func (g *gateway) compatModels(w http.ResponseWriter, r *http.Request, code string) {
	items, failure := g.compatWorkflows(w, r)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	if code != "" {
		item, found := workflowByCode(items, code)
		if !found {
			writeCompatError(w, &compatError{status: 404, code: "model_not_found", message: "未找到上架模型", param: "model"})
			return
		}
		writeJSON(w, 200, modelObject(item))
		return
	}
	list := make([]map[string]any, 0, len(items))
	for _, item := range items {
		list = append(list, modelObject(item))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": list})
}

type imageRef struct {
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"-"`
	Filename string `json:"-"`
	Mime     string `json:"-"`
}

type compatRequest struct {
	Model          string     `json:"model"`
	Prompt         string     `json:"prompt"`
	N              int        `json:"n"`
	Size           string     `json:"size"`
	Seconds        string     `json:"seconds"`
	ResponseFormat string     `json:"response_format"`
	OutputFormat   string     `json:"output_format"`
	Quality        string     `json:"quality"`
	User           string     `json:"user,omitempty"`
	Seed           *int64     `json:"seed,omitempty"`
	Images         []imageRef `json:"-"`
}

type imageReference struct {
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
}

func (g *gateway) compatCreate(w http.ResponseWriter, r *http.Request, kind string) {
	request, failure := parseCompatRequest(w, r, kind)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	items, failure := g.compatWorkflows(w, r)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	item, found := workflowByCode(items, request.Model)
	if !found {
		writeCompatError(w, &compatError{status: 404, code: "model_not_found", message: "未找到上架模型，请查询 /v1/models", param: "model"})
		return
	}
	if (kind == "image" && item.Type != "t2i") || (kind == "edit" && item.Type != "i2i") || (kind == "video" && item.Type != "t2v" && item.Type != "i2v") {
		writeCompatError(w, bad("model", "模型类型与接口不匹配，请选择对应的工作流模型"))
		return
	}
	if kind == "video" && ((item.Type == "i2v" && len(request.Images) == 0) || (item.Type == "t2v" && len(request.Images) != 0)) {
		writeCompatError(w, bad("input_reference", "图生视频模型需要参考图，文生视频模型不接受参考图"))
		return
	}
	key, failure := compatKey(r.Header.Get("Idempotency-Key"))
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	w.Header().Set("Idempotency-Key", key)
	params, failure := compatParams(item, request, key)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	ids, failure := g.compatSubmit(w, r, item, request, params, key)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	if kind == "video" {
		writeJSON(w, 200, map[string]any{"id": fmt.Sprintf("video_%d", ids[0]), "object": "video", "model": item.Code, "status": "queued", "progress": 0, "created_at": time.Now().Unix(), "completed_at": nil, "expires_at": nil, "error": nil, "prompt": request.Prompt, "seconds": request.Seconds, "size": "", "remixed_from_video_id": nil})
		return
	}
	g.compatImageResult(w, r, ids, request)
}

type compatWire struct {
	Model             string           `json:"model"`
	Prompt            string           `json:"prompt"`
	N                 *int             `json:"n"`
	Size              string           `json:"size"`
	Seconds           json.RawMessage  `json:"seconds"`
	ResponseFormat    string           `json:"response_format"`
	OutputFormat      string           `json:"output_format"`
	Quality           string           `json:"quality"`
	User              string           `json:"user"`
	Seed              *int64           `json:"seed"`
	Stream            bool             `json:"stream"`
	Background        string           `json:"background"`
	Style             string           `json:"style"`
	OutputCompression *int             `json:"output_compression"`
	Mask              json.RawMessage  `json:"mask"`
	Images            []imageReference `json:"images"`
	Image             json.RawMessage  `json:"image"`
	InputReference    json.RawMessage  `json:"input_reference"`
}

func parseCompatRequest(w http.ResponseWriter, r *http.Request, kind string) (compatRequest, *compatError) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*maxReference*4/3+2<<20)
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var wire compatWire
	var refs []imageRef
	if mediaType == "multipart/form-data" {
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			return compatRequest{}, bad("body", "multipart 请求无效或超过大小限制")
		}
		defer r.MultipartForm.RemoveAll()
		allowedValues := map[string]bool{"model": true, "prompt": true, "size": true, "response_format": true, "output_format": true, "quality": true, "background": true, "style": true, "stream": true, "n": true, "seed": true, "seconds": true, "input_reference": true, "user": true, "mask": true, "output_compression": true}
		allowedFiles := map[string]bool{"image": true, "image[]": true, "images": true, "input_reference": true, "mask": true}
		for name := range r.MultipartForm.Value {
			if !allowedValues[name] {
				return compatRequest{}, bad(name, "该字段暂不受支持")
			}
		}
		for name := range r.MultipartForm.File {
			if !allowedFiles[name] {
				return compatRequest{}, bad(name, "该文件字段暂不受支持")
			}
		}
		wire.Model, wire.Prompt, wire.Size = r.FormValue("model"), r.FormValue("prompt"), r.FormValue("size")
		wire.User = r.FormValue("user")
		wire.ResponseFormat, wire.OutputFormat, wire.Quality = r.FormValue("response_format"), r.FormValue("output_format"), r.FormValue("quality")
		wire.Background, wire.Style = r.FormValue("background"), r.FormValue("style")
		if value := r.FormValue("stream"); value != "" && value != "false" {
			return compatRequest{}, bad("stream", "当前模型不支持流式图片结果")
		}
		if value := r.FormValue("n"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return compatRequest{}, bad("n", "n 必须是整数")
			}
			wire.N = &n
		}
		if value := r.FormValue("seed"); value != "" {
			seed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return compatRequest{}, bad("seed", "seed 必须是整数")
			}
			wire.Seed = &seed
		}
		if value := r.FormValue("seconds"); value != "" {
			wire.Seconds, _ = json.Marshal(value)
		}
		if r.FormValue("mask") != "" || len(r.MultipartForm.File["mask"]) > 0 {
			return compatRequest{}, bad("mask", "该平台工作流未提供通用 mask 接口")
		}
		if r.FormValue("output_compression") != "" {
			return compatRequest{}, bad("output_compression", "暂不支持指定输出压缩率")
		}
		for _, field := range []string{"image", "image[]", "images", "input_reference"} {
			for _, header := range r.MultipartForm.File[field] {
				if len(refs) >= 4 {
					return compatRequest{}, bad("image", "最多支持四张参考图")
				}
				file, err := header.Open()
				if err != nil {
					return compatRequest{}, bad("image", "无法读取参考图")
				}
				data, err := io.ReadAll(io.LimitReader(file, maxReference+1))
				file.Close()
				if err != nil || len(data) > maxReference {
					return compatRequest{}, bad("image", "单张参考图不能超过 20 MB")
				}
				ref, failure := makeImageRef(data, header.Filename)
				if failure != nil {
					return compatRequest{}, failure
				}
				refs = append(refs, ref)
			}
		}
		if value := r.FormValue("input_reference"); value != "" && kind == "video" {
			raw := json.RawMessage(value)
			if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "data:") {
				raw, _ = json.Marshal(value)
			}
			ref, failure := parseImageReference(raw)
			if failure != nil {
				return compatRequest{}, failure
			}
			refs = append(refs, ref)
		}
	} else if mediaType == "application/json" || mediaType == "" {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
			return compatRequest{}, bad("body", "请求 JSON 无效或包含不支持的字段")
		}
		if wire.Stream {
			return compatRequest{}, bad("stream", "当前模型不支持流式图片结果")
		}
		if len(wire.Mask) > 0 && string(wire.Mask) != "null" {
			return compatRequest{}, bad("mask", "该平台工作流未提供通用 mask 接口")
		}
		if wire.OutputCompression != nil {
			return compatRequest{}, bad("output_compression", "暂不支持指定输出压缩率")
		}
		for _, item := range wire.Images {
			if item.FileID != "" {
				return compatRequest{}, bad("images", "暂不支持 OpenAI file_id，请上传图片或提供 image_url")
			}
			if item.ImageURL == "" {
				return compatRequest{}, bad("images", "缺少 image_url")
			}
			refs = append(refs, imageRef{URL: item.ImageURL})
		}
		for _, field := range []json.RawMessage{wire.Image, wire.InputReference} {
			if len(field) == 0 || string(field) == "null" {
				continue
			}
			ref, failure := parseImageReference(field)
			if failure != nil {
				return compatRequest{}, failure
			}
			refs = append(refs, ref)
		}
	} else {
		return compatRequest{}, bad("body", "请使用 application/json 或 multipart/form-data")
	}
	if strings.TrimSpace(wire.Prompt) == "" || len([]rune(wire.Prompt)) > 32000 {
		return compatRequest{}, bad("prompt", "prompt 不能为空且不能超过 32000 字符")
	}
	if wire.Quality != "" && wire.Quality != "auto" && wire.Quality != "standard" {
		return compatRequest{}, bad("quality", "当前工作流不支持指定该 quality")
	}
	if wire.Background != "" && wire.Background != "auto" {
		return compatRequest{}, bad("background", "当前工作流不支持指定背景透明度")
	}
	if wire.Style != "" {
		return compatRequest{}, bad("style", "请在 prompt 中描述所需风格")
	}
	n := 1
	if wire.N != nil {
		n = *wire.N
	}
	if n < 1 || n > 10 || (kind == "video" && n != 1) {
		return compatRequest{}, bad("n", "n 须为 1 至 10；视频仅支持 1")
	}
	if len(refs) > 4 || (kind == "edit" && len(refs) == 0) || (kind == "image" && len(refs) > 0) || (kind == "video" && len(refs) > 1) {
		return compatRequest{}, bad("image", "参考图数量与接口不匹配")
	}
	model := wire.Model
	if model == "" {
		if kind == "edit" {
			model = "qwen-image-2-1-e"
		} else if kind == "video" && len(refs) > 0 {
			model = "minimax-h3-i2v"
		} else if kind == "video" {
			model = "minimax-h3-t2v"
		} else {
			model = "t2i-krea2-turbo"
		}
	}
	seconds := ""
	if len(wire.Seconds) > 0 && string(wire.Seconds) != "null" {
		if json.Unmarshal(wire.Seconds, &seconds) != nil {
			var v int
			if json.Unmarshal(wire.Seconds, &v) != nil {
				return compatRequest{}, bad("seconds", "seconds 必须是整数或整数字符串")
			}
			seconds = strconv.Itoa(v)
		}
	}
	if kind == "video" && seconds == "" {
		seconds = "5"
	}
	if kind != "video" && seconds != "" {
		return compatRequest{}, bad("seconds", "图片接口不接受 seconds")
	}
	format := wire.ResponseFormat
	if format == "" {
		format = "b64_json"
	}
	if format != "b64_json" && format != "url" {
		return compatRequest{}, bad("response_format", "支持 b64_json 或 url")
	}
	if wire.OutputFormat != "" && wire.OutputFormat != "png" && wire.OutputFormat != "jpeg" {
		return compatRequest{}, bad("output_format", "支持 png 或 jpeg")
	}
	if format == "url" && wire.OutputFormat != "" {
		return compatRequest{}, bad("output_format", "url 模式不转换文件格式")
	}
	return compatRequest{Model: model, Prompt: wire.Prompt, N: n, Size: wire.Size, Seconds: seconds, ResponseFormat: format, OutputFormat: wire.OutputFormat, Quality: wire.Quality, User: wire.User, Seed: wire.Seed, Images: refs}, nil
}

func makeImageRef(data []byte, filename string) (imageRef, *compatError) {
	typeName := http.DetectContentType(data)
	if typeName != "image/png" && typeName != "image/jpeg" && typeName != "image/webp" {
		return imageRef{}, bad("image", "参考图须为 PNG、JPEG 或 WebP")
	}
	return imageRef{Data: data, Filename: filepath.Base(filename), Mime: typeName}, nil
}

func parseImageReference(raw json.RawMessage) (imageRef, *compatError) {
	var value string
	if json.Unmarshal(raw, &value) == nil && value != "" {
		return imageRef{URL: value}, nil
	}
	var object imageReference
	if json.Unmarshal(raw, &object) != nil || object.ImageURL == "" || object.FileID != "" {
		return imageRef{}, bad("image", "请提供 image_url；暂不支持 file_id")
	}
	return imageRef{URL: object.ImageURL}, nil
}

func compatKey(value string) (string, *compatError) {
	if value == "" {
		data := make([]byte, 16)
		if _, err := rand.Read(data); err != nil {
			return "", &compatError{status: 500, code: "internal_error", message: "无法创建请求标识"}
		}
		return "cmp-" + hex.EncodeToString(data), nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`).MatchString(value) {
		return "", bad("Idempotency-Key", "幂等键须为 8 至 128 位字母、数字、下划线或连字符")
	}
	return value, nil
}

func upstreamIdempotencyKey(key string) string {
	sum := sha256.Sum256([]byte("compat:" + key))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func rawInt(raw json.RawMessage, fallback int64) int64 {
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return fallback
	}
	return value
}

func compatParams(item compatWorkflow, req compatRequest, key string) (map[string]json.RawMessage, *compatError) {
	params := make(map[string]json.RawMessage)
	imageFields := 0
	for _, p := range item.Schema {
		if p.Type == "image" {
			imageFields++
			continue
		}
		if p.Key == "prompt" {
			params[p.Key], _ = json.Marshal(req.Prompt)
			continue
		}
		if p.Randomize && p.Type == "int" {
			minimum, maximum := rawInt(p.Min, 0), rawInt(p.Max, 2147483647)
			seed := int64(0)
			if req.Seed != nil {
				seed = *req.Seed
			} else if maximum >= minimum && minimum >= 0 && maximum < int64(^uint64(0)>>1) {
				sum := sha256.Sum256([]byte(key + ":" + p.Key))
				seed = minimum + int64(binary.BigEndian.Uint64(sum[:8])%uint64(maximum-minimum+1))
			} else {
				return nil, bad("seed", "模型的种子范围不受支持")
			}
			if seed < minimum || seed > maximum {
				return nil, bad("seed", "seed 超出模型允许范围")
			}
			params[p.Key], _ = json.Marshal(seed)
			continue
		}
		if len(p.Default) > 0 && string(p.Default) != "null" {
			params[p.Key] = p.Default
		}
	}
	if req.Seed != nil {
		found := false
		for _, p := range item.Schema {
			if p.Key == "seed" && p.Type == "int" {
				if *req.Seed < rawInt(p.Min, 0) || *req.Seed > rawInt(p.Max, 2147483647) {
					return nil, bad("seed", "seed 超出模型允许范围")
				}
				params[p.Key], _ = json.Marshal(*req.Seed)
				found = true
				break
			}
		}
		if !found {
			return nil, bad("seed", "该模型不支持 seed")
		}
	}
	if len(req.Images) > imageFields {
		return nil, bad("image", fmt.Sprintf("该模型最多支持 %d 张参考图", imageFields))
	}
	if req.Size != "" && req.Size != "auto" {
		parts := strings.Split(req.Size, "x")
		if len(parts) != 2 {
			return nil, bad("size", "size 须为宽x高，例如 1024x1024")
		}
		width, e1 := strconv.Atoi(parts[0])
		height, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || width < 1 || height < 1 {
			return nil, bad("size", "size 须为正整数宽x高")
		}
		matched := false
		for _, p := range item.Schema {
			if p.Locked || (p.Key != "ratio" && p.Key != "resolution" && p.Key != "aspect_ratio") {
				continue
			}
			for label, raw := range p.Map {
				var dimensions []int
				if json.Unmarshal(raw, &dimensions) == nil && len(dimensions) >= 2 && dimensions[0] == width && dimensions[1] == height {
					params[p.Key], _ = json.Marshal(label)
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return nil, bad("size", "该模型不能生成所请求的精确尺寸；请使用 size=auto 或工作流支持的尺寸")
		}
	}
	if req.Seconds != "" {
		seconds, err := strconv.ParseInt(req.Seconds, 10, 64)
		if err != nil || seconds <= 0 {
			return nil, bad("seconds", "seconds 须为正整数")
		}
		matched := false
		for _, p := range item.Schema {
			if p.Key != "duration" || p.Locked {
				continue
			}
			if p.Type == "int" && seconds >= rawInt(p.Min, 0) && seconds <= rawInt(p.Max, 1<<31-1) {
				params[p.Key], _ = json.Marshal(seconds)
				matched = true
				break
			}
			for label, raw := range p.Map {
				var v []int64
				if json.Unmarshal(raw, &v) == nil && len(v) > 0 && v[0] == seconds {
					params[p.Key], _ = json.Marshal(label)
					matched = true
					break
				}
			}
		}
		if !matched {
			return nil, bad("seconds", "该模型不支持请求的时长；请查看 /v1/workflows 的 duration 参数")
		}
	}
	return params, nil
}

type compatRecord struct {
	Fingerprint string  `json:"fingerprint"`
	State       string  `json:"state"`
	TaskIDs     []int64 `json:"task_ids,omitempty"`
}

func fingerprint(kind string, request compatRequest) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(struct {
		Kind    string
		Request compatRequest
	}{kind, request})
	for _, ref := range request.Images {
		_ = json.NewEncoder(h).Encode(ref.URL)
		sum := sha256.Sum256(ref.Data)
		_, _ = h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func recordPath(tokenFile, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(filepath.Dir(tokenFile), "compat-submissions", hex.EncodeToString(sum[:])+".json")
}

func saveRecord(path string, record compatRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicWrite(path, string(data)+"\n")
}

func (g *gateway) compatSubmit(w http.ResponseWriter, r *http.Request, item compatWorkflow, request compatRequest, params map[string]json.RawMessage, key string) ([]int64, *compatError) {
	// A single lock keeps the local journal and paid submissions consistent.
	g.compatMu.Lock()
	defer g.compatMu.Unlock()
	kind := item.Type
	signature := fingerprint(kind, request)
	path := recordPath(g.tokenFile, key)
	if data, err := os.ReadFile(path); err == nil {
		var previous compatRecord
		if json.Unmarshal(data, &previous) != nil || previous.Fingerprint == "" {
			return nil, &compatError{status: 500, code: "invalid_journal", message: "本地提交记录损坏，请检查任务列表"}
		}
		if previous.Fingerprint != signature {
			return nil, &compatError{status: 409, code: "idempotency_conflict", message: "同一幂等键对应了不同请求，请为新任务使用新键"}
		}
		if previous.State == "accepted" && len(previous.TaskIDs) == request.N {
			return previous.TaskIDs, nil
		}
		return nil, &compatError{status: 409, code: "submission_unknown", message: "此前提交结果尚未确认，请检查任务列表，避免重复计费", taskIDs: previous.TaskIDs}
	} else if !os.IsNotExist(err) {
		return nil, &compatError{status: 500, code: "journal_unavailable", message: "无法读取本地提交记录"}
	}
	imageIndex := 0
	for _, p := range item.Schema {
		if p.Type != "image" || imageIndex >= len(request.Images) {
			continue
		}
		ref, failure := g.resolveReference(r.Context(), request.Images[imageIndex])
		if failure != nil {
			return nil, failure
		}
		path, failure := g.uploadReference(r, ref)
		if failure != nil {
			return nil, failure
		}
		params[p.Key], _ = json.Marshal(path)
		imageIndex++
	}
	payload := generation{TemplateCode: item.Code, Params: params, Count: request.N}
	quote, failure := g.compatCall(w, r, "POST", "/aigc/quote", payload)
	if failure != nil {
		return nil, failure
	}
	var q struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(quote, &q) != nil || q.Token == "" {
		return nil, &compatError{status: 502, code: "invalid_quote", message: "上游报价缺少 token，任务未提交"}
	}
	if err := saveRecord(path, compatRecord{Fingerprint: signature, State: "submitting"}); err != nil {
		return nil, &compatError{status: 500, code: "journal_unavailable", message: "无法保存提交记录，任务未提交"}
	}
	payload.QuoteToken, payload.IdempotencyKey = q.Token, upstreamIdempotencyKey(key)
	result, failure := g.compatCall(w, r, "POST", "/aigc/tasks", payload)
	if failure != nil {
		if failure.status == 400 || failure.status == 401 || failure.status == 403 || failure.status == 404 || failure.status == 422 {
			_ = os.Remove(path)
		} else {
			failure.code = "submission_unknown"
			failure.message = "任务提交结果未知，请查看任务列表，避免重复计费"
		}
		return nil, failure
	}
	var created struct {
		TaskID  int64   `json:"task_id"`
		TaskIDs []int64 `json:"task_ids"`
	}
	if json.Unmarshal(result, &created) != nil {
		return nil, &compatError{status: 502, code: "submission_unknown", message: "上游响应无法解析，请查看任务列表，避免重复计费"}
	}
	if len(created.TaskIDs) == 0 && created.TaskID > 0 {
		created.TaskIDs = []int64{created.TaskID}
	}
	state := "accepted"
	if len(created.TaskIDs) != request.N {
		state = "partial"
	}
	if err := saveRecord(path, compatRecord{Fingerprint: signature, State: state, TaskIDs: created.TaskIDs}); err != nil {
		return nil, &compatError{status: 500, code: "journal_unavailable", message: "任务已提交但本地记录保存失败，请查看任务列表", taskIDs: created.TaskIDs}
	}
	if state != "accepted" {
		return nil, &compatError{status: 502, code: "partial_submission", message: "实际创建任务数与请求不一致，请查看任务列表", taskIDs: created.TaskIDs}
	}
	return created.TaskIDs, nil
}

func safeImageClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid host")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var last error
		for _, candidate := range ips {
			ip := candidate.IP
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if v := ip.To4(); v != nil && ((v[0] == 100 && v[1] >= 64 && v[1] <= 127) || (v[0] == 198 && (v[1] == 18 || v[1] == 19))) {
				continue
			}
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last != nil {
			return nil, last
		}
		return nil, errors.New("non-public address")
	}
	return &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 4 || r.URL.Scheme != "https" || r.URL.User != nil {
			return errors.New("unsafe redirect")
		}
		return nil
	}}
}

func (g *gateway) resolveReference(ctx context.Context, ref imageRef) (imageRef, *compatError) {
	if len(ref.Data) > 0 {
		return ref, nil
	}
	if strings.HasPrefix(ref.URL, "data:") {
		header, encoded, found := strings.Cut(ref.URL, ",")
		if !found || !strings.HasSuffix(header, ";base64") || len(encoded) > maxReference*4/3+8 {
			return imageRef{}, bad("image_url", "data URL 无效或图片超过 20 MB")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) > maxReference {
			return imageRef{}, bad("image_url", "data URL 无效或图片超过 20 MB")
		}
		return makeImageRef(data, "reference")
	}
	u, err := url.Parse(ref.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return imageRef{}, bad("image_url", "image_url 必须是公网 HTTPS 地址或 data URL")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return imageRef{}, bad("image_url", "参考图 URL 不合法")
	}
	client := safeImageClient()
	// Signed outputs from this platform must work even when campus DNS uses a fake IP.
	// No authorization header or browser cookie is sent for this download.
	if sameSSOOrigin(u, g.upstream) && strings.HasPrefix(u.Path, "/remote-home/") {
		client = g.client
	}
	response, err := client.Do(req)
	if err != nil {
		return imageRef{}, bad("image_url", "无法从公网 HTTPS 地址读取参考图")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return imageRef{}, bad("image_url", "参考图 URL 返回非成功状态")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxReference+1))
	if err != nil || len(data) > maxReference {
		return imageRef{}, bad("image_url", "参考图读取失败或超过 20 MB")
	}
	return makeImageRef(data, "reference")
}

func (g *gateway) uploadReference(r *http.Request, ref imageRef) (string, *compatError) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	name := ref.Filename
	if name == "" || name == "." {
		name = "reference"
	}
	if filepath.Ext(name) == "" {
		if ref.Mime == "image/png" {
			name += ".png"
		} else if ref.Mime == "image/jpeg" {
			name += ".jpg"
		} else {
			name += ".webp"
		}
	}
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "image", "filename": name}))
	headers.Set("Content-Type", ref.Mime)
	part, err := writer.CreatePart(headers)
	if err != nil {
		return "", &compatError{status: 500, code: "upload_error", message: "无法准备参考图上传"}
	}
	if _, err = part.Write(ref.Data); err != nil {
		return "", &compatError{status: 500, code: "upload_error", message: "无法准备参考图上传"}
	}
	if writer.Close() != nil {
		return "", &compatError{status: 500, code: "upload_error", message: "无法准备参考图上传"}
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", g.upstream.String()+"/api/v1/aigc/inputs", &body)
	if err != nil {
		return "", &compatError{status: 500, code: "upload_error", message: "无法准备参考图上传"}
	}
	req.Header.Set("Authorization", r.Context().Value(authContextKey{}).(string))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := g.client.Do(req)
	if err != nil {
		return "", &compatError{status: 502, code: "upload_error", message: "上游参考图上传失败"}
	}
	defer response.Body.Close()
	if response.StatusCode == 401 && g.auth != nil {
		g.auth.reject(req.Header.Get("Authorization"))
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result) != nil {
		return "", &compatError{status: 502, code: "upload_error", message: "上游参考图响应无效"}
	}
	if response.StatusCode != 200 || result.Code != 200 || result.Data.Path == "" {
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = 502
		}
		return "", &compatError{status: status, code: "upload_error", message: "上游参考图上传失败：" + result.Message}
	}
	return result.Data.Path, nil
}

type compatOutput struct {
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	URL      string `json:"url"`
}

type compatTask struct {
	ID           int64          `json:"id"`
	Type         string         `json:"type"`
	TemplateCode string         `json:"template_code"`
	Status       string         `json:"status"`
	Progress     int            `json:"progress"`
	CreatedAt    string         `json:"created_at"`
	FinishedAt   string         `json:"finished_at"`
	ErrorMessage string         `json:"error_message"`
	Outputs      []compatOutput `json:"outputs"`
}

func (g *gateway) compatTask(w http.ResponseWriter, r *http.Request, id int64) (compatTask, *compatError) {
	data, failure := g.compatCall(w, r, "GET", fmt.Sprintf("/aigc/tasks/%d", id), nil)
	if failure != nil {
		return compatTask{}, failure
	}
	var task compatTask
	if json.Unmarshal(data, &task) != nil || task.ID != id {
		return compatTask{}, &compatError{status: 502, code: "invalid_task", message: "上游任务信息格式不符"}
	}
	return task, nil
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (g *gateway) compatImageResult(w http.ResponseWriter, r *http.Request, ids []int64, request compatRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	results := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		var task compatTask
		for {
			var failure *compatError
			task, failure = g.compatTask(w, r, id)
			if failure != nil {
				failure.taskIDs = ids
				writeCompatError(w, failure)
				return
			}
			if task.Status == "succeeded" {
				break
			}
			if task.Status == "failed" || task.Status == "canceled" || task.Status == "cancelled" {
				message := task.ErrorMessage
				if message == "" {
					message = "图片任务失败或已取消，请查看任务详情和结算状态"
				}
				writeCompatError(w, &compatError{status: 502, code: "generation_failed", message: message, taskIDs: ids})
				return
			}
			if !waitContext(ctx, 2500*time.Millisecond) {
				if r.Context().Err() == context.Canceled {
					return
				}
				writeCompatError(w, &compatError{status: 504, code: "generation_timeout", message: "图片仍在生成，请用任务 ID 查询；使用相同幂等键可恢复请求", taskIDs: ids})
				return
			}
		}
		index := -1
		for i, output := range task.Outputs {
			if strings.HasPrefix(output.Mime, "image/") {
				index = i
				break
			}
		}
		if index < 0 {
			writeCompatError(w, &compatError{status: 502, code: "missing_image", message: "任务完成但没有图片文件", taskIDs: ids})
			return
		}
		if request.ResponseFormat == "url" {
			link, failure := g.compatMediaURL(w, r, task.Outputs[index].URL)
			if failure != nil {
				failure.taskIDs = ids
				writeCompatError(w, failure)
				return
			}
			results = append(results, map[string]string{"url": link})
			continue
		}
		data, detected, failure := g.downloadImage(r, id, index)
		if failure != nil {
			failure.taskIDs = ids
			writeCompatError(w, failure)
			return
		}
		if request.OutputFormat != "" && detected != "image/"+request.OutputFormat {
			decoded, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				writeCompatError(w, &compatError{status: 502, code: "unsupported_image_format", message: "无法转换上游图片格式，请使用原生任务下载", taskIDs: ids})
				return
			}
			var output bytes.Buffer
			if request.OutputFormat == "png" {
				err = png.Encode(&output, decoded)
			} else {
				err = jpeg.Encode(&output, decoded, &jpeg.Options{Quality: 90})
			}
			if err != nil {
				writeCompatError(w, &compatError{status: 502, code: "image_conversion_failed", message: "图片格式转换失败，请使用原生任务下载", taskIDs: ids})
				return
			}
			data = output.Bytes()
		}
		results = append(results, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(data)})
	}
	result := map[string]any{"created": time.Now().Unix(), "data": results}
	if request.OutputFormat != "" {
		result["output_format"] = request.OutputFormat
	}
	writeJSON(w, 200, result)
}

func (g *gateway) downloadImage(r *http.Request, id int64, index int) ([]byte, string, *compatError) {
	path := fmt.Sprintf("/api/v1/aigc/tasks/%d/outputs/%d", id, index)
	req, err := http.NewRequestWithContext(r.Context(), "GET", g.upstream.String()+path, nil)
	if err != nil {
		return nil, "", &compatError{status: 500, code: "download_error", message: "无法准备图片下载"}
	}
	req.Header.Set("Authorization", r.Context().Value(authContextKey{}).(string))
	response, err := g.client.Do(req)
	if err != nil {
		return nil, "", &compatError{status: 502, code: "download_error", message: "上游图片下载失败"}
	}
	defer response.Body.Close()
	if response.StatusCode == 401 && g.auth != nil {
		g.auth.reject(req.Header.Get("Authorization"))
	}
	if response.StatusCode != 200 {
		return nil, "", &compatError{status: 502, code: "download_error", message: fmt.Sprintf("上游图片下载返回 HTTP %d", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20+1))
	if err != nil || len(data) > 64<<20 {
		return nil, "", &compatError{status: 502, code: "download_error", message: "图片读取失败或超过 64 MB"}
	}
	detected := http.DetectContentType(data)
	if !strings.HasPrefix(detected, "image/") {
		return nil, "", &compatError{status: 502, code: "invalid_image", message: "上游文件不是可识别的图片"}
	}
	return data, detected, nil
}

func (g *gateway) compatMediaURL(w http.ResponseWriter, r *http.Request, original string) (string, *compatError) {
	u, err := url.Parse(original)
	if err != nil || original == "" || u.User != nil || (u.IsAbs() && (u.Scheme != g.upstream.Scheme || u.Host != g.upstream.Host)) || !strings.HasPrefix(u.Path, "/remote-home/") {
		return "", &compatError{status: 502, code: "invalid_image_url", message: "上游图片 URL 不可用，请使用 b64_json"}
	}
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	data, failure := g.compatCall(w, r, "POST", "/aigc/media-ticket", map[string]string{"url": path})
	if failure != nil {
		return "", failure
	}
	var ticket struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(data, &ticket) != nil {
		return "", &compatError{status: 502, code: "invalid_image_url", message: "上游媒体票据响应无效"}
	}
	signed, err := url.Parse(ticket.URL)
	if err != nil || signed.User != nil || !strings.HasPrefix(signed.Path, "/remote-home/") || (signed.IsAbs() && (signed.Scheme != g.upstream.Scheme || signed.Host != g.upstream.Host)) {
		return "", &compatError{status: 502, code: "invalid_image_url", message: "上游媒体票据地址无效"}
	}
	return g.upstream.ResolveReference(signed).String(), nil
}

var videoPath = regexp.MustCompile(`^/v1/videos/video_([1-9][0-9]*)(/content)?$`)

func (g *gateway) compatVideo(w http.ResponseWriter, r *http.Request) {
	match := videoPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		writeCompatError(w, &compatError{status: 404, code: "not_found", message: "视频任务 ID 格式不正确"})
		return
	}
	id, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		writeCompatError(w, bad("video_id", "视频任务 ID 不合法"))
		return
	}
	task, failure := g.compatTask(w, r, id)
	if failure != nil {
		writeCompatError(w, failure)
		return
	}
	if task.Type != "t2v" && task.Type != "i2v" {
		writeCompatError(w, &compatError{status: 404, code: "not_found", message: "该任务不是视频任务"})
		return
	}
	if match[2] != "" {
		if variant := r.URL.Query().Get("variant"); variant != "" && variant != "video" {
			writeCompatError(w, bad("variant", "当前仅支持下载视频文件"))
			return
		}
		if task.Status != "succeeded" {
			writeCompatError(w, &compatError{status: 409, code: "video_not_ready", message: "视频任务尚未成功完成"})
			return
		}
		index := -1
		for i, output := range task.Outputs {
			if strings.HasPrefix(output.Mime, "video/") {
				index = i
				break
			}
		}
		if index < 0 {
			writeCompatError(w, &compatError{status: 404, code: "not_found", message: "视频输出不存在"})
			return
		}
		r.URL.Path = fmt.Sprintf("/api/v1/aigc/tasks/%d/outputs/%d", id, index)
		r.URL.RawPath = ""
		g.proxy.ServeHTTP(w, r)
		return
	}
	var details struct {
		Params []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"params"`
	}
	if data, err := g.compatCall(w, r, "GET", fmt.Sprintf("/aigc/tasks/%d/details", id), nil); err == nil {
		_ = json.Unmarshal(data, &details)
	}
	seconds, prompt := "", ""
	for _, p := range details.Params {
		if p.Key == "duration" {
			if json.Unmarshal(p.Value, &seconds) != nil {
				seconds = strings.Trim(string(p.Value), `"`)
			}
			seconds = strings.TrimSuffix(seconds, "秒")
		}
		if p.Key == "prompt" {
			_ = json.Unmarshal(p.Value, &prompt)
		}
	}
	status := "in_progress"
	switch task.Status {
	case "queued", "submitted":
		status = "queued"
	case "succeeded":
		status = "completed"
	case "failed", "canceled", "cancelled":
		status = "failed"
	}
	var finished any
	if value, err := time.Parse(time.RFC3339Nano, task.FinishedAt); err == nil {
		finished = value.Unix()
	}
	created := int64(0)
	if value, err := time.Parse(time.RFC3339Nano, task.CreatedAt); err == nil {
		created = value.Unix()
	}
	var taskError any
	if status == "failed" {
		message := task.ErrorMessage
		if message == "" {
			message = "视频任务失败或已取消，请查看原生任务详情"
		}
		taskError = map[string]any{"code": "generation_failed", "message": message}
	}
	writeJSON(w, 200, map[string]any{"id": fmt.Sprintf("video_%d", id), "object": "video", "model": task.TemplateCode, "status": status, "progress": task.Progress, "created_at": created, "completed_at": finished, "expires_at": nil, "error": taskError, "prompt": prompt, "seconds": seconds, "size": "", "remixed_from_video_id": nil})
}
