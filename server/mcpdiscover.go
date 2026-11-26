package server

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/mcphttp"
)

// mcpHealthCheckTimeout 是单个 MCP 启动/手动健康检查的上限。首次 npx 拉包
// 可能较慢（线上实测 browser MCP 冷启动 26s），30s 会误报；45s 与手动 refresh
// 的上限一致，同时多个坏服务器串行也不会无限拖住启动自检。
const mcpHealthCheckTimeout = 90 * time.Second

// mcpClient is the shared surface of a connected MCP server (stdio, Streamable HTTP,
// or legacy SSE),
// so tools/list and cleanup are handled uniformly regardless of transport.
type mcpClient interface {
	Tools(context.Context) ([]actool.CoreTool, error)
	Close() error
}

// connectMCP dials one MCP server per its transport. Callers must Close the client.
func connectMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	switch m.Transport {
	case "stdio":
		if m.Command == "" {
			return nil, fmt.Errorf("stdio 传输缺少命令")
		}
		// 命令不存在是最常见的失败原因，给出可执行的提示而不是裸 exec error。
		if _, err := exec.LookPath(m.Command); err != nil {
			return nil, fmt.Errorf("命令 %q 不在 PATH 里（%v）：装上它，或在「MCP」页把该服务器停用", m.Command, err)
		}
		return mcp.NewStdioClient(ctx, m.Name, m.Command, jsonStrMap(m.Env), jsonStrSlice(m.Args)...)
	case "http":
		if m.URL == "" {
			return nil, fmt.Errorf("http 传输缺少 URL")
		}
		// env map doubles as HTTP headers (e.g. Authorization).
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse 传输缺少 URL")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	default:
		return nil, fmt.Errorf("未知传输方式 %q", m.Transport)
	}
}

// discoverAndCacheMCP connects to one MCP, lists its tools, and persists the tool
// names to mcp_tools_cache so the UI shows them without a live connection.
func (s *Server) discoverAndCacheMCP(ctx context.Context, m *db.MCPServer) error {
	start := time.Now()
	cl, err := connectMCP(ctx, m)
	if err != nil {
		s.recordMCPHealth(m, false, start, err)
		return err
	}
	defer cl.Close()
	ts, err := cl.Tools(ctx)
	if err != nil {
		s.recordMCPHealth(m, false, start, err)
		return err
	}
	tools := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		tools = append(tools, db.MCPTool{Name: t.Name(), Description: t.Description()})
	}
	if err := s.m.pg.SaveMCPTools(m.ID, tools); err != nil {
		// 工具缓存落库失败不是连接失败：MCP 已经连通、tools/list 也成功了。
		// 把它记成健康失败会让一个纯存储抖动触发 10 分钟熔断（v0.9.90）。
		log.Printf("[mcp] %s 工具缓存写入失败（连接正常，不熔断）: %v", m.Name, err)
	}
	s.recordMCPHealth(m, true, start, nil, len(tools))
	log.Printf("[mcp] %s 健康检查通过：%d 个工具，%dms", m.Name, len(tools), time.Since(start).Milliseconds())
	return nil
}

// recordMCPHealth persists one check result (best-effort) and updates the
// in-memory failure cooldown so agent runs don't immediately re-dial a server
// that just failed at startup.
func (s *Server) recordMCPHealth(m *db.MCPServer, ok bool, start time.Time, cause error, toolCount ...int) {
	if s == nil || s.m == nil || s.m.pg == nil || m == nil || m.ID == 0 {
		return
	}
	latency := time.Since(start)
	msg := ""
	if ok {
		clearMCPFailure(m.Name)
	} else {
		msg = redactMCPError(m, cause)
		markMCPFailed(m.Name, 10*time.Minute)
	}
	n := 0
	if len(toolCount) > 0 {
		n = toolCount[0]
	}
	if err := s.m.pg.SaveMCPHealth(m.ID, ok, latency, msg, n); err != nil {
		log.Printf("[mcp] %s 健康结果写入失败: %v", m.Name, err)
	}
}

var (
	// URL userinfo 与 query 可能带 token；错误信息进 DB/UI 前统一脱敏。
	mcpURLUserInfoRe = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)
	mcpURLQueryRe    = regexp.MustCompile(`\?[^\s"']+`)
)

// redactMCPError makes an MCP error safe to persist and show in the UI:
//   - env values (API keys / headers) are replaced with ***;
//   - URL userinfo and query strings are stripped;
//   - newlines collapsed and the result capped at 500 runes.
func redactMCPError(m *db.MCPServer, cause error) string {
	if cause == nil {
		return ""
	}
	msg := cause.Error()
	if m != nil {
		for _, v := range jsonStrMap(m.Env) {
			if len(v) >= 6 {
				msg = strings.ReplaceAll(msg, v, "***")
			}
		}
	}
	msg = mcpURLUserInfoRe.ReplaceAllString(msg, "${1}***@")
	msg = mcpURLQueryRe.ReplaceAllString(msg, "?***")
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		msg = "连接失败（无错误详情）"
	}
	r := []rune(msg)
	if len(r) > 500 {
		msg = string(r[:500]) + "…"
	}
	return msg
}

// checkMCPHealthOnStartup verifies every enabled MCP, not just those with an
// empty tool cache (v0.4.107). A stale cache previously made a dead server look
// healthy forever; now startup records last_ok/last_error/latency/tool_count so
// the UI can show it. Sequential by design: never spawn many stdio servers at
// once. Best-effort and async — startup never blocks on it.
func (s *Server) checkMCPHealthOnStartup() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] 启动健康检查: 读取列表失败: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, mcpHealthCheckTimeout)
		if err := s.discoverAndCacheMCP(ctx, m); err != nil {
			log.Printf("[mcp] 启动健康检查 %s 失败: %v", m.Name, err)
		}
		cancel()
	}
}
