package eval

// 知识库检索评测。
//
// 现有 eval 包评的是“任务是否打中预期漏洞”；这里评的是“kb_search 能不能把
// 正确条目排进前 K”。两者口径不同，所以单独建类型，不污染 Scenario/ScoreTask。
// 纯 Go、只读；评测模式不写检索日志（Search 识别 NoLog）。
//
// 内置样本只依赖本仓库随包分发的 3 个 kb-pack（Web/API、Java 中间件、云/K8s），
// 查询与期望标题都写死在代码里，不访问外网、不依赖数据库里的既有 id。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/chu0119/pojun/db"
)

// GoldenQuery 是一条人工/半自动标注的检索查询。ExpectedIDs 与 ExpectedTitles
// 至少给一个：前者用于固定库基线，后者用于跨部署（id 不稳定）的内置样本。
type GoldenQuery struct {
	ID             string   `json:"id"`
	Query          string   `json:"query"`
	ExpectedIDs    []int64  `json:"expected_ids,omitempty"`
	ExpectedTitles []string `json:"expected_titles,omitempty"`
	K              int      `json:"k,omitempty"`
	Note           string   `json:"note,omitempty"`
}

func (q GoldenQuery) expectedCount() int {
	if len(q.ExpectedIDs) > 0 {
		return len(q.ExpectedIDs)
	}
	return len(q.ExpectedTitles)
}

// KBSearcher 是评测需要的最小检索接口；*db.KBStore 直接满足。
type KBSearcher interface {
	Search(db.KBSearchQuery) ([]*db.KBSearchHit, error)
}

