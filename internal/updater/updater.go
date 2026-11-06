package updater

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultRepository = "superman2003/DeepAICode2API"
const AssetName = "DeepAICode2API_windows_amd64_update.zip"
const UpdateExitCode = 75
const maxDownload = 64 << 20
const maxExpanded = 128 << 20

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var digestPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Protocol int    `json:"protocol"`
	Files    []File `json:"files"`
}
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	// Public release feeds do not publish asset sizes; downloads remain bounded.
	sizeUnknown bool
}

func (a Asset) validSize() bool {
	return (a.Size > 0 || (a.sizeUnknown && a.Size == 0)) && a.Size <= maxDownload
}

type Release struct {
	Tag        string  `json:"tag_name"`
	URL        string  `json:"html_url"`
	Body       string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}
type State struct {
	Phase          string `json:"phase"`
	CurrentVersion string `json:"current_version"`
	Repository     string `json:"repository"`
	TargetVersion  string `json:"target_version,omitempty"`
	ReleaseURL     string `json:"release_url,omitempty"`
	Notes          string `json:"notes,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	Error          string `json:"error,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
	Managed        bool   `json:"managed"`
	Supported      bool   `json:"supported"`
}
type Job struct {
	ID                string `json:"id"`
	Version           string `json:"version"`
	PreviousVersion   string `json:"previous_version"`
	ProjectRoot       string `json:"project_root"`
	DataRoot          string `json:"data_root"`
	EnvironmentSHA256 string `json:"environment_sha256"`
	Accounts          int    `json:"accounts"`
	Files             []File `json:"files"`
}
type Manager struct {
	mu                                   sync.Mutex
	root, data, dir, version, repository string
	client                               *http.Client
	busy                                 bool
	release                              *Release
	shutdown                             func()
}

func New(root, data, version string, shutdown func()) (*Manager, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	m := &Manager{root: root, data: data, dir: filepath.Join(data, "updater"), version: version, repository: DefaultRepository, shutdown: shutdown}
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return nil, err
	}
	var config struct {
		Repository string `json:"repository"`
	}
	if raw, err := os.ReadFile(filepath.Join(m.dir, "config.json")); err == nil && json.Unmarshal(raw, &config) == nil && validRepository(config.Repository) {
		m.repository = config.Repository
	}
	m.client = &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || !trustedURL(req.URL.String()) {
			return errors.New("untrusted update redirect")
		}
		return nil
	}}
	return m, nil
}

func validRepository(value string) bool {
	return repositoryPattern.MatchString(value) && !strings.Contains(value, "..")
}

func newer(target, current string) bool {
	parse := func(value string) ([3]int, bool) {
		var result [3]int
		value = strings.TrimPrefix(value, "v")
		value = strings.SplitN(strings.SplitN(value, "+", 2)[0], "-", 2)[0]
		parts := strings.Split(value, ".")
		if len(parts) != 3 {
			return result, false
		}
		for i, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil {
				return result, false
			}
			result[i] = number
		}
		return result, true
	}
	next, ok := parse(target)
	if !ok {
		return false
	}
	previous, ok := parse(current)
	if !ok {
		return target != current
	}
	for i := range next {
		if next[i] != previous[i] {
			return next[i] > previous[i]
		}
	}
	return false
}
func trustedURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	}
	return false
}
func protectedAssetURL(value, repository string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && strings.HasPrefix(u.Path, "/"+repository+"/releases/download/")
}

func atomicJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func (m *Manager) writeState(state State) error {
	state.UpdatedAt = time.Now().Unix()
	return atomicJSON(filepath.Join(m.dir, "state.json"), state)
}
func (m *Manager) state() State {
	var state State
	raw, err := os.ReadFile(filepath.Join(m.dir, "state.json"))
	if err != nil || json.Unmarshal(raw, &state) != nil {
		state.Phase = "idle"
	}
	state.CurrentVersion = m.version
	state.Repository = m.repository
	state.Managed = os.Getenv("DEEPAI2API_MANAGED_UPDATES") == "1"
	state.Supported = runtime.GOOS == "windows" && runtime.GOARCH == "amd64"
	return state
}
func (m *Manager) Status() State { m.mu.Lock(); defer m.mu.Unlock(); return m.state() }

