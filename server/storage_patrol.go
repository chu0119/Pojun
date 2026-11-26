package server

// 存储巡检：清理“数据库行已经没了、磁盘/审计表还留着”的残留。
//
// 这些残留来自历史版本或崩溃中断的异步清理，不是正常删除路径的一部分：
//   - data/tasks/<id>：任务行已不存在（归档任务仍有行，因此不会被当成孤儿）；
//   - data/sessions/conv-<id>：会话行已不存在；
//   - data/transcripts/conv-<id>.jsonl / exp<id>-*.jsonl：对应行已不存在；
//   - llm_records.task_id 指向已删除任务（异步 LLM 清理崩溃后的少量残留）。
//
// 24 小时保护窗口避免和正在进行的创建/删除竞态；所有删除都限制在 data 目录内。

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pgdb "github.com/chu0119/pojun/db"
)

type orphanPruneStats struct {
	Files int
	Bytes int64
}

// orphanGrace keeps freshly created/deleted paths out of the patrol: a task row
// may be committed a moment after its workspace directory appears.
const orphanGrace = 24 * time.Hour

func (s *Server) pruneOrphanStorage(ctx context.Context, dryRun bool) (orphanPruneStats, error) {
	var stats orphanPruneStats
	if s == nil || s.m == nil || s.m.pg == nil {
		return stats, nil
	}
	root := s.m.dir
	if root == "" {
		return stats, nil
	}
	tasks, err := int64Set(ctx, s.m.pg, `SELECT id FROM tasks`)
	if err != nil {
		return stats, fmt.Errorf("读取任务列表: %w", err)
	}
	explorations, err := int64Set(ctx, s.m.pg, `SELECT id FROM explorations`)
	if err != nil {
		return stats, fmt.Errorf("读取探索列表: %w", err)
	}
	conversations, err := int64Set(ctx, s.m.pg, `SELECT id FROM conversations`)
	if err != nil {
		return stats, fmt.Errorf("读取会话列表: %w", err)
	}
	cutoff := time.Now().Add(-orphanGrace)

	var firstErr error
	remove := func(path string) {
		size, files := pathFootprint(path)
		if !dryRun {
			if err := os.RemoveAll(path); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("删除孤儿 %s: %w", filepath.Base(path), err)
				}
				return
			}
		}
		stats.Files += files
		stats.Bytes += size
	}
	oldEnough := func(path string) bool {
		info, err := os.Lstat(path)
		return err == nil && info.ModTime().Before(cutoff)
	}

	if entries, err := os.ReadDir(filepath.Join(root, "tasks")); err == nil {
		for _, entry := range entries {
			if !pathInsideDir(root, filepath.Join(root, "tasks", entry.Name())) {
				continue
			}
			id, perr := strconv.ParseInt(entry.Name(), 10, 64)
			if perr != nil || tasks[id] {
				continue
			}
			path := filepath.Join(root, "tasks", entry.Name())
			if oldEnough(path) {
				remove(path)
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, "sessions")); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "conv-") {
				continue
			}
			id, perr := strconv.ParseInt(strings.TrimPrefix(name, "conv-"), 10, 64)
			if perr != nil || conversations[id] {
				continue
			}
			path := filepath.Join(root, "sessions", name)
			if !pathInsideDir(root, path) {
				continue
			}
			if oldEnough(path) {
				remove(path)
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, "transcripts")); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(root, "transcripts", name)
			if !pathInsideDir(root, path) {
				continue
			}
			orphan := false
			switch {
			case strings.HasPrefix(name, "conv-") && strings.HasSuffix(name, ".jsonl"):
				id, perr := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, "conv-"), ".jsonl"), 10, 64)
				orphan = perr == nil && !conversations[id]
			case strings.HasPrefix(name, "exp") && strings.HasSuffix(name, ".jsonl"):
				rest := strings.TrimPrefix(name, "exp")
				if i := strings.IndexByte(rest, '-'); i > 0 {
					id, perr := strconv.ParseInt(rest[:i], 10, 64)
					orphan = perr == nil && !explorations[id]
				}
			}
			if orphan && oldEnough(path) {
				remove(path)
			}
		}
	}
	if !dryRun {
		if _, err := s.m.pg.DeleteOrphanLLMRecords(ctx, 1000); err != nil {
			return stats, fmt.Errorf("清理孤儿 LLM 审计: %w", err)
		}
	}
	return stats, firstErr
}

func int64Set(ctx context.Context, pg *pgdb.DB, query string) (map[int64]bool, error) {
	rows, err := pg.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func pathFootprint(path string) (int64, int) {
	var size int64
	var files int
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			size += info.Size()
			files++
		}
		return nil
	})
	return size, files
}
