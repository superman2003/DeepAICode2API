// Package store 负责网关状态的持久化与账号池调度。
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
)

// APIKey 是网关对外签发的调用密钥。
type APIKey struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Group     string `json:"group,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	Requests  int64  `json:"requests"`
}

// Settings 是运行期可调参数。
type Settings struct {
	ModelAliases   map[string]string `json:"model_aliases"`
	RequestTimeout int               `json:"request_timeout_sec"`
	MaxRetries     int               `json:"max_retries"`
	LogKeep        int               `json:"log_keep"`
	CORSOrigin     string            `json:"cors_origin"`
	AdminPassword  string            `json:"admin_password"`
	FallbackModel  string            `json:"fallback_model"`
}

// LogEntry 是一条请求日志（只记元信息，不落对话内容）。
type LogEntry struct {
	Time                int64   `json:"time"`
	Key                 string  `json:"key"`
	Account             string  `json:"account"`
	Model               string  `json:"model"`
	Endpoint            string  `json:"endpoint"`
	Stream              bool    `json:"stream"`
	Status              int     `json:"status"`
	LatencyMS           int64   `json:"latency_ms"`
	PromptLen           int     `json:"prompt_len"`
	OutputLen           int     `json:"output_len"`
	TokensIn            int     `json:"tokens_in"`
	TokensOut           int     `json:"tokens_out"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`
	Cost                float64 `json:"cost"`
	Err                 string  `json:"err,omitempty"`
}

// Stats 是累计统计。
type Stats struct {
	TotalRequests            int64   `json:"total_requests"`
	FailedRequests           int64   `json:"failed_requests"`
	TotalCharsOut            int64   `json:"total_chars_out"`
	TotalTokensIn            int64   `json:"total_tokens_in"`
	TotalTokensOut           int64   `json:"total_tokens_out"`
	TotalCacheReadTokens     int64   `json:"total_cache_read_tokens"`
	TotalCacheCreationTokens int64   `json:"total_cache_creation_tokens"`
	TotalCost                float64 `json:"total_cost"`
	StartedAt                int64   `json:"started_at"`
}

// State 是全部持久化状态。
type State struct {
	Accounts      []*deepai.Account `json:"accounts"`
	Keys          []*APIKey         `json:"keys"`
	Settings      Settings          `json:"settings"`
	Stats         Stats             `json:"stats"`
	Logs          []LogEntry        `json:"logs"`
	Models        []deepai.Model    `json:"models"`
	DefaultModel  string            `json:"default_model"`
	ModelsUpdated int64             `json:"models_updated"`
	MachineID     string            `json:"machine_id"`
}

// Store 是带锁的状态容器。
type Store struct {
	mu     sync.RWMutex
	path   string
	st     *State
	rotate int

	// masterKey 非空时状态文件整体加密（AES-256-GCM）。
	masterKey []byte
	// encrypted 记录上次读写时文件是否已加密（用于启动日志 / 控制台提示）。
	encrypted bool
	// lastSaved 是上次落盘的明文快照，用于跳过无变化的重复写盘。
	lastSaved []byte
}

// DefaultSettings 返回默认设置。
func DefaultSettings() Settings {
	return Settings{
		ModelAliases:   map[string]string{},
		RequestTimeout: 600,
		MaxRetries:     3,
		LogKeep:        500,
		// 默认仅放行本机来源；如需局域网访问，在控制台显式配置。
		CORSOrigin:    "",
		FallbackModel: "",
	}
}

// Open 载入（或初始化）状态文件。
//
// 主密钥来自 DEEPAI2API_MASTER_KEY：配置后写出的状态文件会被整体加密，
// 未配置时保持明文（并在启动日志中警告）。
func Open(path string) (*Store, error) {
	key, err := MasterKeyFromEnv()
	if err != nil {
		return nil, err
	}
	return OpenWithKey(path, key)
}

// OpenWithKey 用指定主密钥载入状态文件（key 为 nil 表示不加密）。
func OpenWithKey(path string, key []byte) (*Store, error) {
	s := &Store{path: path, st: &State{Settings: DefaultSettings()}, masterKey: key}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		plain, encrypted, uerr := unseal(key, raw)
		if uerr != nil {
			return nil, uerr
		}
		s.encrypted = encrypted
		if err := json.Unmarshal(plain, s.st); err != nil {
			return nil, fmt.Errorf("状态文件解析失败: %w", err)
		}
		// 记住原始内容，首次 Save 可直接跳过。
		s.lastSaved = append([]byte(nil), plain...)
		def := DefaultSettings()
		if s.st.Settings.ModelAliases == nil {
			s.st.Settings.ModelAliases = def.ModelAliases
		}
		if s.st.Settings.RequestTimeout == 0 {
			s.st.Settings.RequestTimeout = def.RequestTimeout
		}
		if s.st.Settings.MaxRetries == 0 {
			s.st.Settings.MaxRetries = def.MaxRetries
		}
		if s.st.Settings.LogKeep == 0 {
			s.st.Settings.LogKeep = def.LogKeep
		}
		if s.st.Settings.CORSOrigin == "*" {
			// 历史状态文件里的通配符不再生效，收紧为本机来源。
			s.st.Settings.CORSOrigin = ""
		}
		if s.st.Stats.StartedAt == 0 {
			s.st.Stats.StartedAt = time.Now().Unix()
		}
	case errors.Is(err, os.ErrNotExist):
		s.st.Stats.StartedAt = time.Now().Unix()
	default:
		return nil, err
	}
	return s, nil
}

// Path 返回状态文件路径。
func (s *Store) Path() string { return s.path }

// Save 原子写回状态文件（配置主密钥时整文件加密）。
//
// 内容与上次落盘一致时直接跳过：后台每 30s 调一次 Save，日志/统计没变化时
// 没必要反复重写整个状态文件。
func (s *Store) Save() error {
	s.mu.RLock()
	plain, err := json.MarshalIndent(s.st, "", "  ")
	key := s.masterKey
	last := s.lastSaved
	enc := s.encrypted
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	// 跳过无变化写盘；但如果刚配置了主密钥且磁盘上还是明文，
	// 必须再写一次以完成迁移。
	if bytes.Equal(plain, last) && (len(key) == 0 || enc) {
		return nil
	}

	raw := plain
	if len(key) > 0 {
		sealed, err := seal(key, plain)
		if err != nil {
			return err
		}
		raw = sealed
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}

	s.mu.Lock()
	s.lastSaved = append(s.lastSaved[:0], plain...)
	if len(key) > 0 {
		s.encrypted = true
	}
	s.mu.Unlock()
	return nil
}

// Encrypted 报告磁盘上的状态文件是否为加密格式。
//
// 注意：配置了主密钥但还没保存过时，磁盘上依然是明文，
// 此时本方法返回 false（用 MasterKeySet 判断是否已配置加密）。
func (s *Store) Encrypted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.encrypted
}

// MasterKeySet 报告是否已配置主密钥（配置后每次 Save 都会加密）。
func (s *Store) MasterKeySet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.masterKey) > 0
}

// --- 账号 ---

// Accounts 返回账号快照。
func (s *Store) Accounts() []*deepai.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*deepai.Account, len(s.st.Accounts))
	copy(out, s.st.Accounts)
	return out
}

// Account 按 ID 查找账号。
func (s *Store) Account(id string) *deepai.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// AccountCopy 返回账号的深拷贝快照（含 Theos），可安全地跨 goroutine 读取。
//
// Account() 返回的是号池里的活指针，适合"就地改字段"的管理操作；需要
// 在后台任务里读（比如延迟复探、监控）时应当用这个，否则会和写方裸竞争。
func (s *Store) AccountCopy(id string) *deepai.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		cp := *a
		if a.Theos != nil {
			theos := *a.Theos
			cp.Theos = &theos
		}
		return &cp
	}
	return nil
}

// UpsertAccount 按 API Key 指纹或邮箱去重后写入账号，返回是否为新增。
//
// 同一账号重新登录时 API Key 可能不变，但重新注册/补跑时邮箱相同、
// Key 可能已轮换；按邮箱二次匹配可以避免号池里出现同一账号两份。
func (s *Store) UpsertAccount(a *deepai.Account) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ex := range s.st.Accounts {
		if ex.KeyID() != a.KeyID() && !sameEmail(ex.Email, a.Email) {
			continue
		}
		a.ID = ex.ID
		// 保留运行期状态，避免导入覆盖掉故障计数与配额缓存。
		a.Failures = ex.Failures
		a.LastUsed = ex.LastUsed
		a.Status = ex.Status
		a.LastErr = ex.LastErr
		a.Theos = ex.Theos
		if a.Name == "" {
			a.Name = ex.Name
		}
		if a.Email == "" {
			a.Email = ex.Email
		}
		if a.Password == "" {
			a.Password = ex.Password
		}
		if a.Group == "" {
			a.Group = ex.Group
		}
		if a.Proxy == "" {
			a.Proxy = ex.Proxy
		}
		if a.Fingerprint == "" {
			a.Fingerprint = ex.Fingerprint
		}
		if a.MachineID == "" {
			a.MachineID = ex.MachineID
		}
		if a.UserAgent == "" {
			a.UserAgent = ex.UserAgent
		}
		if a.AcceptLanguage == "" {
			a.AcceptLanguage = ex.AcceptLanguage
		}
		if a.SecCHUA == "" {
			a.SecCHUA = ex.SecCHUA
		}
		if a.Platform == "" {
			a.Platform = ex.Platform
		}
		if a.RegisteredAt == 0 {
			a.RegisteredAt = ex.RegisteredAt
		}
		s.st.Accounts[i] = a
		return false
	}
	s.st.Accounts = append(s.st.Accounts, a)
	return true
}

func sameEmail(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

// RemoveAccount 删除账号。
func (s *Store) RemoveAccount(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.st.Accounts {
		if a.ID == id {
			s.st.Accounts = append(s.st.Accounts[:i], s.st.Accounts[i+1:]...)
			return true
		}
	}
	return false
}

// NextAccount 以轮询方式挑一个可用账号（全部分组）。
func (s *Store) NextAccount() *deepai.Account { return s.NextAccountGroup("") }

// NextAccountGroup 在指定分组内以轮询方式挑一个可用账号。
//
// group 为空表示全部分组。连续失败过多的账号会被跳过；全组不可用时
// 回退到失败最少的那个，保证不会硬阻塞请求。分组不存在时返回 nil，
// 由调用方给出“该分组没有账号”的明确错误。
//
// 调度分两轮：第一轮只考虑「额度未知或额度 > 0」的账号。上游按机器发放
// 每日免费额度，池里常有 balance=0 的账号（免费额度已耗尽），先打过去只会
// 被上游拒一次再换号；第二轮才在去重后的全集里选，保证不会硬阻塞。
func (s *Store) NextAccountGroup(group string) *deepai.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.st.Accounts)
	if n == 0 {
		return nil
	}
	group = strings.TrimSpace(group)
	eligible := func(a *deepai.Account) bool {
		if !a.Enabled {
			return false
		}
		return group == "" || strings.EqualFold(a.GroupName(), group)
	}

	if acct := s.pickLocked(eligible, true); acct != nil {
		return acct
	}
	return s.pickLocked(eligible, false)
}

// pickLocked 在 eligible 里按轮询选一个账号（调用方持锁）。
//
// needQuota 为 true 时跳过已知额度为 0 的账号。
func (s *Store) pickLocked(eligible func(*deepai.Account) bool, needQuota bool) *deepai.Account {
	n := len(s.st.Accounts)
	var best *deepai.Account
	bestIdx := -1
	for i := 0; i < n; i++ {
		idx := (s.rotate + i) % n
		a := s.st.Accounts[idx]
		if !eligible(a) {
			continue
		}
		if needQuota && !hasQuota(a) {
			continue
		}
		if best == nil || a.Failures < best.Failures {
			best = a
			bestIdx = idx
		}
		if a.Failures == 0 {
			s.rotate = (idx + 1) % n
			return a
		}
	}
	if bestIdx >= 0 {
		s.rotate = (bestIdx + 1) % n
	}
	return best
}

// hasQuota 报告账号是否可能还有可用额度。
//
// 保守为准：只有「探测过、启用、且余额能解析为 0」的账号才算耗尽，
// 其余（未探测 / 解析不了 / 已付费）都当作可能可用。
func hasQuota(a *deepai.Account) bool {
	if a == nil || a.Theos == nil || !a.Theos.Enabled {
		return true
	}
	bal := strings.TrimSpace(a.Theos.Balance)
	if bal == "" {
		return true
	}
	v, err := strconv.ParseFloat(bal, 64)
	if err != nil {
		return true
	}
	if v > 0 {
		return true
	}
	// 0 额度只在“刚探过”时才算数：新注册的账号在入池那一刻常常还是 0，
	// 几分钟后才会下发当日额度（见 README §6.10 实测）。把陈旧的 0 当作
	// 未知，避免账号被永久排除在调度之外。
	if a.Theos.CheckedAt <= 0 {
		return true
	}
	return time.Since(time.Unix(a.Theos.CheckedAt, 0)) > QuotaStaleAfter
}

// QuotaStaleAfter 是“0 额度”被判为陈旧、需要重探的时限。
const QuotaStaleAfter = 15 * time.Minute

// MarkAccountResult 记录一次账号调用结果。
func (s *Store) MarkAccountResult(id string, ok bool, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastUsed = time.Now().Unix()
		if ok {
			a.Failures = 0
			a.Status = "ok"
			a.LastErr = ""
			if note != "" {
				a.Note = note
			}
		} else {
			a.Failures++
			a.Status = "error"
			a.LastErr = note
		}
		return
	}
}

// SetAccountAllowance 缓存一次配额探测结果。
func (s *Store) SetAccountAllowance(id string, al *deepai.Allowance, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastUsed = time.Now().Unix()
		if err != nil {
			a.Status = "error"
			a.LastErr = err.Error()
			a.Failures++
			return
		}
		a.Theos = al
		a.Status = "ok"
		a.LastErr = ""
		a.Failures = 0
		return
	}
}

// --- 模型目录 ---

// Models 返回模型目录快照。
func (s *Store) Models() []deepai.Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]deepai.Model, len(s.st.Models))
	copy(out, s.st.Models)
	return out
}

// DefaultModel 返回当前默认模型。
func (s *Store) DefaultModel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.DefaultModel
}

// ModelsUpdated 返回目录更新时间。
func (s *Store) ModelsUpdated() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.ModelsUpdated
}

// SetModels 更新模型目录缓存。
func (s *Store) SetModels(models []deepai.Model, def string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Models = models
	if def != "" {
		s.st.DefaultModel = def
	} else if len(models) > 0 && s.st.DefaultModel == "" {
		s.st.DefaultModel = models[0].ID
	}
	s.st.ModelsUpdated = time.Now().Unix()
}

// SetMachineID 记录本机机器指纹。
func (s *Store) SetMachineID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.MachineID = id
}

// MachineID 返回已记录的机器指纹。
func (s *Store) MachineID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.MachineID
}

// --- 密钥 ---

// Keys 返回密钥快照。
func (s *Store) Keys() []*APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*APIKey, len(s.st.Keys))
	copy(out, s.st.Keys)
	return out
}

// AddKey 新增密钥（group 为空时归入 default）。
func (s *Store) AddKey(name, group string) *APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	group = strings.TrimSpace(group)
	if group == "" {
		group = "default"
	}
	k := &APIKey{
		Key:       "sk-" + deepai.RandomHex(24),
		Name:      name,
		Group:     group,
		Enabled:   true,
		CreatedAt: time.Now().Unix(),
	}
	s.st.Keys = append(s.st.Keys, k)
	return k
}

// KeyGroup 返回某个网关密钥绑定的号池分组（未知密钥返回空串）。
func (s *Store) KeyGroup(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.st.Keys {
		if k.Key == key {
			return k.Group
		}
	}
	return ""
}

// DeleteKey 删除密钥。
func (s *Store) DeleteKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.st.Keys {
		if k.Key == key {
			s.st.Keys = append(s.st.Keys[:i], s.st.Keys[i+1:]...)
			return true
		}
	}
	return false
}

// ToggleKey 启用/停用密钥。
func (s *Store) ToggleKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key {
			k.Enabled = !k.Enabled
			return true
		}
	}
	return false
}

// ValidKey 校验密钥并计数。
func (s *Store) ValidKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Key == key && k.Enabled {
			k.LastUsed = time.Now().Unix()
			k.Requests++
			return true
		}
	}
	return false
}

// HasKeys 报告是否已签发任何密钥。
func (s *Store) HasKeys() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.st.Keys) > 0
}

// --- 设置 / 统计 / 日志 ---

// Settings 返回设置快照。
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.st.Settings
	cp.ModelAliases = map[string]string{}
	for k, v := range s.st.Settings.ModelAliases {
		cp.ModelAliases[k] = v
	}
	return cp
}

// UpdateSettings 局部更新设置。
func (s *Store) UpdateSettings(fn func(*Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st.Settings)
}

// Stats 返回统计快照。
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Stats
}

// Logs 返回最近的日志。
func (s *Store) Logs(limit int) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.st.Logs)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]LogEntry, limit)
	copy(out, s.st.Logs[n-limit:])
	return out
}

// Record 记录一次请求结果并更新统计。
func (s *Store) Record(e LogEntry, outChars int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Stats.TotalRequests++
	if e.Status >= 400 || e.Err != "" {
		s.st.Stats.FailedRequests++
	}
	s.st.Stats.TotalCharsOut += int64(outChars)
	s.st.Stats.TotalTokensIn += int64(e.TokensIn)
	s.st.Stats.TotalCacheReadTokens += int64(e.CacheReadTokens)
	s.st.Stats.TotalCacheCreationTokens += int64(e.CacheCreationTokens)
	s.st.Stats.TotalTokensOut += int64(e.TokensOut)
	s.st.Stats.TotalCost += e.Cost
	s.st.Logs = append(s.st.Logs, e)
	if keep := s.st.Settings.LogKeep; keep > 0 && len(s.st.Logs) > keep {
		s.st.Logs = s.st.Logs[len(s.st.Logs)-keep:]
	}
}

// ClearLogs 清空日志。
func (s *Store) ClearLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Logs = nil
}

// ClearStats 重置统计。
func (s *Store) ClearStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Stats = Stats{StartedAt: time.Now().Unix()}
}
