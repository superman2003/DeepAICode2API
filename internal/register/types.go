// Package register 实现 DeepAI 账号的“注册 → 登录 → 取 Session → 验活 → 入池”
// 端到端链路，并把每次尝试写入 JSONL 台账。
//
// 设计对齐 Jev-Register-Tool：
//   - stages：单账号的全部出网动作；
//   - runner：批量 / 并发 / 补跑，共享带锁台账；
//   - ledger：append-only JSONL，按邮箱并集合并，成功字段不可被失败覆盖；
//   - mail backend：可切换的收件后端（CF Temp Email / 静态域名 / 固定邮箱）。
package register

import (
	"strings"
	"time"
)

// 记录状态（等级见 rank）。
const (
	StatusFailed     = "failed"
	StatusRegistered = "registered" // 会话已建立
	StatusVerified   = "verified"   // 邮箱已验证（获得每日 Theos）
	StatusKeyed      = "keyed"      // API Key 已通过上游验活
	StatusEnrolled   = "enrolled"   // 已进入 2API 号池
)

// rank 只用于台账合并时的升级/降级判断。
var rank = map[string]int{
	"":               0,
	StatusFailed:     0,
	StatusRegistered: 4,
	StatusVerified:   5,
	StatusKeyed:      6,
	StatusEnrolled:   7,
}

// StatusRank 返回状态等级。
func StatusRank(status string) int { return rank[status] }

