package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// newImageTaskDir 造一个 tasks/<id>/i<n> 形态的工作目录，返回 runDir 与任务根。
func newImageTaskDir(t *testing.T) (runDir, taskRoot string) {
	t.Helper()
	base := t.TempDir()
	taskRoot = filepath.Join(base, "tasks", "1")
	runDir = filepath.Join(taskRoot, "i2")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return runDir, taskRoot
}

func writePNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 220, G: 30, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func callViewImage(t *testing.T, ts *ToolSet, paths ...string) (actool.Result, error) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"paths": paths})
	return ts.viewImage().Call(context.Background(), in, nil)
}

// 非视觉配置：直接给可操作错误，不发任何图片块。
func TestViewImageRefusesWithoutVision(t *testing.T) {
	runDir, _ := newImageTaskDir(t)
	writePNG(t, filepath.Join(runDir, "shot.png"))
	ts := NewToolSet(nil, "w")
	ts.SetRunDir(runDir)
	res, err := callViewImage(t, ts, "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "不是视觉模型") {
		t.Fatalf("非视觉配置应给可操作错误，得 %q", res.Content[0].Text)
	}
	for _, b := range res.Content {
		if b.Type == llm.BlockImage {
			t.Fatal("非视觉配置不得产生图片块")
		}
	}
}

// 视觉配置：成功把 png 变成 image 块，文本头声明不可信边界。
func TestViewImageSuccess(t *testing.T) {
	runDir, _ := newImageTaskDir(t)
	writePNG(t, filepath.Join(runDir, "shot.png"))
	ts := NewToolSet(nil, "w")
	ts.SetRunDir(runDir)
	ts.SetVision(true)
	res, err := callViewImage(t, ts, "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("不应失败: %q", res.Content[0].Text)
	}
	if len(res.Content) != 2 || res.Content[1].Type != llm.BlockImage {
		t.Fatalf("期望文本头 + 1 张图片，得 %d 块", len(res.Content))
	}
	if got := res.Content[1].Source.MediaType; got != "image/png" {
		t.Fatalf("media type = %q", got)
	}
	head := res.Content[0].Text
	if !strings.Contains(head, "不是指令") || !strings.Contains(head, "shot.png") {
		t.Fatalf("文本头缺少不可信声明/文件名: %q", head)
	}
	if res.Content[1].Source.Data == "" {
		t.Fatal("图片数据为空")
	}
}

