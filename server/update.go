package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chu0119/pojun/hostd"
	"github.com/chu0119/pojun/selfupdate"
)

// 页面一键更新的 HTTP 面。真正的下载/校验/换装逻辑全在 selfupdate 包里，
// 这里只负责鉴权边界、并发互斥、进度广播，以及把"该退出了"告诉 main。
//
// 重启不由本进程完成：暂存好新版本后进程以 selfupdate.ExitRestart 退出，
// 由守护脚本（start.sh / start.bat，Docker 下是 ENTRYPOINT）重新拉起。

// restartCh 在升级就绪或回滚完成后关闭，main 收到后以 ExitRestart 退出。
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested 返回一个在"请退出并让守护进程重新拉起我"时关闭的 channel。
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState 是本次启动时 selfupdate.Bootstrap 的结论（升级成功 / 刚回滚 /
// 暂存件被丢弃），由 main 注入，供 /api/update/check 如实告诉前端上一次升级的下场。
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState 由 main 在启动时调用一次。
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache 缓存 GitHub 的最新版本查询结果。
//
// 顶栏的"有新版本"提示会在每次整页加载时查一次，而未认证的 GitHub API 是
// 每 IP 每小时 60 次——不缓存的话，多开几个标签页或刷几次页面就把配额耗光了，
// 之后真想更新时反而查不动。用户显式点"检查更新"时可以 force 绕过缓存。
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch 是取数函数，仅为测试留的注入点；为 nil 时走真正的 GitHub 查询。
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 失败结果也缓存一小会儿，否则 GitHub 不可达时每次页面加载都要干等一次超时；
	// 但 TTL 要短，网络恢复后很快就能自己好。
	releaseErrTTL = 2 * time.Minute
	// 查询用的超时。NewClient 的 30 分钟超时是给下载整包用的，查版本不能等那么久。
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// invalidate 清空缓存：切换更新代理后下一次检查/下载必须立即回源，
// 否则旧代理留下的错误要等 2 分钟、成功结果要等 30 分钟才失效。
func (c *releaseCache) invalidate() {
	c.mu.Lock()
	c.rel, c.err, c.at = nil, nil, time.Time{}
	c.mu.Unlock()
}

// 更新链路设置键。
const (
	// settingUpdateProxy 是更新专用代理（空=跟随环境变量/装机/全局）。
	settingUpdateProxy = "update_proxy"
	// settingUpdateLastRun 是最近一次更新的结果（跨重启保留）。
	settingUpdateLastRun = "update_last_run"
)

// updateLastRun 是最近一次更新的结果，跨进程重启保留。
//
// 下载进度本身只存在于进程内；进程重启后前端靠这条记录区分「已更新成功」
// 「更新失败」「上次更新未完成」，而不是只能看到一个变化的版本号。
type updateLastRun struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Status string    `json:"status"` // running | staged | failed
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

// saveUpdateLast 记录最近一次更新状态。写失败不影响升级本身。
func (s *Server) saveUpdateLast(run updateLastRun) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return
	}
	b, err := json.Marshal(run)
	if err != nil {
		return
	}
	if err := s.m.pg.SetSetting(settingUpdateLastRun, string(b)); err != nil {
		log.Printf("[update] 记录更新结果失败：%v", err)
	}
}

// loadUpdateLast 读取最近一次更新状态；没有记录时 ok=false。
func (s *Server) loadUpdateLast() (updateLastRun, bool) {
	var run updateLastRun
	if s == nil || s.m == nil || s.m.pg == nil {
		return run, false
	}
	raw, ok, err := s.m.pg.GetSetting(settingUpdateLastRun)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return run, false
	}
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return run, false
	}
	return run, true
}

