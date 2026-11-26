package db

// 知识库（KB v2）。
//
// 与 skills/（随包分发的静态方法论）和 vuln_cves（外部 CVE 情报）互补：
// 这里存「实战沉淀的方法与技巧」，agent 在任务里检索（kb_search），人工在
// 知识页维护。外部知识包默认 draft，人工采纳后才进入默认检索。
//
// 设计约束：
//   - 新知识默认 status=draft，不参与默认检索。单条错误经验会被后续所有任务
//     检索到，等于让知识库自我污染；人工采纳后才转 approved。
//   - 去重键是「归一化标题」（nkey），同题重复写入只会累加 uses，不堆垃圾。
//   - 检索先用 search_text + FTS/facets 粗筛，再在 Go 侧做 RRF 融合，避免为
//     中文安装分词扩展；条目量级（百到千）下足够快。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	MaxKBTitleRunes       = 200
	MaxKBDescriptionRunes = 2000
	MaxKBDetailRunes      = 20000
	MaxKBCommands         = 50
	MaxKBCommandRunes     = 2000
	MaxKBTags             = 32
	MaxKBTagRunes         = 48
	// MaxKBPageSize 限制单页返回条数（前端分页与 API 共用）。
	MaxKBPageSize = 200
)

// 知识分层。通用知识落 general；安全运营可按 object/infra/weakness/evidence
// 四层组织（对象/基础设施/弱点/证据）。
const (
	KBLayerGeneral  = "general"
	KBLayerObject   = "object"
	KBLayerInfra    = "infra"
	KBLayerWeakness = "weakness"
	KBLayerEvidence = "evidence"
)

const (
	KBStatusDraft      = "draft"
	KBStatusApproved   = "approved"
	KBStatusDeprecated = "deprecated"
)

const (
	KBOriginSeed    = "seed"
	KBOriginLearned = "learned"
	KBOriginManual  = "manual"
)

const (
	KBConfidenceObserved = "observed"
	KBConfidenceInferred = "inferred"
)

var (
	ErrKBEntryInvalid      = errors.New("invalid kb entry")
	ErrKBEntryNameConflict = errors.New("kb entry already exists")
	ErrKBEntryNotFound     = errors.New("kb entry not found")
)

// ValidKBLayer 校验知识分层。
func ValidKBLayer(v string) bool {
	switch v {
	case KBLayerGeneral, KBLayerObject, KBLayerInfra, KBLayerWeakness, KBLayerEvidence:
		return true
	}
	return false
}

// ValidKBStatus 校验条目状态。
func ValidKBStatus(v string) bool {
	switch v {
	case KBStatusDraft, KBStatusApproved, KBStatusDeprecated:
		return true
	}
	return false
}

// ValidKBOrigin 校验来源类型。
func ValidKBOrigin(v string) bool {
	switch v {
	case KBOriginSeed, KBOriginLearned, KBOriginManual:
		return true
	}
	return false
}

// ValidKBConfidence 校验置信度。
func ValidKBConfidence(v string) bool {
	switch v {
	case KBConfidenceObserved, KBConfidenceInferred:
		return true
	}
	return false
}