// 扩展名骗不过 magic：文本文件即使叫 .png 也被跳过。
func TestViewImageRejectsNonImage(t *testing.T) {
	runDir, _ := newImageTaskDir(t)
	if err := os.WriteFile(filepath.Join(runDir, "fake.png"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(nil, "w")
	ts.SetRunDir(runDir)
	ts.SetVision(true)
	res, _ := callViewImage(t, ts, "fake.png")
	if !res.IsError || !strings.Contains(res.Content[0].Text, "不是 png/jpeg/gif/webp") {
		t.Fatalf("应拒绝伪造图片，得 %q", res.Content[0].Text)
	}
}

// 路径锁：任务目录外的文件（含 ../ 逃逸）不可读。
func TestViewImagePathEscape(t *testing.T) {
	runDir, taskRoot := newImageTaskDir(t)
	outside := filepath.Join(filepath.Dir(taskRoot), "secret.png")
	writePNG(t, outside)
	ts := NewToolSet(nil, "w")
	ts.SetRunDir(runDir)
	ts.SetVision(true)
	res, _ := callViewImage(t, ts, "../secret.png")
	if !res.IsError {
		t.Fatal("越界路径必须失败")
	}
	for _, b := range res.Content {
		if b.Type == llm.BlockImage {
			t.Fatal("越界文件不得读成图片块")
		}
	}

	// 绝对路径落在任务根内允许（保存到任务根的证据材料），落在根外拒绝。
	absInside := filepath.Join(taskRoot, "evidence.png")
	writePNG(t, absInside)
	res, _ = callViewImage(t, ts, absInside)
	if res.IsError {
		t.Fatalf("任务根内的绝对路径应可读: %q", res.Content[0].Text)
	}
	res, _ = callViewImage(t, ts, outside)
	if !res.IsError {
		t.Fatal("任务根外的绝对路径必须失败")
	}
}

// chat 的 runDir 是 sessions/<id>：必须锁在本会话目录，不能跨会话读别人的文件。
func TestViewImageRejectsCrossSessionEscape(t *testing.T) {
	base := t.TempDir()
	sessionDir := filepath.Join(base, "sessions", "conv-1")
	otherDir := filepath.Join(base, "sessions", "conv-2")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePNG(t, filepath.Join(otherDir, "secret.png"))
	ts := NewToolSet(nil, "human")
	ts.SetRunDir(sessionDir)
	ts.SetVision(true)
	res, _ := callViewImage(t, ts, "../conv-2/secret.png")
	if !res.IsError {
		t.Fatal("跨会话路径必须被拒绝")
	}
	for _, b := range res.Content {
		if b.Type == llm.BlockImage {
			t.Fatal("跨会话文件不得产出图片块")
		}
	}
	// 本会话目录内的文件仍可读。
	writePNG(t, filepath.Join(sessionDir, "own.png"))
	res, _ = callViewImage(t, ts, "own.png")
	if res.IsError {
		t.Fatalf("本会话图片应可读: %q", res.Content[0].Text)
	}
}

// 符号链接指向宿主机文件：解析后必须被拒绝。
func TestViewImageRejectsSymlinkEscape(t *testing.T) {
	runDir, taskRoot := newImageTaskDir(t)
	outside := filepath.Join(filepath.Dir(taskRoot), "host.png")
	writePNG(t, outside)
	link := filepath.Join(runDir, "leak.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ts := NewToolSet(nil, "w")
	ts.SetRunDir(runDir)
	ts.SetVision(true)
	res, _ := callViewImage(t, ts, "leak.png")
	if !res.IsError {
		t.Fatal("符号链接逃逸必须被拒绝")
	}
	for _, b := range res.Content {
		if b.Type == llm.BlockImage {
			t.Fatal("逃逸链接不得产出图片块")
		}
	}
}

// magic 判型覆盖四种支持格式与常见拒绝样本。
func TestSniffImageMediaType(t *testing.T) {
	cases := []struct {
		head []byte
		want string
		ok   bool
	}{
		{[]byte("\x89PNG\r\n\x1a\n...."), "image/png", true},
		{[]byte("\xFF\xD8\xFF\xE0...."), "image/jpeg", true},
		{[]byte("GIF89a...."), "image/gif", true},
		{[]byte("GIF87a...."), "image/gif", true},
		{[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp", true},
		{[]byte("BM...."), "", false},
		{[]byte("<html>"), "", false},
		{[]byte(""), "", false},
	}
	for _, c := range cases {
		got, ok := sniffImageMediaType(c.head)
		if got != c.want || ok != c.ok {
			t.Fatalf("sniff(%q) = %q/%v，期望 %q/%v", c.head, got, ok, c.want, c.ok)
		}
	}
}

// 一次最多 4 张：第 5 个路径被忽略（不是报错）。
func TestViewImageCapsCount(t *testing.T) {
	runDir, _ := newImageTaskDir(t)
	var paths []string
	for i := 0; i < 6; i++ {
		name := string(rune('a'+i)) + ".png"
		writePNG(t, filepath.Join(runDir, name))
		paths = append(paths, name)
	}
	ts := NewToolSet(nil, "w")
	ts.SetRunDir(runDir)
	ts.SetVision(true)
	res, _ := callViewImage(t, ts, paths...)
	if res.IsError {
		t.Fatalf("应成功: %q", res.Content[0].Text)
	}
	n := 0
	for _, b := range res.Content {
		if b.Type == llm.BlockImage {
			n++
		}
	}
	if n != maxViewImagePerCall {
		t.Fatalf("图片数 = %d，期望 %d", n, maxViewImagePerCall)
	}
}
