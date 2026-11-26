package vulnintel

// EPSS（FIRST Exploit Prediction Scoring System）每日评分同步。
//
// 数据形态（官方每日 CSV，当前以 .csv.gz 发布）：
//
//	#model_version:v2025.03.14,score_date:2026-11-23T00:00:00Z
//	cve,epss,percentile
//	CVE-2021-44228,0.99999,1.00000
//
// 只做元数据：EPSS 是「未来 30 天被利用的概率」，不含任何利用代码。同步只更新
// 已存在的 CVE——没有 CPE 的 stub 行对 search_vulns 没价值，硬插 30 万条只会让
// 表膨胀。同步失败/缺分不影响 NVD/KEV 主流程（独立预算，见 sync.go）。

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultEPSSURL 是 FIRST 官方的每日评分地址（gz 形态，约 2.8MB）。
const defaultEPSSURL = "https://epss.empiricalsecurity.com/epss_scores-current.csv.gz"

// EPSSScore 是 CSV 里的一行评分。
type EPSSScore struct {
	CVE        string
	Score      float64
	Percentile float64
	Date       time.Time
}

// ParseEPSS 解析官方 CSV（兼容 gzip magic 与 # 注释行）。date 从注释里的
// score_date 提取，缺失时为零值（写库时保留原有 epss_date）。
func ParseEPSS(r io.Reader) ([]EPSSScore, error) {
	// 生产路径调用方已解压（syncEPSS 直接给 gzip.Reader）；测试/手工调试可能传
	// 原始 *os.File，这里按 magic 记号兜底解压，避免把 gzip 二进制当 CSV 读。
	if f, ok := r.(*os.File); ok {
		var magic [2]byte
		if _, err := io.ReadFull(f, magic[:]); err == nil {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			if magic[0] == 0x1f && magic[1] == 0x8b {
				gz, err := gzip.NewReader(f)
				if err != nil {
					return nil, err
				}
				defer gz.Close()
				r = gz
			}
		}
	}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	var out []EPSSScore
	var scoreDate time.Time
	headerSeen := false
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 EPSS CSV: %w", err)
		}
		if len(rec) == 0 {
			continue
		}
		first := strings.TrimSpace(rec[0])
		if strings.HasPrefix(first, "#") {
			// 注释行里的逗号会被 CSV 拆成多列，拼回去再按 key:value 解析。
			if d := epssDateFromComment(strings.Join(rec, ",")); !d.IsZero() {
				scoreDate = d
			}
			continue
		}
		if !headerSeen {
			if strings.EqualFold(first, "cve") {
				headerSeen = true
				continue
			}
		}
		if len(rec) < 3 {
			continue
		}
		cve := strings.ToUpper(strings.TrimSpace(rec[0]))
		if !strings.HasPrefix(cve, "CVE-") {
			continue
		}
		score, err1 := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
		pct, err2 := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
		if err1 != nil || err2 != nil {
			continue
		}
		// NaN/±Inf 能通过 ParseFloat，但 JSON 无法编码它们：批量 upsert 会整批
		// 失败并丢掉该批之后的所有行（一条坏数据拖垮整份 feed）。在这里跳过。
		if math.IsNaN(score) || math.IsInf(score, 0) || math.IsNaN(pct) || math.IsInf(pct, 0) {
			continue
		}
		out = append(out, EPSSScore{CVE: cve, Score: score, Percentile: pct, Date: scoreDate})
	}
	return out, nil
}

