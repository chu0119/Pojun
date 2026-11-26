package db

// 知识库内容包（KB packs）。
//
// 包格式（只读文件，不执行、不解包外部数据）：
//
//	skills/<skill>/kb-packs/<pack-id>/
//	  manifest.json   {id,name,version,schema,license,review_status,sources,entry_count,sha256}
//	  entries.jsonl   一行一条（也兼容 entries.json 数组/单条）
//
// 包放在 skills 树内，随现有发布包分发，不需要改 Docker/安装器的拷贝清单。
// 启动播种按 id+version 幂等：同一版本只播一次；改版本号才会重新扫，但同标题
// 条目由 kb_entries 的 nkey 唯一键挡住，不会覆盖用户修改。
//
// 外部导入（pojun-maint library import）默认 draft；只有显式 --status approved
// 才进默认检索。随仓库分发的 skills 包按 manifest.review_status 播种。

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const kbPackManifestFile = "manifest.json"

// KBPackManifest 是内容包元数据。ReviewStatus 决定随包播种条目的初始状态：
// approved 的包视为已人工抽检；其余一律 draft，需在知识页批准后参与检索。
type KBPackManifest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Schema       int      `json:"schema"`
	License      string   `json:"license"`
	ReviewStatus string   `json:"review_status"`
	Sources      []string `json:"sources"`
	EntryCount   int      `json:"entry_count"`
	SHA256       string   `json:"sha256"`
}

// KBPack 是一个已解析、已校验结构的内容包。
type KBPack struct {
	Dir      string
	Manifest KBPackManifest
	Entries  []KBEntryInput
}

// LoadKBPack 读取一个内容包并做结构校验。它不写库，lint/import 与启动播种共用。
func LoadKBPack(dir string) (*KBPack, error) {
	raw, err := os.ReadFile(filepath.Join(dir, kbPackManifestFile))
	if err != nil {
		return nil, fmt.Errorf("读取 manifest: %w", err)
	}
	var m KBPackManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析 manifest: %w", err)
	}
	if m.Schema != 1 && m.Schema != 2 {
		return nil, fmt.Errorf("不支持的包 schema=%d（只支持 1/2）", m.Schema)
	}
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Version) == "" {
		return nil, fmt.Errorf("manifest 缺少 id/version")
	}
	switch m.ReviewStatus {
	case "", "draft", "approved":
	default:
		return nil, fmt.Errorf("review_status=%q 只能是 draft/approved", m.ReviewStatus)
	}

	entriesPath := filepath.Join(dir, "entries.jsonl")
	data, err := os.ReadFile(entriesPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取 entries.jsonl: %w", err)
		}
		entriesPath = filepath.Join(dir, "entries.json")
		data, err = os.ReadFile(entriesPath)
		if err != nil {
			return nil, fmt.Errorf("读取 entries.jsonl/entries.json: %w", err)
		}
	}
	if m.SHA256 != "" {
		// Git autocrlf 可能把 .jsonl 转成 CRLF；manifest 记录的是 LF 内容哈希。
		// 校验前归一换行，避免 Windows 新克隆整包被误判损坏。
		hashData := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		sum := sha256.Sum256(hashData)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, strings.TrimSpace(m.SHA256)) {
			return nil, fmt.Errorf("entries 校验失败：manifest=%s 实际=%s", m.SHA256, got)
		}
	}

	entries, err := decodeKBPackEntries(data, strings.HasSuffix(entriesPath, ".jsonl"))
	if err != nil {
		return nil, err
	}
	status := KBStatusDraft
	if m.ReviewStatus == "approved" {
		status = KBStatusApproved
	}
	for i := range entries {
		entries[i].Status = status
		if entries[i].Source == "" && len(m.Sources) == 1 {
			entries[i].Source = strings.TrimSpace(m.Sources[0])
		}
	}
	if m.EntryCount > 0 && len(entries) != m.EntryCount {
		return nil, fmt.Errorf("entry_count=%d，实际解析 %d 条", m.EntryCount, len(entries))
	}
	return &KBPack{Dir: dir, Manifest: m, Entries: entries}, nil
}

func decodeKBPackEntries(data []byte, jsonl bool) ([]KBEntryInput, error) {
	if !jsonl {
		// entries.json 与旧种子同格式：单对象或数组。
		var one kbSeedEntry
		if err := json.Unmarshal(data, &one); err == nil && strings.TrimSpace(one.Title) != "" {
			return []KBEntryInput{kbSeedEntryToInput(one)}, nil
		}
		var many []kbSeedEntry
		if err := json.Unmarshal(data, &many); err != nil {
			return nil, fmt.Errorf("解析 entries.json: %w", err)
		}
		out := make([]KBEntryInput, 0, len(many))
		for _, e := range many {
			if strings.TrimSpace(e.Title) == "" {
				continue
			}
			out = append(out, kbSeedEntryToInput(e))
		}
		return out, nil
	}

	out := []KBEntryInput{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 8<<20) // 单条正文上限 20k 字符，8MB 足够
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var e kbSeedEntry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return nil, fmt.Errorf("entries.jsonl 第 %d 行: %w", line, err)
		}
		if strings.TrimSpace(e.Title) == "" {
			return nil, fmt.Errorf("entries.jsonl 第 %d 行缺少 title", line)
		}
		out = append(out, kbSeedEntryToInput(e))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 entries.jsonl: %w", err)
	}
	return out, nil
}

