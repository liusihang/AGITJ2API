package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type gateway struct {
	key       string
	tokenFile string
	upstream  *url.URL
	client    *http.Client
	proxy     *httputil.ReverseProxy
	auth      *ssoAuth
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type parameter struct {
	Key       string          `json:"key"`
	Type      string          `json:"type"`
	Default   json.RawMessage `json:"default"`
	Locked    bool            `json:"locked"`
	Randomize bool            `json:"randomize"`
}

type generation struct {
	TemplateCode   string                     `json:"template_code"`
	Params         map[string]json.RawMessage `json:"params"`
	Count          int                        `json:"count"`
	IdempotencyKey string                     `json:"idempotency_key,omitempty"`
	QuoteToken     string                     `json:"quote_token,omitempty"`
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func main() {
	listen := flag.String("listen", env("LISTEN_ADDR", "127.0.0.1:8789"), "监听地址")
	keyFile := flag.String("key-file", env("AIGC_API_KEY_FILE", "data/api-key"), "内部 API Key 文件")
	tokenFile := flag.String("token-file", env("AIGC_TOKEN_FILE", "data/access-token"), "上游 token 文件，每次请求重读")
	cookieFile := flag.String("sso-cookie-file", env("AIGC_SSO_COOKIE_FILE", "data/sso-cookie"), "学校 _idp_session Cookie 文件；空值禁用自动续登")
	ssoInterval := flag.Duration("sso-refresh-interval", 30*time.Minute, "学校 SSO 续登间隔")
	refreshSSO := flag.Bool("refresh-sso", false, "立即通过学校 SSO 更新 token 后退出")
	upstream := flag.String("upstream", "https://agi.tongji.edu.cn", "上游 origin")
	initialize := flag.Bool("init", false, "创建内部 API Key 后退出")
	flag.Parse()
	if *initialize {
		if err := initializeKey(*keyFile); err != nil {
			log.Fatal(err)
		}
		log.Printf("内部 API Key 已准备：%s（未输出密钥）", *keyFile)
		return
	}
	key, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Fatal("无法读取 API Key 文件，请先运行 -init")
	}
	g, err := newGateway(strings.TrimSpace(string(key)), *tokenFile, *upstream)
	if err != nil {
		log.Fatal(err)
	}
	if *ssoInterval < time.Minute {
		log.Fatal("SSO 续登间隔不能少于一分钟")
	}
	g.auth = newSSOAuth(g, *cookieFile, *ssoInterval)
	if *refreshSSO {
		if err := g.auth.refresh(context.Background(), true); err != nil {
			log.Fatal(err)
		}
		return
	}
	server := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go g.auth.run(ctx)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Tongji AIGC proxy listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

func initializeKey(path string) error {
	if existing, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(existing))) < 24 {
			return errors.New("已有 API Key 过短，请手动检查；不会覆盖")
		}
		return os.Chmod(path, 0600)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintln(file, "tj-"+hex.EncodeToString(key))
	return err
}

func newGateway(key, tokenFile, origin string) (*gateway, error) {
	if len(key) < 24 {
		return nil, errors.New("内部 API Key 至少需要 24 字符")
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("upstream 必须是无路径、无凭证的 HTTP(S) origin")
	}
	u.Path = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	forwarder := readRetryTransport{base: transport}
	g := &gateway{key: key, tokenFile: tokenFile, upstream: u, client: &http.Client{
		Transport: forwarder, Timeout: 90 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	g.proxy = &httputil.ReverseProxy{
		Transport: forwarder,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			// Only upstream authentication is sent; never forward the internal key or cookies.
			pr.Out.Header = upstreamHeaders(pr.In.Header, pr.In.Context().Value(authContextKey{}).(string))
		},
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode == 401 && g.auth != nil && r.Request != nil {
				g.auth.reject(r.Request.Header.Get("Authorization"))
			}
			r.Header.Del("Set-Cookie")
			r.Header.Del("Access-Control-Allow-Origin")
			r.Header.Set("Cache-Control", "no-store")
			if r.StatusCode >= 300 && r.StatusCode < 400 {
				return errors.New("unexpected upstream redirect")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logUpstreamError(err)
			writeError(w, 502, "UPSTREAM_UNAVAILABLE", "上游连接失败；生成请求未自动重试，请先核对任务列表")
		},
	}
	return g, nil
}

type readRetryTransport struct{ base http.RoundTripper }

func (t readRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	// A cold upstream connection sometimes closes before returning any headers.
	// Retry that read once; POST and partially delivered file bodies are never retried.
	if r.Method == "GET" && (r.Body == nil || r.Body == http.NoBody) && r.Context().Err() == nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		log.Print("upstream read connection closed; retrying GET once")
		return t.base.RoundTrip(r.Clone(r.Context()))
	}
	return response, err
}

type authContextKey struct{}

// Allowlisted routes prevent this service from becoming a general authenticated proxy.
var taskRoute = regexp.MustCompile(`^/aigc/tasks/[0-9]+(?:/(?:details|archive)|/(?:inputs|outputs)/[0-9]+)?$`)
var cancelRoute = regexp.MustCompile(`^/aigc/tasks/[0-9]+/cancel$`)

