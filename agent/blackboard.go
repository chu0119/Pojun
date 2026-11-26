package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 上下文分层（v0.4.105 / P1-1）：
//
// 态势不再整块 JSON 无差别注入，而是固定三层：
//
//	Index   ≤3KB，永远注入：任务/目标缺口/前沿计数/hints/终态经验/失败方向/覆盖缺口
//	Summary 按需（graph_overview 默认）：最新 N 条事实与漏洞、意图窗口、覆盖矩阵摘要
//	Raw    按需（node_detail / list_facts / list_findings / expand_digest）：完整证据
//
// 分层只发生在【渲染层】，存储层完全不动——被截掉的内容都还能按 id 取回。
// 两条不可折叠的约束（写在这里是因为它们最容易被"压缩"掉）：
//   - hints 是人类/主 agent 挂在图上的战略提示，永远进 Index；
//   - recent_lessons 与失败/零产方向是"别再这么打"的经验，永远进 Index。
//
// 预算吃紧时先删可选段（最新事实/漏洞/缺口），再逐级缩短 hints/lessons 的正文，
// 最后只留 id——但绝不把整类信息折叠成一句摘要或删掉条目。
const (
	// blackboardIndexBudgetBytes 是 Index 层的硬预算（UTF-8 字节，中文约 3 字节/字）。
	blackboardIndexBudgetBytes = 3000
	// blackboardSummaryBudgetBytes 是 Summary 层的目标预算。超过就按固定顺序降级，
	// 保证 planner 每轮注入的态势有上界，而不是随任务规模线性膨胀。
	blackboardSummaryBudgetBytes = 12000
)

// blackboardPlannerGuidance 是代码拥有的提示尾巴（不随 DB 里的可编辑 prompt 丢失）：
// 说明三层读取契约，避免模型把"摘要窗口"误当成"全量事实"。
const blackboardPlannerGuidance = `

**上下文分层读取契约（v0.4.105）**：每轮注入的态势分三层，按需取用——
- **Index（每轮必读，≤3KB）**：目标缺口、前沿计数、hints、终态经验、失败/零产方向。hints 与失败经验**永不折叠**，忽略它们会导致重复踩坑。
- **Summary（默认随 Index 注入）**：最新 8 条事实/漏洞、open/running 窗口、覆盖缺口。**这是窗口、不是全量**。
- **Raw（按需）**：node_detail(id) 看完整证据；list_facts / list_findings 翻更早的记录；expand_digest(id) 展开冷区。摘要里没看到的旧事实不代表不存在——做"是否已覆盖/是否已试过"判断前先翻页确认。`

// BlackboardView 是态势渲染层级。
type BlackboardView int

const (
	BlackboardIndex BlackboardView = iota
	BlackboardSummary
	BlackboardFull
)

func (v BlackboardView) String() string {
	switch v {
	case BlackboardIndex:
		return "index"
	case BlackboardFull:
		return "full"
	default:
		return "summary"
	}
}

// parseBlackboardView 接受 index/summary/full（也接受 detail/raw 作为 full 的别名）。
func parseBlackboardView(s string) BlackboardView {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "index", "idx", "brief":
		return BlackboardIndex
	case "full", "detail", "raw", "all":
		return BlackboardFull
	default:
		return BlackboardSummary
	}
}

// renderBlackboard 生成指定层级的态势文本。
func renderBlackboard(data map[string]any, view BlackboardView) string {
	switch view {
	case BlackboardIndex:
		return renderBlackboardIndex(data)
	case BlackboardFull:
		b, err := json.Marshal(data)
		if err != nil {
			return ""
		}
		return "\n\n【完整态势（graph_overview view=full）】：\n" + string(b)
	default:
		return renderGraphOverview(data)
	}
}

