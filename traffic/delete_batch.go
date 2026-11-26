package traffic

// 批量删除指定 host 的流量。
//
// 旧路径 StageDeleteHostsExact 为了和 PostgreSQL 任务事务对齐，把整份删除放在
// 一个 SQLite 事务里。十几万条交换、十几 GB 正文的单事务提交会触发整库
// checkpoint，既长时间占住 PG 事务，也会让 WAL 涨到不可控。这里改成：
//   - 每批最多 2000 条，独立事务，提交后释放写锁；
//   - 每批前用 ts <= cutoff 限定，绝不碰删除开始后新录的流量；
//   - 定期复核 host 所有权，被其他存活任务接管的 host 立即移出删除集；
//   - 每批提交后做一次 PASSIVE checkpoint，结束时 TRUNCATE，避免 WAL 无限增长。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HostGuard returns the subset of hosts that are still safe to delete. The
// worker supplies a PostgreSQL ownership re-check; nil means "no re-check".
type HostGuard func(ctx context.Context, hosts []string) (map[string]bool, error)

const (
	hostDeleteGuardEvery = 20
	// Checkpoint after every batch: the recorder's own connections still use
	// wal_autocheckpoint=1000, and a small WAL means a recorder commit never
	// triggers a multi-hundred-MB checkpoint in the middle of the cleanup.
	hostDeleteCheckpointEvery = 1
)

// var so tests can shrink it; production uses 2000 rows per SQLite transaction.
var hostDeleteBatchSize = 2000

// DeleteHostsBatched removes traffic for the given hosts in bounded SQLite
// transactions. cutoff is the unix second when the delete was requested; rows
// recorded after it are never touched, so a task that starts on the same host
// after the deletion keeps its fresh traffic.
func (t *Traffic) DeleteHostsBatched(ctx context.Context, hosts []string, cutoff int64, guard HostGuard, progress func(done, total int64)) (int64, error) {
	if t == nil || len(hosts) == 0 {
		return 0, nil
	}
	// A background reclaim may be holding wmu for up to one 60s chunk; stop it
	// now so the first batch does not wait a minute for a page move.
	t.CancelReclaim()
	if cutoff <= 0 {
		cutoff = math.MaxInt64
	}
	unique := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		unique = append(unique, host)
	}
	if len(unique) == 0 {
		return 0, nil
	}

	conn, err := t.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取流量库连接: %w", err)
	}
	defer func() {
		// PRAGMA is per physical connection: leaving autocheckpoint off would
		// silently change every later writer that reuses this connection.
		restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.ExecContext(restoreCtx, `PRAGMA wal_autocheckpoint=1000`)
		cancel()
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
		return 0, fmt.Errorf("关闭自动 checkpoint: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=10000`); err != nil {
		return 0, fmt.Errorf("设置 busy_timeout: %w", err)
	}

	// The host set lives in a TEMP table on this dedicated connection. It
	// survives across the per-batch transactions below and disappears with it.
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _cleanup_hosts(host TEXT PRIMARY KEY)`); err != nil {
		return 0, fmt.Errorf("创建待删 host 表: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM _cleanup_hosts`); err != nil {
		return 0, err
	}
	for i := 0; i < len(unique); i += 300 {
		end := min(i+300, len(unique))
		chunk := unique[i:end]
		args := make([]any, len(chunk))
		placeholders := make([]string, len(chunk))
		for j, host := range chunk {
			args[j] = host
			placeholders[j] = "(?)"
		}
		q := `INSERT OR IGNORE INTO _cleanup_hosts(host) VALUES ` + strings.Join(placeholders, ",")
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return 0, fmt.Errorf("登记待删 host: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _del_ids(rowid INTEGER PRIMARY KEY, id TEXT)`); err != nil {
		return 0, fmt.Errorf("创建待删行表: %w", err)
	}

	if guard != nil {
		if err := trimBlockedHosts(ctx, conn, guard, unique); err != nil {
			return 0, err
		}
	}

	var total int64
	if err := conn.QueryRowContext(ctx, `
SELECT count(*) FROM exchanges
WHERE COALESCE(ts,0) <= ? AND host IN (SELECT host FROM _cleanup_hosts)`, cutoff).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计待删流量: %w", err)
	}
	if total == 0 {
		return 0, nil
	}
	if progress != nil {
		progress(0, total)
	}

	var deleted int64
	for batch := 1; ; batch++ {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if guard != nil && batch%hostDeleteGuardEvery == 0 {
			remaining, err := remainingHosts(ctx, conn)
			if err != nil {
				return deleted, err
			}
			if err := trimBlockedHosts(ctx, conn, guard, remaining); err != nil {
				return deleted, err
			}
			if len(remaining) == 0 {
				break
			}
		}
		n, err := t.deleteHostBatch(ctx, conn, cutoff, hostDeleteBatchSize)
		if err != nil {
			return deleted, err
		}
		if n == 0 {
			break
		}
		deleted += n
		if progress != nil {
			progress(deleted, total)
		}
		if batch%hostDeleteCheckpointEvery == 0 {
			t.checkpointPassive(ctx, conn)
		}
	}

	if deleted > 0 {
		// Blob files are content-addressed and shared across hosts; only a
		// reference sweep after all rows are gone can know which are orphaned.
		t.wmu.Lock()
		gcErr := t.gcBlobs()
		t.wmu.Unlock()
		if gcErr != nil {
			log.Printf("[traffic] 批量删除后回收 blob 失败（下轮继续）：%v", gcErr)
		}
		t.checkpointTruncateBestEffort(conn)
		// 历史 host 目录（老版本落盘的 .http 树）在数据库行删完后才能移除。
		// 只处理仍在待删集合里的 host：被其他任务接管的 host 已经被 guard
		// 移出，它的目录必须保留。失败只是留目录，不影响正确性。
		remaining, _ := remainingHosts(ctx, conn)
		for _, host := range remaining {
			dir := filepath.Join(t.dir, sanitize(host))
			if !pathWithin(t.dir, dir) {
				continue
			}
			if _, statErr := os.Lstat(dir); statErr == nil {
				if rmErr := os.RemoveAll(dir); rmErr != nil {
					log.Printf("[traffic] 删除历史流量目录 %s 失败：%v", dir, rmErr)
				}
			}
		}
	}
	return deleted, nil
}

