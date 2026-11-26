package db

// 知识检索 v2：词法（FTS）+ 结构化 facets + 使用先验三路 RRF。
//
// 为什么不用 pg_trgm/向量：库规模是百到千级，命令/标签/正文里最值钱的是
// “原词命中”；先把 commands/tags 纳入 search_text，再用 built-in simple 词典
// 的 tsvector 命中，已经能覆盖大部分场景。向量留到评测证明有收益后再加。
//
// 中文按二元组写入 search_text（kbTokenize），所以「中文长句」也是可检索的：
// 查询被切成重叠 bigram 后用 OR 组合，避免依赖 pg 的中文分词扩展。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Search 是 agent 检索入口：默认 auto（v2 失败/0 命中回退 legacy）。
// 日志只在拿到最终结果后写一次，避免 v2 0 命中 + legacy 命中时把
// 零结果率记成 100%。
func (s *KBStore) Search(q KBSearchQuery) ([]*KBSearchHit, error) {
	started := time.Now()
	mode := strings.ToLower(strings.TrimSpace(q.Mode))
	query := strings.TrimSpace(q.Query)
	if mode == "legacy" {
		hits, err := s.searchLegacy(q)
		if err != nil {
			return nil, err
		}
		if !q.NoLog {
			s.logRetrieval(q, query, len(hits), hits,
				int(time.Since(started).Milliseconds()), "legacy")
		}
		return hits, nil
	}
	hits, candidates, err := s.searchV2(q)
	chosen := "v2"
	if err != nil || (len(hits) == 0 && query != "") {
		// 存量/同步行 search_text 为空时 v2 会 0 命中；先回退 legacy，
		// 启动回填完成后再走 v2。不能只靠「有一行非空」判断。
		var legacy []*KBSearchHit
		legacy, err = s.searchLegacy(q)
		if err != nil {
			return nil, err
		}
		hits, candidates, chosen = legacy, len(legacy), "legacy"
	}
	if !q.NoLog {
		s.logRetrieval(q, query, candidates, hits,
			int(time.Since(started).Milliseconds()), chosen)
	}
	return hits, nil
}

