package register

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deepaicode2api/internal/deepai"
)

// Enroller 把一条记录写入 2API 号池，返回账号 ID。
type Enroller interface {
	Enroll(ctx context.Context, rec *Record) (string, error)
}

// VerifiedMarker 由入池器可选实现：验证邮件晚到时回填账号的 email_verified。
//
// 入池不再等待验证，因此验证结果可能在入池之后才拿到；实现了本接口的
// 入池器会被后台回填流程调用。
type VerifiedMarker interface {
	MarkVerified(ctx context.Context, accountID, email string) error
}

// EnrollerFunc 把普通函数适配成 Enroller。
type EnrollerFunc func(ctx context.Context, rec *Record) (string, error)

func (f EnrollerFunc) Enroll(ctx context.Context, rec *Record) (string, error) {
	return f(ctx, rec)
}

// Options 是 Pipeline 的运行参数。
type Options struct {
	Config    *Config
	Provider  Provider
	Enroller  Enroller
	Ledger    *Ledger
	Group     string
	Verify    bool
	ProbeChat bool

	// ProxyPool 为每个账号轮询分配代理；为空时使用 Config.Proxy。
	ProxyPool *ProxyPool
	// NoFingerprint 关闭浏览器指纹库（使用 Config.UserAgent / Language）。
	NoFingerprint bool

	VerifyTimeout time.Duration
	PollInterval  time.Duration
	MinInterval   time.Duration

	Log      func(format string, args ...any)
	OnUpdate func(rec *Record)
}

// Pipeline 实现单账号阶段与批量调度。
type Pipeline struct {
	opts Options

	mu        sync.Mutex
	lastStart time.Time

	// 后台验证的在途计数与完成信号（CLI 退出前要等一会儿）。
	verifyPending atomic.Int64
	verifyWG      sync.WaitGroup
}

// New 构造 Pipeline。
func New(opts Options) *Pipeline {
	if opts.Config == nil {
		opts.Config = LoadConfig()
	}
	if opts.VerifyTimeout <= 0 {
		opts.VerifyTimeout = opts.Config.VerifyWait
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = opts.Config.PollEvery
	}
	if opts.MinInterval < 0 {
		opts.MinInterval = opts.Config.MinInterval
	}
	if strings.TrimSpace(opts.Group) == "" {
		opts.Group = opts.Config.DefaultGroup
	}
	return &Pipeline{opts: opts}
}

// RunOne 执行一个账号的完整链路。
func (p *Pipeline) RunOne(ctx context.Context, email, password string) *Record {
	return p.runOne(ctx, email, password, "", "", "")
}