// KBEntry 是知识库的一条知识。
type KBEntry struct {
	ID          int64      `json:"id"`
	NKey        string     `json:"-"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Detail      string     `json:"detail"`
	Commands    []string   `json:"commands"`
	Tags        []string   `json:"tags"`
	Layer       string     `json:"layer"`
	Origin      string     `json:"origin"`
	Status      string     `json:"status"`
	Confidence  string     `json:"confidence"`
	Source      string     `json:"source"`
	TaskID      *int64     `json:"task_id,omitempty"`
	FindingID   *int64     `json:"finding_id,omitempty"`
	Uses        int        `json:"uses"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// v2 结构化字段：检索/筛选用；旧条目为空。
	QualityScore float64    `json:"quality_score,omitempty"`
	Products     []string   `json:"products,omitempty"`
	VulnClasses  []string   `json:"vuln_classes,omitempty"`
	Platforms    []string   `json:"platforms,omitempty"`
	CVEIDs       []string   `json:"cve_ids,omitempty"`
	License      string     `json:"license,omitempty"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
}

// KBEntryInput 是创建/回写载荷（已归一化后使用）。
type KBEntryInput struct {
	Title       string
	Description string
	Detail      string
	Commands    []string
	Tags        []string
	Layer       string
	Origin      string
	Status      string
	Confidence  string
	Source      string
	TaskID      *int64
	FindingID   *int64
	CreatedBy   string
	Products    []string
	VulnClasses []string
	Platforms   []string
	CVEIDs      []string
	License     string
}

// KBEntryPatch 只改显式设置的字段。
type KBEntryPatch struct {
	Title          *string
	Description    *string
	Detail         *string
	Commands       []string
	SetCommands    bool
	Tags           []string
	SetTags        bool
	Layer          *string
	Status         *string
	Confidence     *string
	Source         *string
	Products       []string
	SetProducts    bool
	VulnClasses    []string
	SetVulnClasses bool
	Platforms      []string
	SetPlatforms   bool
	CVEIDs         []string
	SetCVEIDs      bool
	License        *string
}

// KBFilter 是管理页的筛选条件（精确匹配 + 标题/正文模糊搜索）。
type KBFilter struct {
	Query       string
	Layer       string
	Status      string
	Origin      string
	Tags        []string
	MinQuality  float64
	Products    []string
	VulnClasses []string
}

// KBSearchQuery 是 agent 检索用的条件。
type KBSearchQuery struct {
	Query             string
	Tags              []string
	Layers            []string
	Limit             int
	IncludeDraft      bool // 默认 false：草稿不参与检索
	IncludeDeprecated bool
	// NoLog 为 true 时关闭检索日志（评测/只读库用）。
	NoLog bool
	// Mode：auto（默认 v2，失败回退 legacy）/ v2 / legacy。
	Mode        string
	MinQuality  float64
	Products    []string
	VulnClasses []string
	CVEIDs      []string
	// Actor/TaskID 是检索日志的归因必填项。Actor 为空时落库为 "unknown"；
	// TaskID=0 表示非任务会话（聊天/评测），任务内调用必须传真实 tasks.id。
	Actor  string
	TaskID int64
}

// KBSearchHit 是带打分的检索结果（只回摘要，正文走 Get）。
type KBSearchHit struct {
	*KBEntry
	Score    float64  `json:"score"`
	Snippet  string   `json:"snippet,omitempty"`
	Channels []string `json:"channels,omitempty"`
}

const kbEntryCols = `id, nkey, title, description, detail, commands, tags, layer, origin,
       status, confidence, source, task_id, finding_id, uses, last_used_at,
       created_by, created_at, updated_at, quality_score, products, vuln_classes,
       platforms, cve_ids, license, verified_at`

// KBStore 是知识库仓储。全局共享（跨任务），构造后长期持有。
//
// learnedStatus 是 agent 回写条目的默认状态：默认 draft（待审），运维可在设置页
// 切成 approved（自动生效）。用 atomic 存，设置变更时热更新，不影响正在跑的任务。
type KBStore struct {
	db            *DB
	learnedStatus atomic.Value // string
}

// KB 返回知识库仓储。
func (d *DB) KB() *KBStore { return &KBStore{db: d} }

// SetLearnedStatus 设置 agent 回写条目的默认状态（draft/approved）。
func (s *KBStore) SetLearnedStatus(status string) {
	if !ValidKBStatus(status) {
		return
	}
	s.learnedStatus.Store(status)
}

func (s *KBStore) defaultLearnedStatus() string {
	if v, ok := s.learnedStatus.Load().(string); ok && v != "" {
		return v
	}
	return KBStatusDraft
}

func scanKBEntry(row interface{ Scan(...any) error }) (KBEntry, error) {
	var e KBEntry
	var cmdRaw, tagRaw, prodRaw, classRaw, platRaw, cveRaw []byte
	if err := row.Scan(&e.ID, &e.NKey, &e.Title, &e.Description, &e.Detail,
		&cmdRaw, &tagRaw, &e.Layer, &e.Origin, &e.Status, &e.Confidence,
		&e.Source, &e.TaskID, &e.FindingID, &e.Uses, &e.LastUsedAt,
		&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt, &e.QualityScore,
		&prodRaw, &classRaw, &platRaw, &cveRaw, &e.License, &e.VerifiedAt); err != nil {
		return e, err
	}
	e.Commands = []string{}
	if len(cmdRaw) > 0 {
		if err := json.Unmarshal(cmdRaw, &e.Commands); err != nil {
			return e, err
		}
	}
	e.Tags = []string{}
	if len(tagRaw) > 0 {
		if err := json.Unmarshal(tagRaw, &e.Tags); err != nil {
			return e, err
		}
	}
	e.Products = jsonStrings(prodRaw)
	e.VulnClasses = jsonStrings(classRaw)
	e.Platforms = jsonStrings(platRaw)
	e.CVEIDs = jsonStrings(cveRaw)
	return e, nil
}

func jsonStrings(raw []byte) []string {
	out := []string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func marshalKBStrings(items []string) ([]byte, error) {
	if items == nil {
		items = []string{}
	}
	return json.Marshal(items)
}

// kbNKey 是去重键：标题折叠空白 + 转小写。同题不同措辞不会自动合并——
// 宁可留两条让人工合并，也不要把不同经验错误折叠成一条。
func kbNKey(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

func kbNormalizeTags(tags []string) ([]string, error) {
	if len(tags) > MaxKBTags {
		return nil, fmt.Errorf("%w: too many tags (max %d)", ErrKBEntryInvalid, MaxKBTags)
	}
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, tag := range tags {
		t := strings.ToLower(strings.TrimSpace(tag))
		if t == "" || seen[t] {
			continue
		}
		if utf8.RuneCountInString(t) > MaxKBTagRunes {
			return nil, fmt.Errorf("%w: tag %q too long", ErrKBEntryInvalid, t)
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func kbNormalizeCommands(cmds []string) ([]string, error) {
	if len(cmds) > MaxKBCommands {
		return nil, fmt.Errorf("%w: too many commands (max %d)", ErrKBEntryInvalid, MaxKBCommands)
	}
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		c = strings.TrimSpace(utf8Clean(c))
		if c == "" {
			continue
		}
		if utf8.RuneCountInString(c) > MaxKBCommandRunes {
			return nil, fmt.Errorf("%w: one command exceeds %d characters", ErrKBEntryInvalid, MaxKBCommandRunes)
		}
		out = append(out, c)
	}
	return out, nil
}

func normalizeKBEntryInput(in KBEntryInput) (KBEntryInput, string, error) {
	in.Title = strings.Join(strings.Fields(utf8Clean(in.Title)), " ")
	in.Description = strings.TrimSpace(utf8Clean(in.Description))
	in.Detail = strings.TrimSpace(utf8Clean(in.Detail))
	in.Source = strings.TrimSpace(utf8Clean(in.Source))
	in.CreatedBy = strings.TrimSpace(utf8Clean(in.CreatedBy))

	switch {
	case in.Title == "":
		return in, "", fmt.Errorf("%w: title is required", ErrKBEntryInvalid)
	case utf8.RuneCountInString(in.Title) > MaxKBTitleRunes:
		return in, "", fmt.Errorf("%w: title exceeds %d characters", ErrKBEntryInvalid, MaxKBTitleRunes)
	case utf8.RuneCountInString(in.Description) > MaxKBDescriptionRunes:
		return in, "", fmt.Errorf("%w: description exceeds %d characters", ErrKBEntryInvalid, MaxKBDescriptionRunes)
	case utf8.RuneCountInString(in.Detail) > MaxKBDetailRunes:
		return in, "", fmt.Errorf("%w: detail exceeds %d characters", ErrKBEntryInvalid, MaxKBDetailRunes)
	}

	var err error
	if in.Commands, err = kbNormalizeCommands(in.Commands); err != nil {
		return in, "", err
	}
	if in.Tags, err = kbNormalizeTags(in.Tags); err != nil {
		return in, "", err
	}

	if in.Layer == "" {
		in.Layer = KBLayerGeneral
	}
	if !ValidKBLayer(in.Layer) {
		return in, "", fmt.Errorf("%w: unknown layer %q", ErrKBEntryInvalid, in.Layer)
	}
	if in.Origin == "" {
		in.Origin = KBOriginManual
	}
	if !ValidKBOrigin(in.Origin) {
		return in, "", fmt.Errorf("%w: unknown origin %q", ErrKBEntryInvalid, in.Origin)
	}
	if in.Status == "" {
		in.Status = KBStatusDraft
	}
	if !ValidKBStatus(in.Status) {
		return in, "", fmt.Errorf("%w: unknown status %q", ErrKBEntryInvalid, in.Status)
	}
	if in.Confidence == "" {
		in.Confidence = KBConfidenceObserved
	}
	if !ValidKBConfidence(in.Confidence) {
		return in, "", fmt.Errorf("%w: unknown confidence %q", ErrKBEntryInvalid, in.Confidence)
	}
	in.Products = normalizeKBFacetList(in.Products)
	in.VulnClasses = normalizeKBFacetList(in.VulnClasses)
	in.Platforms = normalizeKBFacetList(in.Platforms)
	in.CVEIDs = normalizeKBFacetList(in.CVEIDs)
	in.License = strings.TrimSpace(utf8Clean(in.License))
	return in, kbNKey(in.Title), nil
}

func kbUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Create 新建一条知识。同标题已存在时返回 ErrKBEntryNameConflict。
func (s *KBStore) Create(in KBEntryInput) (*KBEntry, error) {
	in, nkey, err := normalizeKBEntryInput(in)
	if err != nil {
		return nil, err
	}
	cmdJSON, err := marshalKBStrings(in.Commands)
	if err != nil {
		return nil, err
	}
	tagJSON, err := marshalKBStrings(in.Tags)
	if err != nil {
		return nil, err
	}
	in.Products, in.VulnClasses, in.Platforms, in.CVEIDs = inferKBFacets(in)
	searchText := BuildKBSearchText(in)
	prodJSON, _ := marshalKBStrings(in.Products)
	classJSON, _ := marshalKBStrings(in.VulnClasses)
	platJSON, _ := marshalKBStrings(in.Platforms)
	cveJSON, _ := marshalKBStrings(in.CVEIDs)
	quality := ComputeKBQuality(in, 0, nil)
	e, err := scanKBEntry(s.db.QueryRow(`
INSERT INTO kb_entries(title, nkey, description, detail, commands, tags, layer,
                       origin, status, confidence, source, task_id, finding_id, created_by,
                       search_text, products, vuln_classes, platforms, cve_ids, license,
                       quality_score, verified_at)
VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11,$12,$13,$14,
        $15,$16::jsonb,$17::jsonb,$18::jsonb,$19::jsonb,$20,$21,
        CASE WHEN $9='approved' THEN now() END)