func remainingHosts(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT host FROM _cleanup_hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		out = append(out, host)
	}
	return out, rows.Err()
}

func trimBlockedHosts(ctx context.Context, conn *sql.Conn, guard HostGuard, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	allowed, err := guard(ctx, hosts)
	if err != nil {
		return fmt.Errorf("复核 host 所有权: %w", err)
	}
	for _, host := range hosts {
		if allowed[host] {
			continue
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM _cleanup_hosts WHERE host=?`, host); err != nil {
			return fmt.Errorf("移除被其他任务接管的 host %s: %w", host, err)
		}
	}
	return nil
}

// deleteHostBatch removes at most limit rows in one transaction.
func (t *Traffic) deleteHostBatch(ctx context.Context, conn *sql.Conn, cutoff int64, limit int) (int64, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `DELETE FROM _del_ids`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO _del_ids(rowid,id)
SELECT rowid,id FROM exchanges
WHERE COALESCE(ts,0) <= ? AND host IN (SELECT host FROM _cleanup_hosts)
LIMIT ?`, cutoff, limit); err != nil {
		return 0, err
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM _del_ids`).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		committed = true
		return 0, nil
	}
	if t.fts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ex_fts WHERE rowid IN (SELECT rowid FROM _del_ids)`); err != nil {
			return 0, fmt.Errorf("删除全文索引: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM exchange_bodies WHERE id IN (SELECT id FROM _del_ids)`); err != nil {
		return 0, fmt.Errorf("删除正文: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_refs WHERE exchange_id IN (SELECT id FROM _del_ids)`); err != nil {
		return 0, fmt.Errorf("删除 blob 引用: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM evidence_pins WHERE exchange_id IN (SELECT id FROM _del_ids)`); err != nil {
		return 0, fmt.Errorf("删除证据标记: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM exchanges WHERE rowid IN (SELECT rowid FROM _del_ids)`); err != nil {
		return 0, fmt.Errorf("删除索引行: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	return n, nil
}

// checkpointPassive is called outside wmu: checkpointing copies WAL frames back
// into the database and must not stall the recorder. busy_timeout makes a
// reader-held checkpoint give up instead of blocking the cleanup worker.
func (t *Traffic) checkpointPassive(ctx context.Context, conn *sql.Conn) {
	ckCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var busy, logFrames, checkpointed int
	if err := conn.QueryRowContext(ckCtx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		log.Printf("[traffic] 批量删除 checkpoint 失败（下轮继续）：%v", err)
	}
}

func (t *Traffic) checkpointTruncateBestEffort(conn *sql.Conn) {
	ckCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var busy, logFrames, checkpointed int
	if err := conn.QueryRowContext(ckCtx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		log.Printf("[traffic] 批量删除后截断 WAL 失败（下轮继续）：%v", err)
		return
	}
	if busy != 0 {
		log.Printf("[traffic] 批量删除后 WAL 截断被占用（busy=%d log=%d checkpointed=%d）", busy, logFrames, checkpointed)
	}
}
