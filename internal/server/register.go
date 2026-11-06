package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/register"
)

func registrationMaxCount() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("DEEPAI2API_REGISTER_MAX_COUNT")))
	if err != nil || value < 1 {
		return 1000
	}
	if value > 10000 {
		return 10000
	}
	return value
}

// registerJob 是一次批量注册任务的运行状态。
type registerJob struct {
	ID          string
	StartedAt   int64
	FinishedAt  int64
	Count       int
	Concurrency int
	Group       string
	Backend     string
	Domain      string

	mu      sync.Mutex
	running bool
	lastErr string
	records map[string]*register.Record
	order   []string
	cancel  context.CancelFunc
}

type registerJobs struct {
	mu sync.Mutex
	m  map[string]*registerJob
}

func newRegisterJobs() *registerJobs { return &registerJobs{m: map[string]*registerJob{}} }

func (js *registerJobs) put(j *registerJob) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for id, old := range js.m {
		if time.Since(time.Unix(old.StartedAt, 0)) > 24*time.Hour && !old.isRunning() {
			delete(js.m, id)
		}
	}
	js.m[j.ID] = j
}

func (js *registerJobs) get(id string) *registerJob {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.m[id]
}

func (js *registerJobs) list() []*registerJob {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]*registerJob, 0, len(js.m))
	for _, j := range js.m {
		out = append(out, j)
	}
	return out
}

func (j *registerJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

func (j *registerJob) update(rec *register.Record) {
	if rec == nil {
		return
	}
	rec.Normalize()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.records == nil {
		j.records = map[string]*register.Record{}
	}
	if _, ok := j.records[rec.Email]; !ok {
		j.order = append(j.order, rec.Email)
	}
	j.records[rec.Email] = rec
}

func (j *registerJob) finish(records []*register.Record) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.FinishedAt = time.Now().Unix()
	for _, rec := range records {
		if rec == nil {
			continue
		}
		rec.Normalize()
		if j.records == nil {
			j.records = map[string]*register.Record{}
		}
		if _, ok := j.records[rec.Email]; !ok {
			j.order = append(j.order, rec.Email)
		}
		j.records[rec.Email] = rec
	}
}

func (j *registerJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	records := make([]any, 0, len(j.order))
	for _, email := range j.order {
		if rec := j.records[email]; rec != nil {
			records = append(records, sanitizeRegisterRecord(rec))
		}
	}
	return map[string]any{
		"id":          j.ID,
		"running":     j.running,
		"count":       j.Count,
		"concurrency": j.Concurrency,
		"done":        len(records),
		"group":       j.Group,
		"backend":     j.Backend,
		"domain":      j.Domain,
		"started_at":  j.StartedAt,
		"finished_at": j.FinishedAt,
		"error":       j.lastErr,
		"records":     records,
	}
}

// sanitizeRegisterRecord 下发给控制台的记录（绝不包含密码 / Session）。
func sanitizeRegisterRecord(rec *register.Record) map[string]any {
	return map[string]any{
		"email":            rec.Email,
		"status":           rec.Status,
		"stage":            rec.Stage,
		"error":            rec.Error,
		"last_error":       rec.LastError,
		"key":              deepai.MaskSecret(rec.APIKey),
		"account_id":       rec.AccountID,
		"user_pk":          rec.UserPK,
		"username":         rec.Username,
		"plan":             rec.Plan,
		"email_verified":   rec.EmailVerified,
		"theos_balance":    rec.TheosBalance,
		"theos_granted":    rec.TheosGranted,
		"group":            rec.Group,
		"mailbox_provider": rec.MailboxProvider,
		"proxy":            rec.ProxyLabel,
		"fingerprint":      rec.Fingerprint,
		"stages":           rec.Stages,
		"timings":          rec.Timings,
		"created_at":       rec.CreatedAt,
		"finished_at":      rec.FinishedAt,
	}
}

