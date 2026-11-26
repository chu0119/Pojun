package server

// 任务删除后的异步清理 worker。
//
// PostgreSQL 事务只负责删除任务行并把工作写进 cleanup_jobs；这里按顺序领取
// 作业，把流量索引、LLM 审计、任务文件分批清完。每批都是独立的小事务，进程
// 崩溃后作业回到 pending 继续跑；任务文件暂存目录带 journal，重启时按任务行
// 是否存在决定“恢复文件”还是“继续删除”。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pgdb "github.com/chu0119/pojun/db"
)

type cleanupTrafficPayload struct {
	TaskID        int64    `json:"task_id"`
	ExplorationID int64    `json:"exploration_id"`
	Hosts         []string `json:"hosts"`
	Cutoff        int64    `json:"cutoff"`
}

type cleanupLLMPayload struct {
	TaskID int64 `json:"task_id"`
}

type cleanupFilesPayload struct {
	StageDir string `json:"stage_dir"`
}

const (
	cleanupPollInterval = 2 * time.Second
	cleanupLLMBatch     = 1000
	cleanupJobTimeout   = 2 * time.Hour
)

// var so tests can disable the grace period; production waits a minute so a
// concurrent task that starts on the same host is visible to the guard query.
var cleanupJobGrace = 60 * time.Second

func cleanupTaskLabel(job *pgdb.CleanupJob) string {
	if job == nil || job.TaskID == nil {
		return "-"
	}
	return strconv.FormatInt(*job.TaskID, 10)
}