func (m *Manager) SetRepository(repository string) error {
	repository = strings.TrimSpace(repository)
	if !validRepository(repository) {
		return errors.New("仓库格式必须是 owner/repo")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy || m.state().Phase == "applying" {
		return errors.New("更新进行中，不能修改来源")
	}
	if err := atomicJSON(filepath.Join(m.dir, "config.json"), map[string]string{"repository": repository}); err != nil {
		return err
	}
	m.repository = repository
	m.release = nil
	return m.writeState(State{Phase: "idle"})
}

func (m *Manager) request(ctx context.Context, address string) (*http.Response, error) {
	if !trustedURL(address) {
		return nil, errors.New("更新只允许从 GitHub HTTPS 地址下载")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "DeepAICode2API-Updater")
	if req.URL.Hostname() == "api.github.com" {
		req.Header.Set("Accept", "application/vnd.github+json")
		if token := os.Getenv("DEEPAI2API_UPDATE_GITHUB_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 GitHub 更新服务")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (m *Manager) Check(ctx context.Context) (State, error) {
	m.mu.Lock()
	if m.busy {
		state := m.state()
		m.mu.Unlock()
		return state, errors.New("更新任务进行中")
	}
	m.busy = true
	repository := m.repository
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
	release, err := m.latestRelease(ctx, repository)
	if err != nil {
		return m.fail(err)
	}
	if release.Draft || release.Prerelease || !versionPattern.MatchString(release.Tag) {
		return m.fail(errors.New("Release 格式无效或不是稳定版本"))
	}
	var compatible bool
	for _, asset := range release.Assets {
		if asset.Name == AssetName && protectedAssetURL(asset.URL, repository) && asset.validSize() {
			compatible = true
		}
	}
	if !compatible {
		return m.fail(errors.New("该 Release 缺少 Windows amd64 专用升级包"))
	}
	m.mu.Lock()
	m.release = &release
	phase := "available"
	if !newer(release.Tag, m.version) {
		phase = "current"
	}
	state := State{Phase: phase, TargetVersion: release.Tag, ReleaseURL: release.URL, Notes: release.Body}
	err = m.writeState(state)
	state = m.state()
	m.mu.Unlock()
	return state, err
}

func (m *Manager) latestRelease(ctx context.Context, repository string) (Release, error) {
	var release Release
	resp, apiErr := m.request(ctx, "https://api.github.com/repos/"+repository+"/releases/latest")
	if apiErr == nil {
		apiErr = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&release)
		resp.Body.Close()
		if apiErr == nil {
			return release, nil
		}
	}
	if ctx.Err() != nil {
		return Release{}, ctx.Err()
	}
	// The public feed has a separate rate limit from the API and needs no token.
	resp, err := m.request(ctx, "https://github.com/"+repository+"/releases.atom")
	if err != nil {
		return Release{}, fmt.Errorf("%v；Release Feed 备用检查失败：%w", apiErr, err)
	}
	defer resp.Body.Close()
	return releaseFromFeed(io.LimitReader(resp.Body, 2<<20), repository)
}

func releaseFromFeed(reader io.Reader, repository string) (Release, error) {
	var feed struct {
		XMLName xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
		Entries []struct {
			Title string `xml:"title"`
			Links []struct {
				Rel  string `xml:"rel,attr"`
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(reader).Decode(&feed); err != nil {
		return Release{}, errors.New("GitHub Release Feed 格式无效")
	}
	var release Release
	prefix := "/" + repository + "/releases/tag/"
	for _, entry := range feed.Entries {
		for _, link := range entry.Links {
			u, err := url.Parse(link.Href)
			if err != nil || link.Rel != "alternate" || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, prefix) {
				continue
			}
			tag := strings.TrimPrefix(u.Path, prefix)
			if !versionPattern.MatchString(tag) || strings.Contains(strings.SplitN(tag, "+", 2)[0], "-") || !newer(tag, "v0.0.0") {
				continue
			}
			if release.Tag != "" && !newer(tag, release.Tag) {
				continue
			}
			base := "https://github.com/" + repository + "/releases/download/" + url.PathEscape(tag) + "/"
			release = Release{Tag: tag, URL: link.Href, Body: entry.Title, Assets: []Asset{
				{Name: AssetName, URL: base + AssetName, sizeUnknown: true},
				{Name: "SHA256SUMS.txt", URL: base + "SHA256SUMS.txt", sizeUnknown: true},
			}}
		}
	}
	if release.Tag == "" {
		return Release{}, errors.New("GitHub Release Feed 中没有可用的稳定版本")
	}
	return release, nil
}

func (m *Manager) fail(err error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.writeState(State{Phase: "failed", Error: err.Error()})
	return m.state(), err
}

func (m *Manager) Prepare() error {
	m.mu.Lock()
	if m.busy || m.release == nil || m.state().Phase != "available" {
		m.mu.Unlock()
		return errors.New("请先检查可用更新")
	}
	release := *m.release
	repository := m.repository
	m.busy = true
	_ = m.writeState(State{Phase: "downloading", TargetVersion: release.Tag, ReleaseURL: release.URL})
	m.mu.Unlock()
	go func() {
		defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
		if err := m.prepare(context.Background(), release, repository); err != nil {
			_, _ = m.fail(err)
		}
	}()
	return nil
}

func (m *Manager) prepare(ctx context.Context, release Release, repository string) error {
	var asset, checksums Asset
	for _, item := range release.Assets {
		if item.Name == AssetName {
			asset = item
		}
		if item.Name == "SHA256SUMS.txt" {
			checksums = item
		}
	}
	if !protectedAssetURL(asset.URL, repository) || !asset.validSize() {
		return errors.New("升级资源地址或大小无效")
	}
	expected := strings.TrimPrefix(asset.Digest, "sha256:")
	if !digestPattern.MatchString(expected) {
		if !protectedAssetURL(checksums.URL, repository) {
			return errors.New("Release 缺少有效 SHA256 校验文件")
		}
		resp, err := m.request(ctx, checksums.URL)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == AssetName && digestPattern.MatchString(fields[0]) {
				expected = fields[0]
			}
		}
	}
	if !digestPattern.MatchString(expected) {
		return errors.New("无法取得升级包校验值")
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", release.Tag, time.Now().UnixNano()))))[:24]
	stage := filepath.Join(m.dir, "staged", id)
	if err := os.MkdirAll(stage, 0700); err != nil {
		return err
	}
	archive := filepath.Join(stage, "release.zip")
	resp, err := m.request(ctx, asset.URL)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		resp.Body.Close()
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, maxDownload+1))
	file.Close()
	resp.Body.Close()
	if copyErr != nil || written > maxDownload || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return errors.New("升级包下载不完整或 SHA256 校验失败")
	}
	manifest, err := Extract(archive, stage, release.Tag)
	if err != nil {
		return err
	}
	job := Job{ID: id, Version: manifest.Version, PreviousVersion: m.version, ProjectRoot: m.root, DataRoot: m.data, Files: manifest.Files}
	if err := atomicJSON(filepath.Join(stage, "job.json"), job); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeState(State{Phase: "ready", TargetVersion: release.Tag, ReleaseURL: release.URL, JobID: id})
}

// Extract accepts only the fixed program files, never configuration or state.
func Extract(archive, destination, version string) (Manifest, error) {
	var manifest Manifest
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return manifest, err
	}
	defer reader.Close()
	if len(reader.File) > 4 {
		return manifest, errors.New("升级包包含意外文件")
	}
	entries := map[string]*zip.File{}
	var total uint64
	for _, entry := range reader.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if name != "update-manifest.json" && name != "bin/deepaicode2api.exe" && name != "bin/register.exe" {
			return manifest, errors.New("升级包包含配置、数据或不允许的路径")
		}
		if entry.Mode()&os.ModeSymlink != 0 || entry.FileInfo().IsDir() {
			return manifest, errors.New("升级包不允许链接或目录条目")
		}
		if entries[name] != nil {
			return manifest, errors.New("升级包存在重复路径")
		}
		entries[name] = entry
		if entry.UncompressedSize64 > maxExpanded {
			return manifest, errors.New("升级包单个文件过大")
		}
		total += entry.UncompressedSize64
		if total > maxExpanded {
			return manifest, errors.New("升级包解压大小超出限制")
		}
	}
	entry := entries["update-manifest.json"]
	if entry == nil || entry.UncompressedSize64 > 64<<10 {
		return manifest, errors.New("缺少有效升级清单")
	}
	stream, err := entry.Open()
	if err != nil {
		return manifest, err
	}
	err = json.NewDecoder(io.LimitReader(stream, 64<<10)).Decode(&manifest)
	stream.Close()
	if err != nil || manifest.Version != version || manifest.OS != "windows" || manifest.Arch != "amd64" || manifest.Protocol != 1 || len(manifest.Files) != 2 {
		return manifest, errors.New("升级清单版本或平台不兼容")
	}
	seen := map[string]bool{}
	for _, item := range manifest.Files {
		if (item.Path != "bin/deepaicode2api.exe" && item.Path != "bin/register.exe") || !digestPattern.MatchString(item.SHA256) || seen[item.Path] {
			return manifest, errors.New("升级清单文件无效")
		}
		seen[item.Path] = true
		source := entries[item.Path]
		if source == nil {
			return manifest, errors.New("升级包缺少程序文件")
		}
		target := filepath.Join(destination, filepath.FromSlash(item.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return manifest, err
		}
		input, err := source.Open()
		if err != nil {
			return manifest, err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			input.Close()
			return manifest, err
		}
		hash := sha256.New()
		count, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, maxExpanded+1))
		input.Close()
		output.Close()
		if copyErr != nil || count > maxExpanded || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), item.SHA256) {
			return manifest, errors.New("解压程序 SHA256 校验失败")
		}
	}
	return manifest, nil
}

