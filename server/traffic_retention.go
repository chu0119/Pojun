package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/traffic"
)

const (
	settingTrafficRetentionDays = "traffic_retention_days"
	defaultTrafficRetentionDays = 0
	maxTrafficRetentionDays     = 3650

	settingTrafficEvidenceHours = "traffic_evidence_window_hours"
	defaultTrafficEvidenceHours = 24
	maxTrafficEvidenceHours     = 8760

	settingTrafficEvidenceMaxGB = "traffic_evidence_max_gb"
	defaultTrafficEvidenceMaxGB = 2
	maxTrafficEvidenceMaxGB     = 4096

	settingStorageLLMKeepDays = "storage_llm_keep_days"
	defaultStorageLLMKeepDays = 0
	maxStorageLLMKeepDays     = 3650

	settingStorageLLMMaxRows = "storage_llm_max_rows"
	defaultStorageLLMMaxRows = 0
	maxStorageLLMMaxRows     = 10000000

	retentionBatchSize  = 1000
	retentionRunEvery   = 6 * time.Hour
	retentionFirstDelay = 10 * time.Minute
)

type trafficRetentionStatus struct {
	Mode           string `json:"mode"`
	Days           int    `json:"days"`
	EvidenceHours  int    `json:"traffic_evidence_window_hours"`
	EvidenceMaxGB  int    `json:"traffic_evidence_max_gb"`
	LLMKeepDays    int    `json:"storage_llm_keep_days"`
	LLMMaxRows     int    `json:"storage_llm_max_rows"`
	Enabled        bool   `json:"enabled"`
	Cutoff         int64  `json:"cutoff"`
	Total          int    `json:"total"`
	Candidates     int64  `json:"candidates"`
	Protected      int64  `json:"protected"`
	Deletable      int64  `json:"deletable"`
	EvidencePinned int64  `json:"evidence_pinned"`
}

func (s *Server) settingInt(key string, def, lo, hi int) int {
	if s == nil || s.m == nil || s.m.pg == nil {
		return def
	}
	raw, ok, err := s.m.pg.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < lo || n > hi {
		return def
	}
	return n
}

func (s *Server) trafficRetentionDays() int {
	return s.settingInt(settingTrafficRetentionDays, defaultTrafficRetentionDays, 0, maxTrafficRetentionDays)
}

func (s *Server) trafficEvidenceHours() int {
	return s.settingInt(settingTrafficEvidenceHours, defaultTrafficEvidenceHours, 0, maxTrafficEvidenceHours)
}

func (s *Server) trafficEvidenceMaxGB() int {
	return s.settingInt(settingTrafficEvidenceMaxGB, defaultTrafficEvidenceMaxGB, 0, maxTrafficEvidenceMaxGB)
}

func (s *Server) llmKeepDays() int {
	return s.settingInt(settingStorageLLMKeepDays, defaultStorageLLMKeepDays, 0, maxStorageLLMKeepDays)
}

func (s *Server) llmMaxRows() int {
	return s.settingInt(settingStorageLLMMaxRows, defaultStorageLLMMaxRows, 0, maxStorageLLMMaxRows)
}

