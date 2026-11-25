package traffic

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RetentionStats is a dry-run view of age-based cleanup. Protected counts
// exchanges that are older than the cutoff but referenced by PostgreSQL
// evidence/regression/graph state; they are never deleted.
type RetentionStats struct {
	Candidates int64 `json:"candidates"`
	Protected  int64 `json:"protected"`
	Deletable  int64 `json:"deletable"`
}

// RetentionDeleteResult reports a committed retention pass.
type RetentionDeleteResult struct {
	Deleted       int64 `json:"deleted"`
	Protected     int64 `json:"protected"`
	LegacyDirs    int64 `json:"legacy_dirs"`
	BlobReclaimed bool  `json:"blob_reclaimed"`
}

// RetentionPreview counts exchanges older than before, split into protected and
// deletable. It is read-only and takes no write lock.
func (t *Traffic) RetentionPreview(before time.Time, protected map[string]struct{}) (RetentionStats, error) {
	if t == nil || t.db == nil {
		return RetentionStats{}, fmt.Errorf("traffic store unavailable")
	}
	rows, err := t.db.Query(`SELECT id FROM exchanges WHERE ts IS NOT NULL AND ts > 0 AND ts < ?`, before.Unix())
	if err != nil {
		return RetentionStats{}, err
	}
	defer rows.Close()
	var st RetentionStats
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return RetentionStats{}, err
		}
		st.Candidates++
		if _, ok := protected[id]; ok {
			st.Protected++
		} else {
			st.Deletable++
		}
	}
	if err := rows.Err(); err != nil {
		return RetentionStats{}, err
	}
	return st, nil
}

// DeleteUnreferencedBefore removes exchanges older than before whose IDs are not
// in protected. It uses the same deleteWhere path as manual host deletion, so
// bodies, blob refs and the FTS index are removed in one SQLite transaction;
// blob GC + incremental vacuum run afterwards. The caller must supply a
// fail-closed protected set (a failed PG query must not become an empty map).
func (t *Traffic) DeleteUnreferencedBefore(before time.Time, protected map[string]struct{}) (RetentionDeleteResult, error) {
	var res RetentionDeleteResult
	if t == nil || t.db == nil {
		return res, fmt.Errorf("traffic store unavailable")
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()

	rows, err := t.db.Query(`SELECT id, COALESCE(path,'') FROM exchanges WHERE ts IS NOT NULL AND ts > 0 AND ts < ? ORDER BY ts`, before.Unix())
	if err != nil {
		return res, err
	}
	var ids []string
	paths := map[string]struct{}{}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return res, err
		}
		if _, ok := protected[id]; ok {
			res.Protected++
			continue
		}
		ids = append(ids, id)
		if strings.TrimSpace(path) != "" {
			paths[path] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()
	if len(ids) == 0 {
		return res, nil
	}

	tx, err := t.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck
	const batch = 500
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(part)), ",")
		args := make([]any, len(part))
		for i := range part {
			args[i] = part[i]
		}
		n, err := t.deleteWhere(tx, "id IN ("+placeholders+")", args...)
		if err != nil {
			return res, err
		}
		res.Deleted += n
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("提交流量保留清理: %w", err)
	}

	// Remove legacy host directories only when no surviving exchange still points
	// at the same path; a shared path must not be deleted with one old row.
	for rel := range paths {
		if !t.removeLegacyPathIfUnused(rel) {
			continue
		}
		res.LegacyDirs++
	}
	t.reclaim()
	if err := t.gcBlobs(); err != nil {
		return res, fmt.Errorf("回收流量 blob: %w", err)
	}
	res.BlobReclaimed = true
	return res, nil
}

// removeLegacyPathIfUnused is best-effort: retention must not turn a completed
// SQLite deletion into a failed pass because an old directory was already gone
// or is not writable.
func (t *Traffic) removeLegacyPathIfUnused(rel string) bool {
	rel = strings.TrimSpace(filepath.Clean(filepath.FromSlash(rel)))
	if rel == "" || rel == "." {
		return false
	}
	abs := filepath.Join(t.dir, rel)
	if !pathWithin(t.dir, abs) {
		log.Printf("[traffic] retention 拒绝删除流量目录外路径: %s", rel)
		return false
	}
	var n int
	if err := t.db.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE path=?`, rel).Scan(&n); err != nil {
		log.Printf("[traffic] retention 检查旧目录引用失败 path=%s: %v", rel, err)
		return false
	}
	if n > 0 {
		return false
	}
	if err := os.RemoveAll(abs); err != nil {
		log.Printf("[traffic] retention 删除旧目录失败 path=%s: %v", rel, err)
		return false
	}
	return true
}
