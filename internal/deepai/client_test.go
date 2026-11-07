package deepai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 固定机器指纹，避免每个测试都去读 SMBIOS（Windows 下要起 powershell）。
// MachineID 带 sync.Once，必须在任何 Client 构造前设置。
func TestMain(m *testing.M) {
	os.Setenv("DEEPAI_MACHINE_ID", "11111111-2222-3333-4444-555555555555")
	os.Exit(m.Run())
}

func mustErr(t *testing.T, status int, body string) *HTTPError {
	t.Helper()
	return &HTTPError{Op: "上游推理", Status: status, Body: []byte(body)}
}

func TestHTTPErrorRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{400, false, "请求本身的问题"},
		{404, false, "模型不存在，重试无意义"},
		{422, false, "参数非法"},
		{401, true, "鉴权失败可换号"},
		{403, true, ""},
		{409, true, "容量拒绝"},
		{429, true, "限流"},
		{500, true, ""},
		{502, true, ""},
		{503, true, ""},
		{504, true, ""},
		{418, false, "未知 4xx 不重试"},
	}
	for _, c := range cases {
		if got := mustErr(t, c.status, "{}").Retryable(); got != c.want {
			t.Errorf("status=%d Retryable()=%v，期望 %v（%s）", c.status, got, c.want, c.why)
		}
	}
}

func TestHTTPErrorAuthFailed(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, `{}`, true},
		{403, `{"error":{"code":"auth_required"}}`, true},
		{403, `{"error":{"code":"invalid_api_key"}}`, true},
		{403, `{"error":{"code":"email_unverified"}}`, true},
		{403, `{"error":{"code":"session_busy"}}`, false},
		{500, `{}`, false},
	}
	for _, c := range cases {
		if got := mustErr(t, c.status, c.body).AuthFailed(); got != c.want {
			t.Errorf("status=%d body=%s AuthFailed()=%v，期望 %v", c.status, c.body, got, c.want)
		}
	}
}

func TestHTTPErrorCapacityWaiting(t *testing.T) {
	busy := mustErr(t, http.StatusConflict, `{"error":{"code":"session_busy"}}`)
	if !busy.CapacityWaiting() {
		t.Fatal("409 + session_busy 应判定为容量等待")
	}
	if !busy.Retryable() {
		t.Fatal("容量等待必须可重试")
	}

	conflict := mustErr(t, http.StatusConflict, `{"error":{"code":"duplicate"}}`)
	if conflict.CapacityWaiting() {
		t.Fatal("非 session_busy 的 409 不应判定为容量等待")
	}
	if got := mustErr(t, 429, `{"error":{"code":"session_busy"}}`).CapacityWaiting(); got {
		t.Fatal("非 409 即使 code=session_busy 也不算容量等待")
	}
}

func TestHTTPErrorCodeAndMessage(t *testing.T) {
	e := mustErr(t, 409, `{"error":{"message":"模型正忙","code":"session_busy"}}`)
	if e.Code() != "session_busy" {
		t.Fatalf("Code() = %q", e.Code())
	}
	if got := e.Error(); !strings.Contains(got, "模型正忙") {
		t.Fatalf("Error() 应优先使用上游 message，得到 %q", got)
	}

	if got := mustErr(t, 500, "not json").Code(); got != "" {
		t.Fatalf("非 JSON body 的 Code() 应为空，得到 %q", got)
	}
}

func TestHTTPErrorTruncatesLongBody(t *testing.T) {
	// 上游异常时会回吐 HTML / 堆栈，不能整段塞进错误串再写进日志。
	long := strings.Repeat("A", 1000)
	got := mustErr(t, 502, long).Error()
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("长 body 应被截断并以省略号结尾: %q", got)
	}
	if strings.Count(got, "A") > 301 {
		t.Fatalf("截断长度不合理：%d 个 A", strings.Count(got, "A"))
	}

	if got := mustErr(t, 500, "   ").Error(); !strings.Contains(got, "HTTP 500") {
		t.Fatalf("空 body 应回退到状态码: %q", got)
	}
}

func TestRetryAfter(t *testing.T) {
	resp := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}
	fallback := 7 * time.Second

	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", fallback},
		{"3", 3 * time.Second},
		{"1", time.Second},
		{"999", 30 * time.Second}, // 上限 30s，避免被上游拖死
		{"0", fallback},           // 非正数视为无效
		{"-5", fallback},          // 解析失败回退
		{"abc", fallback},         // 非数字回退
		{"Wed, 21 Oct", fallback}, // HTTP-date 不支持，回退
	}
	for _, c := range cases {
		if got := RetryAfter(resp(c.header), fallback); got != c.want {
			t.Errorf("Retry-After=%q → %v，期望 %v", c.header, got, c.want)
		}
	}
}

// newTestClient 构造指向本地假上游的客户端。
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClientWithFingerprint("", "test-ua/1.0")
	c.Origin = srv.URL
	c.Machine = "machine-fingerprint"
	return c, srv
}

