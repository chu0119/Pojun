package server

// 知识库 API：管理页的列表/统计/增删改 + 检索质量摘要。
// 与 task_templates 同一套 handler 形态：指针字段 + raw key 存在性判定 + writeErr。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chu0119/pojun/db"
)

const maxKBRequestBytes = 1 << 20 // 知识正文上限 20k 字符，1MB 请求体足够

type kbEntryRequest struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Detail      *string   `json:"detail"`
	Commands    *[]string `json:"commands"`
	Tags        *[]string `json:"tags"`
	Layer       *string   `json:"layer"`
	Status      *string   `json:"status"`
	Confidence  *string   `json:"confidence"`
	Source      *string   `json:"source"`
	Products    *[]string `json:"products"`
	VulnClasses *[]string `json:"vuln_classes"`
	Platforms   *[]string `json:"platforms"`
	CVEIDs      *[]string `json:"cve_ids"`
	License     *string   `json:"license"`
}

func decodeKBRequest(w http.ResponseWriter, r *http.Request, req *kbEntryRequest) (map[string]struct{}, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxKBRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求正文过大")
		} else {
			writeErr(w, http.StatusBadRequest, "读取请求失败")
		}
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeErr(w, http.StatusBadRequest, "请求不是合法 JSON")
		return nil, false
	}
	if err := json.Unmarshal(body, req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求字段类型不匹配")
		return nil, false
	}
	present := make(map[string]struct{}, len(raw))
	for k := range raw {
		present[k] = struct{}{}
	}
	return present, true
}

func writeKBErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrKBEntryInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, db.ErrKBEntryNameConflict):
		writeErr(w, http.StatusConflict, "同标题的知识条目已存在")
	case errors.Is(err, db.ErrKBEntryNotFound):
		writeErr(w, http.StatusNotFound, "知识条目不存在")
	default:
		writeInternalError(w, "api", err)
	}
}

// kbList 列表：q/status/layer/origin/tag/page/page_size/min_quality/product/vuln_class。
func (s *Server) kbList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > db.MaxKBPageSize {
		pageSize = db.MaxKBPageSize
	}
	filter := db.KBFilter{
		Query:  strings.TrimSpace(q.Get("q")),
		Status: q.Get("status"),
		Layer:  q.Get("layer"),
		Origin: q.Get("origin"),
	}
	for _, tag := range q["tag"] {
		if tag != "" {
			filter.Tags = append(filter.Tags, tag)
		}
	}
	for _, v := range q["product"] {
		if v != "" {
			filter.Products = append(filter.Products, v)
		}
	}
	for _, v := range q["vuln_class"] {
		if v != "" {
			filter.VulnClasses = append(filter.VulnClasses, v)
		}
	}
	if v := strings.TrimSpace(q.Get("min_quality")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			filter.MinQuality = f
		}
	}
	entries, total, err := pg.KB().List(filter, page, pageSize)
	if err != nil {
		writeInternalError(w, "api", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":   entries,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// kbStats 各状态计数（管理页顶部统计）。
func (s *Server) kbStats(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	counts, err := pg.KB().CountByStatus()
	if err != nil {
		writeInternalError(w, "api", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts})
}

// kbFacets 返回 products/vuln_classes/platforms/cve_ids 的取值计数。
func (s *Server) kbFacets(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	facets, err := pg.KB().Facets(r.URL.Query().Get("status"))
	if err != nil {
		writeInternalError(w, "api", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"facets": facets})
}

// kbRetrievalStats 近 N 天检索量/零结果率/P95 延迟（管理页小卡）。
func (s *Server) kbRetrievalStats(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	days := 7
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	// days 在 db 层以文本参数绑定（($1::text || ' days')::interval），避免
	// pgx 把 int 推断成 int4 后在 text 拼接处报 “cannot cast integer to text”。
	st, err := pg.KB().RetrievalStats(days)
	if err != nil {
		writeInternalError(w, "api", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// kbCreate 手工新建一条知识（默认 approved，人工录入即可用）。
func (s *Server) kbCreate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req kbEntryRequest
	if _, ok := decodeKBRequest(w, r, &req); !ok {
		return
	}
	if req.Title == nil || strings.TrimSpace(*req.Title) == "" {
		writeErr(w, http.StatusBadRequest, "title 不能为空")
		return
	}
	in := db.KBEntryInput{
		Title:       *req.Title,
		Description: kbStrValue(req.Description),
		Detail:      kbStrValue(req.Detail),
		Origin:      db.KBOriginManual,
		Status:      db.KBStatusApproved,
		Confidence:  db.KBConfidenceObserved,
		Source:      kbStrValue(req.Source),
		CreatedBy:   "human",
	}
	if req.Commands != nil {
		in.Commands = *req.Commands
	}
	if req.Tags != nil {
		in.Tags = *req.Tags
	}
	if req.Layer != nil {
		in.Layer = *req.Layer
	}
	if req.Status != nil {
		in.Status = *req.Status
	}
	if req.Confidence != nil {
		in.Confidence = *req.Confidence
	}
	if req.Products != nil {
		in.Products = *req.Products
	}
	if req.VulnClasses != nil {
		in.VulnClasses = *req.VulnClasses
	}
	if req.Platforms != nil {
		in.Platforms = *req.Platforms
	}
	if req.CVEIDs != nil {
		in.CVEIDs = *req.CVEIDs
	}
	if req.License != nil {
		in.License = *req.License
	}
	e, err := pg.KB().Create(in)
	if err != nil {
		writeKBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// kbUpdate PATCH 语义：只改给出的字段。
func (s *Server) kbUpdate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad knowledge id")
		return
	}
	var req kbEntryRequest
	if _, ok := decodeKBRequest(w, r, &req); !ok {
		return
	}
	patch := db.KBEntryPatch{
		Title:       req.Title,
		Description: req.Description,
		Detail:      req.Detail,
		Layer:       req.Layer,
		Status:      req.Status,
		Confidence:  req.Confidence,
		Source:      req.Source,
		License:     req.License,
	}
	if req.Products != nil {
		patch.SetProducts = true
		patch.Products = *req.Products
	}
	if req.VulnClasses != nil {
		patch.SetVulnClasses = true
		patch.VulnClasses = *req.VulnClasses
	}
	if req.Platforms != nil {
		patch.SetPlatforms = true
		patch.Platforms = *req.Platforms
	}
	if req.CVEIDs != nil {
		patch.SetCVEIDs = true
		patch.CVEIDs = *req.CVEIDs
	}
	if req.Commands != nil {
		patch.SetCommands = true
		patch.Commands = *req.Commands
	}
	if req.Tags != nil {
		patch.SetTags = true
		patch.Tags = *req.Tags
	}
	e, err := pg.KB().Patch(id, patch)
	if err != nil {
		writeKBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// kbDelete 删除一条。
func (s *Server) kbDelete(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad knowledge id")
		return
	}
	ok, err := pg.KB().Delete(id)
	if err != nil {
		writeInternalError(w, "api", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "知识条目不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func kbStrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
