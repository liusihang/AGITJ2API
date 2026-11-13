package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var errSSOLoginRequired = errors.New("学校 SSO 会话已失效，请正常登录后重新导入 _idp_session Cookie")

type ssoAuth struct {
	tokenFile, cookieFile string
	platform, iam         *url.URL
	transport             http.RoundTripper
	interval              time.Duration
	refreshMu             sync.Mutex // One upstream account; only renewals wait on this lock.
	mu                    sync.Mutex
	lastAttempt           *time.Time
	lastSuccess           *time.Time
	lastError             error
	lastCookie            string
	rejected              string
}

func newSSOAuth(g *gateway, cookieFile string, interval time.Duration) *ssoAuth {
	iam, _ := url.Parse("https://iam.tongji.edu.cn")
	return &ssoAuth{tokenFile: g.tokenFile, cookieFile: cookieFile, platform: g.upstream,
		iam: iam, transport: g.client.Transport, interval: interval}
}

func (a *ssoAuth) cookiePresent() bool {
	info, err := os.Stat(a.cookieFile)
	return a.cookieFile != "" && err == nil && !info.IsDir()
}

func (a *ssoAuth) status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"enabled": a.cookiePresent(), "refresh_interval_seconds": a.interval.Seconds(),
		"last_attempt_at": a.lastAttempt, "last_success_at": a.lastSuccess,
		"last_error": errorString(a.lastError), "login_required": errors.Is(a.lastError, errSSOLoginRequired),
	}
}

func (a *ssoAuth) reject(token string) {
	current, _, err := readToken(a.tokenFile)
	if err != nil || token != current {
		return
	} // Ignore late 401s from an older token.
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejected = token
}

func (a *ssoAuth) isRejected(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return token != "" && token == a.rejected
}

func sameSSOOrigin(u, expected *url.URL) bool {
	if u.User != nil || u.Scheme != "https" || expected.Scheme != "https" || !strings.EqualFold(u.Hostname(), expected.Hostname()) {
		return false
	}
	port, expectedPort := u.Port(), expected.Port()
	if port == "" {
		port = "443"
	}
	if expectedPort == "" {
		expectedPort = "443"
	}
	return port == expectedPort
}

func (a *ssoAuth) needsRefresh(token string, expires *time.Time, err error) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return err != nil || expires == nil || time.Until(*expires) <= time.Hour || (token != "" && token == a.rejected)
}

func (a *ssoAuth) token(ctx context.Context) (string, *time.Time, error) {
	token, expires, err := readToken(a.tokenFile)
	if !a.cookiePresent() || !a.needsRefresh(token, expires, err) {
		return token, expires, err
	}
	_ = a.refresh(ctx, false)
	token, expires, err = readToken(a.tokenFile)
	if err == nil && !a.isRejected(token) {
		// A failed early renewal does not interrupt a still-valid token.
		return token, expires, nil
	}
	return "", expires, errors.New("上游 token 不可用且 SSO 续登未成功，请查看 /v1/status 并更新学校会话")
}