// renderBlackboardIndex 渲染 ≤3KB 的索引。它可单独调用（worker/planner 注入、
// graph_overview view=index），也可作为 summary/full 的前导。
func renderBlackboardIndex(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	// 逐级压缩正文，直到落在预算内。id/计数/结构永远保留；预算吃紧时先丢
	// 可选段（缺口/最新事实/漏洞），再缩短 hints/经验/失败方向的正文——这三类
	// 条目不删除（最多只留 id）。
	type attempt struct {
		cap      int
		optional bool
	}
	for _, a := range []attempt{
		{56, true}, {40, true}, {28, true},
		{24, false}, {16, false}, {8, false}, {0, false},
	} {
		s := buildBlackboardIndex(data, a.cap, a.optional)
		if len(s) <= blackboardIndexBudgetBytes {
			return s
		}
	}
	return buildBlackboardIndex(data, 0, false)
}

func buildBlackboardIndex(data map[string]any, textCap int, optional bool) string {
	var b strings.Builder
	b.WriteString("【态势索引】")
	if task := mapValue(data["task"]); task != nil {
		if g := textValue(task["goal"]); g != "" {
			b.WriteString("目标 " + clipText(g, 80))
		} else if d := textValue(task["description"]); d != "" {
			b.WriteString("任务 " + clipText(d, 80))
		}
	}
	b.WriteString("\n")

	// 目标：未达成优先，全部列出 id/状态/文本。目标数量通常很小。
	goals := sliceMaps(data["goals"])
	unmet, met := 0, 0
	var goalLines []string
	for _, g := range goals {
		state := textValue(g["state"])
		if state == "met" {
			met++
			continue
		}
		unmet++
		if len(goalLines) < 6 {
			line := fmt.Sprintf("#%v[%s] %s", g["id"], state, clipText(textValue(g["text"]), indexCap(textCap, 64)))
			if c := textValue(g["criteria"]); c != "" {
				line += " | 验收: " + clipText(c, indexCap(textCap, 48))
			}
			goalLines = append(goalLines, line)
		}
	}
	b.WriteString(fmt.Sprintf("目标: %d/%d 未达成", unmet, len(goals)))
	if met > 0 {
		b.WriteString(fmt.Sprintf("（已达成 %d）", met))
	}
	if len(goalLines) > 0 {
		b.WriteString(": " + strings.Join(goalLines, "; "))
	}
	b.WriteString("\n")

	// 计数：一行看完规模，避免模型自己数列表。planner 有前沿/终态计数；
	// worker 的瘦身态势没有这些字段，就只报事实/漏洞数，不伪造 0。
	if _, ok := data["frontier_open"]; ok {
		b.WriteString(fmt.Sprintf("规模: open=%v running=%v done=%v facts=%v findings=%v",
			valueOrZero(data["frontier_open"]), len(sliceMaps(data["running_intents"])),
			valueOrZero(data["done_intents_total"]), valueOrZero(data["facts"]), valueOrZero(data["findings_total"])))
	} else {
		b.WriteString(fmt.Sprintf("规模: facts=%v findings=%v",
			valueOrZero(data["facts"]), valueOrZero(data["findings_total"])))
	}
	if cov := mapValue(data["coverage"]); cov != nil {
		if pct, ok := cov["pct"]; ok && pct != nil {
			b.WriteString(fmt.Sprintf(" coverage=%v%%", pct))
		}
		b.WriteString(fmt.Sprintf(" hosts=%v", valueOrZero(cov["host_count"])))
	}
	b.WriteString("\n")

	// hints：战略提示，不折叠。最多展示 10 条正文，其余只列 id（仍然可见）。
	hints := sliceMaps(data["hints"])
	if len(hints) > 0 {
		b.WriteString("HINTS(必读,不折叠):\n")
		for i, h := range hints {
			if i >= 10 {
				var ids []string
				for _, rest := range hints[i:] {
					ids = append(ids, fmt.Sprintf("#%v", rest["id"]))
				}
				b.WriteString(" +其余 " + strings.Join(ids, ",") + "\n")
				break
			}
			b.WriteString(fmt.Sprintf("- #%v[%s] %s\n", h["id"], textValue(h["state"]), clipText(textValue(h["text"]), indexCap(textCap, 72))))
		}
	} else {
		b.WriteString("HINTS: 无\n")
	}

	// 终态经验：引擎确定性写入的 lesson，不折叠。
	lessons := sliceMaps(data["recent_lessons"])
	if len(lessons) > 0 {
		b.WriteString("经验(不折叠):\n")
		for i, r := range lessons {
			if i >= 8 {
				b.WriteString(fmt.Sprintf("- +%d 条见 graph_overview(view=summary)\n", len(lessons)-i))
				break
			}
			lesson := textValue(r["lesson"])
			if lesson == "" {
				lesson = textValue(r["reason"])
			}
			b.WriteString(fmt.Sprintf("- #%v[%s/%s] %s\n", r["intent_id"], textValue(r["dimension"]), textValue(r["state"]), clipText(lesson, indexCap(textCap, 72))))
		}
	}

	// 失败/零产方向：这些是 planner 最容易重复踩的坑，永远进索引。
	failed := failedIntents(data, 8)
	if len(failed) > 0 {
		b.WriteString("失败/零产(不折叠):\n")
		for _, it := range failed {
			b.WriteString(fmt.Sprintf("- #%v %s yields=%d %s | %s\n",
				it["id"], textValue(it["state"]), len(anySlice(it["yields"])),
				clipText(textValue(it["stop_reason"]), indexCap(textCap, 40)),
				clipText(textValue(it["summary"]), indexCap(textCap, 48))))
		}
	}

	// 覆盖缺口：给 planner 的补测线索（可选段，预算不足时最先被挤掉）。
	var gaps []string
	if cov := mapValue(data["coverage"]); cov != nil {
		if m := mapValue(cov["matrix"]); m != nil {
			for i, g := range sliceMaps(m["top_gaps"]) {
				if i >= 3 {
					break
				}
				gaps = append(gaps, fmt.Sprintf("%s×%s 未测%d", textValue(g["dimension"]), textValue(g["asset_type"]), intValue(g["untested"])))
			}
		}
	}
	if optional {
		if len(gaps) > 0 {
			b.WriteString("缺口: " + strings.Join(gaps, "; ") + "\n")
		}

		// 最新事实/漏洞：只给 id + 一行摘要，正文/证据走 Raw 层。
		facts := sliceMaps(data["recent_facts"])
		if len(facts) > 0 {
			b.WriteString("最新事实: ")
			for i, f := range facts {
				if i >= 3 {
					break
				}
				if i > 0 {
					b.WriteString(" | ")
				}
				b.WriteString(fmt.Sprintf("#%v %s", f["id"], clipText(textValue(f["summary"]), indexCap(textCap, 48))))
			}
			b.WriteString("\n")
		}
		findings := sliceMaps(data["finding_list"])
		if len(findings) > 0 {
			b.WriteString("最新漏洞: ")
			for i, f := range findings {
				if i >= 3 {
					break
				}
				if i > 0 {
					b.WriteString(" | ")
				}
				b.WriteString(fmt.Sprintf("#%v %s", f["id"], clipText(textValue(f["summary"]), indexCap(textCap, 48))))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// failedIntents 抽取"失败或零产"的终态意图。判定宽于 state：done 但零 yields 也算
// （正常跑完却什么都没写回 = 空转收场），stop_reason 带"无增量/产出率归零"同样算。
func failedIntents(data map[string]any, limit int) []map[string]any {
	done := sliceMaps(data["recent_done_intents"])
	out := make([]map[string]any, 0, limit)
	for _, it := range done {
		state := textValue(it["state"])
		reason := textValue(it["stop_reason"])
		zeroYield := len(anySlice(it["yields"])) == 0
		isFailed := state == "exhausted" || state == "blocked" || zeroYield ||
			strings.Contains(reason, "无增量") || strings.Contains(reason, "产出率") || strings.Contains(reason, "无写回")
		if !isFailed {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// blackboardSummaryData 生成 Summary 层：保留决策需要的窗口，删掉每轮重复的大块
// 静态信息（完整工具清单、冷区全文、关联任务长文）。所有被删内容仍可按 id 取回。
func blackboardSummaryData(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	out["view"] = "summary"
	out["note"] = "这是态势摘要：最新事实/漏洞只给窗口，原始证据用 node_detail(id)；更早的用 list_facts/list_findings 翻页；完整快照 graph_overview(view=full)。"

	if task := mapValue(data["task"]); task != nil {
		out["task"] = map[string]any{
			"description": clipText(textValue(task["description"]), 800),
			"goal":        clipText(textValue(task["goal"]), 800),
		}
	}
	// 窗口大小：事实/漏洞各 8 条；open/running 各 8 条；recent_done 保留失败方向
	// 优先、其余最新补齐 10 条——失败方向即使很旧也不会被窗口挤掉。
	if v, ok := data["recent_facts"]; ok {
		out["recent_facts"] = capSliceMaps(v, 8)
	}
	if v, ok := data["finding_list"]; ok {
		out["finding_list"] = capSliceMaps(v, 8)
	}
	if v, ok := data["open_intents"]; ok {
		out["open_intents"] = sanitizeIntents(v, 8)
	}
	if v, ok := data["running_intents"]; ok {
		out["running_intents"] = sanitizeIntents(v, 8)
	}
	out["recent_done_intents"] = selectRecentDone(data, 10)

	// 冷区摘要：保留 id/成员数 + 正文首段；全文用 expand_digest。
	if cds := sliceMaps(data["cold_digests"]); len(cds) > 0 {
		kept := make([]map[string]any, 0, len(cds))
		for i, d := range cds {
			if i >= 8 {
				break
			}
			m := map[string]any{"id": d["id"], "member_count": d["member_count"]}
			if body := textValue(d["body"]); body != "" {
				m["body_head"] = clipText(body, 240)
			}
			kept = append(kept, m)
		}
		out["cold_digests"] = kept
		if more := anySlice(data["cold_digests_more"]); len(more) > 0 {
			out["cold_digests_more_count"] = len(more)
			delete(out, "cold_digests_more")
		}
	}

	// 工具清单：摘要层只给名字，不重复每轮几十条用途说明；完整清单 view=full。
	if tools := mapValue(data["tools"]); tools != nil {
		m := map[string]any{}
		if av := anySlice(tools["available"]); len(av) > 0 {
			names := make([]string, 0, len(av))
			for _, x := range av {
				s := textValue(x)
				if i := strings.IndexByte(s, ':'); i > 0 {
					s = s[:i]
				}
				names = append(names, strings.TrimSpace(s))
			}
			m["available"] = names
		}
		if ms := anySlice(tools["missing"]); len(ms) > 0 {
			m["missing_count"] = len(ms)
		}
		m["hint"] = "available 只列命令名；不熟悉的工具先用 --help 确认真实能力，完整用途清单用 graph_overview(view=full)。"
		out["tools"] = m
	}

	// coverage：只要分母/已测/百分比 + 矩阵计数 + 前 5 缺口；by_dimension 全量太大。
	if cov := mapValue(data["coverage"]); cov != nil {
		m := map[string]any{}
		for _, k := range []string{"denominator", "tested", "pct", "status", "host_count"} {
			if v, ok := cov[k]; ok {
				m[k] = v
			}
		}
		if mx := mapValue(cov["matrix"]); mx != nil {
			mxOut := map[string]any{}
			for _, k := range []string{"assets", "tested", "in_progress", "untested_assets", "findings"} {
				if v, ok := mx[k]; ok {
					mxOut[k] = v
				}
			}
			if gaps := sliceMaps(mx["top_gaps"]); len(gaps) > 0 {
				mxOut["top_gaps"] = capSliceMaps(gaps, 5)
			}
			m["matrix"] = mxOut
		}
		out["coverage"] = m
	}

	// 关联任务：摘要层只留前 2 个来源的概要，完整内容走 view=full / 对应只读工具。
	if rt := mapValue(data["related_tasks"]); rt != nil {
		if list := sliceMaps(rt["tasks"]); len(list) > 0 {
			out["related_tasks"] = map[string]any{
				"tasks": capSliceMaps(list, 2),
				"note":  "摘要仅展示前 2 个来源任务；完整内容 graph_overview(view=full)。",
			}
		}
	}
	return out
}

// selectRecentDone 保留失败/零产方向优先，再用最新意图补齐到 limit。
func selectRecentDone(data map[string]any, limit int) []map[string]any {
	done := sliceMaps(data["recent_done_intents"])
	if len(done) <= limit {
		out := make([]map[string]any, 0, len(done))
		for _, it := range done {
			out = append(out, sanitizeIntent(it))
		}
		return out
	}
	seen := make(map[string]bool, len(done))
	out := make([]map[string]any, 0, limit)
	for _, it := range failedIntents(data, limit) {
		out = append(out, sanitizeIntent(it))
		seen[fmt.Sprint(it["id"])] = true
	}
	for _, it := range done {
		if len(out) >= limit {
			break
		}
		if seen[fmt.Sprint(it["id"])] {
			continue
		}
		out = append(out, sanitizeIntent(it))
	}
	return out
}

// sanitizeIntent 去掉 payload 里可能很长、但摘要层不需要的字段。
func sanitizeIntent(it map[string]any) map[string]any {
	m := map[string]any{}
	keep := []string{"id", "summary", "state", "stop_reason", "yields", "parents", "asset_ids", "tokens", "wall_ms", "dimension", "source_task_id", "inherited"}
	for _, k := range keep {
		if v, ok := it[k]; ok {
			m[k] = v
		}
	}
	if s := textValue(m["summary"]); len([]rune(s)) > 240 {
		m["summary"] = clipText(s, 240)
	}
	return m
}

// sanitizeIntents 对窗口内每条意图做字段裁剪（summary 限 240 字），避免单条
// 超长意图摘要把 Summary 预算吃光。
func sanitizeIntents(v any, limit int) []map[string]any {
	xs := capSliceMaps(v, limit)
	out := make([]map[string]any, 0, len(xs))
	for _, it := range xs {
		out = append(out, sanitizeIntent(it))
	}
	return out
}

// trimSummaryToBudget 按固定降级顺序把 Summary 压到预算内。降级顺序刻意把
// hints/recent_lessons/recent_done_intents 放在最后且不删除——这三类是不折叠项。
func trimSummaryToBudget(data map[string]any) map[string]any {
	if jsonBytes(data) <= blackboardSummaryBudgetBytes {
		return data
	}
	// 1) 丢掉关联任务、守卫归因、冷区正文（都可由只读工具取回）。
	delete(data, "related_tasks")
	delete(data, "guard_attributions")
	if cds := sliceMaps(data["cold_digests"]); len(cds) > 0 {
		for _, d := range cds {
			if body := textValue(d["body_head"]); len([]rune(body)) > 80 {
				d["body_head"] = clipText(body, 80)
			}
		}
	}
	if jsonBytes(data) <= blackboardSummaryBudgetBytes {
		return data
	}
	// 2) 意图窗口先收窄（事实/漏洞窗口是决策输入，尽量留到最后再缩）。
	for _, step := range []struct {
		key string
		n   int
	}{{"open_intents", 5}, {"running_intents", 5}} {
		if v, ok := data[step.key]; ok {
			data[step.key] = capSliceMaps(v, step.n)
		}
	}
	if jsonBytes(data) <= blackboardSummaryBudgetBytes {
		return data
	}
	// 3) 覆盖矩阵只留计数，去掉缺口。
	if cov := mapValue(data["coverage"]); cov != nil {
		if mx := mapValue(cov["matrix"]); mx != nil {
			delete(mx, "top_gaps")
		}
	}
	if jsonBytes(data) <= blackboardSummaryBudgetBytes {
		return data
	}
	// 4) 最后才动不折叠项：正文逐级截短，但条目一个不删。先压失败方向与经验，
	//    再压 hints——hints 是人的指令，优先级最高。
	for _, capRunes := range []int{120, 80, 40, 24, 0} {
		for _, m := range sliceMaps(data["goals"]) {
			shortenField(m, "text", capRunes)
			shortenField(m, "criteria", capRunes)
		}
		if task := mapValue(data["task"]); task != nil {
			shortenField(task, "description", capRunes)
			shortenField(task, "goal", capRunes)
		}
		for _, m := range sliceMaps(data["recent_done_intents"]) {
			shortenField(m, "summary", capRunes)
			shortenField(m, "stop_reason", capRunes)
		}
		for _, key := range []string{"open_intents", "running_intents"} {
			for _, m := range sliceMaps(data[key]) {
				shortenField(m, "summary", capRunes)
			}
		}
		for _, m := range sliceMaps(data["recent_lessons"]) {
			shortenField(m, "lesson", capRunes)
			shortenField(m, "reason", capRunes)
			shortenField(m, "summary", capRunes)
		}
		for _, m := range sliceMaps(data["hints"]) {
			shortenField(m, "text", capRunes)
		}
		if jsonBytes(data) <= blackboardSummaryBudgetBytes {
			return data
		}
	}
	// 5) 仍超预算才缩事实/漏洞窗口（8→5→3）。
	for _, step := range []struct {
		key string
		n   int
	}{{"recent_facts", 5}, {"finding_list", 5}, {"recent_facts", 3}, {"finding_list", 3}} {
		if v, ok := data[step.key]; ok {
			data[step.key] = capSliceMaps(v, step.n)
		}
		if jsonBytes(data) <= blackboardSummaryBudgetBytes {
			return data
		}
	}
	return data
}

func shortenField(m map[string]any, key string, n int) {
	s := textValue(m[key])
	if s == "" {
		return
	}
	m[key] = clipText(s, n)
}

func jsonBytes(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func indexCap(cap int, max int) int {
	if cap <= 0 {
		return 0
	}
	if cap < max {
		return cap
	}
	return max
}

func clipText(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func mapValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func sliceMaps(v any) []map[string]any {
	switch xs := v.(type) {
	case []map[string]any:
		return xs
	case []CoverageGap:
		out := make([]map[string]any, 0, len(xs))
		for _, g := range xs {
			out = append(out, map[string]any{
				"dimension": g.Dimension, "label": g.Label, "asset_type": g.AssetType,
				"untested": g.Untested, "samples": g.Samples, "score": g.Score,
			})
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(xs))
		for _, x := range xs {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func capSliceMaps(v any, n int) []map[string]any {
	xs := sliceMaps(v)
	if n < 0 {
		n = 0
	}
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}

func anySlice(v any) []any {
	switch xs := v.(type) {
	case []any:
		return xs
	case []int64:
		out := make([]any, 0, len(xs))
		for _, x := range xs {
			out = append(out, x)
		}
		return out
	case []string:
		out := make([]any, 0, len(xs))
		for _, x := range xs {
			out = append(out, x)
		}
		return out
	}
	return nil
}

func textValue(v any) string {
	s, _ := v.(string)
	return s
}

func intValue(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func valueOrZero(v any) any {
	if v == nil {
		return 0
	}
	return v
}