func (m *Manager) Apply(accounts int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state()
	if !state.Supported || !state.Managed {
		return errors.New("当前启动方式不支持网页重启，请安装含更新器的完整版本并使用 run-windows.cmd 启动")
	}
	if m.busy || state.Phase != "ready" || !regexp.MustCompile(`^[a-f0-9]{24}$`).MatchString(state.JobID) {
		return errors.New("没有已校验的升级包")
	}
	stage := filepath.Join(m.dir, "staged", state.JobID)
	var job Job
	raw, err := os.ReadFile(filepath.Join(stage, "job.json"))
	if err != nil || json.Unmarshal(raw, &job) != nil || job.Version != state.TargetVersion {
		return errors.New("升级任务文件无效")
	}
	for _, item := range job.Files {
		raw, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(item.Path)))
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		if !strings.EqualFold(hex.EncodeToString(hash[:]), item.SHA256) {
			return errors.New("暂存程序已变化，请重新下载")
		}
	}
	environment, err := os.ReadFile(filepath.Join(m.root, ".env"))
	if err != nil {
		return errors.New("无法读取现有 .env")
	}
	hash := sha256.Sum256(environment)
	job.EnvironmentSHA256 = hex.EncodeToString(hash[:])
	job.Accounts = accounts
	if err := atomicJSON(filepath.Join(m.dir, "request.json"), job); err != nil {
		return err
	}
	if err := m.writeState(State{Phase: "applying", TargetVersion: job.Version, JobID: job.ID}); err != nil {
		return err
	}
	if m.shutdown != nil {
		go func() { time.Sleep(400 * time.Millisecond); m.shutdown() }()
	}
	return nil
}

func (m *Manager) CancelApply(message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state()
	state.Phase = "failed"
	state.Error = message
	_ = os.Remove(filepath.Join(m.dir, "request.json"))
	_ = m.writeState(state)
}
