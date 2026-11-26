package mcphttp

import (
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// MCP 的 image content 以前被静默丢弃（只拼 text），截图类工具看起来“空”。
// 现在图片保留成 image 块，并补一条文本标记，保证不可信包装器仍有文本可包。
func TestMCPResultBlocksKeepsImages(t *testing.T) {
	res := mcpResultBlocks(nil, []mcpContentBlock{
		{Type: "text", Text: "screenshot taken"},
		{Type: "image", Data: "iVBORw0KGgo=", MimeType: "image/png"},
	}, false)
	if res.IsError {
		t.Fatal("isError 应透传 false")
	}
	if len(res.Content) != 2 {
		t.Fatalf("期望文本 + 图片两块，得 %d", len(res.Content))
	}
	if res.Content[0].Type != llm.BlockText || !strings.Contains(res.Content[0].Text, "screenshot taken") {
		t.Fatalf("文本块丢失: %+v", res.Content[0])
	}
	if !strings.Contains(res.Content[0].Text, "工具返回 1 张图片") {
		t.Fatalf("缺少图片计数标记: %q", res.Content[0].Text)
	}
	img := res.Content[1]
	if img.Type != llm.BlockImage || img.Source == nil || img.Source.MediaType != "image/png" {
		t.Fatalf("图片块不正确: %+v", img)
	}
	if img.Source.Data != "iVBORw0KGgo=" {
		t.Fatalf("base64 应原样保留，得 %q", img.Source.Data)
	}
}

// 缺 mimeType 时按 image/png 兜底；空 data 记入省略计数。
func TestMCPResultBlocksDefaultsAndSkips(t *testing.T) {
	res := mcpResultBlocks(nil, []mcpContentBlock{
		{Type: "image", Data: "AAAA"},
		{Type: "image", Data: ""},
	}, false)
	if len(res.Content) != 2 {
		t.Fatalf("期望文本 + 1 张图片，得 %d", len(res.Content))
	}
	if res.Content[1].Source.MediaType != "image/png" {
		t.Fatalf("默认 media type = %q", res.Content[1].Source.MediaType)
	}
	if !strings.Contains(res.Content[0].Text, "另有 1 张") {
		t.Fatalf("应提示省略数量: %q", res.Content[0].Text)
	}
}

// 单次最多保留 4 张；超出部分记入标记，不影响文本。
func TestMCPResultBlocksCapsImages(t *testing.T) {
	var content []mcpContentBlock
	for i := 0; i < 6; i++ {
		content = append(content, mcpContentBlock{Type: "image", Data: "AAAA", MimeType: "image/jpeg"})
	}
	res := mcpResultBlocks(nil, content, true)
	n := 0
	for _, b := range res.Content {
		if b.Type == llm.BlockImage {
			n++
		}
	}
	if n != mcpMaxImages {
		t.Fatalf("图片数 = %d，期望 %d", n, mcpMaxImages)
	}
	if !res.IsError {
		t.Fatal("isError 应透传 true")
	}
	if !strings.Contains(res.Content[0].Text, "另有 2 张") {
		t.Fatalf("应提示超限数量: %q", res.Content[0].Text)
	}
}
