package vulnintel

// StatusSnapshot 聚合状态页所需数据:各源同步水位 + 库内条数。
// 一次查询搞定,前端不需要拼多次请求。
import (
	"context"
	"strings"
	"time"
)

func (s *Store) StatusSnapshot(ctx context.Context) map[string]any {
	sources := []SyncState{}
	rows, err := s.db.QueryContext(ctx, `SELECT source,
		COALESCE(to_char(last_sync_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),''),
		cursor, total_synced, last_error, enabled FROM vuln_sync_state ORDER BY source`)
	if err == nil {
		for rows.Next() {
			var st SyncState
			var last string
			if err := rows.Scan(&st.Source, &last, &st.Cursor, &st.TotalSynced, &st.LastError, &st.Enabled); err == nil {
				if t, e := time.Parse(time.RFC3339, last); e == nil {
					st.LastSyncAt = &t
				}
				sources = append(sources, st)
			}
		}
		rows.Close()
	}
	counts := map[string]int64{}
	for table, q := range map[string]string{
		"cves": `SELECT count(*) FROM vuln_cves`,
		"kev":  `SELECT count(*) FROM vuln_cves WHERE kev`,
		"pocs": `SELECT count(*) FROM vuln_pocs`,
		"ghsa": `SELECT count(*) FROM vuln_cves WHERE source='ghsa'`,
		"epss": `SELECT count(*) FROM vuln_cves WHERE epss IS NOT NULL`,
	} {
		var n int64
		_ = s.db.QueryRowContext(ctx, q).Scan(&n)
		counts[table] = n
	}
	// EPSS 覆盖率：状态页要回答「评分到底补了多少」，光看条数看不出比例。
	// 只更新已有 CVE 的设计下，scored/total 是唯一可信的进度指标。
	var total, scored int64
	_ = s.db.QueryRowContext(ctx, `SELECT count(*), count(epss) FROM vuln_cves`).Scan(&total, &scored)
	pct := 0.0
	if total > 0 {
		pct = float64(scored) * 100 / float64(total)
	}
	return map[string]any{
		"sources": sources,
		"counts":  counts,
		"coverage": map[string]any{
			"epss": map[string]any{
				"scored": scored,
				"total":  total,
				"pct":    pct,
			},
		},
	}
}

// EgressHint 读取用户配置的回连可达地址说明(settings.egress_hint)。
func (s *Store) EgressHint(ctx context.Context) string {
	var v string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='egress_hint'`).Scan(&v)
	return strings.TrimSpace(v)
}

// EgressPortMap 读取端口映射说明(settings.egress_port_map),如 "4444-4450:4444-4450"。
func (s *Store) EgressPortMap(ctx context.Context) string {
	var v string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='egress_port_map'`).Scan(&v)
	return strings.TrimSpace(v)
}
