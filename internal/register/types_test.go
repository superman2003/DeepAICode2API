package register

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMergeKeepsRegistrationMetadata(t *testing.T) {
	metadata := &Record{Email: "same@example.com", Status: StatusRegistered, MachineID: "original-machine", UserAgent: "original-agent", AcceptLanguage: "zh-CN", SecCHUA: "browser-hint", Platform: "Windows"}
	for _, status := range []string{StatusRegistered, StatusEnrolled} {
		merged := Merge(metadata, &Record{Email: metadata.Email, Status: status})
		if merged.MachineID != metadata.MachineID || merged.UserAgent != metadata.UserAgent || merged.AcceptLanguage != metadata.AcceptLanguage || merged.SecCHUA != metadata.SecCHUA || merged.Platform != metadata.Platform {
			t.Fatalf("metadata erased by %s update", status)
		}
	}
	missing := &Record{Email: metadata.Email, Status: metadata.Status}
	filled := Merge(missing, metadata)
	if filled.MachineID != metadata.MachineID || filled.UserAgent != metadata.UserAgent {
		t.Fatal("same-rank merge failed to fill missing registration metadata")
	}
	other := &Record{Email: metadata.Email, Status: metadata.Status, MachineID: "different-machine", UserAgent: "different-agent"}
	kept := Merge(metadata, other)
	if kept.MachineID != metadata.MachineID || kept.UserAgent != metadata.UserAgent {
		t.Fatal("progress merge replaced an existing identity")
	}
}

func TestStatusRankMonotonic(t *testing.T) {
	order := []string{
		"", StatusFailed, StatusRegistered, StatusVerified, StatusKeyed, StatusEnrolled,
	}
	for i := 1; i < len(order); i++ {
		if StatusRank(order[i]) < StatusRank(order[i-1]) {
			t.Fatalf("等级不单调：%q(%d) 低于 %q(%d)",
				order[i], StatusRank(order[i]), order[i-1], StatusRank(order[i-1]))
		}
	}
	if StatusRank(StatusFailed) != StatusRank("") {
		t.Fatal("空状态应与 failed 同级")
	}
	if StatusRank("unknown-status") != 0 {
		t.Fatalf("未知状态等级应为 0，实际 %d", StatusRank("unknown-status"))
	}
}

func TestRecordNormalize(t *testing.T) {
	r := &Record{Key: "  a@b.com  "}
	r.Normalize()
	if r.Email != "a@b.com" || r.Key != "a@b.com" {
		t.Fatalf("邮箱应被裁剪并回填 Key: %#v", r)
	}
	if r.Group != "default" {
		t.Fatalf("空分组应归一为 default，实际 %q", r.Group)
	}
	if r.CreatedAt == 0 {
		t.Fatal("CreatedAt 应被填充")
	}

	// Email 为空时用 Key 兜底。
	r2 := &Record{Key: "k@b.com"}
	r2.Normalize()
	if r2.Email != "k@b.com" {
		t.Fatalf("Email 应从 Key 回填，实际 %q", r2.Email)
	}
}

func TestRecordSetError(t *testing.T) {
	r := NewRecord("a@b.com")
	r.Status = StatusKeyed
	r.SetError("login", errors.New("boom"))
	if r.Status != StatusFailed {
		t.Fatalf("SetError 应把状态置为 failed，实际 %q", r.Status)
	}
	if r.Stage != "login" || r.Error != "boom" {
		t.Fatalf("阶段/错误未记录: %#v", r)
	}

	// nil 错误不应改变任何东西。
	before := *r
	r.SetError("x", nil)
	if r.Stage != before.Stage || r.Status != before.Status {
		t.Fatal("SetError(nil) 不应修改记录")
	}
}

func TestRecordWorthDelivering(t *testing.T) {
	cases := []struct {
		status string
		key    string
		want   bool
	}{
		{StatusEnrolled, "k", true},
		{StatusKeyed, "k", true},
		{StatusVerified, "k", true},
		{StatusEnrolled, "", false},    // 没 key 不交付
		{StatusRegistered, "k", false}, // 没验活不交付
		{StatusFailed, "k", false},
	}
	for _, c := range cases {
		r := &Record{Status: c.status, APIKey: c.key}
		if got := r.WorthDelivering(); got != c.want {
			t.Errorf("status=%s key=%q → %v，期望 %v", c.status, c.key, got, c.want)
		}
	}
}