func (s *KBStore) searchV2(q KBSearchQuery) ([]*KBSearchHit, int, error) {
	if q.Limit <= 0 {
		q.Limit = 8
	}
	if q.Limit > 30 {
		q.Limit = 30
	}
	query := strings.TrimSpace(q.Query)
	if query == "" && len(q.Tags) == 0 && len(q.Products) == 0 &&
		len(q.VulnClasses) == 0 && len(q.CVEIDs) == 0 {
		return []*KBSearchHit{}, 0, nil
	}

	conds, args := kbBaseConds(q)
	lexical, err := s.kbLexicalCandidates(query, conds, args)
	if err != nil {
		return nil, 0, err
	}
	structured, err := s.kbStructuredCandidates(q, conds, args)
	if err != nil {
		return nil, 0, err
	}
	var usage []*KBEntry
	if query == "" {
		usage, err = s.kbUsageCandidates(conds, args)
		if err != nil {
			return nil, 0, err
		}
	}

	type agg struct {
		entry    *KBEntry
		score    float64
		channels []string
	}
	byID := map[int64]*agg{}
	add := func(list []*KBEntry, weight float64, channel string) {
		for i, e := range list {
			a := byID[e.ID]
			if a == nil {
				a = &agg{entry: e}
				byID[e.ID] = a
			}
			a.score += weight / float64(60+i+1)
			a.channels = append(a.channels, channel)
		}
	}
	add(lexical, 1.0, "lexical")
	add(structured, 1.1, "facet")
	add(usage, 0.3, "usage")

	out := make([]*KBSearchHit, 0, len(byID))
	for _, a := range byID {
		// quality 只做小幅 boost，避免把“写得长但没用”的条目顶上去。
		score := a.score * (0.85 + 0.15*clamp01(a.entry.QualityScore))
		out = append(out, &KBSearchHit{
			KBEntry:  a.entry,
			Score:    score,
			Channels: dedupeStrings(a.channels),
			Snippet:  kbSnippet(a.entry, query),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Uses != out[j].Uses {
			return out[i].Uses > out[j].Uses
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, len(lexical) + len(structured) + len(usage), nil
}

func kbBaseConds(q KBSearchQuery) ([]string, []any) {
	var conds []string
	var args []any
	statuses := []string{KBStatusApproved}
	if q.IncludeDraft {
		statuses = append(statuses, KBStatusDraft)
	}
	if q.IncludeDeprecated {
		statuses = append(statuses, KBStatusDeprecated)
	}
	ph := make([]string, 0, len(statuses))
	for _, st := range statuses {
		args = append(args, st)
		ph = append(ph, fmt.Sprintf("$%d", len(args)))
	}
	conds = append(conds, fmt.Sprintf("k.status IN (%s)", strings.Join(ph, ",")))
	for _, l := range q.Layers {
		if !ValidKBLayer(l) {
			continue
		}
		args = append(args, l)
		conds = append(conds, fmt.Sprintf("k.layer = $%d", len(args)))
	}
	for _, tag := range q.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		raw, _ := json.Marshal([]string{tag})
		args = append(args, raw)
		conds = append(conds, fmt.Sprintf("k.tags @> $%d::jsonb", len(args)))
	}
	return conds, args
}

func (s *KBStore) kbLexicalCandidates(query string, conds []string, args []any) ([]*KBEntry, error) {
	tokens := kbTokenize(query)
	if len(tokens) == 0 {
		return nil, nil
	}
	tsq := strings.Join(tokens, " | ")
	args = append(args, tsq, 300)
	sqlText := fmt.Sprintf(`SELECT %s FROM kb_entries k
WHERE %s AND k.search_text <> ''
  AND to_tsvector('simple', k.search_text) @@ to_tsquery('simple', $%d)
ORDER BY ts_rank_cd(to_tsvector('simple', k.search_text), to_tsquery('simple', $%d)) DESC,
         k.uses DESC, k.updated_at DESC
LIMIT $%d`, kbEntryCols, strings.Join(conds, " AND "), len(args)-1, len(args)-1, len(args))
	return s.scanKBEntries(sqlText, args...)
}

func (s *KBStore) kbStructuredCandidates(q KBSearchQuery, conds []string, args []any) ([]*KBEntry, error) {
	if len(q.Products) == 0 && len(q.VulnClasses) == 0 && len(q.CVEIDs) == 0 && q.MinQuality <= 0 {
		return nil, nil
	}
	conds = append([]string{}, conds...)
	args = append([]any{}, args...)
	addFacet := func(col string, values []string) {
		for _, v := range values {
			v = strings.ToLower(strings.TrimSpace(v))
			if v == "" {
				continue
			}
			raw, _ := json.Marshal([]string{v})
			args = append(args, raw)
			conds = append(conds, fmt.Sprintf("k.%s @> $%d::jsonb", col, len(args)))
		}
	}
	addFacet("products", q.Products)
	addFacet("vuln_classes", q.VulnClasses)
	addFacet("cve_ids", q.CVEIDs)
	if q.MinQuality > 0 {
		args = append(args, q.MinQuality)
		conds = append(conds, fmt.Sprintf("k.quality_score >= $%d", len(args)))
	}
	args = append(args, 100)
	sqlText := fmt.Sprintf(`SELECT %s FROM kb_entries k WHERE %s
ORDER BY k.quality_score DESC, k.uses DESC, k.updated_at DESC LIMIT $%d`,
		kbEntryCols, strings.Join(conds, " AND "), len(args))
	return s.scanKBEntries(sqlText, args...)
}

func (s *KBStore) kbUsageCandidates(conds []string, args []any) ([]*KBEntry, error) {
	conds = append([]string{}, conds...)
	args = append([]any{}, args...)
	args = append(args, 50)
	sqlText := fmt.Sprintf(`SELECT %s FROM kb_entries k WHERE %s
ORDER BY k.uses DESC, k.updated_at DESC LIMIT $%d`,
		kbEntryCols, strings.Join(conds, " AND "), len(args))
	return s.scanKBEntries(sqlText, args...)
}

func (s *KBStore) scanKBEntries(query string, args ...any) ([]*KBEntry, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*KBEntry{}
	for rows.Next() {
		e, err := scanKBEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *KBStore) logRetrieval(q KBSearchQuery, query string, candidates int, hits []*KBSearchHit, latencyMS int, mode string) {
	ids := make([]int64, 0, len(hits))
	scores := make([]float64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
		scores = append(scores, h.Score)
	}
	rawIDs, _ := json.Marshal(ids)
	rawScores, _ := json.Marshal(scores)
	filters, _ := json.Marshal(map[string]any{"tags": q.Tags, "layers": q.Layers,
		"products": q.Products, "vuln_classes": q.VulnClasses, "cve_ids": q.CVEIDs})
	actor := strings.TrimSpace(q.Actor)
	if actor == "" {
		// 归因必填：宁可记 unknown，也不要落一行空 actor 让统计看起来正常、实际全空。
		actor = "unknown"
	}
	if strings.TrimSpace(mode) == "" {
		mode = "v2"
	}
	query = utf8Clean(query)
	filters = jsonbClean(filters)
	if _, err := s.db.Exec(`INSERT INTO kb_retrieval_logs
		(actor, task_id, mode, query, tokens, filters, candidates, returned, top_ids, top_scores, latency_ms)
		VALUES ($1, $2, $11, $3, $4::text[], $5::jsonb, $6, $7,
		        (SELECT ARRAY(SELECT jsonb_array_elements_text($8::jsonb)::bigint)),
		        (SELECT ARRAY(SELECT jsonb_array_elements_text($9::jsonb)::real)), $10)`,
		actor, q.TaskID, query, marshalStringArray(kbTokenize(query)), string(filters),
		candidates, len(hits), string(rawIDs), string(rawScores), latencyMS, mode); err != nil {
		// 检索本身已经返回；日志写失败只影响归因，不能反过来让搜索失败。
		// 但静默吞掉会让统计“看起来正常、实际全空”，所以至少留一条服务端日志。
		log.Printf("[kb] 检索日志写入失败 actor=%q task=%d: %v", actor, q.TaskID, err)
	}
}

// BuildKBSearchText 把可检索字段压成空格分隔 token。标题/描述/标签重复加权，
// commands 与 detail 取前 8000 字符，避免单条正文把索引撑爆。
func BuildKBSearchText(in KBEntryInput) string {
	var b strings.Builder
	appendTokens := func(text string, repeat int) {
		for i := 0; i < repeat; i++ {
			for _, t := range kbTokenize(text) {
				b.WriteString(t)
				b.WriteByte(' ')
			}
		}
	}
	appendTokens(in.Title, 3)
	appendTokens(in.Description, 2)
	appendTokens(strings.Join(in.Tags, " "), 2)
	for _, c := range in.Commands {
		appendTokens(c, 1)
	}
	appendTokens(firstRunes(in.Detail, 8000), 1)
	appendTokens(strings.Join(in.Products, " "), 2)
	appendTokens(strings.Join(in.VulnClasses, " "), 2)
	appendTokens(strings.Join(in.Platforms, " "), 1)
	appendTokens(strings.Join(in.CVEIDs, " "), 2)
	return strings.TrimSpace(b.String())
}

// ComputeKBQuality 是启发式质量分（0..1），只用于小幅排序，不做硬门槛。
func ComputeKBQuality(in KBEntryInput, uses int, verifiedAt *time.Time) float64 {
	q := 0.15
	q += clamp01(float64(utf8.RuneCountInString(in.Detail)) / 4000 * 0.20)
	q += clamp01(float64(len(in.Commands)) / 3 * 0.20)
	if strings.TrimSpace(in.Source) != "" {
		q += 0.10
	}
	if in.Confidence == KBConfidenceObserved {
		q += 0.10
	} else {
		q += 0.05
	}
	q += clamp01(float64(uses) / 50 * 0.15)
	if verifiedAt != nil {
		q += 0.10
	}
	if in.Status == KBStatusDeprecated {
		q -= 0.25
	}
	if in.Status == KBStatusDraft {
		q -= 0.05
	}
	return clamp01(q)
}

func normalizeKBFacetList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, v := range items {
		v = strings.ToLower(strings.TrimSpace(utf8Clean(v)))
		if v == "" || seen[v] || utf8.RuneCountInString(v) > 120 {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= 64 {
			break
		}
	}
	sort.Strings(out)
	return out
}

var kbCVERe = regexp.MustCompile(`(?i)\bCVE-\d{4}-\d{4,}\b`)

func inferKBFacets(in KBEntryInput) ([]string, []string, []string, []string) {
	products := in.Products
	classes := in.VulnClasses
	platforms := in.Platforms
	cves := in.CVEIDs
	text := in.Title + " " + in.Description + " " + in.Detail + " " + strings.Join(in.Commands, " ")
	if len(cves) == 0 {
		for _, m := range kbCVERe.FindAllString(text, 16) {
			cves = append(cves, strings.ToUpper(m))
		}
	}
	return normalizeKBFacetList(products), normalizeKBFacetList(classes),
		normalizeKBFacetList(platforms), normalizeKBFacetList(cves)
}

func kbSnippet(e *KBEntry, query string) string {
	tokens := kbTokenize(query)
	if len(tokens) == 0 {
		return ""
	}
	text := e.Description + "\n" + strings.Join(e.Commands, "\n") + "\n" + e.Detail
	lower := strings.ToLower(text)
	pos := -1
	for _, t := range tokens {
		if i := strings.Index(lower, strings.ToLower(t)); i >= 0 {
			// 转成 rune 下标再切片：中文/emoji 按字节切会把 UTF-8 截断，
			// JSON 编码后变成 U+FFFD。
			pos = utf8.RuneCountInString(lower[:i])
			break
		}
	}
	if pos < 0 {
		return ""
	}
	runes := []rune(text)
	start := pos - 40
	if start < 0 {
		start = 0
	}
	end := start + 160
	if end > len(runes) {
		end = len(runes)
	}
	return strings.TrimSpace(string(runes[start:end]))
}

func dedupeStrings(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, v := range items {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// refreshKBDerived 在 Patch 后重算 search_text/quality/facets（字段可能被改）。
func (s *KBStore) refreshKBDerived(ctx context.Context, id int64) (*KBEntry, error) {
	e, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	in := kbInputFromEntry(e)
	in.Products, in.VulnClasses, in.Platforms, in.CVEIDs = inferKBFacets(in)
	search := BuildKBSearchText(in)
	quality := ComputeKBQuality(in, e.Uses, e.VerifiedAt)
	prodJSON, _ := marshalKBStrings(in.Products)
	classJSON, _ := marshalKBStrings(in.VulnClasses)
	platJSON, _ := marshalKBStrings(in.Platforms)
	cveJSON, _ := marshalKBStrings(in.CVEIDs)
	if _, err := s.db.ExecContext(ctx, `UPDATE kb_entries SET search_text=$2, quality_score=$3,
		products=$4::jsonb, vuln_classes=$5::jsonb, platforms=$6::jsonb, cve_ids=$7::jsonb,
		verified_at=CASE WHEN status='approved' AND verified_at IS NULL THEN now() ELSE verified_at END
		WHERE id=$1`, id, search, quality, prodJSON, classJSON, platJSON, cveJSON); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func kbInputFromEntry(e *KBEntry) KBEntryInput {
	return KBEntryInput{
		Title: e.Title, Description: e.Description, Detail: e.Detail,
		Commands: e.Commands, Tags: e.Tags, Layer: e.Layer, Origin: e.Origin,
		Status: e.Status, Confidence: e.Confidence, Source: e.Source,
		Products: e.Products, VulnClasses: e.VulnClasses, Platforms: e.Platforms,
		CVEIDs: e.CVEIDs, License: e.License,
	}
}

// KBWriteFields 计算写入 kb_entries 时需要的派生字段（供同步/导入路径复用）。
func KBWriteFields(in KBEntryInput) (search string, quality float64, products, vulnClasses, platforms, cveIDs []string) {
	products, vulnClasses, platforms, cveIDs = inferKBFacets(in)
	search = BuildKBSearchText(in)
	quality = ComputeKBQuality(in, 0, nil)
	return
}

// BackfillDerived 只回填 search_text 为空的存量/同步条目；启动时异步调用，
// 避免 v2 之前的历史行在检索里永久不可见。
func (s *KBStore) BackfillDerived(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM kb_entries WHERE search_text='' ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		// 关库/停机时尽快退出：每条一次 ctx 检查，避免对已关闭的连接继续 UPDATE。
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if _, err := s.refreshKBDerived(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// RebuildDerived 回填存量条目的 search_text/quality/facets（可重复执行）。
func (s *KBStore) RebuildDerived(ctx context.Context, dryRun bool, progress func(done, total int)) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM kb_entries ORDER BY id`)
	if err != nil {
		return 0, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if !dryRun {
			if _, err := s.refreshKBDerived(ctx, id); err != nil {
				return n, err
			}
		}
		n++
		if progress != nil && (i%200 == 0 || i == len(ids)-1) {
			progress(i+1, len(ids))
		}
	}
	return n, nil
}

// searchLegacy 是 v2 之前的关键词粗筛 + token 重叠打分，仅用于 mode=legacy
// 与 v2 0 命中回退（存量行 search_text 为空时）。
func (s *KBStore) searchLegacy(q KBSearchQuery) ([]*KBSearchHit, error) {
	if q.Limit <= 0 {
		q.Limit = 8
	}
	if q.Limit > 30 {
		q.Limit = 30
	}
	query := strings.TrimSpace(q.Query)
	if query == "" && len(q.Tags) == 0 {
		return []*KBSearchHit{}, nil
	}

	var conds []string
	var args []any

	statuses := []string{KBStatusApproved}
	if q.IncludeDraft {
		statuses = append(statuses, KBStatusDraft)
	}
	if q.IncludeDeprecated {
		statuses = append(statuses, KBStatusDeprecated)
	}
	ph := make([]string, 0, len(statuses))
	for _, st := range statuses {
		args = append(args, st)
		ph = append(ph, fmt.Sprintf("$%d", len(args)))
	}
	conds = append(conds, fmt.Sprintf("k.status IN (%s)", strings.Join(ph, ",")))

	if len(q.Layers) > 0 {
		lph := make([]string, 0, len(q.Layers))
		for _, l := range q.Layers {
			if !ValidKBLayer(l) {
				continue
			}
			args = append(args, l)
			lph = append(lph, fmt.Sprintf("$%d", len(args)))
		}
		if len(lph) > 0 {
			conds = append(conds, fmt.Sprintf("k.layer IN (%s)", strings.Join(lph, ",")))
		}
	}
	for _, tag := range q.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		raw, _ := json.Marshal([]string{tag})
		args = append(args, raw)
		conds = append(conds, fmt.Sprintf("k.tags @> $%d::jsonb", len(args)))
	}
	if query != "" {
		var clause string
		clause, args = kbTextOR("k", query, args)
		if clause != "" {
			conds = append(conds, clause)
		}
	}

	// 粗筛上限：打分要读正文，先把候选压到可控规模。
	const prefilter = 300
	args = append(args, prefilter)
	sqlText := fmt.Sprintf(`SELECT %s FROM kb_entries k WHERE %s
ORDER BY (k.uses * 2 + 1) DESC, k.updated_at DESC
LIMIT $%d`, kbEntryCols, strings.Join(conds, " AND "), len(args))

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	qTokens := map[string]struct{}{}
	for _, t := range kbTokenize(query) {
		qTokens[t] = struct{}{}
	}
	if len(qTokens) == 0 {
		// 纯 tag 检索：不打分，按使用次数/新近度返回。
		out := []*KBSearchHit{}
		for rows.Next() {
			e, err := scanKBEntry(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, &KBSearchHit{KBEntry: &e})
			if len(out) >= q.Limit {
				break
			}
		}
		return out, rows.Err()
	}

	hits := []*KBSearchHit{}
	for rows.Next() {
		e, err := scanKBEntry(rows)
		if err != nil {
			return nil, err
		}
		score := kbScore(qTokens, &e)
		if score <= 0 {
			continue
		}
		hits = append(hits, &KBSearchHit{KBEntry: &e, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Uses != hits[j].Uses {
			return hits[i].Uses > hits[j].Uses
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

// kbScore 计算 token 重叠分：标题权重 2，其余权重 1，归一到 [0,1]。
func kbScore(q map[string]struct{}, e *KBEntry) float64 {
	seen := map[string]bool{}
	hits := 0.0
	for _, t := range kbTokenize(e.Title) {
		if _, ok := q[t]; ok && !seen[t] {
			seen[t] = true
			hits += 2
		}
	}
	body := e.Description + " " + strings.Join(e.Tags, " ")
	if d := e.Detail; d != "" {
		if len(d) > 500 {
			d = d[:500]
		}
		body += " " + d
	}
	for _, t := range kbTokenize(body) {
		if _, ok := q[t]; ok && !seen[t] {
			seen[t] = true
			hits++
		}
	}
	if hits == 0 {
		return 0
	}
	return hits / float64(len(q)*2)
}