func allowed(method, path string) bool {
	if method == "GET" {
		switch path {
		case "/aigc/templates", "/aigc/templates/timing", "/aigc/health", "/aigc/resource-quota", "/aigc/tasks":
			return true
		}
		return taskRoute.MatchString(path)
	}
	if method == "POST" {
		switch path {
		case "/aigc/inputs", "/aigc/estimate", "/aigc/quote", "/aigc/tasks", "/aigc/media-ticket", "/aigc/prompt/optimize":
			return true
		}
		return cancelRoute.MatchString(path)
	}
	return false
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(provided), []byte(g.key)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, 401, "INVALID_API_KEY", "需要有效的内部 Bearer API Key")
		return
	}
	token, expires, tokenErr := readToken(g.tokenFile)
	if r.Method == "GET" && r.URL.Path == "/v1/status" {
		sso := map[string]any{"enabled": false}
		if g.auth != nil {
			sso = g.auth.status()
			if g.auth.isRejected(token) {
				tokenErr = errors.New("上游已拒绝当前 token，下一次请求将尝试 SSO 续登")
			}
		}
		writeJSON(w, 200, map[string]any{"code": 200, "data": map[string]any{
			"token_ready": tokenErr == nil, "token_expires_at": expires,
			"token_error": errorString(tokenErr), "automatic_sso_refresh": sso["enabled"], "sso": sso,
		}})
		return
	}
	if g.auth != nil {
		token, expires, tokenErr = g.auth.token(r.Context())
	}
	if tokenErr != nil {
		writeError(w, 503, "UPSTREAM_TOKEN_UNAVAILABLE", tokenErr.Error())
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, token))
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/workflows":
		g.workflows(w, r)
		return
	case r.Method == "POST" && (r.URL.Path == "/v1/generations" || r.URL.Path == "/v1/estimate"):
		g.generate(w, r)
		return
	}
	path := ""
	if strings.HasPrefix(r.URL.Path, "/api/v1/aigc/") {
		path = strings.TrimPrefix(r.URL.Path, "/api/v1")
	} else if strings.HasPrefix(r.URL.Path, "/v1/") {
		path = "/aigc/" + strings.TrimPrefix(r.URL.Path, "/v1/")
	}
	if r.URL.RawPath != "" || !allowed(r.Method, path) {
		writeError(w, 404, "NOT_FOUND", "接口不存在")
		return
	}
	r.URL.Path = "/api/v1" + path
	r.URL.RawPath = ""
	// Stream uploads and large media; ReverseProxy preserves Range/206 and cancellation.
	g.proxy.ServeHTTP(w, r)
}

func readToken(path string) (string, *time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, errors.New("未找到可读的上游 token 文件，请导入网页登录 token")
	}
	return parseToken(data)
}

func parseToken(data []byte) (string, *time.Time, error) {
	token := strings.TrimSpace(string(data))
	if strings.HasPrefix(token, "{") {
		var object struct {
			Authorization string `json:"authorization"`
			AccessToken   string `json:"access_token"`
		}
		if json.Unmarshal(data, &object) != nil {
			return "", nil, errors.New("上游 token JSON 格式错误")
		}
		token = object.Authorization
		if token == "" {
			token = object.AccessToken
		}
	}
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", nil, errors.New("上游 token 为空或包含非法空白")
	}
	var expires *time.Time
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var payload struct {
			Exp int64 `json:"exp"`
		}
		if json.Unmarshal(claims, &payload) == nil && payload.Exp > 0 {
			value := time.Unix(payload.Exp, 0).UTC()
			expires = &value
			if !value.After(time.Now()) {
				return "", expires, errors.New("上游 token 已过期，请正常 SSO 登录后重新导入；无需重启服务")
			}
		}
	}
	// JWT expiry is advisory; the platform remains responsible for signature validation.
	return "Bearer " + token, expires, nil
}

func upstreamHeaders(in http.Header, auth string) http.Header {
	out := make(http.Header)
	for _, name := range []string{"Content-Type", "Accept", "Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		for _, value := range in.Values(name) {
			out.Add(name, value)
		}
	}
	out.Set("Authorization", auth)
	return out
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func logUpstreamError(err error) {
	// Drop url.Error's URL; query strings may contain signed media tickets.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	log.Printf("upstream transport: %v", err)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": status, "error_code": code, "message": message, "data": nil})
}