// runOne 允许补跑时复用历史代理、指纹与机器指纹。
func (p *Pipeline) runOne(ctx context.Context, email, password, proxyOverride, fpID, machineID string) *Record {
	rec := NewRecord(email)
	rec.Group = p.effectiveGroup()
	rec.Source = "register"
	if strings.TrimSpace(password) == "" {
		password = RandomPassword(18)
	}
	rec.Password = password

	// 代理：补跑优先复用历史代理；否则按池轮询；最后回落到固定代理。
	proxy := strings.TrimSpace(proxyOverride)
	if proxy == "" && p.opts.ProxyPool != nil {
		proxy = p.opts.ProxyPool.Next()
	}
	if proxy == "" {
		proxy = strings.TrimSpace(p.opts.Config.Proxy)
	}
	rec.Proxy = proxy
	rec.ProxyLabel = ProxyLabel(proxy)

	since := time.Now().Add(-10 * time.Second)

	// 1) 邮箱
	t := time.Now()
	mb, err := p.mailboxFor(ctx, email)
	if err != nil {
		rec.Mark("mailbox", "failed", time.Since(t))
		rec.SetError("mailbox", err)
		return p.finish(rec)
	}
	email = mb.Email()
	rec.Email, rec.Key = email, email
	rec.MailboxProvider = mb.Provider()
	rec.Mark("mailbox", "ok", time.Since(t))
	p.logf("[%s] 邮箱就绪（%s）", email, mb.Provider())

	// 指纹：补跑优先复用；否则按邮箱稳定分配，保证同一账号请求头一致。
	fp := Fingerprint{}
	if fpID == "off" {
		fp = Fingerprint{ID: "off", UserAgent: p.opts.Config.UserAgent, AcceptLanguage: "en-US,en;q=0.9", Language: p.opts.Config.Language}
	} else if !p.opts.NoFingerprint {
		fp = FingerprintByID(fpID)
		if fp.ID == "" {
			fp = AssignFingerprint(email)
		}
	}
	if fp.ID == "" {
		fp = Fingerprint{ID: "off", UserAgent: p.opts.Config.UserAgent, AcceptLanguage: "en-US,en;q=0.9", Language: p.opts.Config.Language}
	}
	rec.Fingerprint = fp.ID
	// 把指纹快照一并带上：推理侧要发同一套请求头。
	rec.UserAgent = fp.UserAgent
	rec.AcceptLanguage = fp.AcceptLanguage
	rec.SecCHUA = fp.SecCHUA
	rec.Platform = fp.Platform

	// 机器指纹：上游按 X-DeepAI-Code-Machine 每天发一次免费额度，
	// 所以必须一号一个；补跑时复用历史值，否则额度会重新“抽奖”。
	rec.MachineID = strings.TrimSpace(machineID)
	if rec.MachineID == "" {
		rec.MachineID = deepai.NewMachineUUID()
	}

	web := NewWebClientWithFingerprint(p.opts.Config, fp, proxy)

	// 2) 预检：邮箱是否已存在（失败不阻断注册）
	//
	// 刚新建的地址必然未注册，这一步纯属浪费一次网络往返（实测 3.3s）；
	// 只有补跑 / 用户指定地址时才需要真的查一下。
	exists := false
	if mb.Fresh() {
		rec.Mark("exists", "skipped (fresh)", 0)
	} else {
		t = time.Now()
		var existsErr error
		exists, existsErr = web.UserExists(ctx, email)
		if existsErr != nil {
			rec.Mark("exists", "warning", time.Since(t))
			p.logf("[%s] 预检失败，继续尝试注册：%v", email, existsErr)
		} else {
			rec.Mark("exists", boolText(exists), time.Since(t))
		}
	}

	// 3) 注册 / 登录
	if exists {
		t = time.Now()
		key, err := web.Login(ctx, email, password)
		if err != nil {
			rec.Mark("login", "failed", time.Since(t))
			rec.SetError("login", fmt.Errorf("邮箱已注册且登录失败: %w", err))
			return p.finish(rec)
		}
		rec.SessionKey = key
		rec.Mark("login", "ok", time.Since(t))
		p.logf("[%s] 已存在，登录成功", email)
	} else {
		_ = web.SaveSignupData(ctx)

		t = time.Now()
		key, err := web.Register(ctx, email, password)
		switch {
		case err == nil:
			rec.SessionKey = key
			rec.Mark("register", "ok", time.Since(t))
			p.logf("[%s] 注册成功", email)
		case isAlreadyRegistered(err):
			// 预检失败 / 邮箱规范化（例如 +tag）导致“看起来不存在”时，
			// 直接登录即可复用同一账号。
			rec.Mark("register", "exists", time.Since(t))
			p.logf("[%s] 邮箱已存在，转为登录", email)
		default:
			// 上游 504 / 超时无法确定账号到底建没建成：用登录兜底一次。
			// 建成了 → 继续（幂等）；没建成 → 登录失败，才判死。
			lk, lerr := web.Login(ctx, email, password)
			if lerr != nil {
				rec.Mark("register", "failed", time.Since(t))
				rec.SetError("register", err)
				return p.finish(rec)
			}
			rec.SessionKey = lk
			rec.Mark("register", "recovered-by-login", time.Since(t))
			p.logf("[%s] 注册接口异常但登录成功（账号已建），继续：%v", email, err)
		}

		t = time.Now()
		if loginKey, err := web.Login(ctx, email, password); err != nil {
			rec.Mark("login", "warning", time.Since(t))
			p.logf("[%s] 注册后登录校验失败（沿用注册会话）：%v", email, err)
		} else {
			rec.SessionKey = loginKey
			rec.Mark("login", "ok", time.Since(t))
		}
	}

	// 3.5) 验证邮件等待与后续阶段并行。
	//
	// 实测 verify 占单号 E2E 的 55%（14.8s / 27.0s），而它只是等邮件，
	// 后面的取 Session / Key / 验活 / 入池都不依赖验证结果（日志里
	// "取到 API Key" 早于 "邮箱验证完成"）。所以把等待丢到 goroutine 里，
	// 主流程继续跑，最后再 join。
	//
	// 只在需要验证且收件箱可用时启动；等待本身不碰 web 客户端，
	// 因此不存在与主流程的并发访问。
	var verifyCh chan verifyOutcome
	if p.opts.Verify {
		verifyCh = make(chan verifyOutcome, 1)
		go func() {
			vt := time.Now()
			link, verr := mb.WaitForVerification(ctx, since, p.opts.VerifyTimeout, p.opts.PollInterval)
			verifyCh <- verifyOutcome{link: link, err: verr, waited: time.Since(vt)}
		}()
	}

	// 4) 取 Session / 用户信息 / API Key
	t = time.Now()
	user, err := web.FetchUser(ctx)
	if err != nil {
		rec.Mark("session", "failed", time.Since(t))
		rec.SetError("session", err)
		return p.finish(rec)
	}
	rec.UserPK = user.PK
	rec.Username = user.Username
	rec.Plan = user.UserProfile2.Plan
	rec.APIKey = strings.TrimSpace(user.UserProfile.APIKey)
	rec.SessionID, rec.CSRFToken = web.Session()
	rec.Status = StatusRegistered
	rec.Mark("session", "ok", time.Since(t))
	if rec.APIKey == "" {
		rec.SetError("session", errors.New("会话未返回 API Key"))
		return p.finish(rec)
	}
	p.logf("[%s] 取到 API Key（%s，plan=%s）", email, deepai.MaskSecret(rec.APIKey), rec.Plan)

	// 5) 验活（与验证等待并行）
	//
	// validate 只看 API Key，不依赖邮箱验证，所以把它放到 join 之前，
	// 让 14s 级的收信等待被它遮住一部分。
	up := deepai.NewClientWithFingerprint(proxy, fp.UserAgent)
	t = time.Now()
	validateErr := p.validate(ctx, rec, up)
	validateElapsed := time.Since(t)

	// 6) 邮箱验证：只在「上游要求已验证」时才同步等待，其余情况移出关键路径
	//
	// 实测 verify 占 E2E 的 83.6% 且波动极大（14.8s → 56.1s），而上游并不因
	// 邮箱未验证就拒发额度（第五批 14 个超时账号最终全部拿到 200）。
	// 所以默认不在入池前等待，改为入池后由后台补验证并回填。
	if validateErr != nil && p.opts.Verify && verifyCh != nil && isUnverifiedError(validateErr) {
		out := <-verifyCh
		verifyCh = nil
		p.applyVerify(ctx, rec, web, out)
		t = time.Now()
		validateErr = p.validate(ctx, rec, up)
		validateElapsed += time.Since(t)
	}

	if validateErr != nil {
		rec.Mark("validate", "failed", validateElapsed)
		rec.SetError("validate", validateErr)
		return p.finish(rec)
	}
	rec.Status = StatusKeyed
	rec.Mark("validate", "ok", validateElapsed)
	p.logf("[%s] Key 验活通过（theos balance=%s granted=%s）", email, rec.TheosBalance, rec.TheosGranted)

	// 7) 入池
	if p.opts.Enroller == nil {
		rec.Mark("enroll", "skipped", 0)
	} else {
		t = time.Now()
		id, err := p.opts.Enroller.Enroll(ctx, rec)
		if err != nil {
			rec.Mark("enroll", "failed", time.Since(t))
			rec.Error = "入池失败: " + err.Error()
			p.logf("[%s] 入池失败（Key 已保留）：%v", email, err)
		} else {
			rec.AccountID = id
			rec.Status = StatusEnrolled
			rec.Mark("enroll", "ok", time.Since(t))
			p.logf("[%s] 已入池：%s", email, id)
		}
	}

	// 8) 验证结果后台回填：入池已经完成，不再等邮件
	if !p.opts.Verify {
		rec.Mark("verify", "skipped", 0)
	} else if verifyCh != nil {
		rec.Mark("verify", "pending", 0)
		// WaitGroup 的 Add 必须在 go 之前：写在 goroutine 里会与 Wait 竞争。
		p.verifyPending.Add(1)
		p.verifyWG.Add(1)
		go p.verifyBackfill(rec, web, verifyCh)
	}

	return p.finish(rec)
}