// DiscoverKBPacks 返回 root 下所有 <skill>/kb-packs/<id> 目录（含 manifest.json）。
// root 不存在时返回空（未安装 skills 的部署不应因此启动失败）。
func DiscoverKBPacks(root string) ([]string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, nil
	}
	pattern := filepath.Join(root, "*", "kb-packs", "*")
	dirs, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, kbPackManifestFile)); err != nil {
			continue
		}
		out = append(out, dir)
	}
	sort.Strings(out)
	return out, nil
}

// SeedKBPacks 扫描并幂等播种 root 下的全部内容包。单个包失败不阻断其它包；
// 调用方负责记录 errs（server 启动日志 / maint 输出）。
func (d *DB) SeedKBPacks(root string) (seeded, skipped int, errs []error) {
	dirs, err := DiscoverKBPacks(root)
	if err != nil {
		return 0, 0, []error{err}
	}
	for _, dir := range dirs {
		p, err := LoadKBPack(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
			continue
		}
		flag := kbPackFlag(p.Manifest.ID, p.Manifest.Version)
		if v, _, _ := d.GetSetting(flag); v == "done" {
			skipped++
			continue
		}
		if err := d.SeedKBPack(p); err != nil {
			errs = append(errs, fmt.Errorf("%s@%s: %w", p.Manifest.ID, p.Manifest.Version, err))
			continue
		}
		seeded++
	}
	return seeded, skipped, errs
}

// SeedKBPack 按 id+version 幂等播种一个内容包。
func (d *DB) SeedKBPack(p *KBPack) error {
	if p == nil {
		return fmt.Errorf("空知识包")
	}
	return d.SeedKBEntriesBatch(kbPackFlag(p.Manifest.ID, p.Manifest.Version), p.Entries)
}

// kbPackFlag 生成只含字母数字和 ._- 的 setting key。
func kbPackFlag(id, version string) string {
	return "kb_pack_" + safeFlagPart(id) + "_" + safeFlagPart(version)
}

func safeFlagPart(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "pack"
	}
	return out
}

// SetKBPackStatus 覆盖包内全部条目的初始状态（外部导入默认 draft 用）。
func SetKBPackStatus(p *KBPack, status string) error {
	if !ValidKBStatus(status) {
		return fmt.Errorf("%w: unknown status %q", ErrKBEntryInvalid, status)
	}
	for i := range p.Entries {
		p.Entries[i].Status = status
	}
	return nil
}

// ValidateKBPack 做内容完整性校验（lint 与单测共用，不连库）。返回的问题
// 均视为硬错误：缺 source、缺可复现命令、缺验证方法、缺边界说明、标题重复、
// 单条字段超限。破坏性命令/密钥/公网 IP 由 pojun-maint library lint 额外扫描。
func ValidateKBPack(p *KBPack) []error {
	if p == nil {
		return []error{fmt.Errorf("空知识包")}
	}
	var errs []error
	m := p.Manifest
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Version) == "" {
		errs = append(errs, fmt.Errorf("manifest 缺少 id/version"))
	}
	if strings.TrimSpace(m.Name) == "" {
		errs = append(errs, fmt.Errorf("manifest 缺少 name"))
	}
	if strings.TrimSpace(m.License) == "" {
		errs = append(errs, fmt.Errorf("manifest 缺少 license（外部来源必须写许可证/使用方式）"))
	}
	if len(m.Sources) == 0 {
		errs = append(errs, fmt.Errorf("manifest sources 为空（必须列出原始来源 URL/标准号）"))
	}
	if m.EntryCount > 0 && len(p.Entries) != m.EntryCount {
		errs = append(errs, fmt.Errorf("entry_count=%d，实际 %d 条", m.EntryCount, len(p.Entries)))
	}
	seen := map[string]string{}
	for i, e := range p.Entries {
		label := fmt.Sprintf("第 %d 条《%s》", i+1, e.Title)
		key := kbNKey(e.Title)
		if key == "" {
			errs = append(errs, fmt.Errorf("%s 缺少 title", label))
			continue
		}
		if prev, ok := seen[key]; ok {
			errs = append(errs, fmt.Errorf("%s 与《%s》标题重复", label, prev))
		} else {
			seen[key] = e.Title
		}
		if strings.TrimSpace(e.Source) == "" {
			errs = append(errs, fmt.Errorf("%s 缺少 source（必须能追溯）", label))
		}
		if strings.TrimSpace(e.Description) == "" && strings.TrimSpace(e.Detail) == "" {
			errs = append(errs, fmt.Errorf("%s description/detail 都为空", label))
		}
		if len(e.Commands) == 0 {
			errs = append(errs, fmt.Errorf("%s 缺少可复现命令", label))
		}
		if !kbHasVerification(e) {
			errs = append(errs, fmt.Errorf("%s 缺少验证方法（detail 需含「验证」或 verification 字段）", label))
		}
		if !kbHasBoundary(e) {
			errs = append(errs, fmt.Errorf("%s 缺少边界/误报说明（detail 需含「误报/边界/前置/适用/限制」）", label))
		}
		if err := ValidateKBEntryInput(e); err != nil {
			errs = append(errs, fmt.Errorf("%s %v", label, err))
		}
	}
	return errs
}

func kbHasVerification(e KBEntryInput) bool {
	return strings.Contains(e.Detail, "验证") || strings.Contains(e.Detail, "复现")
}

func kbHasBoundary(e KBEntryInput) bool {
	for _, k := range []string{"误报", "边界", "前置", "适用", "限制", "不适用"} {
		if strings.Contains(e.Detail, k) {
			return true
		}
	}
	return false
}

// ValidateKBEntryInput 暴露给 lint/import 的条目校验（复用 Create 的归一化规则）。
func ValidateKBEntryInput(in KBEntryInput) error {
	_, _, err := normalizeKBEntryInput(in)
	return err
}