func (a *ssoAuth) run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		if a.cookiePresent() {
			if err := a.refresh(ctx, true); err != nil && ctx.Err() == nil {
				log.Printf("SSO renewal: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *ssoAuth) refresh(ctx context.Context, force bool) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if !force {
		token, expires, err := readToken(a.tokenFile)
		if !a.needsRefresh(token, expires, err) {
			return nil
		}
	}
	value, err := os.ReadFile(a.cookieFile)
	if err != nil {
		err = errors.New("无法读取学校 SSO Cookie 文件，请检查路径和读取权限")
		a.mu.Lock()
		now := time.Now().UTC()
		a.lastAttempt, a.lastError = &now, err
		a.mu.Unlock()
		return err
	}
	cookie := strings.TrimSpace(string(value))
	a.mu.Lock()
	if cookie == a.lastCookie && a.lastError != nil && a.lastAttempt != nil && time.Since(*a.lastAttempt) < time.Minute {
		err := a.lastError
		a.mu.Unlock()
		return err
	}
	now := time.Now().UTC()
	a.lastAttempt, a.lastCookie = &now, cookie
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	token, replacementCookie, err := a.renew(ctx, cookie)
	if err == nil && replacementCookie != cookie {
		err = atomicWrite(a.cookieFile, replacementCookie+"\n")
		if err != nil {
			err = errors.New("无法保存更新后的学校 SSO 会话")
		}
	}
	if err == nil {
		if atomicWrite(a.tokenFile, token+"\n") != nil {
			err = errors.New("新 token 已验证，但无法写入 token 文件")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastError = err
	if err == nil {
		now := time.Now().UTC()
		a.lastSuccess, a.rejected, a.lastCookie = &now, "", replacementCookie
		log.Print("SSO renewal succeeded; upstream token updated")
	}
	return err
}

func (a *ssoAuth) renew(ctx context.Context, cookie string) (string, string, error) {
	initial := &http.Cookie{Name: "_idp_session", Value: cookie, Path: "/idp", Secure: true}
	if cookie == "" || initial.Valid() != nil {
		return "", "", errSSOLoginRequired
	}
	jar, _ := cookiejar.New(nil)
	iamPath := a.iam.ResolveReference(&url.URL{Path: "/idp/"})
	jar.SetCookies(iamPath, []*http.Cookie{initial})
	client := &http.Client{Transport: a.transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	platformClient := &http.Client{Transport: a.transport, CheckRedirect: client.CheckRedirect}
	replacementCookie := cookie
	get := func(target, auth string) (int, http.Header, []byte, error) {
		u, err := url.Parse(target)
		if err != nil || !(sameSSOOrigin(u, a.platform) || sameSSOOrigin(u, a.iam)) {
			if err == nil {
				log.Printf("SSO refused redirect origin: %s://%s", u.Scheme, u.Host)
			}
			return 0, nil, nil, errors.New("SSO 跳转到预期 HTTPS 站点之外，已停止")
		}
		req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
		if err != nil {
			return 0, nil, nil, errors.New("SSO 请求地址不合法")
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		requestClient := platformClient
		if sameSSOOrigin(u, a.iam) && strings.HasPrefix(u.Path, "/idp/") {
			requestClient = client
		}
		response, err := requestClient.Do(req)
		if err != nil {
			return 0, nil, nil, errors.New("SSO 网络请求失败，旧 token 保持不变")
		}
		defer response.Body.Close()
		if requestClient == client {
			for _, updated := range response.Cookies() {
				if updated.Name == "_idp_session" && updated.Value != "" && updated.MaxAge >= 0 {
					// Prefer the latest Set-Cookie even if its Path changed.
					replacementCookie = updated.Value
				}
			}
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil {
			return 0, nil, nil, errors.New("SSO 响应读取失败")
		}
		return response.StatusCode, response.Header, body, nil
	}
	status, _, body, err := get(a.platform.String()+"/api/v1/auth/sso/discover?domain=tongji", "")
	if err != nil {
		return "", "", err
	}
	var discovery struct {
		Code int `json:"code"`
		Data struct {
			RedirectURL string `json:"redirect_url"`
		} `json:"data"`
	}
	if status != 200 || json.Unmarshal(body, &discovery) != nil || discovery.Code != 200 || discovery.Data.RedirectURL == "" {
		return "", "", errors.New("SSO 发现接口未成功")
	}
	target, token := discovery.Data.RedirectURL, ""
	for hop := 0; hop < 12; hop++ {
		u, err := url.Parse(target)
		if err != nil {
			return "", "", errors.New("SSO 跳转地址不合法")
		}
		if sameSSOOrigin(u, a.platform) && u.Path == "/sso/callback" {
			token = u.Query().Get("token")
			break
		}
		status, headers, _, err := get(target, "")
		if err != nil {
			return "", "", err
		}
		if status == 200 || status == 401 || status == 403 {
			return "", "", errSSOLoginRequired
		}
		if status != 301 && status != 302 && status != 303 && status != 307 && status != 308 {
			return "", "", fmt.Errorf("SSO 返回异常状态 HTTP %d", status)
		}
		location, err := url.Parse(headers.Get("Location"))
		if err != nil || headers.Get("Location") == "" {
			return "", "", errors.New("SSO 缺少有效跳转地址")
		}
		target = u.ResolveReference(location).String()
	}
	_, expires, err := parseToken([]byte(token))
	if err != nil || expires == nil {
		return "", "", errors.New("SSO 未返回有效的新 JWT")
	}
	status, _, body, err = get(a.platform.String()+"/api/v1/aigc/resource-quota", "Bearer "+token)
	var validated envelope
	if err != nil || status != 200 || json.Unmarshal(body, &validated) != nil || validated.Code != 200 {
		return "", "", errors.New("新 token 未通过 AIGC 验证，旧 token 保持不变")
	}
	return token, replacementCookie, nil
}

func atomicWrite(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
