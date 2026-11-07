// Package deepai 实现 DeepAI Code 桌面端上游服务的逆向复刻。
//
// 上游是 OpenAI 兼容的 HTTPS 接口：
//
//	POST https://api.deepai.org/deepai_code/chat/completions
//	POST https://api.deepai.org/deepai_code/responses        （仅 openai-responses 模型）
//	GET  https://api.deepai.org/deepai_code/models
//	GET  https://api.deepai.org/deepai_code/theos
//	POST https://api.deepai.org/deepai_code/web_search
//	POST https://api.deepai.org/deepai_code/find_service
//	GET  https://api.deepai.org/get_usage_summary
//	POST https://api.deepai.org/speech_to_text
//
// 鉴权使用桌面端登录后写入的 API Key（`Api-Key` 头），并附带
// `X-DeepAI-Code-Theos` 车道版本与 `X-DeepAI-Code-Machine` 机器指纹。
package deepai

import "strings"

// 上游协议常量。
const (
	// LaneVersion 是本网关对齐的 Theos 计费车道版本。
	LaneVersion = "4"
	// ProtocolClientVersion is the official client version this wire format follows.
	ProtocolClientVersion = "0.3.19"

	// 请求头名称。
	HeaderKeyName  = "Api-Key"             // 上游统一使用 Api-Key（大小写不敏感）
	HeaderLane     = "X-DeepAI-Code-Theos" // 车道版本
	HeaderMachine  = "X-DeepAI-Code-Machine"
	HeaderAffinity = "X-Session-Affinity"
	HeaderInstance = "X-DeepAI-Code-Instance"
	HeaderCheckIn  = "X-DeepAI-Code-Check-In"
	HeaderBeacon   = "X-DeepAI-Code-Beacon"

	// 路径前缀。
	ModelBasePath = "/deepai_code"

	// 模型目录路径（相对 ModelBasePath）。
	PathModels       = "/models"
	PathChat         = "/chat/completions"
	PathResponses    = "/responses"
	PathTheos        = "/theos"
	PathWebSearch    = "/web_search"
	PathFindService  = "/find_service"
	PathSessionsList = "/sessions/list"
	PathSignIn       = "/desktop_auth/"
	PathExchange     = "/desktop_auth/exchange/"

	// 根路径（不在 /deepai_code 之下）。
	PathUsageSummary = "/get_usage_summary"
	PathSpeechToText = "/speech_to_text"
	PathAttachments  = "/chat_attachments/upload"

	// DefaultOrigin 是生产环境上游地址。
	DefaultOrigin = "https://api.deepai.org"

	// DefaultModel 是目录不可用时的兜底模型。
	DefaultModel = "glm-5.3-flash"
)

// Model 是上游模型目录中的一个条目。
type Model struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Reasoning       bool              `json:"reasoning"`
	Input           []string          `json:"input"`
	ContextWindow   int               `json:"contextWindow"`
	MaxTokens       int               `json:"maxTokens"`
	Premier         bool              `json:"premier,omitempty"`
	API             string            `json:"api,omitempty"` // 空 = chat/completions；openai-responses = /responses
	TheosPerRequest string            `json:"theosPerRequest,omitempty"`
	TheosPerSecond  map[string]string `json:"theosPerSecond,omitempty"`
}

// IsResponses 报告该模型是否走 OpenAI Responses 端点。
func (m Model) IsResponses() bool { return m.API == "openai-responses" }

// Free 报告该模型是否免费（每次请求 0 Theos）。
func (m Model) Free() bool {
	if m.TheosPerRequest != "" {
		return m.TheosPerRequest == "0" || m.TheosPerRequest == "0.0" || m.TheosPerRequest == "0.00"
	}
	return !m.Premier
}

// SupportsImage 报告模型是否接受图片输入。
func (m Model) SupportsImage() bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// Credentials 是桌面端保存的 DeepAI 账号凭据。
type Credentials struct {
	APIKey   string `json:"apiKey"`
	Username string `json:"username"`
	IsPro    bool   `json:"isPro"`
}

