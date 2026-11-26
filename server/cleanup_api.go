package server

import (
	"log"
	"net/http"
	"strconv"
	"time"
)

// cleanupJobView is the management API shape. payload is intentionally omitted:
// a traffic job can carry hundreds of host names, and the UI only needs the
// counts/state.
type cleanupJobView struct {
	ID        int64     `json:"id"`
	TaskID    *int64    `json:"task_id,omitempty"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	Total     int64     `json:"total"`
	Done      int64     `json:"done"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) listCleanupJobs(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	activeOnly := r.URL.Query().Get("active") == "1"
	jobs, err := s.m.pg.ListCleanupJobs(r.Context(), activeOnly)
	if err != nil {
		// 清理队列是运维辅助信息；原始数据库错误可能含 SQL/连接串片段，不回显。
		log.Printf("[cleanup] 读取作业列表失败：%v", err)
		writeErr(w, http.StatusInternalServerError, "读取清理作业失败")
		return
	}
	out := make([]cleanupJobView, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, cleanupJobView{
			ID: job.ID, TaskID: job.TaskID, Kind: job.Kind, State: job.State,
			Attempts: job.Attempts, Total: job.Total, Done: job.Done,
			LastError: job.LastError, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"jobs": out})
}

func (s *Server) retryCleanupJob(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "无效的作业 ID")
		return
	}
	ok, err := s.m.pg.RetryCleanupJob(r.Context(), id)
	if err != nil {
		log.Printf("[cleanup] 重试作业 %d 失败：%v", id, err)
		writeErr(w, http.StatusInternalServerError, "重试清理作业失败")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "作业不存在或不是失败状态")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