// PendingVerify 返回仍在后台等待验证的账号数。
func (p *Pipeline) PendingVerify() int { return int(p.verifyPending.Load()) }

// WaitVerify 最多等 d，让后台验证有机会跑完；返回是否全部完成。
//
// 验证已经不在关键路径上，但 CLI 是短命进程：不等一下的话，批次一结束
// 进程退出，后台 goroutine 全被带走，号池里的 email_verified 就不准了。
func (p *Pipeline) WaitVerify(d time.Duration) bool {
	if p == nil || p.PendingVerify() == 0 {
		return true
	}
	if d <= 0 {
		return false
	}
	done := make(chan struct{})
	go func() {
		p.verifyWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// applyVerify 处理一次验证结果（点链接 + 打点），返回是否验证成功。
func (p *Pipeline) applyVerify(ctx context.Context, rec *Record, web *WebClient, out verifyOutcome) bool {
	vt := time.Now()
	switch {
	case errors.Is(out.err, ErrNoInbox):
		rec.Mark("verify", "skipped (no inbox)", 0)
		p.logf("[%s] 邮箱后端无收件箱，跳过邮件验证", rec.Email)
	case out.err != nil:
		rec.Mark("verify", "warning", out.waited)
		rec.LastError = "邮箱验证未完成: " + out.err.Error()
		p.logf("[%s] 等待验证邮件失败：%v", rec.Email, out.err)
	default:
		if err := verifyEmailThrottled(ctx, web, out.link); err != nil {
			rec.Mark("verify", "warning", out.waited+time.Since(vt))
			rec.LastError = "验证链接访问失败: " + err.Error()
			p.logf("[%s] 验证链接访问失败：%v", rec.Email, err)
			return false
		}
		rec.EmailVerified = true
		if strings.HasPrefix(rec.LastError, "邮箱验证未完成:") || strings.HasPrefix(rec.LastError, "验证链接访问失败:") {
			rec.LastError = ""
		}
		if StatusRank(rec.Status) < StatusRank(StatusVerified) {
			rec.Status = StatusVerified
		}
		rec.Mark("verify", "ok", out.waited+time.Since(vt))
		p.logf("[%s] 邮箱验证完成", rec.Email)
		return true
	}
	return false
}

// verifyBackfill 在入池之后继续等验证邮件，成功后回填 email_verified。
//
// 用脱离调用方 ctx 的独立超时：批次的 ctx 在 RunBatch 返回时就会被取消，
// 而验证往往在这之后才完成。所有写入都作用在记录副本上，避免与
// finish()/notify() 的读取竞争。
func (p *Pipeline) verifyBackfill(rec *Record, web *WebClient, ch chan verifyOutcome) {
	cp := cloneRecord(rec)
	if cp == nil {
		return
	}
	defer func() {
		p.verifyPending.Add(-1)
		p.verifyWG.Done()
		if r := recover(); r != nil {
			p.logf("[%s] 后台验证异常：%v", cp.Email, r)
		}
	}()

	wait := p.opts.VerifyTimeout
	if wait <= 0 {
		wait = DefaultVerifyWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Minute)
	defer cancel()

	var out verifyOutcome
	select {
	case out = <-ch:
	case <-ctx.Done():
		return
	}
	verified := p.applyVerify(ctx, cp, web, out)
	if vm, ok := p.opts.Enroller.(VerifiedMarker); ok && verified {
		if err := vm.MarkVerified(ctx, cp.AccountID, cp.Email); err != nil {
			p.logf("[%s] 回填验证状态失败：%v", cp.Email, err)
		}
	}

	// Persist and publish both success and failure so the UI never stays pending after a timeout.
	if p.opts.Ledger != nil {
		_ = p.opts.Ledger.Append(cp)
	}
	p.notify(cp)
}

// theosRetryDelays 是 /theos 验活遇到瞬时错误时的退避序列。
//
// 实测批次里 4/50 的失败全是这一类：No exit node / TLS handshake timeout /
// unexpected EOF —— 全是出口抖动，重试一两次就能救回。
var theosRetryDelays = []time.Duration{time.Second, 3 * time.Second}

// theosWithRetry 查询配额，瞬时错误退避重试；鉴权/参数错误直接返回。
func theosWithRetry(ctx context.Context, up *deepai.Client, acct *deepai.Account) (*deepai.Allowance, error) {
	al, err := up.Theos(ctx, acct)
	if err == nil {
		return al, nil
	}
	if !deepai.IsTransient(err) {
		return nil, err
	}
	for i, delay := range theosRetryDelays {
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
		al, err = up.Theos(ctx, acct)
		if err == nil {
			return al, nil
		}
		if !deepai.IsTransient(err) {
			return nil, err
		}
		if i == len(theosRetryDelays)-1 {
			break
		}
	}
	return nil, err
}

// isUnverifiedError 报告错误是否因为“邮箱还没验证”。
func isUnverifiedError(err error) bool {
	var he *deepai.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.Code() == "email_unverified"
}

// verifyOutcome 是并行验证等待的结果。
type verifyOutcome struct {
	link   string
	err    error
	waited time.Duration
}

// validate 用 /theos 验证 API Key（可选再做一次最小对话探针）。
func (p *Pipeline) validate(ctx context.Context, rec *Record, up *deepai.Client) error {
	if strings.TrimSpace(rec.APIKey) == "" {
		return errors.New("缺少 API Key")
	}
	if up == nil {
		up = deepai.NewClientWithFingerprint(rec.Proxy, "")
	}
	vctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	acct := &deepai.Account{
		APIKey: rec.APIKey, Proxy: rec.Proxy, MachineID: rec.MachineID,
		UserAgent: rec.UserAgent, AcceptLanguage: rec.AcceptLanguage,
		SecCHUA: rec.SecCHUA, Platform: rec.Platform,
	}
	al, err := theosWithRetry(vctx, up, acct)
	if err != nil {
		return fmt.Errorf("Theos 验活失败: %w", err)
	}
	rec.TheosBalance = al.Balance
	rec.TheosGranted = al.Granted
	rec.TheosRefreshAt = al.RefreshAt

	if p.opts.ProbeChat {
		payload := map[string]any{
			"model":      firstNonEmpty(p.opts.Config.ProbeModel, DefaultProbeModel),
			"stream":     false,
			"max_tokens": 8,
			"messages": []any{
				map[string]any{"role": "user", "content": "Reply with exactly: OK"},
			},
		}
		body, _ := json.Marshal(payload)
		resp, err := up.OpenModel(vctx, acct, deepai.PathChat, body, "")
		if err != nil {
			return fmt.Errorf("对话验活失败: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("对话验活 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
		}
	}
	return nil
}

// RunBatch 并发跑 N 个账号。
func (p *Pipeline) RunBatch(ctx context.Context, count, concurrency int, email, password string) []*Record {
	if count <= 0 {
		count = 1
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > count {
		concurrency = count
	}
	results := make([]*Record, count)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				p.waitTurn(ctx)
				e, pw := "", ""
				if count == 1 {
					e, pw = email, password
				}
				results[i] = p.RunOne(ctx, e, pw)
			}
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return results
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// RunRecords 对已有台账记录补跑（幂等；已入池的直接跳过）。
func (p *Pipeline) RunRecords(ctx context.Context, records []*Record, concurrency int) []*Record {
	if concurrency <= 0 {
		concurrency = 1
	}
	results := make([]*Record, len(records))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				old := records[i]
				if old == nil {
					continue
				}
				if old.Status == StatusEnrolled && old.APIKey != "" {
					results[i] = old
					continue
				}
				p.waitTurn(ctx)
				var rec *Record
				if old.APIKey != "" && strings.TrimSpace(old.Password) == "" {
					rec = p.resumeWithKey(ctx, old)
				} else {
					rec = p.runOne(ctx, old.Email, old.Password, old.Proxy, old.Fingerprint, old.MachineID)
					rec = Merge(old, rec)
					_ = p.opts.Ledger.Append(rec)
					p.notify(rec)
				}
				results[i] = rec
			}
		}()
	}
	for i := range records {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return results
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// resumeWithKey 在只有 API Key 的情况下补验活 / 补入池。
func (p *Pipeline) resumeWithKey(ctx context.Context, old *Record) *Record {
	rec := *old
	rec.Group = firstNonEmpty(rec.Group, p.effectiveGroup())
	if strings.TrimSpace(rec.Proxy) == "" && p.opts.ProxyPool != nil {
		rec.Proxy = p.opts.ProxyPool.Next()
		rec.ProxyLabel = ProxyLabel(rec.Proxy)
	}
	t := time.Now()
	fp := FingerprintByID(rec.Fingerprint)
	up := deepai.NewClientWithFingerprint(rec.Proxy, fp.UserAgent)
	// 历史台账里的记录可能没有机器指纹（改动之前生成的），补一个。
	if strings.TrimSpace(rec.MachineID) == "" {
		rec.MachineID = deepai.NewMachineUUID()
	}
	if err := p.validate(ctx, &rec, up); err != nil {
		rec.Mark("validate", "failed", time.Since(t))
		rec.Error = err.Error()
		rec.Status = StatusFailed
		_ = p.opts.Ledger.Append(&rec)
		p.notify(&rec)
		return &rec
	}
	rec.Status = StatusKeyed
	rec.Mark("validate", "ok", time.Since(t))
	if p.opts.Enroller != nil {
		t = time.Now()
		id, err := p.opts.Enroller.Enroll(ctx, &rec)
		if err != nil {
			rec.Mark("enroll", "failed", time.Since(t))
			rec.Error = "入池失败: " + err.Error()
		} else {
			rec.AccountID = id
			rec.Status = StatusEnrolled
			rec.Mark("enroll", "ok", time.Since(t))
		}
	}
	if rec.FinishedAt == 0 {
		rec.FinishedAt = float64(time.Now().UnixNano()) / 1e9
	}
	merged := Merge(old, &rec)
	_ = p.opts.Ledger.Append(merged)
	p.notify(merged)
	return merged
}

// finish 归一化、写台账、回调并返回记录。
func (p *Pipeline) finish(rec *Record) *Record {
	rec.Normalize()
	if rec.FinishedAt == 0 {
		rec.FinishedAt = float64(time.Now().UnixNano()) / 1e9
	}
	if p.opts.Ledger != nil {
		if err := p.opts.Ledger.Append(rec); err != nil {
			p.logf("[%s] 写台账失败：%v", rec.Email, err)
		}
	}
	p.notify(rec)
	p.logf("[%s] 结果：%s%s", rec.Email, rec.Status, errSuffix(rec))
	return rec
}

func (p *Pipeline) notify(rec *Record) {
	if p.opts.OnUpdate != nil && rec != nil {
		p.opts.OnUpdate(rec)
	}
}

// mailboxFor 为指定邮箱准备收件箱；未指定邮箱时创建一个新的。
func (p *Pipeline) mailboxFor(ctx context.Context, email string) (Mailbox, error) {
	email = strings.TrimSpace(email)
	if email != "" {
		if ip, ok := p.opts.Provider.(InboxProvider); ok {
			if mb, ok := ip.Inbox(email); ok {
				return mb, nil
			}
		}
		name := "none"
		if p.opts.Provider != nil {
			name = p.opts.Provider.Name()
		}
		return &noInboxMailbox{email: email, provider: name}, nil
	}
	if p.opts.Provider == nil {
		return nil, errors.New("未配置邮箱后端且未指定邮箱")
	}
	return p.opts.Provider.Create(ctx)
}

func (p *Pipeline) effectiveGroup() string {
	g := strings.TrimSpace(p.opts.Group)
	if g == "" {
		g = strings.TrimSpace(p.opts.Config.DefaultGroup)
	}
	if g == "" {
		g = "default"
	}
	return g
}

// waitTurn 控制注册请求的最小间隔（默认 800ms）。
func (p *Pipeline) waitTurn(ctx context.Context) {
	if p.opts.MinInterval <= 0 {
		return
	}
	p.mu.Lock()
	wait := time.Until(p.lastStart.Add(p.opts.MinInterval))
	p.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	p.mu.Lock()
	p.lastStart = time.Now()
	p.mu.Unlock()
}

func (p *Pipeline) logf(format string, args ...any) {
	if p.opts.Log != nil {
		p.opts.Log(format, args...)
		return
	}
	log.Printf(format, args...)
}

func boolText(v bool) string {
	if v {
		return "exists"
	}
	return "new"
}

func errSuffix(rec *Record) string {
	if strings.TrimSpace(rec.Error) == "" {
		return ""
	}
	return "（" + rec.Error + "）"
}

// RandomPassword 生成满足常见强度要求的随机密码。
func RandomPassword(n int) string {
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const lower = "abcdefghijkmnopqrstuvwxyz"
	const digits = "23456789"
	const symbols = "!@#$%^&*"
	all := upper + lower + digits + symbols
	if n < 8 {
		n = 8
	}
	buf := make([]byte, 0, n)
	pick := func(set string) byte {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return set[0]
		}
		return set[i.Int64()]
	}
	buf = append(buf, pick(upper), pick(lower), pick(digits), pick(symbols))
	for len(buf) < n {
		buf = append(buf, pick(all))
	}
	// 洗牌
	for i := len(buf) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			break
		}
		buf[i], buf[j.Int64()] = buf[j.Int64()], buf[i]
	}
	return string(buf)
}

// isAlreadyRegistered 判断注册错误是否表示邮箱已存在。
func isAlreadyRegistered(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already has an account") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already registered")
}