// RetrievalMetrics 是一组查询的聚合指标。
type RetrievalMetrics struct {
	Queries      int       `json:"queries"`
	RecallAt5    float64   `json:"recall_at_5"`
	MRRAt10      float64   `json:"mrr_at_10"`
	NDCGAt10     float64   `json:"ndcg_at_10"`
	NoResultRate float64   `json:"no_result_rate"`
	P95LatencyMS float64   `json:"p95_latency_ms"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

// LoadGoldenQueries 读 JSONL（每行一条）。空行与 # 注释跳过。
func LoadGoldenQueries(path string) ([]GoldenQuery, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []GoldenQuery
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var q GoldenQuery
		if err := json.Unmarshal([]byte(text), &q); err != nil {
			return nil, fmt.Errorf("golden line %d: %w", line, err)
		}
		if strings.TrimSpace(q.Query) == "" || q.expectedCount() == 0 {
			continue
		}
		if q.K <= 0 {
			q.K = 10
		}
		out = append(out, q)
	}
	return out, sc.Err()
}

// WriteGoldenQueries 写 JSONL（构建基线用）。
func WriteGoldenQueries(path string, queries []GoldenQuery) error {
	var b strings.Builder
	for _, q := range queries {
		raw, _ := json.Marshal(q)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// BuildGoldenFromKB 用 approved 条目的标题生成“自检查询”。这是基线，不是
// 人工黄金集：它衡量“从标题回查自己”的下限。
func BuildGoldenFromKB(ctx context.Context, kb *db.KBStore, limit int) ([]GoldenQuery, error) {
	if limit <= 0 || limit > 500 {
		limit = 120
	}
	var out []GoldenQuery
	page := 1
	for len(out) < limit {
		entries, total, err := kb.List(db.KBFilter{Status: db.KBStatusApproved}, page, 100)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			title := strings.TrimSpace(e.Title)
			if title == "" {
				continue
			}
			out = append(out, GoldenQuery{
				ID:          fmt.Sprintf("kb-%d", e.ID),
				Query:       title,
				ExpectedIDs: []int64{e.ID},
				K:           10,
				Note:        "title-derived baseline",
			})
			if len(out) >= limit {
				break
			}
		}
		if page*100 >= total {
			break
		}
		page++
	}
	_ = ctx
	return out, nil
}

// EvaluateQueries 逐条检索并计算 Recall@5 / MRR@10 / nDCG@10。
// ExpectedTitles 与命中标题按小写、空白折叠比较；ExpectedIDs 优先。
func EvaluateQueries(ctx context.Context, searcher KBSearcher, queries []GoldenQuery) RetrievalMetrics {
	m := RetrievalMetrics{Queries: len(queries), StartedAt: time.Now()}
	if len(queries) == 0 {
		m.FinishedAt = time.Now()
		return m
	}
	var recall, mrr, ndcg float64
	var noResult int
	latencies := make([]float64, 0, len(queries))
	for _, q := range queries {
		start := time.Now()
		hits, err := searcher.Search(db.KBSearchQuery{Query: q.Query, Limit: 10, NoLog: true})
		latencies = append(latencies, float64(time.Since(start).Milliseconds()))
		if err != nil || len(hits) == 0 {
			noResult++
			continue
		}
		relevant := make([]bool, len(hits))
		idSet := map[int64]bool{}
		for _, id := range q.ExpectedIDs {
			idSet[id] = true
		}
		titleSet := map[string]bool{}
		for _, t := range q.ExpectedTitles {
			titleSet[normGoldenTitle(t)] = true
		}
		for i, h := range hits {
			if idSet[h.ID] || titleSet[normGoldenTitle(h.Title)] {
				relevant[i] = true
			}
		}
		expectedN := q.expectedCount()
		recall += recallAtK(relevant, expectedN, 5)
		mrr += reciprocalRank(relevant, 10)
		ndcg += ndcgAtK(relevant, expectedN, 10)
	}
	n := float64(len(queries))
	m.RecallAt5 = recall / n
	m.MRRAt10 = mrr / n
	m.NDCGAt10 = ndcg / n
	m.NoResultRate = float64(noResult) / n
	m.P95LatencyMS = percentile(latencies, 0.95)
	m.FinishedAt = time.Now()
	_ = ctx
	return m
}

func normGoldenTitle(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func recallAtK(relevant []bool, expectedN, k int) float64 {
	if expectedN == 0 {
		return 0
	}
	hit := 0
	for i, rel := range relevant {
		if i >= k {
			break
		}
		if rel {
			hit++
		}
	}
	if hit > expectedN {
		hit = expectedN
	}
	return float64(hit) / float64(expectedN)
}

func reciprocalRank(relevant []bool, k int) float64 {
	for i, rel := range relevant {
		if i >= k {
			break
		}
		if rel {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func ndcgAtK(relevant []bool, expectedN, k int) float64 {
	dcg := 0.0
	for i, rel := range relevant {
		if i >= k {
			break
		}
		if rel {
			dcg += 1 / log2(float64(i+2))
		}
	}
	ideal := 0.0
	for i := 0; i < expectedN && i < k; i++ {
		ideal += 1 / log2(float64(i+2))
	}
	if ideal == 0 {
		return 0
	}
	return dcg / ideal
}

func log2(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return math.Log2(v)
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	// 最近秩法（nearest-rank）：P95 of 5 = 最大值。用 ceil 而不是 floor，
	// 避免小样本把 P95 报成比实际更好的分位数。
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// BuiltinGoldenQueries 是随二进制分发的检索回归样本：覆盖 3 个已发布
// kb-pack 的中文长句、英文组件名与云元数据场景。期望标题必须与包内标题
// 完全一致（大小写/空白归一后比较）；包未导入时对应查询会记入无结果率。
func BuiltinGoldenQueries() []GoldenQuery {
	return []GoldenQuery{
		{ID: "web-jwt", Query: "JWT alg none kid 注入 弱密钥", ExpectedTitles: []string{"JWT 测试：alg/none、kid 注入与弱密钥"}},
		{ID: "web-idor", Query: "越权 IDOR 双身份 A/B 矩阵", ExpectedTitles: []string{"IDOR/越权的 A/B 双身份矩阵"}},
		{ID: "web-graphql", Query: "GraphQL introspection 批量查询", ExpectedTitles: []string{"GraphQL introspection 与批量查询滥用"}},
		{ID: "web-race", Query: "优惠券 并发 竞态 库存", ExpectedTitles: []string{"竞态条件：优惠券/提现/库存"}},
		{ID: "web-ssrf", Query: "SSRF 云元数据 169.254.169.254 凭据", ExpectedTitles: []string{"SSRF 云元数据验证（AWS/GCP/阿里云）"}},
		{ID: "java-shiro", Query: "Shiro rememberMe 反序列化 deleteMe", ExpectedTitles: []string{"Shiro rememberMe 反序列化：识别与边界"}},
		{ID: "java-log4j", Query: "Log4j2 JNDI OOB User-Agent", ExpectedTitles: []string{"Log4j2 JNDI：入口枚举与 OOB"}},
		{ID: "java-nacos", Query: "Nacos 未授权 默认 token 密钥", ExpectedTitles: []string{"Nacos 未授权/默认 token 密钥"}},
		{ID: "java-actuator", Query: "Spring Boot Actuator heapdump 取证", ExpectedTitles: []string{"Spring Boot Actuator：env/heapdump 取证"}},
		{ID: "cloud-imds", Query: "AWS IMDS 凭据链 角色 STS", ExpectedTitles: []string{"AWS IMDS 凭据链验证"}},
		{ID: "cloud-s3", Query: "S3 桶 匿名 列出 对象", ExpectedTitles: []string{"AWS S3 桶配置错误验证"}},
		{ID: "cloud-k8s", Query: "K8s API Server 匿名 RBAC 探测", ExpectedTitles: []string{"K8s API Server 匿名/RBAC 探测"}},
	}
}
