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
)

const (
	settingTrafficRetentionDays = "traffic_retention_days"
	defaultTrafficRetentionDays = 0
	maxTrafficRetentionDays     = 3650
)

type trafficRetentionStatus struct {
	Days       int   `json:"days"`
	Enabled    bool  `json:"enabled"`
	Cutoff     int64 `json:"cutoff"`
	Total      int   `json:"total"`
	Candidates int64 `json:"candidates"`
	Protected  int64 `json:"protected"`
	Deletable  int64 `json:"deletable"`
}

func (s *Server) trafficRetentionDays() int {
	if s == nil || s.m == nil || s.m.pg == nil {
		return defaultTrafficRetentionDays
	}
	raw, ok, err := s.m.pg.GetSetting(settingTrafficRetentionDays)
	if err != nil || !ok {
		return defaultTrafficRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 || n > maxTrafficRetentionDays {
		return defaultTrafficRetentionDays
	}
	return n
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
	days := s.trafficRetentionDays()
	st := trafficRetentionStatus{Days: days, Enabled: days > 0}
	if days <= 0 {
		if tr := s.m.Traffic(); tr != nil {
			n, _ := tr.Count()
			st.Total = n
		}
		return st, nil
	}
	tr := s.m.Traffic()
	if tr == nil {
		return st, fmt.Errorf("流量存储未初始化")
	}
	cutoff := trafficRetentionCutoff(days, time.Now())
	st.Cutoff = cutoff.Unix()
	total, err := tr.Count()
	if err != nil {
		return st, err
	}
	st.Total = total
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
	DryRun        bool  `json:"dry_run"`
	Days          int   `json:"days"`
	Cutoff        int64 `json:"cutoff"`
	Candidates    int64 `json:"candidates"`
	Protected     int64 `json:"protected"`
	Deletable     int64 `json:"deletable"`
	Deleted       int64 `json:"deleted,omitempty"`
	LegacyDirs    int64 `json:"legacy_dirs,omitempty"`
	BlobReclaimed bool  `json:"blob_reclaimed,omitempty"`
}

func (s *Server) runTrafficRetentionLocked(ctx context.Context, dryRun bool) (trafficRetentionRunResult, error) {
	var out trafficRetentionRunResult
	days := s.trafficRetentionDays()
	out.DryRun, out.Days = dryRun, days
	if days <= 0 {
		return out, fmt.Errorf("流量保留策略未开启（traffic_retention_days=0）")
	}
	tr := s.m.Traffic()
	if tr == nil {
		return out, fmt.Errorf("流量存储未初始化")
	}
	cutoff := trafficRetentionCutoff(days, time.Now())
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

// startTrafficRetentionWorker runs the configured TTL in the background. It
// deliberately waits before the first pass: a freshly upgraded instance should
// not start deleting old traffic before the operator has seen the setting.
func (s *Server) startTrafficRetentionWorker() {
	if s == nil || s.ctx == nil {
		return
	}
	go func() {
		timer := time.NewTimer(10 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				days := s.trafficRetentionDays()
				if days > 0 {
					s.trafficRetentionMu.Lock()
					_, err := s.runTrafficRetentionLocked(s.ctx, false)
					s.trafficRetentionMu.Unlock()
					if err != nil {
						log.Printf("[traffic-retention] 自动清理失败: %v", err)
					}
				}
				timer.Reset(6 * time.Hour)
			}
		}
	}()
}
