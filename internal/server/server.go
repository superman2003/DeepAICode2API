// Package server 暴露 OpenAI 兼容的 HTTP 接口。
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
	"deepaicode2api/internal/updater"
)

// Server 是网关 HTTP 服务。
type Server struct {
	Store         *store.Store
	Version       string
	Updater       *updater.Manager
	maintenanceMu sync.Mutex
	updating      bool
	Up            *deepai.Client
	// Login 保存进行中的浏览器登录会话（内存态）。
	Login *loginSessions
	// Registers 保存批量注册任务（内存态）。
	Registers *registerJobs

	refreshMu       sync.Mutex
	proxyMu         sync.Mutex
	proxyClients    map[string]*deepai.Client
	sessionMu       sync.Mutex
	sessionAccounts map[string]sessionAccountBinding

	// quotaReprobeDelays 是入池后 0 额度的复探退避序列（测试可注入短值）。
	quotaReprobeDelays []time.Duration
}

// New 构造服务。
func New(st *store.Store) *Server {
	return &Server{
		Store: st, Up: deepai.NewClient(), Login: newLoginSessions(),
		Registers: newRegisterJobs(), proxyClients: map[string]*deepai.Client{}, sessionAccounts: map[string]sessionAccountBinding{},
		quotaReprobeDelays: quotaReprobeDelays,
	}
}

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 探活
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)

	// 推理（OpenAI 兼容）
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))

	// Anthropic 兼容
	mux.HandleFunc("/v1/messages", s.withAuth(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.withAuth(s.handleAnthropicCountTokens))

	// 音频
	mux.HandleFunc("/v1/audio/transcriptions", s.withAuth(s.handleTranscriptions))
	mux.HandleFunc("/v1/audio/translations", s.withAuth(s.handleTranscriptions))

	// DeepAI 原生能力扩展
	mux.HandleFunc("/v1/deepai/web_search", s.withAuth(s.handleWebSearch))
	mux.HandleFunc("/v1/deepai/find_service", s.withAuth(s.handleFindService))
	mux.HandleFunc("/v1/deepai/theos", s.withAuth(s.handleTheos))
	mux.HandleFunc("/v1/deepai/usage", s.withAuth(s.handleUsage))
	// 兼容简写
	mux.HandleFunc("/v1/web_search", s.withAuth(s.handleWebSearch))
	mux.HandleFunc("/v1/theos", s.withAuth(s.handleTheos))
	mux.HandleFunc("/v1/usage", s.withAuth(s.handleUsage))

	// 控制台 API（统一走管理鉴权 + 同源校验）
	mux.HandleFunc("/admin/api/state", s.withAdminAuth(s.handleAdminState))
	mux.HandleFunc("/admin/api/accounts", s.withAdminAuth(s.handleAdminAccounts))
	mux.HandleFunc("/admin/api/keys", s.withAdminAuth(s.handleAdminKeys))
	mux.HandleFunc("/admin/api/logs", s.withAdminAuth(s.handleAdminLogs))
	mux.HandleFunc("/admin/api/stats", s.withAdminAuth(s.handleAdminStats))
	mux.HandleFunc("/admin/api/settings", s.withAdminAuth(s.handleAdminSettings))
	mux.HandleFunc("/admin/api/checkin", s.withAdminAuth(s.handleAdminCheckin))
	mux.HandleFunc("/admin/api/models", s.withAdminAuth(s.handleAdminModels))
	mux.HandleFunc("/admin/api/login/start", s.withAdminAuth(s.handleLoginStart))
	mux.HandleFunc("/admin/api/login/poll", s.withAdminAuth(s.handleLoginPoll))
	mux.HandleFunc("/admin/api/register", s.withAdminAuth(s.handleAdminRegister))
	mux.HandleFunc("/admin/api/register/mailbox-test", s.withAdminAuth(s.handleAdminMailboxTest))
	mux.HandleFunc("/admin/api/update", s.withAdminAuth(s.handleAdminUpdate))
	// 浏览器登录跳转：session id 是一次性随机数，服务端只跳到固定的上游登录页。
	mux.HandleFunc("/login/open", s.handleLoginOpen)

	mux.HandleFunc("/", s.handleRoot)

	return s.withCORS(mux)
}

// RefreshModels 从上游拉取模型目录并落盘。
func (s *Server) RefreshModels(ctx context.Context) ([]deepai.Model, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, def, err := s.Up.FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	s.Store.SetModels(models, def)
	if err := s.Store.Save(); err != nil {
		log.Printf("保存模型目录失败: %v", err)
	}
	return models, nil
}