// Record 是一个账号在一次运行中的完整记录。
//
// 字段同时是台账行、交付物行和控制台进度行；密码 / Session 只在本地台账
// 与状态文件中保存（文件权限 0600）。
type Record struct {
	Key    string `json:"key"` // 唯一键：邮箱
	Email  string `json:"email"`
	Status string `json:"status"`
	Stage  string `json:"stage,omitempty"`
	Error  string `json:"error,omitempty"`
	// LastError 只在“本次比历史更差”时保留原因，避免覆盖历史成功记录。
	LastError string  `json:"last_error,omitempty"`
	CreatedAt float64 `json:"created_at"`
	// FinishedAt 是本次尝试结束的时间戳（秒，含小数）。
	// 与 CreatedAt 相减即为该账号端到端耗时，用于批量压测的速率统计。
	FinishedAt float64 `json:"finished_at,omitempty"`

	Stages  map[string]string  `json:"stages,omitempty"`
	Timings map[string]float64 `json:"timings,omitempty"`

	Password      string `json:"password,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
	AccountID     string `json:"account_id,omitempty"`
	UserPK        int64  `json:"user_pk,omitempty"`
	Username      string `json:"username,omitempty"`
	Plan          string `json:"plan,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	SessionID     string `json:"session_id,omitempty"`
	CSRFToken     string `json:"csrf_token,omitempty"`
	SessionKey    string `json:"session_key,omitempty"`

	TheosBalance   string `json:"theos_balance,omitempty"`
	TheosGranted   string `json:"theos_granted,omitempty"`
	TheosRefreshAt string `json:"theos_refresh_at,omitempty"`

	MailboxProvider string `json:"mailbox_provider,omitempty"`
	Proxy           string `json:"proxy,omitempty"`
	ProxyLabel      string `json:"proxy_label,omitempty"`
	Fingerprint     string `json:"fingerprint,omitempty"`
	// MachineID 是该账号专用的机器指纹（原始 UUID）。
	// 上游按它发放每日免费额度，必须一号一个。
	MachineID string `json:"machine_id,omitempty"`
	// 浏览器指纹快照（推理侧复用，保证注册与推理特征一致）。
	UserAgent      string `json:"user_agent,omitempty"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	SecCHUA        string `json:"sec_ch_ua,omitempty"`
	Platform       string `json:"platform,omitempty"`
	Group          string `json:"group,omitempty"`
	Source         string `json:"source,omitempty"`
}

// NewRecord 构造一条记录。
func NewRecord(email string) *Record {
	email = strings.TrimSpace(email)
	return &Record{
		Key:       email,
		Email:     email,
		Status:    StatusFailed,
		CreatedAt: float64(time.Now().UnixNano()) / 1e9,
		Stages:    map[string]string{},
		Timings:   map[string]float64{},
		Source:    "register",
	}
}

// Mark 记录一个阶段的耗时与结果。
func (r *Record) Mark(stage, result string, d time.Duration) {
	if r.Stages == nil {
		r.Stages = map[string]string{}
	}
	if r.Timings == nil {
		r.Timings = map[string]float64{}
	}
	r.Stage = stage
	r.Stages[stage] = result
	if d > 0 {
		r.Timings[stage] = d.Seconds()
	}
}

// SetError 记录致命错误。
func (r *Record) SetError(stage string, err error) {
	if err == nil {
		return
	}
	r.Stage = stage
	r.Status = StatusFailed
	r.Error = err.Error()
}

// Normalize 修正空键并规范化分组。
func (r *Record) Normalize() {
	r.Email = strings.TrimSpace(r.Email)
	if r.Email == "" {
		r.Email = strings.TrimSpace(r.Key)
	}
	r.Key = r.Email
	if strings.TrimSpace(r.Group) == "" {
		r.Group = "default"
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = float64(time.Now().UnixNano()) / 1e9
	}
	if r.Proxy != "" && r.ProxyLabel == "" {
		r.ProxyLabel = ProxyLabel(r.Proxy)
	}
}

// E2ESeconds 返回该账号本次尝试的端到端耗时（秒）。
//
// 未结束时返回 0；负值（时钟回拨 / 补跑复用了历史 CreatedAt）也返回 0。
func (r *Record) E2ESeconds() float64 {
	if r == nil || r.FinishedAt <= 0 || r.CreatedAt <= 0 {
		return 0
	}
	if d := r.FinishedAt - r.CreatedAt; d > 0 {
		return d
	}
	return 0
}

// WorthDelivering 报告是否值得写入交付物。
func (r *Record) WorthDelivering() bool {
	return r.APIKey != "" && (r.Status == StatusKeyed || r.Status == StatusEnrolled || r.Status == StatusVerified)
}

// Merge 合并同一邮箱的两条记录（台账的“并集合并”规则）。
//
//   - 新记录等级更高：新记录胜出，但保留旧记录里已赚到的字段；
//   - 同级：并集，新值胜出；
//   - 新记录等级更低：整体保留旧记录，只记录本次失败原因。
func Merge(old, new *Record) *Record {
	if old == nil {
		return new
	}
	if new == nil {
		return old
	}
	// 深拷贝入参：合并过程会就地写 Stages / Timings，浅拷贝会让调用方
	// 手里的历史记录与合并结果共享 map（后续 Mark 会静默污染台账）。
	old, new = cloneRecord(old), cloneRecord(new)
	ro, rn := StatusRank(old.Status), StatusRank(new.Status)
	if rn < ro {
		out := *old
		if e := strings.TrimSpace(new.Error); e != "" && e != strings.TrimSpace(old.Error) {
			out.LastError = e
		}
		return &out
	}

	out := *new
	if rn == ro {
		out = *old
		mergeRecord(&out, new)
	}
	// 已赚到的字段不允许被空值覆盖。
	if out.APIKey == "" {
		out.APIKey = old.APIKey
	}
	if out.AccountID == "" {
		out.AccountID = old.AccountID
	}
	if out.Password == "" {
		out.Password = old.Password
	}
	if out.SessionID == "" {
		out.SessionID = old.SessionID
	}
	if out.Email == "" {
		out.Email = old.Email
	}
	if out.UserPK == 0 {
		out.UserPK = old.UserPK
	}
	if out.EmailVerified == false {
		out.EmailVerified = old.EmailVerified
	}
	if out.Proxy == "" {
		out.Proxy = old.Proxy
	}
	if out.ProxyLabel == "" {
		out.ProxyLabel = old.ProxyLabel
	}
	if out.Fingerprint == "" {
		out.Fingerprint = old.Fingerprint
	}
	if out.MachineID == "" {
		out.MachineID = old.MachineID
	}
	if out.UserAgent == "" {
		out.UserAgent = old.UserAgent
	}
	if out.AcceptLanguage == "" {
		out.AcceptLanguage = old.AcceptLanguage
	}
	if out.SecCHUA == "" {
		out.SecCHUA = old.SecCHUA
	}
	if out.Platform == "" {
		out.Platform = old.Platform
	}
	if out.Stages == nil {
		out.Stages = map[string]string{}
	}
	for k, v := range old.Stages {
		if _, ok := out.Stages[k]; !ok {
			out.Stages[k] = v
		}
	}
	if out.Timings == nil {
		out.Timings = map[string]float64{}
	}
	for k, v := range old.Timings {
		if _, ok := out.Timings[k]; !ok {
			out.Timings[k] = v
		}
	}
	out.Normalize()
	return &out
}

// cloneRecord 复制一条记录及其 map 字段（不共享底层存储）。
func cloneRecord(r *Record) *Record {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Stages != nil {
		cp.Stages = make(map[string]string, len(r.Stages))
		for k, v := range r.Stages {
			cp.Stages[k] = v
		}
	}
	if r.Timings != nil {
		cp.Timings = make(map[string]float64, len(r.Timings))
		for k, v := range r.Timings {
			cp.Timings[k] = v
		}
	}
	return &cp
}

// mergeRecord 把 src 的非零字段补进 dst（同级并集）。
func mergeRecord(dst, src *Record) {
	if src.Email != "" {
		dst.Email = src.Email
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.Stage != "" {
		dst.Stage = src.Stage
	}
	if src.Error != "" {
		dst.Error = src.Error
	}
	if src.APIKey != "" {
		dst.APIKey = src.APIKey
	}
	if src.AccountID != "" {
		dst.AccountID = src.AccountID
	}
	if src.Password != "" {
		dst.Password = src.Password
	}
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.CSRFToken != "" {
		dst.CSRFToken = src.CSRFToken
	}
	if src.SessionKey != "" {
		dst.SessionKey = src.SessionKey
	}
	if src.UserPK != 0 {
		dst.UserPK = src.UserPK
	}
	if src.Username != "" {
		dst.Username = src.Username
	}
	if src.Plan != "" {
		dst.Plan = src.Plan
	}
	if src.EmailVerified {
		dst.EmailVerified = true
	}
	if src.TheosBalance != "" {
		dst.TheosBalance = src.TheosBalance
	}
	if src.TheosGranted != "" {
		dst.TheosGranted = src.TheosGranted
	}
	if src.TheosRefreshAt != "" {
		dst.TheosRefreshAt = src.TheosRefreshAt
	}
	// 完成时间取较新的一次，供端到端速率统计使用。
	if src.FinishedAt > dst.FinishedAt {
		dst.FinishedAt = src.FinishedAt
	}
	if src.MailboxProvider != "" {
		dst.MailboxProvider = src.MailboxProvider
	}
	if src.Proxy != "" {
		dst.Proxy = src.Proxy
	}
	if src.ProxyLabel != "" {
		dst.ProxyLabel = src.ProxyLabel
	}
	if src.Fingerprint != "" {
		dst.Fingerprint = src.Fingerprint
	}
	// Fill absent metadata without replacing the identity already recorded for
	// this account. Progress-only updates often omit these fields.
	if dst.MachineID == "" {
		dst.MachineID = src.MachineID
	}
	if dst.UserAgent == "" {
		dst.UserAgent = src.UserAgent
	}
	if dst.AcceptLanguage == "" {
		dst.AcceptLanguage = src.AcceptLanguage
	}
	if dst.SecCHUA == "" {
		dst.SecCHUA = src.SecCHUA
	}
	if dst.Platform == "" {
		dst.Platform = src.Platform
	}
	if src.Group != "" {
		dst.Group = src.Group
	}
	if dst.Stages == nil {
		dst.Stages = map[string]string{}
	}
	for k, v := range src.Stages {
		dst.Stages[k] = v
	}
	if dst.Timings == nil {
		dst.Timings = map[string]float64{}
	}
	for k, v := range src.Timings {
		dst.Timings[k] = v
	}
}