// handleAdminRegister 启动 / 查询批量注册任务。
func (s *Server) handleAdminRegister(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id != "" {
			job := s.Registers.get(id)
			if job == nil {
				writeErr(w, http.StatusNotFound, "任务不存在或已过期", "invalid_request_error")
				return
			}
			writeJSON(w, http.StatusOK, job.snapshot())
			return
		}
		jobs := s.Registers.list()
		out := make([]map[string]any, 0, len(jobs))
		for _, job := range jobs {
			out = append(out, job.snapshot())
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
	case http.MethodPost:
		s.startRegisterJob(w, r)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if job := s.Registers.get(id); job != nil && job.cancel != nil {
			job.cancel()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法", "invalid_request_error")
	}
}

// Mailbox credentials are request-scoped; never include them in job snapshots.
type mailboxConfigRequest struct {
	Domain         string `json:"domain"`
	MailBackend    string `json:"mail_backend"`
	QQIMAPHost     string `json:"qq_imap_host"`
	QQIMAPPort     int    `json:"qq_imap_port"`
	QQIMAPUser     string `json:"qq_imap_user"`
	QQIMAPAuthCode string `json:"qq_imap_auth_code"`
	QQIMAPFolder   string `json:"qq_imap_folder"`
	TempMailLOLKey string `json:"tempmail_lol_api_key"`
}

func (b mailboxConfigRequest) apply(cfg *register.Config) {
	if b.Domain != "" {
		cfg.DefaultDomain = strings.TrimSpace(b.Domain)
	}
	if b.MailBackend != "" {
		cfg.MailBackend = strings.ToLower(strings.TrimSpace(b.MailBackend))
	}
	if b.QQIMAPHost != "" {
		cfg.QQIMAPHost = strings.TrimSpace(b.QQIMAPHost)
	}
	if b.QQIMAPPort != 0 {
		cfg.QQIMAPPort = b.QQIMAPPort
	}
	if b.QQIMAPUser != "" {
		cfg.QQIMAPUser = strings.TrimSpace(b.QQIMAPUser)
	}
	if b.QQIMAPAuthCode != "" {
		cfg.QQIMAPAuthCode = strings.TrimSpace(b.QQIMAPAuthCode)
	}
	if b.QQIMAPFolder != "" {
		cfg.QQIMAPFolder = strings.TrimSpace(b.QQIMAPFolder)
	}
	if b.TempMailLOLKey != "" {
		cfg.TempMailLOLKey = strings.TrimSpace(b.TempMailLOLKey)
	}
	if strings.EqualFold(strings.TrimSpace(b.MailBackend), "tempmail_lol") {
		cfg.DefaultDomain = strings.TrimSpace(b.Domain) // Empty means random; do not inherit a QQ domain.
	}
}

func (s *Server) mailboxConfig(body mailboxConfigRequest) *register.Config {
	cfg := register.LoadConfig()
	body.apply(cfg)
	if cfg.TempMailLOLTokenDir == "" {
		cfg.TempMailLOLTokenDir = filepath.Join(filepath.Dir(s.Store.Path()), "exports", "tempmail-lol-inboxes")
	}
	return cfg
}

func (s *Server) handleAdminMailboxTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body mailboxConfigRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	cfg := s.mailboxConfig(body)
	if body.MailBackend == "" {
		cfg.MailBackend = "qqimap"
	}
	backend := cfg.AutoBackend()
	if missing := cfg.Validate(backend); len(missing) > 0 {
		writeErr(w, http.StatusBadRequest, "邮箱配置不完整："+strings.Join(missing, ", "), "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := register.CheckProvider(ctx, cfg, backend, cfg.DefaultDomain); err != nil {
		writeErr(w, http.StatusBadRequest, "邮箱连接测试失败："+err.Error(), "mailbox_error")
		return
	}
	message := "邮箱登录和收件文件夹检查通过；请确认域名邮件已转发到此邮箱。"
	if backend == "tempmail_lol" {
		message = "TempMail.lol 配置检查通过；开始注册时会创建邮箱并验证 API 权限。"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message})
}