// startCleanupWorker starts the single cleanup lane. One job at a time is
// deliberate: traffic deletion is disk-bound, and parallel jobs would only
// fight for the same SQLite write lock and PG connection pool.
func (s *Server) startCleanupWorker(ctx context.Context) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return
	}
	// Recover before the HTTP listener starts serving: a concurrent user delete
	// could otherwise create a fresh stage directory and this scan would race it.
	if n, err := s.m.pg.RequeueStaleCleanupJobs(ctx); err != nil {
		log.Printf("[cleanup] 回收超时作业失败：%v", err)
	} else if n > 0 {
		log.Printf("[cleanup] 回收 %d 个超时作业", n)
	}
	s.recoverTaskFileStaging()
	s.pinEvidenceTraffic(ctx)
	go func() {
		ticker := time.NewTicker(cleanupPollInterval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if s.runOneCleanupJob(ctx) {
				continue // 队列里可能还有，立刻取下一条
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// pinEvidenceTraffic backfills evidence pins from PostgreSQL. Evidence-only
// capture must never delete a row a live finding still references, and pins are
// written inside the traffic SQLite database so the pruning path can stay local.
func (s *Server) pinEvidenceTraffic(ctx context.Context) {
	if s == nil || s.m == nil || s.m.traffic == nil || s.m.pg == nil || s.m.TrafficMode() != "evidence" {
		return
	}
	protected, err := s.m.pg.ProtectedTrafficIDs(ctx)
	if err != nil {
		log.Printf("[cleanup] 读取证据流量保护集失败：%v", err)
		return
	}
	if len(protected) == 0 {
		return
	}
	ids := make([]string, 0, len(protected))
	for id := range protected {
		ids = append(ids, id)
	}
	if err := s.m.traffic.PinEvidence(ids); err != nil {
		log.Printf("[cleanup] 回填证据流量 pin 失败：%v", err)
		return
	}
	log.Printf("[cleanup] 已标记 %d 条受保护流量，证据模式清理不会删除", len(ids))
}

func (s *Server) recoverTaskFileStaging() {
	taskExists := func(id string) bool {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			return true // 解析不了就按存在处理，宁可保留也不误删
		}
		task, err := s.m.pg.GetTask(n)
		if err != nil {
			return true
		}
		return task != nil
	}
	n, err := recoverTaskFileStaging(s.m.dir, taskExists)
	if err != nil {
		log.Printf("[cleanup] 恢复任务文件暂存完成 %d 个，错误：%v", n, err)
	} else if n > 0 {
		log.Printf("[cleanup] 恢复任务文件暂存 %d 个", n)
	}
}

func (s *Server) runOneCleanupJob(parent context.Context) bool {
	job, err := s.m.pg.ClaimCleanupJob(parent, cleanupJobGrace)
	if err != nil {
		log.Printf("[cleanup] 领取作业失败：%v", err)
		return false
	}
	if job == nil {
		return false
	}
	s.executeCleanupJob(parent, job)
	return true
}

// executeCleanupJob runs one already-claimed job and records its outcome.
func (s *Server) executeCleanupJob(parent context.Context, job *pgdb.CleanupJob) {
	ctx, cancel := context.WithTimeout(parent, cleanupJobTimeout)
	defer cancel()
	start := time.Now()
	log.Printf("[cleanup] start job=%d kind=%s task=%s attempts=%d", job.ID, job.Kind, cleanupTaskLabel(job), job.Attempts)

	var runErr error
	switch job.Kind {
	case "traffic":
		runErr = s.runCleanupTraffic(ctx, job)
	case "llm":
		runErr = s.runCleanupLLM(ctx, job)
	case "files":
		runErr = s.runCleanupFiles(ctx, job)
	default:
		runErr = fmt.Errorf("未知清理类型 %q", job.Kind)
	}
	if runErr != nil {
		log.Printf("[cleanup] failed job=%d kind=%s in %s: %v", job.ID, job.Kind, time.Since(start).Round(time.Millisecond), runErr)
		if err := s.m.pg.FinishCleanupJob(context.Background(), job.ID, runErr.Error()); err != nil {
			log.Printf("[cleanup] 记录失败结果 job=%d: %v", job.ID, err)
		}
		return
	}
	if err := s.m.pg.FinishCleanupJob(context.Background(), job.ID, ""); err != nil {
		log.Printf("[cleanup] 记录完成结果 job=%d: %v", job.ID, err)
	}
	log.Printf("[cleanup] done job=%d kind=%s in %s", job.ID, job.Kind, time.Since(start).Round(time.Millisecond))
}

func (s *Server) runCleanupTraffic(ctx context.Context, job *pgdb.CleanupJob) error {
	var payload cleanupTrafficPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("解析流量清理参数: %w", err)
	}
	if s.m.traffic == nil {
		return nil // 没开录制代理，本来就没有流量要清
	}
	if len(payload.Hosts) == 0 {
		return nil
	}
	guard := func(ctx context.Context, hosts []string) (map[string]bool, error) {
		shared, err := s.m.pg.TrafficHostsStillShared(ctx, payload.TaskID, payload.ExplorationID, hosts)
		if err != nil {
			return nil, err
		}
		allowed := make(map[string]bool, len(hosts))
		for _, host := range hosts {
			if !shared[host] {
				allowed[host] = true
			}
		}
		return allowed, nil
	}
	var lastLog int64
	deleted, err := s.m.traffic.DeleteHostsBatched(ctx, payload.Hosts, payload.Cutoff, guard, func(done, total int64) {
		_ = s.m.pg.UpdateCleanupJobProgress(ctx, job.ID, done, total)
		if done-lastLog >= 20000 {
			lastLog = done
			log.Printf("[cleanup] job=%d 流量 %d/%d", job.ID, done, total)
		}
	})
	if err != nil {
		return fmt.Errorf("删除流量（已删 %d 条）: %w", deleted, err)
	}
	return nil
}

func (s *Server) runCleanupLLM(ctx context.Context, job *pgdb.CleanupJob) error {
	var payload cleanupLLMPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("解析 LLM 清理参数: %w", err)
	}
	if payload.TaskID <= 0 {
		return nil
	}
	total, err := s.m.pg.CountLLMRecordsForTask(ctx, payload.TaskID)
	if err != nil {
		return fmt.Errorf("统计 LLM 记录: %w", err)
	}
	_ = s.m.pg.UpdateCleanupJobProgress(ctx, job.ID, 0, total)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := s.m.pg.DeleteLLMRecordsBatch(ctx, payload.TaskID, cleanupLLMBatch)
		if err != nil {
			return fmt.Errorf("删除 LLM 记录（已删 %d 条）: %w", done, err)
		}
		done += n
		_ = s.m.pg.UpdateCleanupJobProgress(ctx, job.ID, done, total)
		if n < cleanupLLMBatch {
			return nil
		}
	}
}

func (s *Server) runCleanupFiles(ctx context.Context, job *pgdb.CleanupJob) error {
	var payload cleanupFilesPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("解析文件清理参数: %w", err)
	}
	if payload.StageDir == "" {
		return nil
	}
	if !validTaskStagingDir(s.m.dir, payload.StageDir) {
		return fmt.Errorf("拒绝清理暂存目录之外的路径 %q", payload.StageDir)
	}
	_ = s.m.pg.UpdateCleanupJobProgress(ctx, job.ID, 0, 1)
	if err := os.RemoveAll(payload.StageDir); err != nil {
		return fmt.Errorf("删除任务文件暂存 %s: %w", payload.StageDir, err)
	}
	_ = s.m.pg.UpdateCleanupJobProgress(ctx, job.ID, 1, 1)
	return nil
}

// validTaskStagingDir keeps a malformed/attacker-controlled payload from
// turning the cleanup worker into an arbitrary recursive delete. Only direct
// children of <data>/.delete-staging named task-* are accepted.
func validTaskStagingDir(dataDir, raw string) bool {
	base := filepath.Join(dataDir, ".delete-staging")
	abs, err := filepath.Abs(raw)
	if err != nil {
		return false
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	if strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	return strings.HasPrefix(filepath.Base(abs), "task-")
}
