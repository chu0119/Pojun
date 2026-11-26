package db

// 内置/知识包条目的播种与加载。
//
// 种子正文放在 skills/<skill>/kb-packs/<id>/{manifest.json,entries.jsonl}，
// 随技能目录分发，便于人工审阅与版本管理。启动时由 server 读取后调用
// SeedKBPacks 幂等播种：settings flag 记 id+version，插过即跳过；用户后续的
// 修改/删除不会被复活。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// kbSeedEntry 是种子/知识包 JSON 的结构。与 KBEntryInput 的差别是它不含
// 运行期字段（task_id / created_by），并允许省略 layer/confidence/source。
// Verification/Boundary 是可选的显式字段；缺省时由 detail 里的「验证/误报」
// 小节承载（lint 会检查至少一种形式存在）。
type kbSeedEntry struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Detail       string   `json:"detail"`
	Commands     []string `json:"commands"`
	Tags         []string `json:"tags"`
	Layer        string   `json:"layer"`
	Confidence   string   `json:"confidence"`
	Source       string   `json:"source"`
	Products     []string `json:"products"`
	VulnClasses  []string `json:"vuln_classes"`
	Platforms    []string `json:"platforms"`
	CVEIDs       []string `json:"cve_ids"`
	License      string   `json:"license"`
	Verification string   `json:"verification,omitempty"`
	Boundary     string   `json:"boundary,omitempty"`
}

// LoadKBSeeds 读取种子目录（*.json，每个文件一条或一个数组）或单个文件。
func LoadKBSeeds(path string) ([]KBEntryInput, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			files = append(files, filepath.Join(path, e.Name()))
		}
		sort.Strings(files)
	} else {
		files = []string{path}
	}

	out := []KBEntryInput{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		// 一个文件既可以是单条对象，也可以是数组。
		var one kbSeedEntry
		if err := json.Unmarshal(raw, &one); err == nil && one.Title != "" {
			out = append(out, kbSeedEntryToInput(one))
			continue
		}
		var many []kbSeedEntry
		if err := json.Unmarshal(raw, &many); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		for _, e := range many {
			if e.Title == "" {
				continue
			}
			out = append(out, kbSeedEntryToInput(e))
		}
	}
	return out, nil
}

func kbSeedEntryToInput(e kbSeedEntry) KBEntryInput {
	layer := e.Layer
	if layer == "" {
		layer = KBLayerGeneral
	}
	conf := e.Confidence
	if conf == "" {
		conf = KBConfidenceObserved
	}
	detail := strings.TrimSpace(e.Detail)
	if v := strings.TrimSpace(e.Verification); v != "" && !strings.Contains(detail, v) {
		detail += "\n\n验证：" + v
	}
	if b := strings.TrimSpace(e.Boundary); b != "" && !strings.Contains(detail, b) {
		detail += "\n\n边界/误报：" + b
	}
	return KBEntryInput{
		Title:       e.Title,
		Description: e.Description,
		Detail:      detail,
		Commands:    e.Commands,
		Tags:        e.Tags,
		Layer:       layer,
		Origin:      KBOriginSeed,
		Status:      KBStatusApproved, // 种子是人工整理过的，直接可用
		Confidence:  conf,
		Source:      e.Source,
		CreatedBy:   "system",
		Products:    e.Products,
		VulnClasses: e.VulnClasses,
		Platforms:   e.Platforms,
		CVEIDs:      e.CVEIDs,
		License:     e.License,
	}
}

// SeedKBEntriesBatch 用调用方给定的 setting flag 幂等播种一批条目。
// 内容包（kb_pack_<id>_<version>）与手工导入共用；flag 已 done 时整批跳过，
// 保证同一版本只播一次、不复活用户删掉的条目。
func (d *DB) SeedKBEntriesBatch(flag string, entries []KBEntryInput) error {
	if v, _, _ := d.GetSetting(flag); v == "done" {
		return nil
	}
	if len(entries) == 0 {
		// 没有种子就先把批次标记落下，避免每次启动重复扫描。
		return d.SetSetting(flag, "done")
	}
	s := d.KB()
	for _, in := range entries {
		if in.Origin == "" {
			in.Origin = KBOriginSeed
		}
		if in.Status == "" {
			in.Status = KBStatusApproved
		}
		if _, err := s.Create(in); err != nil {
			if err == ErrKBEntryNameConflict {
				continue
			}
			return fmt.Errorf("kb seed %q: %w", in.Title, err)
		}
	}
	return d.SetSetting(flag, "done")
}