// withCORS 处理跨域。
//
// 默认只放行本机来源（localhost / 127.0.0.1 / [::1]，任意端口）；
// 需局域网访问时在设置里显式列出逗号分隔的来源。不提供通配符。
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.corsOrigin(r.Header.Get("Origin"))
		writeCORSHeaders(w, allowed)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeCORSHeaders 仅在来源通过校验时写入跨域响应头。
func writeCORSHeaders(w http.ResponseWriter, allowed string) {
	if allowed == "" || hasWildcard(allowed) {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", allowed)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-beta, X-Admin-Password, X-Session-Affinity, session_id, x-codex-session-id, x-session-id")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
}

// corsOrigin 返回应当回给浏览器的 Allow-Origin 值，空串表示不放行。
func (s *Server) corsOrigin(raw string) string {
	if raw == "" || raw == "null" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}

	// 1) 显式允许列表（逗号分隔，值来自控制台设置）
	if cfg := strings.TrimSpace(s.Store.Settings().CORSOrigin); cfg != "" {
		for _, entry := range strings.Split(cfg, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !hasWildcard(entry) && strings.EqualFold(entry, raw) {
				return entry
			}
		}
		return ""
	}

	// 2) 默认仅本机：主机名必须在环回集合内
	if !isLoopbackHost(u.Hostname()) {
		return ""
	}
	if port := u.Port(); port != "" {
		return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
	}
	return u.Scheme + "://" + u.Hostname()
}

func hasWildcard(s string) bool { return strings.IndexByte(s, '*') >= 0 }

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// withAuth 校验 Bearer 密钥。
//
// 尚未签发任何密钥时：
//   - 来自本机回环的请求放行（首次使用的便利，且便于控制台自举）；
//   - 来自非回环来源的请求一律 503 setup_required。
//     否则在监听 0.0.0.0（容器 / 局域网）时，从首次启动到创建第一个密钥之间
//     网关就是一个无需鉴权的开放中继。
//
// 一旦签发了密钥，所有来源都强制校验。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Store.HasKeys() {
			if isLoopbackAddr(r.RemoteAddr) {
				next(w, r)
				return
			}
			writeErr(w, http.StatusServiceUnavailable,
				"网关尚未签发 API 密钥，请先在本机控制台创建密钥（/admin）", "setup_required")
			return
		}
		key := extractKey(r)
		if key == "" {
			writeErr(w, http.StatusUnauthorized, "缺少 API 密钥", "invalid_request_error")
			return
		}
		if !s.Store.ValidKey(key) {
			writeErr(w, http.StatusUnauthorized, "API 密钥无效或已停用", "invalid_request_error")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), clientKeyContext{}, deepai.KeyFingerprint(key)))
		next(w, r)
	}
}

// withAdminAuth 保护控制台接口。
//
// 鉴权策略：
//   - 已设置管理密码：必须通过 X-Admin-Password 或 Authorization: Bearer 提供；
//   - 未设置管理密码：仅允许来自本机环回的请求（避免局域网内裸奔）。
//
// 另外对写操作（非 GET/HEAD/OPTIONS）做同源校验，阻断跨站请求伪造。
func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pw := s.Store.Settings().AdminPassword; pw != "" {
			if !secureEqual(adminPasswordFrom(r), pw) {
				writeErr(w, http.StatusUnauthorized, "管理密码缺失或错误", "invalid_request_error")
				return
			}
		} else if !isLoopbackAddr(r.RemoteAddr) {
			writeErr(w, http.StatusForbidden, "未设置管理密码，管理接口仅允许本机访问", "invalid_request_error")
			return
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.adminSameOrigin(r) {
				writeErr(w, http.StatusForbidden, "跨站请求被拒绝", "invalid_request_error")
				return
			}
		}
		next(w, r)
	}
}

func adminPasswordFrom(r *http.Request) string {
	if v := r.Header.Get("X-Admin-Password"); v != "" {
		return v
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	return ""
}

func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) adminSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if cfg := strings.TrimSpace(s.Store.Settings().CORSOrigin); cfg != "" {
		for _, entry := range strings.Split(cfg, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !hasWildcard(entry) && strings.EqualFold(entry, origin) {
				return true
			}
		}
	}
	return false
}

