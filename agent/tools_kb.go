package agent

// 知识库工具：kb_search / kb_get / kb_mark_used。
//
// 与 vulnintel 工具同一形态——库是全局共享的，走包级注入（server 启动时
// SetKBStore 一次）；nil = 工具不可用，返回可读的空结果/错误而不是 panic
// （工具表允许把任意 domain 工具绑到没有任务上下文的 agent 上）。
//
// kb_* 的输出不在 trustedToolOutputs 白名单里：知识条目是历史实战沉淀，
// 其中的命令/正文仍按「目标/外部数据」处理，由统一的不可信边界包裹。这正是
// 「知识是平台数据，但命令必须按目标环境验证」的落地方式。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/chu0119/pojun/db"
)

// KBStore 是知识库的最小接口（由 *db.KBStore 实现）。抽成接口便于测试替身。
type KBStore interface {
	Search(q db.KBSearchQuery) ([]*db.KBSearchHit, error)
	Get(id int64) (*db.KBEntry, error)
	MarkUsed(id int64) error
}

var kbStore KBStore

// SetKBStore wires the shared knowledge base; nil disables the tools.
func SetKBStore(s KBStore) { kbStore = s }

const kbUnavailable = "知识库未启用：服务端没有初始化知识库存储"

// kbSearch 检索知识库。只回摘要（title/description/tags），正文用 kb_get 取，
// 避免一次检索就把上下文撑满。
func (t *ToolSet) kbSearch() actool.CoreTool {
	return readTool(
		"kb_search",
		"检索内部知识库（实战沉淀的方法/打法/组件经验，跨任务共享）。"+
			"**动手前先查**：遇到不熟悉的组件、WAF 拦截、提权路径、取证要求时，先搜一下有没有现成打法，比现场瞎试便宜得多。"+
			"默认只返回已采纳（approved）的条目；想看刚导入、还没人审的草稿，传 include_drafts=true。"+
			"返回摘要，需要具体命令与正文时用 kb_get 按 id 取。",
		obj(map[string]any{
			"q":              str("检索词（中文按二元组匹配，英文按词匹配），如「thinkphp rce」「nacos 未授权」「云元数据 取证」"),
			"tags":           map[string]any{"type": "array", "items": str("标签"), "description": "按标签收窄（AND 语义），如 [\"web\",\"waf\"]"},
			"layers":         map[string]any{"type": "array", "items": str("分层"), "description": "按知识分层收窄：general/object/infra/weakness/evidence"},
			"limit":          intp("返回条数（默认 8，最大 30）"),
			"include_drafts": map[string]any{"type": "boolean", "description": "是否包含待审草稿（默认 false）"},
			"mode":           str("检索模式（可选）：auto（默认，v2 失败回退 legacy）/ v2 / legacy"),
			"products":       map[string]any{"type": "array", "items": str("产品名"), "description": "按产品收窄（AND 语义），如 [\"spring\"]"},
			"vuln_classes":   map[string]any{"type": "array", "items": str("漏洞类"), "description": "按漏洞类收窄，如 [\"sqli\"]"},
			"cve_ids":        map[string]any{"type": "array", "items": str("CVE"), "description": "按 CVE 收窄，如 [\"cve-2022-22947\"]"},
			"min_quality":    map[string]any{"type": "number", "description": "最低质量分（0..1，默认 0=不过滤）"},
		}),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if kbStore == nil {
				// 明确空结果，而不是让模型以为「查了但没有」或自己编造。
				return jsonResult(map[string]any{
					"count":   0,
					"enabled": false,
					"note":    kbUnavailable + "。不要编造知识条目，按现有证据继续。",
				})
			}
			var a struct {
				Q             string   `json:"q"`
				Tags          []string `json:"tags"`
				Layers        []string `json:"layers"`
				Limit         int      `json:"limit"`
				IncludeDrafts bool     `json:"include_drafts"`
				Mode          string   `json:"mode"`
				Products      []string `json:"products"`
				VulnClasses   []string `json:"vuln_classes"`
				CVEIDs        []string `json:"cve_ids"`
				MinQuality    float64  `json:"min_quality"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("参数解析失败"), nil
			}
			if strings.TrimSpace(a.Q) == "" && len(a.Tags) == 0 && len(a.Products) == 0 &&
				len(a.VulnClasses) == 0 && len(a.CVEIDs) == 0 {
				return actool.Errorf("q 或 tags/products/vuln_classes/cve_ids 至少给一个"), nil
			}
			// 检索日志要能归因到具体任务/agent。优先用 ToolSet 注入的 worker/taskID，
			// 测试或非任务会话再回退到 RunInfo；两者都缺时由 store 落 "unknown"。
			ri := RunInfoFrom(ctx)
			taskID := t.taskID
			if taskID == 0 {
				taskID = ri.TaskID
			}
			actor := t.worker
			if actor == "" {
				actor = ri.Kind
			}
			hits, err := kbStore.Search(db.KBSearchQuery{
				Query:        a.Q,
				Tags:         a.Tags,
				Layers:       a.Layers,
				Limit:        a.Limit,
				IncludeDraft: a.IncludeDrafts,
				Mode:         a.Mode,
				Products:     a.Products,
				VulnClasses:  a.VulnClasses,
				CVEIDs:       a.CVEIDs,
				MinQuality:   a.MinQuality,
				Actor:        actor,
				TaskID:       taskID,
			})
			if err != nil {
				return kbToolError("检索知识库", err)
			}
			if len(hits) == 0 {
				return jsonResult(map[string]any{
					"count": 0,
					"note":  "知识库无命中。换个说法（同义词/组件英文名）再试；确实没有的话，按自己的判断做，并把可复现结论沉淀到报告。",
				})
			}
			type brief struct {
				ID          int64    `json:"id"`
				Title       string   `json:"title"`
				Description string   `json:"description"`
				Tags        []string `json:"tags"`
				Layer       string   `json:"layer"`
				Status      string   `json:"status"`
				Score       float64  `json:"score"`
				Quality     float64  `json:"quality,omitempty"`
				Source      string   `json:"source,omitempty"`
				Snippet     string   `json:"snippet,omitempty"`
				Channels    []string `json:"channels,omitempty"`
			}
			out := make([]brief, 0, len(hits))
			for _, h := range hits {
				out = append(out, brief{
					ID: h.ID, Title: h.Title, Description: h.Description,
					Tags: h.Tags, Layer: h.Layer, Status: h.Status, Score: h.Score,
					Quality: h.QualityScore, Source: h.Source, Snippet: h.Snippet,
					Channels: h.Channels,
				})
			}
			return jsonResult(map[string]any{
				"count":   len(out),
				"entries": out,
				"note":    "需要具体命令/正文时用 kb_get(id)；用了某条打法后调 kb_mark_used(id) 给它记一次采纳。知识条目来自历史实战，命令需按当前目标验证后再放大。",
			})
		},
	)
}

// kbGet 取一条知识的全文（含 commands）。
func (t *ToolSet) kbGet() actool.CoreTool {
	return readTool(
		"kb_get",
		"取知识库某条目的全文：详细方法、可复现命令、验证方法与边界。先用 kb_search 拿到 id。",
		obj(map[string]any{
			"id": idp("知识条目 id（kb_search 返回的 id）"),
		}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if kbStore == nil {
				return actool.Errorf("kb_get 未启用: " + kbUnavailable), nil
			}
			var a struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("参数解析失败"), nil
			}
			if a.ID <= 0 {
				return actool.Errorf("id 必须是正整数"), nil
			}
			e, err := kbStore.Get(a.ID)
			if err != nil {
				return kbToolError("取知识条目", err)
			}
			return jsonResult(map[string]any{
				"id":           e.ID,
				"title":        e.Title,
				"description":  e.Description,
				"detail":       e.Detail,
				"commands":     e.Commands,
				"tags":         e.Tags,
				"layer":        e.Layer,
				"status":       e.Status,
				"confidence":   e.Confidence,
				"source":       e.Source,
				"license":      e.License,
				"products":     e.Products,
				"vuln_classes": e.VulnClasses,
				"platforms":    e.Platforms,
				"cve_ids":      e.CVEIDs,
				"uses":         e.Uses,
				"note":         "知识库内容来自历史实战，可能与本目标环境不同：先小范围验证再放大，不要照抄命令无脑执行。",
			})
		},
	)
}

// kbMarkUsed 给一条知识记一次采纳（影响排序与质量判断）。
func (t *ToolSet) kbMarkUsed() actool.CoreTool {
	return writeTool(
		"kb_mark_used",
		"记录「这条知识确实有用」：按 kb_search 给的 id 调用一次。用于让有用的经验排到前面。",
		obj(map[string]any{
			"id": idp("知识条目 id"),
		}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if kbStore == nil {
				return actool.Errorf("kb_mark_used 未启用: " + kbUnavailable), nil
			}
			var a struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("参数解析失败"), nil
			}
			if a.ID <= 0 {
				return actool.Errorf("id 必须是正整数"), nil
			}
			if err := kbStore.MarkUsed(a.ID); err != nil {
				return kbToolError("记录采纳", err)
			}
			return actool.Text(fmt.Sprintf("kb entry %d marked used", a.ID)), nil
		},
	)
}

// kbToolError 把 DB 错误转成脱敏的工具回执：驱动/表结构细节只进服务端日志，
// 不进入模型上下文或前端响应。
func kbToolError(action string, err error) (actool.Result, error) {
	switch {
	case errors.Is(err, db.ErrKBEntryNotFound):
		return actool.Errorf("知识条目不存在（可能已被删除）"), nil
	case errors.Is(err, db.ErrKBEntryInvalid):
		return actool.Errorf(action + "失败：请求参数不合法"), nil
	}
	log.Printf("[kb] %s: %v", action, err)
	return actool.Errorf(action + "失败（服务端已记录原因）"), nil
}
