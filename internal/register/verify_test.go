package register

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyBackfillPublishesAndPersistsTimeout(t *testing.T) {
	ledger := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"))
	defer ledger.Close()
	var update *Record
	p := New(Options{Config: &Config{}, Ledger: ledger, OnUpdate: func(rec *Record) { update = rec }})
	rec := NewRecord("saved@example.com")
	rec.Status = StatusEnrolled
	rec.APIKey = "fake-key"
	rec.Mark("verify", "pending", 0)
	ch := make(chan verifyOutcome, 1)
	ch <- verifyOutcome{err: context.DeadlineExceeded, waited: time.Minute}
	p.verifyPending.Add(1)
	p.verifyWG.Add(1)
	p.verifyBackfill(rec, nil, ch)
	if update == nil || update.Stages["verify"] != "warning" || update.Status != StatusEnrolled || !strings.Contains(update.LastError, "邮箱验证未完成") {
		t.Fatalf("timeout left UI pending: %#v", update)
	}
	if rec.Stages["verify"] != "pending" {
		t.Fatal("background verification modified the original record")
	}
	records, err := ledger.Load()
	if err != nil || len(records) != 1 || records[0].Stages["verify"] != "warning" {
		t.Fatalf("timeout not persisted: %v %v", records, err)
	}
}

// HTTPEnroller 必须实现 VerifiedMarker，否则后台验证无法回填。
var _ VerifiedMarker = (*HTTPEnroller)(nil)

// applyVerify 的非网络分支：无收件箱 / 等待失败都要落成可读的阶段结果。
func TestApplyVerifyNonNetworkPaths(t *testing.T) {
	p := New(Options{})

	noInbox := &Record{Email: "a@b.com", Status: StatusKeyed}
	if ok := p.applyVerify(context.Background(), noInbox, nil, verifyOutcome{err: ErrNoInbox}); ok {
		t.Fatal("ErrNoInbox 不应判定为验证成功")
	}
	if got := noInbox.Stages["verify"]; got != "skipped (no inbox)" {
		t.Fatalf("阶段标记 = %q", got)
	}
	if noInbox.EmailVerified {
		t.Fatal("不应标记为已验证")
	}

	timeout := &Record{Email: "a@b.com", Status: StatusKeyed}
	if ok := p.applyVerify(context.Background(), timeout, nil, verifyOutcome{
		err: context.DeadlineExceeded, waited: 120 * time.Second,
	}); ok {
		t.Fatal("等待失败不应判定为验证成功")
	}
	if got := timeout.Stages["verify"]; got != "warning" {
		t.Fatalf("阶段标记 = %q", got)
	}
	if timeout.Timings["verify"] != 120 {
		t.Fatalf("应记录等待耗时，实际 %v", timeout.Timings["verify"])
	}
}

func TestMarkVerifiedSendsAction(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		auth = r.Header.Get("X-Admin-Password")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	e := NewHTTPEnroller(srv.URL, "pw")
	if err := e.MarkVerified(context.Background(), "acc-1", "a@b.com"); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if got["action"] != "mark-verified" {
		t.Fatalf("action = %v", got["action"])
	}
	if got["id"] != "acc-1" || got["email"] != "a@b.com" {
		t.Fatalf("payload = %#v", got)
	}
	if auth != "pw" {
		t.Fatalf("管理密码未带上：%q", auth)
	}
}

// 网关返回非 2xx 时要报错，让后台流程记日志。
func TestMarkVerifiedPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"no such account"}}`)
	}))
	defer srv.Close()

	if err := NewHTTPEnroller(srv.URL, "").MarkVerified(context.Background(), "x", "y"); err == nil {
		t.Fatal("404 应返回错误")
	}
}