ON CONFLICT (nkey) DO NOTHING
RETURNING `+kbEntryCols,
		in.Title, nkey, in.Description, in.Detail, cmdJSON, tagJSON, in.Layer,
		in.Origin, in.Status, in.Confidence, in.Source, in.TaskID, in.FindingID, in.CreatedBy,
		searchText, prodJSON, classJSON, platJSON, cveJSON, in.License, quality))
	if err == sql.ErrNoRows {
		return nil, ErrKBEntryNameConflict
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// UpsertLearned 写入 agent 学到的知识：同标题已存在时只累加 uses 并返回原条目，
// 不覆盖人工整理过的正文。created 表示本次是否新建。
func (s *KBStore) UpsertLearned(in KBEntryInput) (*KBEntry, bool, error) {
	in.Origin = KBOriginLearned
	if in.Status == "" {
		in.Status = s.defaultLearnedStatus()
	}
	in, nkey, err := normalizeKBEntryInput(in)
	if err != nil {
		return nil, false, err
	}
	cmdJSON, err := marshalKBStrings(in.Commands)
	if err != nil {
		return nil, false, err
	}
	tagJSON, err := marshalKBStrings(in.Tags)
	if err != nil {
		return nil, false, err
	}
	in.Products, in.VulnClasses, in.Platforms, in.CVEIDs = inferKBFacets(in)
	searchText := BuildKBSearchText(in)
	prodJSON, _ := marshalKBStrings(in.Products)
	classJSON, _ := marshalKBStrings(in.VulnClasses)
	platJSON, _ := marshalKBStrings(in.Platforms)
	cveJSON, _ := marshalKBStrings(in.CVEIDs)
	quality := ComputeKBQuality(in, 0, nil)
	var e KBEntry
	var cmdRaw, tagRaw, prodRaw, classRaw, platRaw, cveRaw []byte
	var inserted bool
	err = s.db.QueryRow(`