func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sanitizedSettings 返回可下发给前端的设置（管理密码不外泄）。
func sanitizedSettings(st store.Settings) map[string]any {
	return map[string]any{
		"model_aliases":       st.ModelAliases,
		"request_timeout_sec": st.RequestTimeout,
		"max_retries":         st.MaxRetries,
		"log_keep":            st.LogKeep,
		"cors_origin":         st.CORSOrigin,
		"fallback_model":      st.FallbackModel,
		"admin_password_set":  st.AdminPassword != "",
	}
}

// resolveKeyRef 把完整密钥或脱敏密钥解析为完整密钥。
func (s *Server) resolveKeyRef(ref string) string {
	for _, k := range s.Store.Keys() {
		if k.Key == ref || mask(k.Key) == ref {
			return k.Key
		}
	}
	return ref
}

func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// --- 基础端点 ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accts := s.Store.Accounts()
	ok := 0
	for _, a := range accts {
		// 尚未探测过的账号按“可用”计，避免刚导入时健康度显示为 0。
		if a.Enabled && a.Status != "error" {
			ok++
		}
	}
	models := s.Store.Models()
	if len(models) == 0 {
		models = []deepai.Model{deepai.DefaultModelEntry()}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"version":       s.Version,
		"accounts":      len(accts),
		"ready":         ok,
		"models":        len(models),
		"default_model": s.Store.DefaultModel(),
		"machine_id":    shortMachine(s.Store.MachineID()),
		"machine_ready": s.Store.MachineID() != "",
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	catalog := s.Store.Models()
	if len(catalog) == 0 {
		catalog = []deepai.Model{deepai.DefaultModelEntry()}
	}
	models := registry.List(catalog, s.Store.Settings().ModelAliases)
	data := make([]map[string]any, 0, len(models))
	now := time.Now().Unix()
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": "deepai",
			"deepai": map[string]any{
				"name":              m.Name,
				"reasoning":         m.Reasoning,
				"input":             m.Input,
				"context_window":    m.ContextWindow,
				"max_tokens":        m.MaxTokens,
				"premier":           m.Premier,
				"free":              m.Free(),
				"api":               m.API,
				"theos_per_request": m.TheosPerRequest,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleRoot 返回控制台单页；其余未知路径返回 404。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeErr(w, http.StatusNotFound, "未找到该路径", "invalid_request_error")
		return
	}
	serveConsole(w, r)
}

// --- 控制台 API ---

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	accts := s.Store.Accounts()
	safe := make([]map[string]any, 0, len(accts))
	for _, a := range accts {
		safe = append(safe, map[string]any{
			"id": a.ID, "name": a.Name, "source": a.Source,
			"key": deepai.MaskSecret(a.APIKey), "key_id": a.KeyID(),
			"username": a.Username, "email": a.Email, "is_pro": a.IsPro,
			"plan": a.Plan, "email_verified": a.EmailVerified,
			"user_pk": a.UserPK, "group": a.GroupName(),
			"proxy": deepai.MaskProxy(a.Proxy), "fingerprint": a.Fingerprint,
			// 机器指纹只下发摘要：上游按它发额度，完整值没必要出网。
			"machine": maskMachine(a.MachineID), "has_machine": a.MachineID != "",
			"registered_at": a.RegisteredAt, "last_verified": a.LastVerified,
			"enabled": a.Enabled, "status": a.Status, "note": a.Note,
			"last_err": a.LastErr, "failures": a.Failures, "last_used": a.LastUsed,
			"theos": a.Theos,
		})
	}
	keys := s.Store.Keys()
	safeKeys := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		safeKeys = append(safeKeys, map[string]any{
			"key": mask(k.Key), "name": k.Name, "group": k.Group, "enabled": k.Enabled,
			"created_at": k.CreatedAt, "last_used": k.LastUsed, "requests": k.Requests,
		})
	}
	catalog := s.Store.Models()
	if len(catalog) == 0 {
		catalog = []deepai.Model{deepai.DefaultModelEntry()}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":               safe,
		"keys":                   safeKeys,
		"models":                 registry.List(catalog, s.Store.Settings().ModelAliases),
		"default_model":          s.Store.DefaultModel(),
		"models_updated":         s.Store.ModelsUpdated(),
		"machine_id":             shortMachine(s.Store.MachineID()),
		"settings":               sanitizedSettings(s.Store.Settings()),
		"stats":                  s.Store.Stats(),
		"upstream":               s.Up.Origin,
		"registration_max_count": registrationMaxCount(),
		"version":                s.Version,
	})
}

