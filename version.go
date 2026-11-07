package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ZCode 客户端版本解析: 用于填充 User-Agent: ZCode/<semver> 与 X-ZCode-App-Version。
//
// zcode-plan(体验套餐)风控要求 UA 的 semver >= 当前官方底线(2026-11-07 实测 3.17),
// 官方每次发版几乎都会抬这个底线, 所以版本不能焊死在配置里, 而应动态获取:
//
//	static: 用配置里的 app_version (旧行为)
//	local:  扫本机 ZCode 桌面客户端的 rollout 日志, 取里面真实的 User-Agent 版本
//	remote: 拉官网发版页, 取 releases/<semver>/ 链接里的最大版本
//	auto:   先 local, 取不到再 remote
//
// 动态模式共用进程内缓存(默认 1 小时), 失败后 60 秒内不重试,
// 版本只升不降, 且始终不低于 app_version_floor, 因此刷新失败也不会把线上打挂。
const (
	versionSourceStatic = "static"
	versionSourceLocal  = "local"
	versionSourceRemote = "remote"
	versionSourceAuto   = "auto"

	defaultVersionCheckURL    = "https://zcode.z.ai/cn"
	defaultVersionRefreshSecs = 3600
	defaultAppVersionFloor    = "3.17.0"

	localRolloutTailBytes    = 512 << 10
	localRolloutFilesToScan  = 2
	remoteVersionFetchMax    = 4 << 20
	remoteVersionFetchBudget = 15 * time.Second
	versionRetryAfterFailure = 60 * time.Second
	browserUserAgent         = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

var (
	semverRE   = regexp.MustCompile("^([0-9]+)\\.([0-9]+)\\.([0-9]+)$")
	releaseRE  = regexp.MustCompile("releases/([0-9]+\\.[0-9]+\\.[0-9]+)/")
	clientUARE = regexp.MustCompile("ZCode/([0-9]+\\.[0-9]+\\.[0-9]+)")
)

type versionResolver struct {
	mu       sync.Mutex
	value    string
	fetched  time.Time
	failed   time.Time
	inflight bool
}

var appVersions = &versionResolver{}

// appVersion 返回本次请求应当使用的 ZCode 客户端版本。
// 动态来源只在后台刷新, 请求路径上永远读缓存, 不会因网络阻塞请求。
func appVersion(cfg pluginConfig) string {
	static := strings.TrimSpace(cfg.AppVersion)
	if static == "" {
		static = defaultAppVersion
	}
	floor := strings.TrimSpace(cfg.AppVersionFloor)
	if floor == "" {
		floor = defaultAppVersionFloor
	}
	source := strings.ToLower(strings.TrimSpace(cfg.VersionSource))
	if source == "" || source == versionSourceStatic {
		if best := maxVersion(static, floor); best != "" {
			return best
		}
		return static
	}
	appVersions.refreshAsync(cfg, source)
	if best := maxVersion(static, floor, appVersions.current()); best != "" {
		return best
	}
	return static
}

func (r *versionResolver) current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value
}

func (r *versionResolver) refreshAsync(cfg pluginConfig, source string) {
	ttl := time.Duration(cfg.VersionRefreshSecs) * time.Second
	if ttl <= 0 {
		ttl = defaultVersionRefreshSecs * time.Second
	}

	r.mu.Lock()
	if r.inflight {
		r.mu.Unlock()
		return
	}
	if !r.fetched.IsZero() && time.Since(r.fetched) < ttl {
		r.mu.Unlock()
		return
	}
	if !r.failed.IsZero() && time.Since(r.failed) < versionRetryAfterFailure {
		r.mu.Unlock()
		return
	}
	r.inflight = true
	r.mu.Unlock()

	go func() {
		version, err := resolveClientVersion(cfg, source)
		r.record(version, err)
	}()
}

// record 写入一次解析结果: 失败只记冷却时间, 成功则只升不降。
// compareVersion 对空串返回 0, 所以首次写入必须单独判断, 否则缓存永远为空。
func (r *versionResolver) record(version string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.inflight = false
	if err != nil || version == "" {
		r.failed = time.Now()
		return
	}
	r.failed = time.Time{}
	r.fetched = time.Now()
	if r.value == "" || compareVersion(version, r.value) > 0 {
		r.value = version
	}
}