// call buffers only JSON control responses. File transfers always use ReverseProxy.
func (g *gateway) call(w http.ResponseWriter, r *http.Request, method, path string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, g.upstream.String()+"/api/v1"+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", r.Context().Value(authContextKey{}).(string))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := g.client.Do(req)
	if err != nil {
		logUpstreamError(err)
		return 0, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == 401 && g.auth != nil {
		g.auth.reject(req.Header.Get("Authorization"))
	}
	for _, name := range []string{"Retry-After", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	return response.StatusCode, data, err
}

func (g *gateway) getData(w http.ResponseWriter, r *http.Request, method, path string, body any) (json.RawMessage, bool) {
	status, data, err := g.call(w, r, method, path, body)
	if err != nil {
		writeError(w, 502, "UPSTREAM_UNAVAILABLE", "上游连接失败；生成请求未自动重试，请先核对任务列表")
		return nil, false
	}
	var value envelope
	if json.Unmarshal(data, &value) != nil {
		writeError(w, 502, "UPSTREAM_INVALID_RESPONSE", "上游未返回有效 JSON")
		return nil, false
	}
	if status < 200 || status >= 300 || value.Code != 200 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(data)
		return nil, false
	}
	return value.Data, true
}

func (g *gateway) templates(w http.ResponseWriter, r *http.Request) ([]map[string]json.RawMessage, bool) {
	data, ok := g.getData(w, r, "GET", "/aigc/templates", nil)
	if !ok {
		return nil, false
	}
	var list []map[string]json.RawMessage
	if json.Unmarshal(data, &list) != nil {
		writeError(w, 502, "UPSTREAM_INVALID_TEMPLATES", "模板清单格式不符")
		return nil, false
	}
	for _, item := range list {
		var schema string
		if json.Unmarshal(item["param_schema"], &schema) == nil {
			if !json.Valid([]byte(schema)) {
				writeError(w, 502, "UPSTREAM_INVALID_TEMPLATES", "模板参数格式不符")
				return nil, false
			}
			item["param_schema"] = json.RawMessage(schema)
		}
	}
	return list, true
}

func (g *gateway) workflows(w http.ResponseWriter, r *http.Request) {
	list, ok := g.templates(w, r)
	if ok {
		writeJSON(w, 200, map[string]any{"code": 200, "data": list})
	}
}

func (g *gateway) generate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	input := generation{Count: 1}
	if err := decoder.Decode(&input); err != nil {
		writeError(w, 400, "INVALID_REQUEST", "需要有效的 JSON：template_code、params、count 和 idempotency_key")
		return
	}
	if decoder.Decode(new(any)) != io.EOF || input.TemplateCode == "" || input.Count < 1 || input.QuoteToken != "" {
		writeError(w, 400, "INVALID_REQUEST", "请求格式错误；便捷接口会自行获取 quote_token")
		return
	}
	if input.Params == nil {
		input.Params = make(map[string]json.RawMessage)
	}
	if r.URL.Path == "/v1/generations" {
		headerKey := r.Header.Get("Idempotency-Key")
		if headerKey != "" && input.IdempotencyKey != "" && headerKey != input.IdempotencyKey {
			writeError(w, 400, "INVALID_IDEMPOTENCY_KEY", "请求头与正文的幂等键不一致")
			return
		}
		if headerKey != "" {
			input.IdempotencyKey = headerKey
		}
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`).MatchString(input.IdempotencyKey) {
			writeError(w, 400, "IDEMPOTENCY_KEY_REQUIRED", "请先保存本次请求和 8–128 字符的 Idempotency-Key，再提交")
			return
		}
		w.Header().Set("Idempotency-Key", input.IdempotencyKey)
	}
	list, ok := g.templates(w, r)
	if !ok {
		return
	}
	var schema []parameter
	found := false
	for _, item := range list {
		var code string
		_ = json.Unmarshal(item["code"], &code)
		if code == input.TemplateCode {
			found = true
			if json.Unmarshal(item["param_schema"], &schema) != nil {
				writeError(w, 502, "UPSTREAM_INVALID_TEMPLATES", "模板参数格式不符")
				return
			}
			break
		}
	}
	if !found {
		writeError(w, 404, "WORKFLOW_NOT_FOUND", "未找到上架工作流，请刷新 /v1/workflows")
		return
	}
	estimateParams := make(map[string]json.RawMessage)
	for _, p := range schema {
		value, supplied := input.Params[p.Key]
		if !supplied || string(value) == "null" {
			if len(p.Default) > 0 && string(p.Default) != "null" {
				value = p.Default
				input.Params[p.Key] = value
			}
		}
		if !p.Locked && !p.Randomize && (p.Type == "select" || p.Type == "int" || p.Type == "float") && len(value) > 0 && string(value) != "null" && string(value) != `""` {
			estimateParams[p.Key] = value
		}
	}
	if r.URL.Path == "/v1/estimate" {
		data, ok := g.getData(w, r, "POST", "/aigc/estimate", map[string]any{"template_code": input.TemplateCode, "params": estimateParams, "count": input.Count})
		if ok {
			writeJSON(w, 200, map[string]any{"code": 200, "data": data})
		}
		return
	}
	key := input.IdempotencyKey
	input.IdempotencyKey = ""
	quote, ok := g.getData(w, r, "POST", "/aigc/quote", input)
	if !ok {
		return
	}
	var quoted struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(quote, &quoted) != nil || quoted.Token == "" {
		writeError(w, 502, "UPSTREAM_INVALID_QUOTE", "上游报价缺少 token，未提交任务")
		return
	}
	input.IdempotencyKey, input.QuoteToken = key, quoted.Token
	data, ok := g.getData(w, r, "POST", "/aigc/tasks", input)
	if ok {
		writeJSON(w, 200, map[string]any{"code": 200, "message": "任务已提交", "data": data})
	}
}