func epssDateFromComment(s string) time.Time {
	for _, part := range strings.Split(strings.TrimPrefix(s, "#"), ",") {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != "score_date" {
			continue
		}
		v := strings.TrimSpace(kv[1])
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// UpsertEPSS 分批更新已有 CVE 的 EPSS 字段，返回实际更新行数。
//
// 返回 count 是「实际命中的行数」而非「提交的行数」：CSV 里有 30 万条，库里
// 只有同步到的那部分 CVE——覆盖率靠这个数字判断，虚报会让状态页失真。
func (s *Store) UpsertEPSS(ctx context.Context, scores []EPSSScore) (int64, error) {
	const batch = 5000
	var updated int64
	for start := 0; start < len(scores); start += batch {
		end := start + batch
		if end > len(scores) {
			end = len(scores)
		}
		n, err := s.upsertEPSSBatch(ctx, scores[start:end])
		if err != nil {
			return updated, err
		}
		updated += n
	}
	return updated, nil
}

func (s *Store) upsertEPSSBatch(ctx context.Context, scores []EPSSScore) (int64, error) {
	type row struct {
		CVE   string  `json:"cve"`
		Score float64 `json:"epss"`
		Pct   float64 `json:"pct"`
		Date  *string `json:"dt"`
	}
	rows := make([]row, 0, len(scores))
	for _, sc := range scores {
		r := row{CVE: sc.CVE, Score: sc.Score, Pct: sc.Percentile}
		if !sc.Date.IsZero() {
			d := sc.Date.UTC().Format("2006-01-02")
			r.Date = &d
		}
		rows = append(rows, r)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return 0, err
	}
	// 一条语句更新整批：41 万条 EPSS 若逐条 Exec 会产生同量往返与 WAL，
	// 这里用 json_to_recordset 让 PG 自己 join。空日期保留原有 epss_date。
	res, err := s.db.ExecContext(ctx, `UPDATE vuln_cves v
		SET epss=s.epss, epss_percentile=s.pct,
		    epss_date=COALESCE(s.dt, v.epss_date)
		FROM json_to_recordset($1::json) AS s(cve text, epss double precision, pct double precision, dt date)
		WHERE v.cve_id=s.cve`, string(raw))
	if err != nil {
		return 0, fmt.Errorf("更新 EPSS 批次: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// syncEPSS 拉取每日 CSV 并更新已有 CVE 的 EPSS 字段。
//
// 失败语义：任何一步出错都写 last_error 并把错误返回（不伪成功）；只有「解析出
// 记录且至少更新到 0 行」才算完成——后者是正常现象（库还没同步到那些 CVE），
// 但如果是 0 条解析记录则明确记错，避免状态页显示「同步成功」却全无数据。
func syncEPSS(ctx context.Context, st *Store, cfg Config) error {
	u := strings.TrimSpace(cfg.EPSSURL)
	if u == "" {
		u = defaultEPSSURL
	}
	transport, err := newFallbackTransport(cfg.ProxyURL)
	if err != nil {
		_ = st.recordError(ctx, "epss", err)
		return err
	}
	client := &http.Client{Timeout: 3 * time.Minute, Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		_ = st.recordError(ctx, "epss", err)
		return err
	}
	req.Header.Set("Accept", "application/gzip,text/csv,*/*")
	resp, err := client.Do(req)
	if err != nil {
		_ = st.recordError(ctx, "epss", err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("epss status %d", resp.StatusCode)
		_ = st.recordError(ctx, "epss", err)
		return err
	}
	body, err := maybeGunzip(resp.Body)
	if err != nil {
		_ = st.recordError(ctx, "epss", err)
		return fmt.Errorf("epss gzip: %w", err)
	}
	scores, err := ParseEPSS(body)
	if err != nil {
		_ = st.recordError(ctx, "epss", err)
		return err
	}
	updated, err := st.UpsertEPSS(ctx, scores)
	state, stateErr := st.GetSyncState(ctx, "epss")
	if stateErr != nil {
		return stateErr
	}
	state.LastSyncAt = nowPtr()
	state.TotalSynced = updated
	state.Enabled = true
	switch {
	case err != nil:
		state.LastError = err.Error()
	case len(scores) == 0:
		state.LastError = "EPSS CSV 没有有效记录"
	case ctx.Err() != nil:
		state.LastError = ctx.Err().Error()
	default:
		state.LastError = ""
	}
	if saveErr := st.SaveSyncState(ctx, state); saveErr != nil && err == nil {
		err = saveErr
	}
	log.Printf("[vulnintel] epss synced: parsed=%d updated=%d", len(scores), updated)
	return err
}

// maybeGunzip 按 magic 判断响应体是否 gzip。官方地址是 .gz，但镜像/代理可能
// 直接回明文 CSV；两种都接受，避免「地址换了格式就整轮失败」。
func maybeGunzip(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		return gzip.NewReader(br)
	}
	return br, nil
}
