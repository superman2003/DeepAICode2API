// DeepAICode2API 把本机 DeepAI Code 桌面端的上游模型服务，
// 逆向复刻成一个 OpenAI / Anthropic 兼容的 Go 网关。
//
// 复用桌面端已登录的 API Key，无需另申请密钥；编译为单文件 Go 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/proxychain"
	"deepaicode2api/internal/server"
	"deepaicode2api/internal/store"
	"deepaicode2api/internal/updater"
)

// version 由构建时注入：-ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	var (
		host       = flag.String("host", envOr("DEEPAI2API_HOST", "127.0.0.1"), "监听地址")
		port       = flag.Int("port", envInt("DEEPAI2API_PORT", 10087), "监听端口")
		dataPath   = flag.String("data", envOr("DEEPAI2API_DATA_PATH", ""), "状态文件路径（默认在可执行文件同目录）")
		deepaiDir  = flag.String("deepai-dir", envOr("DEEPAI2API_DATA_DIR", ""), "DeepAI Code 数据目录（留空自动探测）")
		importOnly = flag.Bool("import", false, "仅执行一次桌面端凭据导入后退出")
		noImport   = flag.Bool("no-import", false, "启动时不自动导入桌面端凭据")
		adminPw    = flag.String("admin-password", envOr("DEEPAI2API_ADMIN_PASSWORD", ""), "管理密码（非空时覆盖状态文件中的设置）")
		allowOpen  = flag.Bool("allow-insecure", envBool("DEEPAI2API_ALLOW_INSECURE", false), "允许非回环监听但不设管理密码（不安全）")
		showVer    = flag.Bool("version", false, "打印版本后退出")
		checkState = flag.Bool("check-state", false, "只校验账号状态文件并打印数量，不出网")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("deepaicode2api", version)
		return
	}

	if *dataPath == "" {
		*dataPath = defaultDataPath()
	}

	st, err := store.Open(*dataPath)
	if err != nil {
		log.Fatalf("打开状态文件失败: %v", err)
	}
	if *checkState {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "accounts": len(st.Accounts()), "keys": len(st.Keys()), "encrypted": st.Encrypted()})
		return
	}
	log.Printf("状态文件: %s", st.Path())
	if egress := strings.TrimSpace(os.Getenv(proxychain.EnvEgressProxy)); egress != "" {
		log.Printf("远程代理连接出口: %s（保留代理池，先经此出口连接）", deepai.MaskProxy(egress))
	}
	if st.MasterKeySet() {
		log.Printf("状态文件加密: 已启用（AES-256-GCM，密钥来自 %s）", store.EnvMasterKey)
	} else {
		log.Printf("⚠ 状态文件未加密：上游 API Key / 账号密码以明文落盘。")
		log.Printf("  建议设置 %s=$(openssl rand -hex 32) 后重启，后续保存会自动加密。", store.EnvMasterKey)
	}

	machine := deepai.MachineID()
	if machine != "" {
		st.SetMachineID(machine)
		log.Printf("机器指纹: %s…（用于上游按天发放免费 Theos）", machine[:12])
	} else {
		log.Printf("⚠ 未能读取机器指纹：上游会拒绝所有模型请求（426 lane_version_unsupported）")
		log.Printf("  请设置 DEEPAI_MACHINE_ID=<原始机器UUID>；Windows 可执行：")
		log.Printf("  powershell -Command \"(Get-CimInstance Win32_ComputerSystemProduct).UUID\"")
	}

	if *importOnly {
		runImport(st, *deepaiDir)
		return
	}

	// 命令行 / 环境变量优先级高于状态文件；设置后立即落盘。
	if *adminPw != "" {
		st.UpdateSettings(func(s *store.Settings) { s.AdminPassword = *adminPw })
		if err := st.Save(); err != nil {
			log.Printf("保存管理密码失败: %v", err)
		} else {
			log.Printf("已启用管理密码（来自命令行 / DEEPAI2API_ADMIN_PASSWORD）")
		}
	}

	if !*noImport {
		autoImport(st, *deepaiDir)
	}
	if envAcct := deepai.EnvAccount(); envAcct != nil {
		if isNew := st.UpsertAccount(envAcct); isNew {
			log.Printf("已从 DEEPAI_API_KEY 添加账号 %s", envAcct.Name)
			_ = st.Save()
		}
	}

	// 非回环监听意味着网关已接入局域网 / 公网，此时两项底线不能靠“默认值”撑着：
	// 管理面必须有密码，数据面必须已有密钥。否则从启动到人工创建密钥之间，
	// 上游账号池就是一个无需鉴权的开放中继。
	if !isLoopbackListen(*host) {
		if st.Settings().AdminPassword == "" && !*allowOpen {
			log.Fatalf("监听地址 %q 不是回环地址，但未设置管理密码：控制台会对局域网开放。\n"+
				"  请用 -admin-password 或 DEEPAI2API_ADMIN_PASSWORD 设置密码；\n"+
				"  确实需要无密码运行请显式加 -allow-insecure（或 DEEPAI2API_ALLOW_INSECURE=1）。", *host)
		}
		if !st.HasKeys() {
			k := st.AddKey("bootstrap", "default")
			if err := st.Save(); err != nil {
				log.Fatalf("生成引导密钥失败: %v", err)
			}
			log.Printf("⚠ 非回环监听且尚未签发密钥，已自动生成引导密钥（仅本次启动显示，请立即保存）：")
			log.Printf("    %s", k.Key)
			log.Printf("  之后可在控制台改名或吊销。")
		}
	}

	srv := server.New(st)
	srv.Version = version
	updateStop := make(chan struct{}, 1)
	projectRoot := filepath.Dir(os.Args[0])
	if executable, err := os.Executable(); err == nil {
		projectRoot = filepath.Dir(executable)
	}
	if strings.EqualFold(filepath.Base(projectRoot), "bin") {
		projectRoot = filepath.Dir(projectRoot)
	}
	manager, updateErr := updater.New(projectRoot, filepath.Dir(st.Path()), version, func() {
		select {
		case updateStop <- struct{}{}:
		default:
		}
	})
	if updateErr != nil {
		log.Printf("更新器初始化失败: %v", updateErr)
	} else {
		srv.Updater = manager
	}

	// Backport upstream b0ec437: external catalog fetches never delay the
	// listener or updater health checks. Refresh the cache in background.

	addr := net.JoinHostPort(*host, fmt.Sprintf("%d", *port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// 流式响应可能持续数分钟，不设 WriteTimeout
	}

	go func() {
		log.Printf("DeepAICode2API %s 已启动: http://%s/", version, addr)
		if st.HasKeys() {
			log.Printf("已签发 API 密钥，调用需携带 Authorization: Bearer sk-...")
		} else {
			log.Printf("尚未签发 API 密钥：仅本机回环可无密钥调用，非回环来源返回 503（请在控制台创建）")
		}
		if st.Settings().AdminPassword != "" {
			log.Printf("管理接口已启用密码校验（X-Admin-Password / Bearer）")
		} else {
			log.Printf("未设置管理密码，管理接口仅允许本机访问（可用 -admin-password 设置）")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	// 后台任务：定期落盘 + 定期刷新模型目录。
	stopBG := make(chan struct{})
	go background(st, srv, stopBG)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	updateRequested := false
	select {
	case <-stop:
	case <-updateStop:
		updateRequested = true
	}

	close(stopBG)
	log.Printf("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("关闭异常: %v", err)
	}
	if err := st.Save(); err != nil {
		log.Printf("保存状态失败: %v", err)
		if updateRequested {
			if manager != nil {
				manager.CancelApply("保存账号状态失败，升级已取消")
			}
			os.Exit(1)
		}
	}
	log.Printf("已退出")
	if updateRequested {
		os.Exit(updater.UpdateExitCode)
	}
}

// background 负责定期持久化与模型目录刷新。
func background(st *store.Store, srv *server.Server, stop <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if models, err := srv.RefreshModels(ctx); err != nil {
		log.Printf("拉取模型目录失败（沿用缓存）: %v", err)
	} else {
		log.Printf("模型目录已更新：%d 个模型，默认 %s", len(models), st.DefaultModel())
	}
	cancel()
	saveTick := time.NewTicker(30 * time.Second)
	defer saveTick.Stop()
	modelTick := time.NewTicker(6 * time.Hour)
	defer modelTick.Stop()

	for {
		select {
		case <-stop:
			return
		case <-saveTick.C:
			if err := st.Save(); err != nil {
				log.Printf("定期保存状态失败: %v", err)
			}
		case <-modelTick.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			models, err := srv.RefreshModels(ctx)
			cancel()
			if err != nil {
				log.Printf("定期刷新模型目录失败: %v", err)
			} else {
				log.Printf("模型目录已刷新：%d 个模型", len(models))
			}
		}
	}
}

// autoImport 在启动时尝试导入本机桌面端凭据。
func autoImport(st *store.Store, dir string) {
	results, err := deepai.ImportAllFromDesktop(dir)
	if err != nil {
		log.Printf("未自动导入桌面端凭据（可在控制台手动导入）: %v", err)
		return
	}
	added, updated := 0, 0
	for _, res := range results {
		if st.UpsertAccount(res.Account) {
			added++
		} else {
			updated++
		}
	}
	if err := st.Save(); err != nil {
		log.Printf("保存账号失败: %v", err)
		return
	}
	for _, res := range results {
		log.Printf("已导入账号 %s（%s，username=%s）", res.Account.Name, res.Path, res.Account.Username)
	}
	log.Printf("桌面端凭据导入完成：新增 %d，更新 %d", added, updated)
}

func runImport(st *store.Store, dir string) {
	results, err := deepai.ImportAllFromDesktop(dir)
	if err != nil {
		log.Fatalf("导入失败: %v", err)
	}
	added, updated := 0, 0
	for _, res := range results {
		isNew := st.UpsertAccount(res.Account)
		state := "更新"
		if isNew {
			state = "新增"
			added++
		} else {
			updated++
		}
		fmt.Printf("%s账号 %s\n凭据文件: %s\nusername: %s\nKey: %s\n\n",
			state, res.Account.Name, res.Path, res.Account.Username, deepai.MaskSecret(res.Account.APIKey))
	}
	if err := st.Save(); err != nil {
		log.Fatalf("保存失败: %v", err)
	}
	fmt.Printf("共导入 %d 个账号（新增 %d，更新 %d）→ %s\n", len(results), added, updated, st.Path())
}

// defaultDataPath 返回默认状态文件路径。
//
// 优先使用用户私有配置目录（Windows 上是 %AppData%，Unix 上是 ~/.config）：
// 状态文件含上游 Key 与账号密码，放在可执行文件同目录时往往会落在
// 共享目录（Program Files、共享盘）里，同机任意用户都能读到。
//
// 为不打断既有部署：如果旧位置（可执行文件同目录）已经有状态文件，继续用它，
// 否则号池看起来会“凭空消失”。
func defaultDataPath() string {
	const name = "deepaicode2api-data.json"

	if exe, err := os.Executable(); err == nil {
		legacy := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "deepaicode2api", name)
	}
	return name
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// isLoopbackListen 判断监听地址是否只接受本机连接。
//
// 空串 / 0.0.0.0 / :: 都表示监听全部网卡，属于对外暴露，不算回环。
func isLoopbackListen(host string) bool {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