func TestClientSendsRequiredHeaders(t *testing.T) {
	var got http.Header
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	acct := &Account{APIKey: "sk-upstream-key"}
	_, err := c.OpenModel(context.Background(), acct, PathChat, []byte(`{"model":"x"}`), "affinity-1")
	if err != nil {
		t.Fatalf("OpenModel: %v", err)
	}

	if gotPath != ModelBasePath+PathChat {
		t.Fatalf("请求路径 = %q，期望 %q", gotPath, ModelBasePath+PathChat)
	}
	if v := got.Get(HeaderKeyName); v != "sk-upstream-key" {
		t.Fatalf("%s = %q", HeaderKeyName, v)
	}
	if v := got.Get(HeaderLane); v != LaneVersion {
		t.Fatalf("%s = %q，期望 %q", HeaderLane, v, LaneVersion)
	}
	if v := got.Get(HeaderMachine); v != "machine-fingerprint" {
		t.Fatalf("%s = %q", HeaderMachine, v)
	}
	if v := got.Get(HeaderAffinity); v != "affinity-1" {
		t.Fatalf("%s = %q", HeaderAffinity, v)
	}
	if v := got.Get("User-Agent"); v != "test-ua/1.0" {
		t.Fatalf("User-Agent = %q", v)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Fatalf("Content-Type = %q", v)
	}
}

func TestClientEmptyAffinityOmitsHeader(t *testing.T) {
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	})
	if _, err := c.OpenModel(context.Background(), &Account{APIKey: "k"}, PathChat, []byte(`{}`), ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	if v, ok := got[HeaderAffinity]; ok {
		t.Fatalf("空 affinity 不应发送 %s，实际 %v", HeaderAffinity, v)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	// 上游对无效 Key 会 302 到登录页；跟随只会把鉴权失败伪装成 200。
	var finalHits int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			finalHits++
			_, _ = io.WriteString(w, "login page")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})

	resp, err := c.OpenModel(context.Background(), &Account{APIKey: "bad"}, PathChat, []byte(`{}`), "")
	if err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d，期望 302（不跟随）", resp.StatusCode)
	}
	if finalHits != 0 {
		t.Fatal("不应跟随重定向到登录页")
	}
}

func TestFetchModels(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelBasePath+PathModels {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"id":"m1","name":"M1"},{"id":"m2","name":"M2"}],"default":"m2"}`)
	})

	models, def, err := c.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "m1" {
		t.Fatalf("模型目录解析错误: %#v", models)
	}
	if def != "m2" {
		t.Fatalf("default = %q，期望 m2", def)
	}
	if len(models[0].Input) != 1 || models[0].Input[0] != "text" {
		t.Fatalf("缺失 input 应补为 [text]，实际 %#v", models[0].Input)
	}
}

func TestFetchModelsNormalizes(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// 无 default、有重复 ID、有空 ID。
		_, _ = io.WriteString(w, `{"models":[{"id":"a"},{"id":"a"},{"id":""},{"id":"b","input":["text","image"]}]}`)
	})
	models, def, err := c.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("去重后应为 2 个模型，实际 %d: %#v", len(models), models)
	}
	if def != "a" {
		t.Fatalf("无 default 时应回退到首个模型，实际 %q", def)
	}
	if len(models[1].Input) != 2 {
		t.Fatalf("已声明的 input 不应被覆盖: %#v", models[1].Input)
	}
}

func TestFetchModelsEmptyIsError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	if _, _, err := c.FetchModels(context.Background()); err == nil {
		t.Fatal("空模型目录应返回错误")
	}
}

func TestTheosParsesAllowance(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.50000","granted":"0.00000","lane_version":2}`)
	})
	al, err := c.Theos(context.Background(), &Account{APIKey: "k"})
	if err != nil {
		t.Fatalf("Theos: %v", err)
	}
	if !al.Enabled || al.Balance != "1.50000" || al.Granted != "0.00000" {
		t.Fatalf("配额解析错误: %#v", al)
	}
	if al.CheckedAt == 0 {
		t.Fatal("CheckedAt 应被填充")
	}
}

func TestTheosReturnsHTTPError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
	})
	_, err := c.Theos(context.Background(), &Account{APIKey: "bad"})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("错误类型 = %T，期望 *HTTPError", err)
	}
	if !httpErr.AuthFailed() {
		t.Fatal("应判定为鉴权失败")
	}
}

// asHTTPError 是 errors.As 的薄包装，避免测试文件重复 import errors 语义混淆。
func asHTTPError(err error, target **HTTPError) bool {
	e, ok := err.(*HTTPError)
	if ok {
		*target = e
	}
	return ok
}

func TestJSONBodyWithInstructionsIsVerbatim(t *testing.T) {
	var got []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	})
	body := []byte(`{"model":"x","messages":[{"role":"system","content":"Follow caller instructions."},{"role":"user","content":"你好"}]}`)
	if _, err := c.OpenModel(context.Background(), &Account{APIKey: "k"}, PathChat, body, ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("请求体被改写:\n got=%s\nwant=%s", got, body)
	}
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
}

// IsTransient 决定关键路径要不要退避重试，误判的代价是浪费一次重试
// 或把一个本该救回的账号判死。
func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"400 参数错误", mustErr(t, 400, `{"error":{"code":"invalid"}}`), false},
		{"401 鉴权失败", mustErr(t, 401, `{}`), false},
		{"403", mustErr(t, 403, `{}`), false},
		{"404", mustErr(t, 404, `{}`), false},
		{"429 限流", mustErr(t, 429, `{}`), true},
		{"408", mustErr(t, 408, `{}`), true},
		{"500", mustErr(t, 500, `{}`), true},
		{"503", mustErr(t, 503, `{}`), true},
		{"unexpected eof", errors.New(`Get "https://x": unexpected EOF`), true},
		{"no exit node", errors.New(`Get "https://x": No exit node`), true},
		{"tls handshake", errors.New("net/http: TLS handshake timeout"), true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"调用方取消", context.Canceled, false},
		{"超时", context.DeadlineExceeded, true},
		{"未知错误", errors.New("something else"), false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient=%v，期望 %v", c.name, got, c.want)
		}
	}
}