INSERT INTO kb_entries(title, nkey, description, detail, commands, tags, layer,
                       origin, status, confidence, source, task_id, finding_id, created_by,
                       search_text, products, vuln_classes, platforms, cve_ids, license,
                       quality_score, verified_at)
VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11,$12,$13,$14,
        $15,$16::jsonb,$17::jsonb,$18::jsonb,$19::jsonb,$20,$21,
        CASE WHEN $9='approved' THEN now() END)
ON CONFLICT (nkey) DO UPDATE
   SET uses = kb_entries.uses + 1, last_used_at = now()
RETURNING `+kbEntryCols+`, (xmax = 0)`,
		in.Title, nkey, in.Description, in.Detail, cmdJSON, tagJSON, in.Layer,
		in.Origin, in.Status, in.Confidence, in.Source, in.TaskID, in.FindingID, in.CreatedBy,
		searchText, prodJSON, classJSON, platJSON, cveJSON, in.License, quality,
	).Scan(&e.ID, &e.NKey, &e.Title, &e.Description, &e.Detail,
		&cmdRaw, &tagRaw, &e.Layer, &e.Origin, &e.Status, &e.Confidence,
		&e.Source, &e.TaskID, &e.FindingID, &e.Uses, &e.LastUsedAt,
		&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt, &e.QualityScore,
		&prodRaw, &classRaw, &platRaw, &cveRaw, &e.License, &e.VerifiedAt, &inserted)
	if err != nil {
		return nil, false, err
	}
	e.Commands = []string{}
	_ = json.Unmarshal(cmdRaw, &e.Commands)
	e.Tags = []string{}
	_ = json.Unmarshal(tagRaw, &e.Tags)
	e.Products = jsonStrings(prodRaw)
	e.VulnClasses = jsonStrings(classRaw)
	e.Platforms = jsonStrings(platRaw)
	e.CVEIDs = jsonStrings(cveRaw)
	return &e, inserted, nil
}

// Get 按 id 取一条。不存在返回 ErrKBEntryNotFound。
func (s *KBStore) Get(id int64) (*KBEntry, error) {
	e, err := scanKBEntry(s.db.QueryRow(`SELECT `+kbEntryCols+` FROM kb_entries WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, ErrKBEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// GetByNKey 按去重键取一条（供回写判重）。