// get 返回最新 Release，命中缓存则不访问网络。
//
// 取数期间一直持有锁：并发请求会排队等同一次查询的结果，而不是各自去打 GitHub
// （页面刚加载时多个标签页同时来查，正是最容易触发限流的时刻）。
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// 请求被取消（用户关了标签页）不代表 GitHub 有问题，别把它写进缓存，
	// 否则下一个访客会拿到一条莫名其妙的"已取消"错误。
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress 是推给前端的一条进度。
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // 仅下载阶段有意义；其余为 -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub 持有一次升级的进度并广播给 SSE 订阅者。
//
// running 同时充当互斥：升级期间再次 POST /api/update/apply 直接 409，
// 避免两个 goroutine 同时往同一个 pojun.new 写。
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin 抢占升级权限，已在进行中则返回 false。
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "准备中…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish 结束一次升级。err 为 nil 表示暂存成功，等待重启。
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "更新失败", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "新版本已就绪，正在重启…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout 必须在持有 h.mu 时调用。订阅者 channel 是有缓冲的，满了就丢——
// 进度是可丢弃的瞬时信息，绝不能让一个卡住的 SSE 连接阻塞升级本身。
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck 查询 GitHub 上的最新正式版并与当前版本比较。
//
// 前端也会直连 api.github.com（GitHub 的 CORS 是 *），但**以本接口为准**：
// 下载是后端做的，只有后端能访问 GitHub 才谈得上更新。浏览器能连、服务器连不上
// 的情况很常见（服务器在内网、或代理只配在浏览器上），那时点更新必然失败，
// 不如在检查这一步就如实报错。
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}
	// 运行态：页面刷新后据此自动接回 SSE，而不是把进行中的更新当成“没在更新”。
	cur, running := updHub.snapshot()
	out["update"] = map[string]any{
		"running": running,
		"phase":   cur.Phase,
		"percent": cur.Percent,
		"message": cur.Message,
		"version": cur.Version,
		"error":   cur.Error,
	}
	// 更新代理：原值用于页面回填，生效值+来源用于解释「到底走哪条路」。
	savedProxy, effectiveProxy, proxySource := s.updateProxyInfo()
	out["update_proxy"] = savedProxy
	out["update_proxy_effective"] = effectiveProxy
	out["update_proxy_source"] = proxySource
	// 上次更新结果（跨重启）：页面刷新后仍能看到成功/失败/未完成。
	if last, ok := s.loadUpdateLast(); ok {
		out["last_update"] = last
	}

	// 顶栏提示走缓存（默认）；用户点"检查更新"时带 force=1 强制回源。
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(effectiveProxy)
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 开发构建（dev / git describe 带后缀）没有可比较的版本号。放行只会
		// 用正式版覆盖掉本地正在调试的二进制，所以直接不给更新。
		out["reason"] = fmt.Sprintf("当前版本 %q 不是正式发布版本，已禁用一键更新", current)
	}
	writeJSON(w, 200, out)
}

// updateApply 下载并暂存新版本，完成后让进程退出交给守护脚本重启。
//
// 立刻返回 202，实际工作在后台 goroutine 上跑：整包下载可能要几分钟，
// 挂在请求上会被反代超时掐断。进度走 /api/update/stream。
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// 走缓存：确保装上的就是用户在界面上看到并确认的那个版本。
	client := selfupdate.NewClient(s.updateProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("当前版本 %q 不是正式发布版本，已禁用一键更新", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("当前已是最新版本 %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "已有一个更新正在进行中")
		return
	}
	s.saveUpdateLast(updateLastRun{From: current, To: rel.TagName, Status: "running", At: time.Now()})

	go func() {
		// 刻意用 s.ctx 而不是请求的 ctx：HTTP 响应一返回请求就结束了，
		// 挂在它上面下载会立刻被取消。
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		if err != nil {
			s.saveUpdateLast(updateLastRun{From: current, To: rel.TagName, Status: "failed", Error: err.Error(), At: time.Now()})
		} else {
			s.saveUpdateLast(updateLastRun{From: current, To: rel.TagName, Status: "staged", At: time.Now()})
		}
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 更新失败：%v", err)
			return
		}
		log.Printf("[update] %s → %s 已暂存，即将退出以完成换装", current, rel.TagName)
		// 留一点时间把最后一条进度推给前端，再触发退出。
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback 主动退回上一版本（换装前备份的 pojun.old）。
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "更新正在进行中，无法回滚")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 已手动回滚到上一版本，即将退出以完成切换")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// errUpdateProxyInvalid 区分“代理格式不对”（400）与“设置写不进去”（500）。
var errUpdateProxyInvalid = errors.New("更新代理无效")

// saveUpdateProxy 校验并保存更新代理；空串=清除，回到环境变量/装机/全局回退链。
func (s *Server) saveUpdateProxy(raw string) error {
	clean, err := hostd.ValidateProxy(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w：%v", errUpdateProxyInvalid, err)
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return errors.New("设置存储不可用")
	}
	if err := s.m.pg.SetSetting(settingUpdateProxy, clean); err != nil {
		return err
	}
	// 代理变了，版本查询/下载必须立刻走新链路，不能等缓存过期。
	relCache.invalidate()
	return nil
}

// updateProxyProbe 是代理连通性测试的取数函数，单测可替换，避免打外网。
var updateProxyProbe = selfupdate.FetchLatest

// updateProxyTest 用给定（或当前生效的）代理真正查一次 GitHub Release。
//
// 只访问固定的 Release API，不接受任意 URL——既验证了更新链路，又没有 SSRF 面。
func (s *Server) updateProxyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	clean, err := hostd.ValidateProxy(strings.TrimSpace(req.Proxy))
	if err != nil {
		writeErr(w, 400, "更新代理无效："+err.Error())
		return
	}
	if clean == "" {
		clean = s.updateProxy() // 空=测当前生效代理（可能仍是空=直连）
	}
	ctx, cancel := context.WithTimeout(r.Context(), releaseTimeout)
	defer cancel()
	start := time.Now()
	rel, err := updateProxyProbe(ctx, selfupdate.NewClient(clean))
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok": false, "proxy": clean, "latency_ms": latency, "error": err.Error(),
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "proxy": clean, "latest": rel.TagName, "latency_ms": latency,
	})
}

// updateStream 以 SSE 推送更新进度。
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 先补一条当前状态，页面刷新后能立刻看到进行中的升级。
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