func TestMergeNil(t *testing.T) {
	r := NewRecord("a@b.com")
	if got := Merge(nil, r); got != r {
		t.Fatal("old 为 nil 时应直接返回 new")
	}
	if got := Merge(r, nil); got != r {
		t.Fatal("new 为 nil 时应直接返回 old")
	}
	if Merge(nil, nil) != nil {
		t.Fatal("两边都为 nil 应返回 nil")
	}
}

func TestMergeUpgradeKeepsEarnedFields(t *testing.T) {
	old := &Record{
		Email: "a@b.com", Key: "a@b.com",
		Status: StatusRegistered, Password: "pw", SessionID: "sess", Proxy: "http://p:1",
	}
	next := &Record{
		Email: "a@b.com", Key: "a@b.com",
		Status: StatusKeyed, APIKey: "sk-1", AccountID: "acc-1",
	}
	out := Merge(old, next)

	if out.Status != StatusKeyed {
		t.Fatalf("升级后状态 = %q", out.Status)
	}
	if out.APIKey != "sk-1" || out.AccountID != "acc-1" {
		t.Fatalf("新记录字段丢失: %#v", out)
	}
	// 新记录没带密码/session/代理，必须从旧记录继承。
	if out.Password != "pw" || out.SessionID != "sess" || out.Proxy != "http://p:1" {
		t.Fatalf("已赚到的字段被空值覆盖: %#v", out)
	}
	if out.Group != "default" {
		t.Fatalf("合并后应 Normalize 分组，实际 %q", out.Group)
	}
}

func TestMergeDowngradeKeepsOldButRecordsReason(t *testing.T) {
	old := &Record{
		Email: "a@b.com", Key: "a@b.com",
		Status: StatusEnrolled, APIKey: "sk-keep", AccountID: "acc-keep",
	}
	next := &Record{
		Email: "a@b.com", Key: "a@b.com",
		Status: StatusFailed, Error: "proxy dead",
	}
	out := Merge(old, next)

	if out.Status != StatusEnrolled {
		t.Fatalf("降级记录不应覆盖成功状态，实际 %q", out.Status)
	}
	if out.APIKey != "sk-keep" || out.AccountID != "acc-keep" {
		t.Fatalf("成功字段不应丢失: %#v", out)
	}
	if out.LastError != "proxy dead" {
		t.Fatalf("应记录本次失败原因，实际 %q", out.LastError)
	}

	// 原记录不能被就地修改。
	if old.LastError != "" {
		t.Fatal("Merge 不应修改入参 old")
	}
}

func TestMergeSameLevelUnion(t *testing.T) {
	old := &Record{
		Email: "a@b.com", Key: "a@b.com", Status: StatusVerified,
		EmailVerified: true, TheosBalance: "1.0",
		Stages: map[string]string{"login": "ok"},
	}
	next := &Record{
		Email: "a@b.com", Key: "a@b.com", Status: StatusVerified,
		Username: "u1", Plan: "free",
		Stages: map[string]string{"verify": "ok"},
	}
	out := Merge(old, next)

	if !out.EmailVerified || out.TheosBalance != "1.0" {
		t.Fatalf("同级合并应保留旧字段: %#v", out)
	}
	if out.Username != "u1" || out.Plan != "free" {
		t.Fatalf("同级合并应吸收新字段: %#v", out)
	}
	if len(out.Stages) != 2 || out.Stages["login"] != "ok" || out.Stages["verify"] != "ok" {
		t.Fatalf("Stages 应是并集: %#v", out.Stages)
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	a := &Record{
		Email: "a@b.com", Key: "a@b.com", Status: StatusEnrolled,
		APIKey: "k", AccountID: "acc", Stages: map[string]string{"x": "1"},
	}
	b := &Record{
		Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		Password: "pw", Stages: map[string]string{"y": "2"},
	}
	once := Merge(a, b)
	twice := Merge(once, b)
	if once.Status != twice.Status || once.APIKey != twice.APIKey ||
		once.Password != twice.Password || len(once.Stages) != len(twice.Stages) {
		t.Fatalf("Merge 不幂等:\nonce=%#v\ntwice=%#v", once, twice)
	}
}

func TestMergeDoesNotAliasStageMaps(t *testing.T) {
	// 台账合并后若与入参共享 map，后续 Mark 会污染历史记录。
	old := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		Stages: map[string]string{"old": "1"}}
	next := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		Stages: map[string]string{"new": "2"}}

	out := Merge(old, next)
	out.Stages["mutated"] = "3"
	if _, ok := old.Stages["mutated"]; ok {
		t.Fatal("合并结果不应共享旧记录的 Stages map")
	}
	if _, ok := next.Stages["mutated"]; ok {
		t.Fatal("合并结果不应共享新记录的 Stages map")
	}
}