func (s *KBStore) GetByNKey(nkey string) (*KBEntry, error) {
	e, err := scanKBEntry(s.db.QueryRow(`SELECT `+kbEntryCols+` FROM kb_entries WHERE nkey=$1`, nkey))
	if err == sql.ErrNoRows {
		return nil, ErrKBEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// kbTextOR 把查询切成 token，生成「任一词命中」的 ILIKE 粗筛条件。
//
// 不能用整串 `%q%` 匹配：知识条目里的写法几乎不会和查询逐字相同
// （查 "ThinkPHP RCE"、正文写 "ThinkPHP 路由 RCE"），整串匹配会先被粗筛
// 丢掉，后面打分再准也没用。
func kbTextOR(alias, query string, args []any) (string, []any) {
	tokens := kbTokenize(query)
	if len(tokens) == 0 {
		return "", args
	}
	seen := map[string]bool{}
	ors := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if seen[t] {
			continue
		}
		if len(ors) >= 8 { // 粗筛条件不宜无限膨胀
			break
		}
		seen[t] = true
		args = append(args, "%"+escapeLike(t)+"%")
		n := len(args)
		ors = append(ors, fmt.Sprintf(
			`(%[1]s.title ILIKE $%[2]d ESCAPE '\' OR %[1]s.description ILIKE $%[2]d ESCAPE '\' OR %[1]s.detail ILIKE $%[2]d ESCAPE '\')`,
			alias, n))
	}
	if len(ors) == 0 {
		return "", args
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// kbWhere 构造管理页筛选条件（共享给分页与计数）。
func kbWhere(f KBFilter) (string, []any) {
	var conds []string
	var args []any
	add := func(col, val string) {
		if val == "" {
			return
		}
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("k.%s = $%d", col, len(args)))
	}
	add("layer", f.Layer)
	add("status", f.Status)
	add("origin", f.Origin)
	if q := strings.TrimSpace(f.Query); q != "" {
		var clause string
		clause, args = kbTextOR("k", q, args)
		if clause != "" {
			conds = append(conds, clause)
		}
	}
	for _, tag := range f.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		raw, _ := json.Marshal([]string{tag})
		args = append(args, raw)
		conds = append(conds, fmt.Sprintf("k.tags @> $%d::jsonb", len(args)))
	}
	addFacet := func(col string, values []string) {
		for _, v := range values {
			v = strings.ToLower(strings.TrimSpace(v))
			if v == "" {
				continue
			}
			raw, _ := json.Marshal([]string{v})
			args = append(args, raw)
			conds = append(conds, fmt.Sprintf("k.%s @> $%d::jsonb", col, len(args)))
		}
	}
	addFacet("products", f.Products)
	addFacet("vuln_classes", f.VulnClasses)
	if f.MinQuality > 0 {
		args = append(args, f.MinQuality)
		conds = append(conds, fmt.Sprintf("k.quality_score >= $%d", len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// List 返回管理页的一页数据与总数。page 从 1 开始。
func (s *KBStore) List(f KBFilter, page, pageSize int) ([]*KBEntry, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > MaxKBPageSize {
		pageSize = MaxKBPageSize
	}
	where, args := kbWhere(f)

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM kb_entries k`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || page > (total-1)/pageSize+1 {
		return []*KBEntry{}, total, nil
	}

	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	q := fmt.Sprintf(`SELECT %s FROM kb_entries k%s
ORDER BY k.updated_at DESC, k.id DESC
LIMIT $%d OFFSET $%d`, kbEntryCols, where, len(args)+1, len(args)+2)
	rows, err := s.db.Query(q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*KBEntry{}
	for rows.Next() {
		e, err := scanKBEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}

// Facets 返回各结构化维度的取值计数（管理页筛选器用）。只统计指定状态，
// 空状态表示全部；结果按计数降序、值升序，最多每维 50 项。
func (s *KBStore) Facets(status string) (map[string][]KBFacet, error) {
	out := map[string][]KBFacet{}
	for _, col := range []string{"products", "vuln_classes", "platforms", "cve_ids"} {
		args := []any{}
		where := "k." + col + " IS NOT NULL"
		if status != "" && ValidKBStatus(status) {
			args = append(args, status)
			where += fmt.Sprintf(" AND k.status=$%d", len(args))
		}
		q := fmt.Sprintf(`SELECT value, COUNT(*) FROM kb_entries k,
			LATERAL jsonb_array_elements_text(k.%s) AS value
			WHERE %s GROUP BY value ORDER BY COUNT(*) DESC, value ASC LIMIT 50`, col, where)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		items := []KBFacet{}
		for rows.Next() {
			var f KBFacet
			if err := rows.Scan(&f.Value, &f.Count); err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, f)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		out[col] = items
	}
	return out, nil
}

// KBFacet 是一个 facet 取值及其条目数。
type KBFacet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// KBRetrievalStats 是近 N 天检索质量摘要。
type KBRetrievalStats struct {
	Days          int     `json:"days"`
	Total         int     `json:"total"`
	ZeroResult    int     `json:"zero_result"`
	ZeroRate      float64 `json:"zero_result_rate"`
	P95LatencyMS  float64 `json:"p95_latency_ms"`
	Actors        int     `json:"actors"`
	AttributedPct float64 `json:"attributed_rate"`
}

// RetrievalStats 汇总近 days 天的检索日志。
//
// days 必须以文本参数绑定：pgx 会把 int 推断为 int4，而 SQL 里
// `$1::text || ' days'` 期望 text，直接把 7 传进去会得到
// "cannot cast type integer to text"（线上曾因此 500）。这里先转成字符串，
// 并在 SQL 侧显式 ::text，两层都保证绑定类型正确。
func (s *KBStore) RetrievalStats(days int) (*KBRetrievalStats, error) {
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}
	st := &KBRetrievalStats{Days: days}
	err := s.db.QueryRow(`SELECT count(*),
		count(*) FILTER (WHERE returned=0),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms),0),
		count(DISTINCT actor),
		count(*) FILTER (WHERE actor <> '' AND actor <> 'unknown' AND task_id > 0)
		FROM kb_retrieval_logs
		WHERE created_at > now() - ($1::text || ' days')::interval`, strconv.Itoa(days)).
		Scan(&st.Total, &st.ZeroResult, &st.P95LatencyMS, &st.Actors, &st.AttributedPct)
	if err != nil {
		return nil, err
	}
	if st.Total > 0 {
		st.ZeroRate = float64(st.ZeroResult) / float64(st.Total)
		st.AttributedPct = st.AttributedPct / float64(st.Total)
	}
	return st, nil
}

// Patch 原子更新指定字段。
func (s *KBStore) Patch(id int64, p KBEntryPatch) (*KBEntry, error) {
	if p.Title == nil && p.Description == nil && p.Detail == nil && !p.SetCommands &&
		!p.SetTags && p.Layer == nil && p.Status == nil && p.Confidence == nil && p.Source == nil &&
		!p.SetProducts && !p.SetVulnClasses && !p.SetPlatforms && !p.SetCVEIDs && p.License == nil {
		return nil, fmt.Errorf("%w: no fields supplied", ErrKBEntryInvalid)
	}
	var nkey *string
	if p.Title != nil {
		title := strings.Join(strings.Fields(utf8Clean(*p.Title)), " ")
		if title == "" {
			return nil, fmt.Errorf("%w: title is required", ErrKBEntryInvalid)
		}
		if utf8.RuneCountInString(title) > MaxKBTitleRunes {
			return nil, fmt.Errorf("%w: title exceeds %d characters", ErrKBEntryInvalid, MaxKBTitleRunes)
		}
		key := kbNKey(title)
		p.Title = &title
		nkey = &key
	}
	if p.Layer != nil && !ValidKBLayer(*p.Layer) {
		return nil, fmt.Errorf("%w: unknown layer %q", ErrKBEntryInvalid, *p.Layer)
	}
	if p.Status != nil && !ValidKBStatus(*p.Status) {
		return nil, fmt.Errorf("%w: unknown status %q", ErrKBEntryInvalid, *p.Status)
	}
	if p.Confidence != nil && !ValidKBConfidence(*p.Confidence) {
		return nil, fmt.Errorf("%w: unknown confidence %q", ErrKBEntryInvalid, *p.Confidence)
	}
	var cmds []string
	var err error
	if p.SetCommands {
		if cmds, err = kbNormalizeCommands(p.Commands); err != nil {
			return nil, err
		}
	}
	var tags []string
	if p.SetTags {
		if tags, err = kbNormalizeTags(p.Tags); err != nil {
			return nil, err
		}
	}
	cmdJSON, _ := marshalKBStrings(cmds)
	tagJSON, _ := marshalKBStrings(tags)
	prodJSON, _ := marshalKBStrings(normalizeKBFacetList(p.Products))
	classJSON, _ := marshalKBStrings(normalizeKBFacetList(p.VulnClasses))
	platJSON, _ := marshalKBStrings(normalizeKBFacetList(p.Platforms))
	cveJSON, _ := marshalKBStrings(normalizeKBFacetList(p.CVEIDs))

	e, err := scanKBEntry(s.db.QueryRow(`UPDATE kb_entries
SET title      = CASE WHEN $2 THEN $3::text ELSE title END,
    nkey       = CASE WHEN $2 THEN $4::text ELSE nkey END,
    description= CASE WHEN $5 THEN $6::text ELSE description END,
    detail     = CASE WHEN $7 THEN $8::text ELSE detail END,
    commands   = CASE WHEN $9 THEN $10::jsonb ELSE commands END,
    tags       = CASE WHEN $11 THEN $12::jsonb ELSE tags END,
    layer      = CASE WHEN $13 THEN $14::text ELSE layer END,
    status     = CASE WHEN $15 THEN $16::text ELSE status END,
    confidence = CASE WHEN $17 THEN $18::text ELSE confidence END,
    source     = CASE WHEN $19 THEN $20::text ELSE source END,
    products   = CASE WHEN $21 THEN $22::jsonb ELSE products END,
    vuln_classes = CASE WHEN $23 THEN $24::jsonb ELSE vuln_classes END,
    platforms  = CASE WHEN $25 THEN $26::jsonb ELSE platforms END,
    cve_ids    = CASE WHEN $27 THEN $28::jsonb ELSE cve_ids END,
    license    = CASE WHEN $29 THEN $30::text ELSE license END
WHERE id=$1
RETURNING `+kbEntryCols,
		id,
		p.Title != nil, p.Title, nkey,
		p.Description != nil, p.Description,
		p.Detail != nil, p.Detail,
		p.SetCommands, cmdJSON,
		p.SetTags, tagJSON,
		p.Layer != nil, p.Layer,
		p.Status != nil, p.Status,
		p.Confidence != nil, p.Confidence,
		p.Source != nil, p.Source,
		p.SetProducts, prodJSON,
		p.SetVulnClasses, classJSON,
		p.SetPlatforms, platJSON,
		p.SetCVEIDs, cveJSON,
		p.License != nil, p.License,
	))
	if err == sql.ErrNoRows {
		return nil, ErrKBEntryNotFound
	}
	if kbUniqueViolation(err) {
		return nil, ErrKBEntryNameConflict
	}
	if err != nil {
		return nil, err
	}
	return s.refreshKBDerived(context.Background(), e.ID)
}

// SetStatus 切换条目状态（批准/弃用/退回草稿）。
func (s *KBStore) SetStatus(id int64, status string) (*KBEntry, error) {
	if !ValidKBStatus(status) {
		return nil, fmt.Errorf("%w: unknown status %q", ErrKBEntryInvalid, status)
	}
	return s.Patch(id, KBEntryPatch{Status: &status})
}

// MarkUsed 记录一次采纳，用于排序与质量判断。
func (s *KBStore) MarkUsed(id int64) error {
	res, err := s.db.Exec(`UPDATE kb_entries SET uses = uses + 1, last_used_at = now() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrKBEntryNotFound
	}
	return nil
}

// Delete 删除一条，返回是否存在。
func (s *KBStore) Delete(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM kb_entries WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountByStatus 返回各状态条目数（管理页统计）。
func (s *KBStore) CountByStatus() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM kb_entries GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// kbTokenize 切分检索词：中文/日文按二元组（避免单字把「边界/界面」混在一起），
// 其余按连续字母/数字成词；不引入分词依赖。
func kbTokenize(s string) []string {
	out := []string{}
	var cur strings.Builder
	var han []rune
	flushWord := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	// 中文按二元组切分：单字会把「边界」「界面」混在一起，粗筛和打分都失真；
	// 单字串（如「路」）才退回单字。日文假名同理。
	flushHan := func() {
		if len(han) == 1 {
			out = append(out, string(han[0]))
		}
		for i := 0; i+1 < len(han); i++ {
			out = append(out, string(han[i:i+2]))
		}
		han = han[:0]
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			cur.WriteRune(r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return out
}

// escapeLike 转义 ILIKE 通配符，保证用户输入按字面匹配。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