func trafficRetentionCutoff(days int, now time.Time) time.Time {
	if days <= 0 {
		return time.Time{}
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// trafficRetentionStatusLocked computes a dry-run view. The caller holds
// trafficRetentionMu (or is the HTTP handler, which takes it).
func (s *Server) trafficRetentionStatusLocked() (trafficRetentionStatus, error) {
	st := trafficRetentionStatus{
		Mode:          s.m.TrafficMode(),
		Days:          s.trafficRetentionDays(),
		EvidenceHours: s.trafficEvidenceHours(),
		EvidenceMaxGB: s.trafficEvidenceMaxGB(),
		LLMKeepDays:   s.llmKeepDays(),
		LLMMaxRows:    s.llmMaxRows(),
	}
	if st.Mode == "evidence" {
		st.Enabled = st.EvidenceHours > 0 || st.EvidenceMaxGB > 0
	} else {
		st.Enabled = st.Days > 0
	}
	tr := s.m.Traffic()
	if tr == nil {
		return st, nil
	}
	total, _ := tr.Count()
	st.Total = total
	if !st.Enabled {
		return st, nil
	}
	if st.Mode == "evidence" {
		if st.EvidenceHours > 0 {
			cutoff := time.Now().Add(-time.Duration(st.EvidenceHours) * time.Hour).Unix()
			st.Cutoff = cutoff
			n, err := tr.PreviewUnpinnedBefore(cutoff)
			if err != nil {
				return st, err
			}
			st.Candidates, st.Deletable = n, n
		}
		if pinned, err := s.m.pg.ProtectedTrafficIDs(context.Background()); err == nil {
			st.EvidencePinned = int64(len(pinned))
			st.Protected = int64(len(pinned))
		}
		return st, nil
	}
	cutoff := trafficRetentionCutoff(st.Days, time.Now())
	st.Cutoff = cutoff.Unix()
	protected, err := s.m.pg.ProtectedTrafficIDs(context.Background())
	if err != nil {
		return st, fmt.Errorf("读取流量保护集失败: %w", err)
	}
	preview, err := tr.RetentionPreview(cutoff, protected)
	if err != nil {
		return st, err
	}
	st.Candidates, st.Protected, st.Deletable = preview.Candidates, preview.Protected, preview.Deletable
	return st, nil
}

func (s *Server) getTrafficRetention(w http.ResponseWriter, r *http.Request) {
	if s.m.Traffic() == nil {
		writeErr(w, 404, "流量存储未初始化")
		return
	}
	s.trafficRetentionMu.Lock()
	defer s.trafficRetentionMu.Unlock()
	st, err := s.trafficRetentionStatusLocked()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) runTrafficRetentionHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DryRun *bool `json:"dry_run"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "请求格式错误")
			return
		}
	}
	// 缺省仍是 dry-run：真正的删除必须由 UI/调用方显式选择。
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	s.trafficRetentionMu.Lock()
	defer s.trafficRetentionMu.Unlock()
	res, err := s.runTrafficRetentionLocked(context.Background(), dryRun)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

type trafficRetentionRunResult struct {
	DryRun        bool   `json:"dry_run"`
	Mode          string `json:"mode"`
	Days          int    `json:"days"`
	EvidenceHours int    `json:"traffic_evidence_window_hours,omitempty"`
	Cutoff        int64  `json:"cutoff"`
	Candidates    int64  `json:"candidates"`
	Protected     int64  `json:"protected"`
	Deletable     int64  `json:"deletable"`
	Deleted       int64  `json:"deleted,omitempty"`
	LegacyDirs    int64  `json:"legacy_dirs,omitempty"`
	BlobReclaimed bool   `json:"blob_reclaimed,omitempty"`
	Capacity      bool   `json:"capacity,omitempty"`
}

func (s *Server) runTrafficRetentionLocked(ctx context.Context, dryRun bool) (trafficRetentionRunResult, error) {
	var out trafficRetentionRunResult
	out.DryRun = dryRun
	out.Mode = s.m.TrafficMode()
	if out.Mode == "off" {
		return out, fmt.Errorf("流量捕获已关闭（traffic_mode=off），没有可作为保留策略的对象")
	}
	tr := s.m.Traffic()
	if tr == nil {
		return out, fmt.Errorf("流量存储未初始化")
	}
	if out.Mode == "evidence" {
		return s.runEvidenceRetentionLocked(ctx, tr, dryRun)
	}
	out.Days = s.trafficRetentionDays()
	if out.Days <= 0 {
		return out, fmt.Errorf("流量保留策略未开启（traffic_retention_days=0）")
	}
	cutoff := trafficRetentionCutoff(out.Days, time.Now())
	out.Cutoff = cutoff.Unix()
	protected, err := s.m.pg.ProtectedTrafficIDs(ctx)
	if err != nil {
		// Fail closed: a failed protection query must never become "empty set".
		return out, fmt.Errorf("读取流量保护集失败，已中止清理: %w", err)
	}
	if dryRun {
		st, err := tr.RetentionPreview(cutoff, protected)
		if err != nil {
			return out, err
		}
		out.Candidates, out.Protected, out.Deletable = st.Candidates, st.Protected, st.Deletable
		return out, nil
	}
	res, err := tr.DeleteUnreferencedBefore(cutoff, protected)
	if err != nil {
		return out, err
	}
	out.Candidates = res.Deleted + res.Protected
	out.Protected = res.Protected
	out.Deletable = res.Deleted
	out.Deleted = res.Deleted
	out.LegacyDirs = res.LegacyDirs
	out.BlobReclaimed = res.BlobReclaimed
	return out, nil
}

// runEvidenceRetentionLocked prunes only traffic that no finding pinned. Pins
// are backfilled from PostgreSQL first; any failure aborts the pass, because
// "could not read the protection set" must never mean "nothing is protected".
func (s *Server) runEvidenceRetentionLocked(ctx context.Context, tr *traffic.Traffic, dryRun bool) (trafficRetentionRunResult, error) {
	out := trafficRetentionRunResult{DryRun: dryRun, Mode: "evidence"}
	hours := s.trafficEvidenceHours()
	maxGB := s.trafficEvidenceMaxGB()
	if hours <= 0 && maxGB <= 0 {
		return out, fmt.Errorf("证据模式留存未配置（traffic_evidence_window_hours / traffic_evidence_max_gb 都为 0）")
	}
	protected, err := s.m.pg.ProtectedTrafficIDs(ctx)
	if err != nil {
		return out, fmt.Errorf("读取流量保护集失败，已中止清理: %w", err)
	}
	ids := make([]string, 0, len(protected))
	for id := range protected {
		ids = append(ids, id)
	}
	if err := tr.PinEvidence(ids); err != nil {
		return out, fmt.Errorf("写入证据流量标记失败，已中止清理: %w", err)
	}
	if hours > 0 {
		cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
		out.Cutoff = cutoff
		out.EvidenceHours = hours
		if dryRun {
			n, err := tr.PreviewUnpinnedBefore(cutoff)
			if err != nil {
				return out, err
			}
			out.Candidates, out.Deletable = n, n
		} else {
			n, err := tr.PruneUnpinnedBefore(cutoff)
			if err != nil {
				return out, err
			}
			out.Candidates, out.Deletable, out.Deleted = n, n, n
		}
	}
	if maxGB > 0 {
		max := int64(maxGB) << 30
		st, err := tr.Stats()
		if err != nil {
			return out, err
		}
		if st.IndexBytes+st.BlobBytes > max {
			out.Capacity = true
			if !dryRun {
				n, err := tr.PruneUnpinnedToSize(max)
				if err != nil {
					return out, err
				}
				out.Deleted += n
				out.Deletable += n
			}
		}
	}
	if !dryRun {
		out.Protected = int64(len(protected))
	}
	return out, nil
}

type llmRetentionRunResult struct {
	DryRun    bool  `json:"dry_run"`
	KeepDays  int   `json:"keep_days"`
	MaxRows   int   `json:"max_rows"`
	Expired   int64 `json:"expired"`
	Trimmed   int64 `json:"trimmed"`
	Deleted   int64 `json:"deleted"`
	Remaining int64 `json:"remaining"`
}

// runLLMRetentionLocked applies storage_llm_keep_days and storage_llm_max_rows.
// Both default to 0 (disabled), so an upgraded instance never deletes audit rows
// until the operator opts in.
func (s *Server) runLLMRetentionLocked(ctx context.Context, dryRun bool) (llmRetentionRunResult, error) {
	out := llmRetentionRunResult{DryRun: dryRun, KeepDays: s.llmKeepDays(), MaxRows: s.llmMaxRows()}
	if s.m == nil || s.m.pg == nil {
		return out, fmt.Errorf("数据库未就绪")
	}
	if !dryRun && out.KeepDays <= 0 && out.MaxRows <= 0 {
		return out, nil
	}
	if out.KeepDays > 0 {
		cutoff := time.Now().Add(-time.Duration(out.KeepDays) * 24 * time.Hour)
		if dryRun {
			if err := s.m.pg.QueryRowContext(ctx, `SELECT count(*) FROM llm_records WHERE ts < $1`, cutoff).Scan(&out.Expired); err != nil {
				return out, err
			}
		} else {
			for {
				n, err := s.m.pg.PruneLLMRecordsBefore(ctx, cutoff, retentionBatchSize)
				if err != nil {
					return out, err
				}
				out.Deleted += n
				if n < retentionBatchSize {
					break
				}
				if err := ctx.Err(); err != nil {
					return out, err
				}
			}
		}
	}
	if out.MaxRows > 0 {
		if dryRun {
			var n int64
			if err := s.m.pg.QueryRowContext(ctx, `SELECT count(*) FROM llm_records`).Scan(&n); err != nil {
				return out, err
			}
			if n > int64(out.MaxRows) {
				out.Trimmed = n - int64(out.MaxRows)
			}
		} else {
			n, err := s.m.pg.TrimLLMRecordsToMaxRows(ctx, int64(out.MaxRows), retentionBatchSize)
			if err != nil {
				return out, err
			}
			out.Trimmed = n
			out.Deleted += n
		}
	}
	if err := s.m.pg.QueryRowContext(ctx, `SELECT count(*) FROM llm_records`).Scan(&out.Remaining); err != nil {
		return out, err
	}
	return out, nil
}

// startTrafficRetentionWorker runs the configured TTL in the background. It
// deliberately waits before the first pass: a freshly upgraded instance should
// not start deleting old traffic before the operator has seen the setting.
func (s *Server) startTrafficRetentionWorker() {
	if s == nil || s.ctx == nil {
		return
	}
	go func() {
		timer := time.NewTimer(retentionFirstDelay)
		defer timer.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				s.runScheduledRetention(s.ctx)
				timer.Reset(retentionRunEvery)
			}
		}
	}()
}

// runScheduledRetention runs traffic/LLM retention and the orphan patrol. The
// traffic pass keeps its mutex so a manual pass and the timer cannot delete
// concurrently. Failures are logged, never fatal: retention is disk hygiene.
func (s *Server) runScheduledRetention(ctx context.Context) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return
	}
	configured := false
	switch s.m.TrafficMode() {
	case "evidence":
		configured = s.trafficEvidenceHours() > 0 || s.trafficEvidenceMaxGB() > 0
	case "full":
		configured = s.trafficRetentionDays() > 0
	}
	if s.m.traffic != nil && configured {
		s.trafficRetentionMu.Lock()
		if _, err := s.runTrafficRetentionLocked(ctx, false); err != nil {
			log.Printf("[traffic-retention] 自动清理失败: %v", err)
		}
		s.trafficRetentionMu.Unlock()
	}
	if res, err := s.runLLMRetentionLocked(ctx, false); err != nil {
		log.Printf("[llm-retention] 自动清理失败: %v", err)
	} else if res.Deleted > 0 {
		log.Printf("[llm-retention] 已清理 %d 条 LLM 审计（保留 %d 天 / %d 行）", res.Deleted, res.KeepDays, res.MaxRows)
	}
	if _, err := s.pruneOrphanStorage(ctx, false); err != nil {
		log.Printf("[storage-patrol] 孤儿清理失败: %v", err)
	}
}