func TestRecordJSONRoundTrip(t *testing.T) {
	r := NewRecord("a@b.com")
	r.Status = StatusEnrolled
	r.APIKey = "sk-1"
	r.Mark("login", "ok", 0)
	r.Normalize()

	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	var back Record
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化: %v", err)
	}
	if back.Status != r.Status || back.APIKey != r.APIKey || back.Stages["login"] != "ok" {
		t.Fatalf("往返不一致:\n原=%#v\n新=%#v", r, back)
	}
	// 空字段应被 omitempty 省掉，避免台账膨胀。
	if json.Valid(raw) && containsField(raw, "theos_refresh_at") {
		t.Fatal("空的可选字段应被省略")
	}
}

func containsField(raw []byte, field string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[field]
	return ok
}

func TestRecordE2ESeconds(t *testing.T) {
	r := &Record{CreatedAt: 100, FinishedAt: 127.5}
	if got := r.E2ESeconds(); got != 27.5 {
		t.Fatalf("E2E = %v，期望 27.5", got)
	}
	// 未结束 / 时钟回拨 / 零值都返回 0，避免统计时出现负数。
	for _, bad := range []*Record{
		{CreatedAt: 100},
		{CreatedAt: 0, FinishedAt: 10},
		{CreatedAt: 200, FinishedAt: 100},
		nil,
	} {
		if got := bad.E2ESeconds(); got != 0 {
			t.Fatalf("%#v 的 E2E 应为 0，实际 %v", bad, got)
		}
	}
}

func TestMergeKeepsNewerFinishedAt(t *testing.T) {
	// 补跑会重新写 FinishedAt；合并后统计要拿到较新的结束时间。
	old := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		CreatedAt: 100, FinishedAt: 130}
	next := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		CreatedAt: 100, FinishedAt: 160}
	if got := Merge(old, next).FinishedAt; got != 160 {
		t.Fatalf("FinishedAt = %v，期望取较新的 160", got)
	}

	// 旧值更新时不能被退回去。
	earlier := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		CreatedAt: 100, FinishedAt: 120}
	if got := Merge(next, earlier).FinishedAt; got != 160 {
		t.Fatalf("FinishedAt 不应回退，实际 %v", got)
	}
}

func TestMergeDoesNotAliasTimingsAcrossAttempts(t *testing.T) {
	// 端到端统计依赖每个尝试的 Timings 不被合并污染。
	old := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		Timings: map[string]float64{"verify": 14.8}}
	next := &Record{Email: "a@b.com", Key: "a@b.com", Status: StatusKeyed,
		Timings: map[string]float64{"enroll": 2.0}}
	out := Merge(old, next)
	if out.Timings["verify"] != 14.8 || out.Timings["enroll"] != 2.0 {
		t.Fatalf("Timings 合并错误: %#v", out.Timings)
	}
	out.Timings["verify"] = 0
	if old.Timings["verify"] != 14.8 {
		t.Fatal("合并结果与入参共享 Timings map")
	}
}
