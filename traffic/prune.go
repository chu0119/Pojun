package traffic

// 流量按时间/容量回收（证据模式）。
//
// 录制库是磁盘增长的主要来源：每条 MITM 交换都会写索引行 + 可能的 blob。
// 全量模式下由服务端补齐 PostgreSQL 保护集后调用 DeleteUnreferencedBefore；
// 证据模式（只长期保留与漏洞/证据有关的流量）则用这里的入口：
//   - PruneUnpinnedBefore：删除 cutoff 之前、没有 evidence_pins 的交换；
//   - PruneUnpinnedToSize：超容量时按最旧优先删除未绑定记录。
//
// pin 由证据入库时写入，服务端在巡检前还会用 PostgreSQL 保护集回填，任何一步
// 失败都中止本轮清理（fail closed），绝不把“读不到保护集”当成“没有保护”。

import (
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
)

// TrafficStats 是存储页展示用的轻量统计。
type TrafficStats struct {
	IndexBytes int64 `json:"index_bytes"`
	BlobBytes  int64 `json:"blob_bytes"`
	Exchanges  int64 `json:"exchanges"`
	OldestTS   int64 `json:"oldest_ts"`
	NewestTS   int64 `json:"newest_ts"`
}

// BlobBytes 返回内容寻址 blob 目录占用的字节数（含两个历史分层布局）。
func (t *Traffic) BlobBytes() int64 {
	if t == nil {
		return 0
	}
	root := filepath.Join(t.dir, "_blobs")
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Stats 汇总索引/blob/条数与时间范围。查询失败时返回已取得的磁盘大小。
func (t *Traffic) Stats() (TrafficStats, error) {
	if t == nil {
		return TrafficStats{}, nil
	}
	st := TrafficStats{IndexBytes: t.indexBytes(), BlobBytes: t.BlobBytes()}
	var oldest, newest *int64
	if err := t.db.QueryRow(`SELECT count(*), min(ts), max(ts) FROM exchanges`).Scan(&st.Exchanges, &oldest, &newest); err != nil {
		return st, err
	}
	if oldest != nil {
		st.OldestTS = *oldest
	}
	if newest != nil {
		st.NewestTS = *newest
	}
	return st, nil
}

// PruneUnpinnedBefore removes exchanges older than cutoff that no finding has
// pinned. Evidence-only capture uses this instead of PruneBefore: findings keep
// their own snapshot copy, and the pin keeps the original searchable.
func (t *Traffic) PruneUnpinnedBefore(cutoffUnix int64) (int64, error) {
	if t == nil {
		return 0, nil
	}
	return t.pruneUnpinned(cutoffUnix, 0)
}

// PruneUnpinnedToSize deletes the oldest unpinned exchanges until the index and
// blob files fall under maxBytes. Pinned (evidence-referenced) rows are never
// selected, so the cap can be exceeded when evidence itself is larger.
func (t *Traffic) PruneUnpinnedToSize(maxBytes int64) (int64, error) {
	if t == nil || maxBytes <= 0 {
		return 0, nil
	}
	return t.pruneUnpinned(0, maxBytes)
}

func (t *Traffic) pruneUnpinned(cutoffUnix, maxBytes int64) (int64, error) {
	t.CancelReclaim()
	var total int64
	for batch := 0; batch < 512; batch++ {
		if maxBytes > 0 && batch%10 == 0 {
			st, err := t.Stats()
			if err != nil {
				return total, err
			}
			if st.IndexBytes+st.BlobBytes <= maxBytes {
				break
			}
		}
		t.wmu.Lock()
		ids, err := t.pickUnpinnedIDs(cutoffUnix, 500)
		if err == nil && len(ids) > 0 {
			err = t.deleteIDsLocked(ids)
		}
		t.wmu.Unlock()
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			break
		}
		total += int64(len(ids))
	}
	if total > 0 {
		t.wmu.Lock()
		if err := t.gcBlobs(); err != nil {
			log.Printf("[traffic] 证据模式回收 blob 失败: %v", err)
		}
		t.wmu.Unlock()
		t.reclaim()
	}
	return total, nil
}

func (t *Traffic) pickUnpinnedIDs(cutoffUnix int64, limit int) ([]string, error) {
	q := `SELECT e.id FROM exchanges e
WHERE NOT EXISTS (SELECT 1 FROM evidence_pins p WHERE p.exchange_id=e.id)
  AND COALESCE(e.ts,0) > 0`
	args := []any{}
	if cutoffUnix > 0 {
		q += ` AND e.ts < ?`
		args = append(args, cutoffUnix)
	}
	q += ` ORDER BY e.ts ASC, e.id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := t.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteIDsLocked removes one bounded batch. Caller holds wmu.
func (t *Traffic) deleteIDsLocked(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	in := strings.Join(placeholders, ",")
	tx, err := t.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if t.fts {
		if _, err := tx.Exec(`DELETE FROM ex_fts WHERE rowid IN (SELECT rowid FROM exchanges WHERE id IN (`+in+`))`, args...); err != nil {
			return fmt.Errorf("证据模式删除全文索引: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM exchange_bodies WHERE id IN (`+in+`)`, args...); err != nil {
		return fmt.Errorf("证据模式删除正文: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM blob_refs WHERE exchange_id IN (`+in+`)`, args...); err != nil {
		return fmt.Errorf("证据模式删除 blob 引用: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM evidence_pins WHERE exchange_id IN (`+in+`)`, args...); err != nil {
		return fmt.Errorf("证据模式删除标记: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM exchanges WHERE id IN (`+in+`)`, args...); err != nil {
		return fmt.Errorf("证据模式删除索引行: %w", err)
	}
	return tx.Commit()
}

// PreviewUnpinnedBefore is the read-only dry-run used by the storage page.
func (t *Traffic) PreviewUnpinnedBefore(cutoffUnix int64) (int64, error) {
	if t == nil {
		return 0, nil
	}
	var n int64
	err := t.db.QueryRow(`SELECT count(*) FROM exchanges e
WHERE COALESCE(e.ts,0) > 0 AND e.ts < ?
  AND NOT EXISTS (SELECT 1 FROM evidence_pins p WHERE p.exchange_id=e.id)`, cutoffUnix).Scan(&n)
	return n, err
}