func (s *Server) startRegisterJob(w http.ResponseWriter, r *http.Request) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.updating {
		writeErr(w, http.StatusServiceUnavailable, "程序正在升级，请完成后再开始注册", "updating")
		return
	}
	var body struct {
		mailboxConfigRequest
		Count         int    `json:"count"`
		Concurrency   int    `json:"concurrency"`
		Email         string `json:"email"`
		Password      string `json:"password"`
		Group         string `json:"group"`
		Verify        *bool  `json:"verify"`
		Proxy         string `json:"proxy"`
		ProxyPool     string `json:"proxy_pool"`
		NoFingerprint bool   `json:"no_fingerprint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	if body.Count <= 0 {
		body.Count = 1
	}
	if maximum := registrationMaxCount(); body.Count > maximum {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("单次最多注册 %d 个账号", maximum), "invalid_request_error")
		return
	}
	if body.Concurrency <= 0 {
		body.Concurrency = 1
	}
	if body.Concurrency > 5 {
		body.Concurrency = 5
	}
	if body.Concurrency > body.Count {
		body.Concurrency = body.Count
	}
	body.Email = strings.TrimSpace(body.Email)
	if body.Email != "" && body.Count > 1 {
		writeErr(w, http.StatusBadRequest, "指定邮箱时 count 必须为 1", "invalid_request_error")
		return
	}
	group := strings.TrimSpace(body.Group)
	if group == "" {
		group = "default"
	}

	cfg := s.mailboxConfig(body.mailboxConfigRequest)
	if body.Proxy != "" {
		cfg.Proxy = strings.TrimSpace(body.Proxy)
	}
	poolRaw := strings.TrimSpace(body.ProxyPool)
	if poolRaw == "" {
		poolRaw = cfg.ProxyPool
	}
	if poolRaw == "" && cfg.ProxyFile != "" {
		if raw, err := register.ReadProxyFile(cfg.ProxyFile); err == nil {
			poolRaw = raw
		}
	}
	pool, err := register.NewProxyPool(poolRaw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "代理池配置错误: "+err.Error(), "invalid_request_error")
		return
	}
	backend := cfg.AutoBackend()
	if missing := cfg.Validate(backend); len(missing) > 0 {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("邮箱后端 %s 缺少配置：%s（请在注册页面或 .env 中填写）", backend, strings.Join(missing, ", ")),
			"invalid_request_error")
		return
	}
	verify := true
	if body.Verify != nil {
		verify = *body.Verify
	}

	provider, err := register.NewProvider(cfg, backend, cfg.DefaultDomain, body.Email)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "初始化邮箱后端失败: "+err.Error(), "invalid_request_error")
		return
	}
	if backend == "qqimap" && verify {
		if err := provider.(register.HealthProvider).Health(r.Context()); err != nil {
			writeErr(w, http.StatusBadRequest, "注册前邮箱检查失败："+err.Error(), "mailbox_error")
			return
		}
	}

	jobID := deepai.RandomHex(8)
	ctx, cancel := context.WithCancel(context.Background())
	job := &registerJob{
		ID:          jobID,
		StartedAt:   time.Now().Unix(),
		Count:       body.Count,
		Concurrency: body.Concurrency,
		Group:       group,
		Backend:     backend,
		Domain:      cfg.DefaultDomain,
		running:     true,
		records:     map[string]*register.Record{},
		cancel:      cancel,
	}
	s.Registers.put(job)

	go s.runRegisterJob(ctx, job, cfg, provider, body.Email, body.Password, verify, pool, body.NoFingerprint)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": jobID})
}

func (s *Server) runRegisterJob(ctx context.Context, job *registerJob, cfg *register.Config, provider register.Provider, email, password string, verify bool, pool *register.ProxyPool, noFingerprint bool) {
	if capped, ok := pool.CapConcurrency(job.Concurrency); ok {
		log.Printf("[register] 并发 %d 超过出口数 %d，压到 %d", job.Concurrency, capped, capped)
		job.Concurrency = capped
	}

	baseDir := filepath.Dir(s.Store.Path())
	ledger := register.NewLedger(filepath.Join(baseDir, "exports", "register-ledger.jsonl"))
	// 缓冲写：任务结束必须刷盘并关闭句柄。
	defer func() { _ = ledger.Close() }()

	pipe := register.New(register.Options{
		Config:        cfg,
		Provider:      provider,
		Ledger:        ledger,
		Enroller:      localEnroller{s: s},
		Group:         job.Group,
		Verify:        verify,
		ProxyPool:     pool,
		NoFingerprint: noFingerprint,
		VerifyTimeout: cfg.VerifyWait,
		PollInterval:  cfg.PollEvery,
		MinInterval:   cfg.MinInterval,
		Log: func(format string, args ...any) {
			log.Printf("[register] "+format, args...)
		},
		OnUpdate: job.update,
	})

	log.Printf("[register] 任务 %s 启动：count=%d concurrency=%d group=%s backend=%s", job.ID, job.Count, job.Concurrency, job.Group, job.Backend)
	records := pipe.RunBatch(ctx, job.Count, job.Concurrency, email, password)
	job.finish(records)
	log.Printf("[register] 任务 %s 完成：%d 个记录", job.ID, len(records))

	if _, err := register.WriteDeliverables(filepath.Join(baseDir, "result"), records); err != nil {
		log.Printf("[register] 写交付物失败: %v", err)
	}
}

// localEnroller 是进程内入池器：既写入号池，也能回填验证状态。
//
// 实现 register.VerifiedMarker，因此后台验证完成后会自动回填 email_verified。
type localEnroller struct{ s *Server }

func (e localEnroller) Enroll(_ context.Context, rec *register.Record) (string, error) {
	return e.s.enrollRecord(rec)
}

func (e localEnroller) MarkVerified(_ context.Context, accountID, email string) error {
	return e.s.markAccountVerified(accountID, email)
}

// markAccountVerified 把账号标记为邮箱已验证（按 ID 或邮箱定位）。
func (s *Server) markAccountVerified(id, email string) error {
	var target *deepai.Account
	if strings.TrimSpace(id) != "" {
		target = s.Store.Account(strings.TrimSpace(id))
	}
	if target == nil && strings.TrimSpace(email) != "" {
		for _, a := range s.Store.Accounts() {
			if strings.EqualFold(a.Email, strings.TrimSpace(email)) {
				target = a
				break
			}
		}
	}
	if target == nil {
		return errors.New("账号不存在")
	}
	if target.EmailVerified {
		return nil // 幂等
	}
	target.EmailVerified = true
	return s.Store.Save()
}

// enrollRecord 把注册链路的结果写入号池（服务端内进程调用）。
func (s *Server) enrollRecord(rec *register.Record) (string, error) {
	if rec == nil {
		return "", errors.New("空记录")
	}
	reg := deepai.Registration{
		Email:          rec.Email,
		Password:       rec.Password,
		Username:       rec.Username,
		APIKey:         rec.APIKey,
		SessionID:      rec.SessionID,
		CSRFToken:      rec.CSRFToken,
		UserPK:         rec.UserPK,
		Plan:           rec.Plan,
		EmailVerified:  rec.EmailVerified,
		Group:          rec.Group,
		Proxy:          rec.Proxy,
		Fingerprint:    rec.Fingerprint,
		MachineID:      rec.MachineID,
		UserAgent:      rec.UserAgent,
		AcceptLanguage: rec.AcceptLanguage,
		SecCHUA:        rec.SecCHUA,
		Platform:       rec.Platform,
		Source:         "register",
	}
	id, _, err := s.enrollRegistration(reg, &deepai.Allowance{
		Enabled:   true,
		Balance:   rec.TheosBalance,
		Granted:   rec.TheosGranted,
		RefreshAt: rec.TheosRefreshAt,
		CheckedAt: time.Now().Unix(),
	})
	return id, err
}

// enrollRegistration 是入池的唯一写入口（管理接口与注册任务共用）。
func (s *Server) enrollRegistration(reg deepai.Registration, al *deepai.Allowance) (string, bool, error) {
	acct, err := deepai.AccountFromRegistration(reg)
	if err != nil {
		return "", false, err
	}
	now := time.Now().Unix()
	acct.Status = "ok"
	acct.LastUsed = now
	acct.LastVerified = now
	acct.Failures = 0
	acct.LastErr = ""
	if al != nil {
		acct.Theos = al
	}

	isNew := s.Store.UpsertAccount(acct)
	// Upsert 会保留旧运行态；把本次验证结果显式写回。
	if stored := s.Store.Account(acct.ID); stored != nil {
		stored.Status = "ok"
		stored.Failures = 0
		stored.LastErr = ""
		stored.LastUsed = now
		stored.LastVerified = now
		stored.EmailVerified = reg.EmailVerified
		stored.Plan = reg.Plan
		if al != nil {
			stored.Theos = al
		}
	}
	if err := s.Store.Save(); err != nil {
		return "", isNew, err
	}

	// 刚注册完的账号，上游往往还没把当日额度发下来：此时 /theos 会返回
	// balance=0，但几分钟后同一个 Key 再查就是 200。若不复探，这个 0 会被
	// 当成事实缓存下来，调度侧就会长期跳过该账号。
	if al == nil || !positiveQuota(al) {
		s.scheduleQuotaReprobe(acct.ID)
	}
	return acct.ID, isNew, nil
}

// positiveQuota 报告配额是否明确大于 0。
//
// 解析不了 / 字段缺失都按“未知”（返回 false）处理：宁可多探一次，
// 也不要把可用账号判死。
func positiveQuota(al *deepai.Allowance) bool {
	if al == nil || !al.Enabled {
		return false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(al.Balance), 64)
	return err == nil && v > 0
}

// quotaReprobeDelays 是入池后 0 额度的复探退避序列。
//
// 实测新账号在注册后 3~5 分钟内会拿到当日额度，所以从 3 分钟起探，
// 最多 3 次；拿到正额度即停止。
var quotaReprobeDelays = []time.Duration{3 * time.Minute, 6 * time.Minute, 12 * time.Minute}

// scheduleQuotaReprobe 异步复探某个账号的配额（仅当它仍是 0 时）。
//
// 每个账号一个 goroutine，次数有上限，不会自我放大。
func (s *Server) scheduleQuotaReprobe(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	delays := s.quotaReprobeDelays
	if len(delays) == 0 {
		return
	}
	go func() {
		for _, d := range delays {
			time.Sleep(d)
			a := s.Store.Account(id)
			if a == nil {
				return // 已被删除
			}
			if positiveQuota(a.Theos) {
				return // 期间被别的路径探到了
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			al, err := s.clientFor(a).Theos(ctx, a)
			cancel()
			s.Store.SetAccountAllowance(id, al, err)
			if err := s.Store.Save(); err != nil {
				log.Printf("[quota] 保存复探结果失败: %v", err)
			}
			if err == nil && positiveQuota(al) {
				log.Printf("[quota] %s 复探拿到额度: balance=%s granted=%s", a.Name, al.Balance, al.Granted)
				return
			}
		}
	}()
}