// 账号管理接口的 action 取值。
const (
	actionImport        = "import"
	actionManual        = "manual"
	actionEnroll        = "enroll"
	actionRemove        = "remove"
	actionToggle        = "toggle"
	actionClearErrors   = "clear-errors"
	actionAssignMachine = "assign-machine"
	actionSetGroup      = "set-group"
	actionMarkVerified  = "mark-verified"
)

func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		Action        string `json:"action"`
		ID            string `json:"id"`
		Dir           string `json:"dir"`
		APIKey        string `json:"api_key"`
		Name          string `json:"name"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		Email         string `json:"email"`
		SessionID     string `json:"session_id"`
		CSRFToken     string `json:"csrftoken"`
		Plan          string `json:"plan"`
		Group         string `json:"group"`
		UserPK        int64  `json:"user_pk"`
		EmailVerified bool   `json:"email_verified"`
		TheosBalance  string `json:"theos_balance"`
		TheosGranted  string `json:"theos_granted"`
		// SkipProbe 让调用方跳过入池时的 /theos 探测。
		// 注册链路刚刚验活过并随请求带上了 theos_balance/granted，
		// 再探一次既慢（~2s）又会引入全新的失败点。
		SkipProbe      bool   `json:"skip_probe"`
		Proxy          string `json:"proxy"`
		Fingerprint    string `json:"fingerprint"`
		MachineID      string `json:"machine_id"`
		FromGroup      string `json:"from_group"`
		UserAgent      string `json:"user_agent"`
		AcceptLanguage string `json:"accept_language"`
		SecCHUA        string `json:"sec_ch_ua"`
		Platform       string `json:"platform"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}

	switch body.Action {
	case actionImport:
		results, err := deepai.ImportAllFromDesktop(body.Dir)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "导入失败: "+err.Error(), "invalid_request_error")
			return
		}
		imported := make([]map[string]any, 0, len(results))
		for _, res := range results {
			isNew := s.Store.UpsertAccount(res.Account)
			imported = append(imported, map[string]any{
				"id": res.Account.ID, "name": res.Account.Name,
				"username": res.Account.Username, "path": res.Path, "new": isNew,
			})
		}
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(imported), "imported": imported})
	case actionManual:
		acct, err := deepai.AccountFromAPIKey(body.APIKey, body.Name, "manual", false)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		if strings.TrimSpace(body.Group) != "" {
			acct.Group = strings.TrimSpace(body.Group)
		}
		isNew := s.Store.UpsertAccount(acct)
		_ = s.Store.Save()
		go s.probeAndSave(acct)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "new": isNew, "id": acct.ID})
	case actionEnroll:
		reg := deepai.Registration{
			Email: body.Email, Password: body.Password, Username: body.Username,
			APIKey: body.APIKey, SessionID: body.SessionID, CSRFToken: body.CSRFToken,
			UserPK: body.UserPK, Plan: body.Plan, EmailVerified: body.EmailVerified,
			Group: body.Group, Proxy: body.Proxy, Fingerprint: body.Fingerprint,
			MachineID: strings.TrimSpace(body.MachineID),
			UserAgent: strings.TrimSpace(body.UserAgent), AcceptLanguage: strings.TrimSpace(body.AcceptLanguage),
			SecCHUA: strings.TrimSpace(body.SecCHUA), Platform: strings.TrimSpace(body.Platform),
			Source: "register",
		}
		// 每个账号必须有独立机器指纹；否则上游的每日免费额度会与其他账号互斥。
		if reg.MachineID == "" {
			reg.MachineID = deepai.NewMachineUUID()
		}
		acct, err := deepai.AccountFromRegistration(reg)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		// 调用方已验活并带上结果时直接采信；否则自己探（控制台手工入池走这条）。
		var al *deepai.Allowance
		switch {
		case body.SkipProbe && strings.TrimSpace(body.TheosBalance) != "":
			al = &deepai.Allowance{
				Enabled:   true,
				Balance:   strings.TrimSpace(body.TheosBalance),
				Granted:   strings.TrimSpace(body.TheosGranted),
				CheckedAt: time.Now().Unix(),
			}
		default:
			probed, err := s.probeAccountQuota(acct)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "Key 验活失败: "+err.Error(), "invalid_request_error")
				return
			}
			al = probed
			if strings.TrimSpace(body.TheosBalance) != "" {
				al.Balance = strings.TrimSpace(body.TheosBalance)
			}
			if strings.TrimSpace(body.TheosGranted) != "" {
				al.Granted = strings.TrimSpace(body.TheosGranted)
			}
		}
		id, isNew, err := s.enrollRegistration(reg, al)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "入池失败: "+err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "id": id, "new": isNew,
			"email_verified": body.EmailVerified, "theos": al,
		})
	case actionRemove:
		ok := s.Store.RemoveAccount(body.ID)
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
	case actionToggle:
		if a := s.Store.Account(body.ID); a != nil {
			a.Enabled = !a.Enabled
			_ = s.Store.Save()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case actionClearErrors:
		if a := s.Store.Account(body.ID); a != nil {
			a.Failures = 0
			a.Status = "unknown"
			a.LastErr = ""
			_ = s.Store.Save()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case actionAssignMachine:
		// 为存量账号补一个独立机器指纹。只补空值，不覆盖已有指纹
		// （覆盖会让该账号已被上游记住的额度归属失效）。
		assigned := 0
		var targets []*deepai.Account
		for _, a := range s.Store.Accounts() {
			if body.ID != "" && a.ID != body.ID {
				continue
			}
			if strings.TrimSpace(a.MachineID) != "" {
				continue
			}
			a.MachineID = deepai.NewMachineUUID()
			assigned++
			targets = append(targets, a)
		}
		_ = s.Store.Save()
		// 重新探测配额，让调度能立即看到新指纹下的额度。
		go s.probeAccounts(targets)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "assigned": assigned})
	case actionMarkVerified:
		// 后台验证完成后回填：入池时 email_verified 可能还是 false。
		if err := s.markAccountVerified(body.ID, body.Email); err != nil {
			writeErr(w, http.StatusNotFound, err.Error(), "invalid_request_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case actionSetGroup:
		// 把账号迁到另一个分组（例如把压测批次的账号并入日常号池）。
		// id 为空时按 from_group 批量迁移，便于整批归位。
		target := strings.TrimSpace(body.Group)
		if target == "" {
			writeErr(w, http.StatusBadRequest, "group 不能为空", "invalid_request_error")
			return
		}
		moved := 0
		for _, a := range s.Store.Accounts() {
			if body.ID != "" && a.ID != body.ID {
				continue
			}
			if body.FromGroup != "" && !strings.EqualFold(a.GroupName(), body.FromGroup) {
				continue
			}
			if a.GroupName() == target {
				continue
			}
			a.Group = target
			moved++
		}
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved, "group": target})
	default:
		writeErr(w, http.StatusBadRequest, "未知操作: "+body.Action, "invalid_request_error")
	}
}

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys := s.Store.Keys()
		safe := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			safe = append(safe, map[string]any{
				"key": mask(k.Key), "name": k.Name, "group": k.Group, "enabled": k.Enabled,
				"created_at": k.CreatedAt, "last_used": k.LastUsed, "requests": k.Requests,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": safe})
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
		k := s.Store.AddKey(body.Name, body.Group)
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k.Key})
	case http.MethodDelete:
		q := r.URL.Query()
		if ref := q.Get("key"); ref != "" {
			_ = s.Store.DeleteKey(s.resolveKeyRef(ref))
		}
		if ref := q.Get("toggle"); ref != "" {
			_ = s.Store.ToggleKey(s.resolveKeyRef(ref))
		}
		_ = s.Store.Save()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法", "invalid_request_error")
	}
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.Store.ClearLogs()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.Store.Logs(limit)})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.Store.ClearStats()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, s.Store.Stats())
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, sanitizedSettings(s.Store.Settings()))
		return
	}
	var body struct {
		ModelAliases   map[string]string `json:"model_aliases"`
		RequestTimeout *int              `json:"request_timeout_sec"`
		MaxRetries     *int              `json:"max_retries"`
		LogKeep        *int              `json:"log_keep"`
		CORSOrigin     *string           `json:"cors_origin"`
		AdminPassword  *string           `json:"admin_password"`
		FallbackModel  *string           `json:"fallback_model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	s.Store.UpdateSettings(func(st *store.Settings) {
		if body.ModelAliases != nil {
			st.ModelAliases = body.ModelAliases
		}
		if body.RequestTimeout != nil && *body.RequestTimeout > 0 {
			st.RequestTimeout = *body.RequestTimeout
		}
		if body.MaxRetries != nil && *body.MaxRetries > 0 {
			st.MaxRetries = *body.MaxRetries
		}
		if body.LogKeep != nil && *body.LogKeep > 0 {
			st.LogKeep = *body.LogKeep
		}
		if body.CORSOrigin != nil {
			st.CORSOrigin = *body.CORSOrigin
		}
		if body.AdminPassword != nil {
			st.AdminPassword = strings.TrimSpace(*body.AdminPassword)
		}
		if body.FallbackModel != nil {
			st.FallbackModel = strings.TrimSpace(*body.FallbackModel)
		}
	})
	_ = s.Store.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": sanitizedSettings(s.Store.Settings())})
}

// handleAdminCheckin 对全部（或指定）账号查询一次 Theos 配额。
//
// 并发度限制为 4：串行探测 20 个账号最坏要 10 分钟，全并发又容易
// 把探针本身变成上游的风控特征。
func (s *Server) handleAdminCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)

	targets := s.Store.Accounts()
	if body.ID != "" {
		targets = nil
		if a := s.Store.Account(body.ID); a != nil {
			targets = append(targets, a)
		}
	}

	const workers = 4
	type checkinResult struct {
		account *deepai.Account
		theos   *deepai.Allowance
		err     error
	}
	in := make(chan *deepai.Account)
	out := make(chan checkinResult, len(targets))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range in {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				al, err := s.clientFor(a).Theos(ctx, a)
				cancel()
				out <- checkinResult{account: a, theos: al, err: err}
			}
		}()
	}
	go func() {
		defer close(in)
		for _, a := range targets {
			select {
			case in <- a:
			case <-r.Context().Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	results := make([]map[string]any, 0, len(targets))
	for res := range out {
		ok := res.err == nil
		note := ""
		if res.err != nil {
			note = res.err.Error()
		}
		s.Store.SetAccountAllowance(res.account.ID, res.theos, res.err)
		results = append(results, map[string]any{
			"id": res.account.ID, "name": res.account.Name,
			"ok": ok, "err": note, "theos": res.theos,
		})
	}
	_ = s.Store.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// handleAdminModels 刷新模型目录；GET 返回缓存。
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		catalog := s.Store.Models()
		writeJSON(w, http.StatusOK, map[string]any{
			"models":         registry.List(catalog, s.Store.Settings().ModelAliases),
			"default_model":  s.Store.DefaultModel(),
			"models_updated": s.Store.ModelsUpdated(),
		})
		return
	}
	models, err := s.RefreshModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "刷新模型目录失败: "+err.Error(), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(models), "default_model": s.Store.DefaultModel(),
	})
}

// probeAccountQuota 探测一个账号的配额；瞬时错误重试一次。
//
// 入池是唯一会阻塞调用方的探测点，而 /theos 的失败几乎都是出口抖动
// （No exit node / TLS handshake / unexpected EOF），重试一次能救回绝大多数。
func (s *Server) probeAccountQuota(acct *deepai.Account) (*deepai.Allowance, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		al, err := s.clientFor(acct).Theos(ctx, acct)
		cancel()
		if err == nil {
			return al, nil
		}
		lastErr = err
		if !deepai.IsTransient(err) {
			return nil, err // 参数 / 鉴权问题，重试无意义
		}
	}
	return nil, lastErr
}

// probeAndSave 异步探测单个账号并落盘。
func (s *Server) probeAndSave(a *deepai.Account) {
	s.probeAccounts([]*deepai.Account{a})
}

// probeAccounts 以有界并发探测一批账号的配额并落盘。
//
// 串行探测 20 个账号最坏要 10 分钟（每个 30s 超时），所以限制到 4 并发；
// 再高容易把探针本身变成风控特征。
func (s *Server) probeAccounts(accts []*deepai.Account) {
	const workers = 4
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, a := range accts {
		if a == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a *deepai.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			al, err := s.clientFor(a).Theos(ctx, a)
			s.Store.SetAccountAllowance(a.ID, al, err)
		}(a)
	}
	wg.Wait()
	_ = s.Store.Save()
}

// --- 工具函数 ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg, typ string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

// mask 对敏感串做脱敏，仅保留首尾片段。
func mask(s string) string {
	if len(s) <= 10 {
		if s == "" {
			return ""
		}
		return s[:1] + "***"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// shortMachine 只展示机器指纹前 12 位。
func shortMachine(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// maskMachine 返回机器指纹的可读摘要。
//
// 账号级 machine 是原始 UUID，完整值不上控制台（它决定上游怎么发额度）。
func maskMachine(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) <= 8 {
		return raw
	}
	return raw[:8] + "…"
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return http.StatusBadGateway
}