// Allowance 是 /theos 返回的配额信息。
type Allowance struct {
	Enabled     bool   `json:"enabled"`
	Balance     string `json:"balance,omitempty"`
	Granted     string `json:"granted,omitempty"`
	RefreshAt   string `json:"refresh_at,omitempty"`
	LaneVersion int    `json:"lane_version,omitempty"`
	CheckedAt   int64  `json:"checked_at,omitempty"`
}

// UsageSummary 是 /get_usage_summary 的返回（字段按需保留）。
type UsageSummary struct {
	IsPro             bool    `json:"is_pro"`
	Plan              string  `json:"plan"`
	IsAuthenticated   bool    `json:"is_authenticated"`
	CreditsRemaining  float64 `json:"credits_remaining"`
	TotalOverageCost  float64 `json:"total_overage_cost"`
	HasOverages       bool    `json:"has_overages"`
	PeriodDescription string  `json:"period_description"`
}

// Account 是一个 DeepAI 上游账号（= 一个 API Key）。
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"` // desktop / manual / oauth / register
	APIKey   string `json:"api_key"`
	Username string `json:"username"`
	IsPro    bool   `json:"is_pro"`

	// 网页账号信息（注册链路写入；用于补跑与审计）。
	Email         string `json:"email,omitempty"`
	Password      string `json:"password,omitempty"`
	WebSessionID  string `json:"session_id,omitempty"`
	CSRFToken     string `json:"csrf_token,omitempty"`
	UserPK        int64  `json:"user_pk,omitempty"`
	Plan          string `json:"plan,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	RegisteredAt  int64  `json:"registered_at,omitempty"`
	LastVerified  int64  `json:"last_verified,omitempty"`

	// Group 是号池分组（空串等价于 default）。
	Group string `json:"group,omitempty"`

	// Proxy 是该账号推理时使用的上游代理（完整 URL，仅本地保存）。
	Proxy       string `json:"proxy,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`

	// 浏览器指纹快照（注册时写入）。推理侧带上同一套头，否则“用 Chrome 注册、
	// 用空 UA 推理”本身就是一个可被识别的异常信号。
	UserAgent      string `json:"user_agent,omitempty"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	SecCHUA        string `json:"sec_ch_ua,omitempty"`
	Platform       string `json:"platform,omitempty"`

	// MachineID 是该账号专用的机器指纹原始值（UUID）。
	//
	// 上游按 X-DeepAI-Code-Machine 每天发放一次免费额度；多个账号共用
	// 一个指纹时只有第一个能拿到额度。注册时每号生成一个。
	// 空串表示回退到进程级 MachineID()（历史账号 + 桌面端导入）。
	MachineID string `json:"machine_id,omitempty"`

	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"` // ok / error / unknown
	Note     string `json:"note"`
	LastUsed int64  `json:"last_used"`
	LastErr  string `json:"last_err"`
	Failures int    `json:"failures"`

	// Theos 是最近一次 /theos 探测结果（只做展示，不做调度）。
	Theos *Allowance `json:"theos,omitempty"`
}

// GroupName 返回规范化后的分组名。
func (a *Account) GroupName() string {
	if a == nil {
		return "default"
	}
	g := strings.TrimSpace(a.Group)
	if g == "" {
		return "default"
	}
	return g
}

// Registration 是网页注册链路产出的账号信息。
type Registration struct {
	Email          string
	Password       string
	Username       string
	APIKey         string
	SessionID      string
	CSRFToken      string
	UserPK         int64
	Plan           string
	EmailVerified  bool
	Group          string
	Proxy          string
	Fingerprint    string
	MachineID      string
	UserAgent      string
	AcceptLanguage string
	SecCHUA        string
	Platform       string
	Source         string
}

// KeyID 返回 API Key 的稳定短标识（用于去重与日志，不泄露明文）。
func (a *Account) KeyID() string { return KeyFingerprint(a.APIKey) }

// SessionID 返回用于展示/排障的会话标识。
func (a *Account) SessionID() string { return ShortHash(a.APIKey) }

// DefaultModelEntry 是目录不可用时的内置兜底条目。
func DefaultModelEntry() Model {
	return Model{
		ID:            DefaultModel,
		Name:          "GLM 5.3 Flash",
		Reasoning:     true,
		Input:         []string{"text", "image"},
		ContextWindow: 1048576,
		MaxTokens:     131072,
	}
}
