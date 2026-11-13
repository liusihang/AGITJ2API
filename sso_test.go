package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testJWT(expires time.Time) string {
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d}`, time.Now().Unix(), expires.Unix())
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".test-signature"
}

type ssoTestServer struct {
	g           *gateway
	a           *ssoAuth
	mode        atomic.Int32
	discoveries atomic.Int32
	posts       atomic.Int32
	fresh       string
}

func setupSSO(t *testing.T) *ssoTestServer {
	t.Helper()
	f := &ssoTestServer{fresh: testJWT(time.Now().Add(24 * time.Hour))}
	var origin string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/sso/discover":
			f.discoveries.Add(1)
			fmt.Fprintf(w, `{"code":200,"data":{"redirect_url":%q}}`, origin+"/idp/oauth2/authorize")
		case "/idp/oauth2/authorize":
			cookie, err := r.Cookie("_idp_session")
			if err != nil || (cookie.Value != "test-school-session" && cookie.Value != "rotated-school-session" && cookie.Value != "new-school-session") || r.Header.Get("Authorization") != "" {
				t.Error("school cookie missing or Bearer leaked to IdP")
			}
			switch f.mode.Load() {
			case 1:
				fmt.Fprint(w, `<form><input type="password"></form>`)
				return
			case 3:
				http.Redirect(w, r, "https://unexpected.invalid/?token=private-oauth-value", 302)
				return
			case 4:
				http.Redirect(w, r, strings.Replace(origin, "https://", "http://", 1)+"/sso/callback?token="+f.fresh, 302)
				return
			case 5:
				http.SetCookie(w, &http.Cookie{Name: "_idp_session", Value: "rotated-school-session", Path: "/", Secure: true})
			}
			http.Redirect(w, r, "/idp/AuthnEngine", 302)
		case "/idp/AuthnEngine":
			http.Redirect(w, r, origin+"/sso/callback?token="+url.QueryEscape(f.fresh), 302)
		case "/api/v1/aigc/resource-quota":
			if _, err := r.Cookie("_idp_session"); err == nil {
				t.Error("school cookie leaked to AIGC")
			}
			if f.mode.Load() == 2 || r.Header.Get("Authorization") != "Bearer "+f.fresh {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"code":401}`)
				return
			}
			fmt.Fprint(w, `{"code":200,"data":{"gpu_quota":2}}`)
		case "/api/v1/aigc/tasks":
			if r.Method == "POST" {
				f.posts.Add(1)
			}
			if r.Header.Get("Authorization") != "Bearer "+f.fresh {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"code":401}`)
				return
			}
			fmt.Fprint(w, `{"code":200,"data":[]}`)
		default:
			t.Error("unexpected route", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	origin = server.URL
	dir := t.TempDir()
	tokenFile, cookieFile := filepath.Join(dir, "token"), filepath.Join(dir, "cookie")
	if err := os.WriteFile(tokenFile, []byte(testJWT(time.Now().Add(8*time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cookieFile, []byte("test-school-session"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := newGateway(testKey, tokenFile, origin)
	if err != nil {
		t.Fatal(err)
	}
	g.client.Transport = readRetryTransport{base: server.Client().Transport}
	g.proxy.Transport = g.client.Transport
	f.g = g
	f.a = newSSOAuth(g, cookieFile, 30*time.Minute)
	f.a.iam, _ = url.Parse(origin)
	g.auth = f.a
	return f
}

func TestSSORenewalValidatesAndPersistsCookieRotation(t *testing.T) {
	f := setupSSO(t)
	f.mode.Store(5)
	if err := f.a.refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	token, _, err := readToken(f.a.tokenFile)
	if err != nil || token != "Bearer "+f.fresh {
		t.Fatal("new token not saved", err)
	}
	cookie, _ := os.ReadFile(f.a.cookieFile)
	if strings.TrimSpace(string(cookie)) != "rotated-school-session" {
		t.Fatal("rotated cookie not saved")
	}
	for _, path := range []string{f.a.tokenFile, f.a.cookieFile} {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("credential permissions", info.Mode())
		}
	}
	status := f.a.status()
	if status["last_success_at"] == nil || status["last_error"] != "" {
		t.Fatal(status)
	}
}

func TestSSOMissingTokenBootstrapsOnRequest(t *testing.T) {
	f := setupSSO(t)
	_ = os.Remove(f.a.tokenFile)
	w := perform(f.g, "GET", "/v1/tasks", "")
	if w.Code != 200 || f.discoveries.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), f.discoveries.Load())
	}
}

func TestSSOConcurrentExpiredRequestsShareRenewal(t *testing.T) {
	f := setupSSO(t)
	_ = os.WriteFile(f.a.tokenFile, []byte(testJWT(time.Now().Add(-time.Hour))), 0600)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := perform(f.g, "GET", "/v1/tasks", ""); w.Code != 200 {
				t.Error(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if f.discoveries.Load() != 1 {
		t.Fatal("duplicate renewal", f.discoveries.Load())
	}
}

func TestSSOFailureKeepsValidTokenAndNewCookieRetries(t *testing.T) {
	f := setupSSO(t)
	f.mode.Store(1)
	old := testJWT(time.Now().Add(5 * time.Minute))
	_ = os.WriteFile(f.a.tokenFile, []byte(old), 0600)
	for i := 0; i < 2; i++ {
		token, _, err := f.a.token(context.Background())
		if err != nil || token != "Bearer "+old {
			t.Fatal("valid token lost", err)
		}
	}
	if f.discoveries.Load() != 1 || f.a.status()["login_required"] != true {
		t.Fatal("failure cooldown or login status", f.a.status())
	}
	f.mode.Store(0)
	_ = os.WriteFile(f.a.cookieFile, []byte("new-school-session"), 0600)
	token, _, err := f.a.token(context.Background())
	if err != nil || token != "Bearer "+f.fresh || f.discoveries.Load() != 2 {
		t.Fatal("import did not recover", err)
	}
}

func TestSSOExpiredSessionReturns503AndPreservesFile(t *testing.T) {
	f := setupSSO(t)
	f.mode.Store(1)
	old := testJWT(time.Now().Add(-time.Hour))
	_ = os.WriteFile(f.a.tokenFile, []byte(old), 0600)
	w := perform(f.g, "GET", "/v1/tasks", "")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(f.a.tokenFile)
	if string(after) != old {
		t.Fatal("old token file overwritten")
	}
	if f.a.status()["login_required"] != true {
		t.Fatal(f.a.status())
	}
}

func TestSSOUnauthorizedDoesNotReplayPost(t *testing.T) {
	f := setupSSO(t)
	w := perform(f.g, "POST", "/v1/tasks", `{"idempotency_key":"saved-key-123"}`)
	if w.Code != 401 || f.posts.Load() != 1 || f.discoveries.Load() != 0 {
		t.Fatal(w.Code, f.posts.Load())
	}
	status := perform(f.g, "GET", "/v1/status", "")
	if !strings.Contains(status.Body.String(), `"token_ready":false`) {
		t.Fatal("rejected token reported as ready", status.Body.String())
	}
	w = perform(f.g, "GET", "/v1/tasks", "")
	if w.Code != 200 || f.posts.Load() != 1 || f.discoveries.Load() != 1 {
		t.Fatal(w.Code, f.posts.Load(), f.discoveries.Load())
	}
}

func TestSSOInvalidCandidateOrRedirectKeepsToken(t *testing.T) {
	for _, mode := range []int32{2, 3, 4} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			f := setupSSO(t)
			f.mode.Store(mode)
			before, _ := os.ReadFile(f.a.tokenFile)
			err := f.a.refresh(context.Background(), true)
			if err == nil || strings.Contains(err.Error(), f.fresh) || strings.Contains(err.Error(), "private-oauth-value") {
				t.Fatal("unsafe or missing error", err)
			}
			after, _ := os.ReadFile(f.a.tokenFile)
			if string(before) != string(after) {
				t.Fatal("token overwritten")
			}
		})
	}
}

func TestSSOBackgroundRefreshRunsAndStops(t *testing.T) {
	f := setupSSO(t)
	f.a.interval = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.a.run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for f.discoveries.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal goroutine did not stop")
	}
	if f.discoveries.Load() < 2 {
		t.Fatal("background interval did not run")
	}
}

func TestSSOOriginAcceptsDefaultPortAndRejectsOtherOrigins(t *testing.T) {
	expected, _ := url.Parse("https://iam.tongji.edu.cn")
	for target, want := range map[string]bool{
		"https://iam.tongji.edu.cn/idp/":              true,
		"https://IAM.TONGJI.EDU.CN:443/idp/":          true,
		"https://iam.tongji.edu.cn:8443/idp/":         false,
		"http://iam.tongji.edu.cn/idp/":               false,
		"https://iam.tongji.edu.cn.evil.invalid/idp/": false,
		"https://user@iam.tongji.edu.cn/idp/":         false,
	} {
		u, _ := url.Parse(target)
		if sameSSOOrigin(u, expected) != want {
			t.Error(target, want)
		}
	}
}