// resolveClientVersion 按来源取版本。auto 会同时查本地日志与官网发版页并取较大者:
// 本地日志可能是旧客户端(如 3.14.4), 只查它会被 app_version_floor 顶回底线而不是最新版;
// 官网发版页是当前最新桌面端版本, 但机器可能根本连不上外网。
func resolveClientVersion(cfg pluginConfig, source string) (string, error) {
	local := ""
	if source == versionSourceLocal || source == versionSourceAuto {
		local = localClientVersion(cfg.VersionLocalPaths)
		if source == versionSourceLocal {
			if local == "" {
				return "", fmt.Errorf("no local ZCode client version found")
			}
			return local, nil
		}
	}

	remote, err := remoteClientVersion(cfg.VersionCheckURL)
	if source == versionSourceRemote {
		return remote, err
	}

	best := maxVersion(local, remote)
	if best == "" {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("no ZCode client version resolved")
	}
	return best, nil
}

// localClientVersion 从本机 ZCode 桌面客户端的 rollout 日志里取 User-Agent 版本。
// 日志每行是一次模型请求, request.headers 里带着当时客户端真实发送的 user-agent。
func localClientVersion(extraDirs []string) string {
	dirs := append(defaultRolloutDirs(), extraDirs...)
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		for _, path := range newestFiles(dir, localRolloutFilesToScan) {
			best := maxVersion(matchesVersion(clientUARE, readTail(path, localRolloutTailBytes))...)
			if best != "" {
				return best
			}
		}
	}
	return ""
}

func defaultRolloutDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".zcode", "cli", "rollout")}
}

// newestFiles 返回目录下按修改时间倒序的前 n 个 *.jsonl。
func newestFiles(dir string, n int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	files := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, candidate{path: filepath.Join(dir, entry.Name()), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	if len(files) > n {
		files = files[:n]
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths
}

func readTail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	return string(data)
}

// remoteClientVersion 从官网发版页的 releases/<semver>/ 下载链接里取最大版本。
// 该页面是服务端渲染的静态 HTML, 不需要登录, 最大版本就是当前最新桌面端版本。
func remoteClientVersion(checkURL string) (string, error) {
	checkURL = strings.TrimSpace(checkURL)
	if checkURL == "" {
		checkURL = defaultVersionCheckURL
	}
	req, err := http.NewRequest(http.MethodGet, checkURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	client := &http.Client{Timeout: remoteVersionFetchBudget}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("version check HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, remoteVersionFetchMax))
	if err != nil {
		return "", err
	}
	best := maxVersion(matchesVersion(releaseRE, string(body))...)
	if best == "" {
		return "", fmt.Errorf("no release version found at %s", checkURL)
	}
	return best, nil
}

func matchesVersion(re *regexp.Regexp, body string) []string {
	matches := re.FindAllStringSubmatch(body, -1)
	versions := make([]string, 0, len(matches))
	for _, match := range matches {
		versions = append(versions, match[1])
	}
	return versions
}

func versionParts(value string) ([]int, bool) {
	match := semverRE.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return nil, false
	}
	parts := make([]int, 3)
	for i := 1; i <= 3; i++ {
		number, err := strconv.Atoi(match[i])
		if err != nil {
			return nil, false
		}
		parts[i-1] = number
	}
	return parts, true
}

func compareVersion(a, b string) int {
	left, okLeft := versionParts(a)
	right, okRight := versionParts(b)
	if !okLeft || !okRight {
		return 0
	}
	for i := 0; i < 3; i++ {
		if left[i] == right[i] {
			continue
		}
		if left[i] > right[i] {
			return 1
		}
		return -1
	}
	return 0
}

// maxVersion 返回合法的最大值; 全部非法时返回空串。
func maxVersion(versions ...string) string {
	best := ""
	for _, version := range versions {
		if _, ok := versionParts(version); !ok {
			continue
		}
		version = strings.TrimSpace(version)
		if best == "" || compareVersion(version, best) > 0 {
			best = version
		}
	}
	return best
}
