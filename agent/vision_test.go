package agent

import (
	"context"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// captureProvider records the completion request the guard actually forwards.
type captureProvider struct {
	got   llm.CompletionRequest
	calls int
}

func (p *captureProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls++
	p.got = req
	return func(yield func(llm.StreamEvent, error) bool) {}
}

func (p *captureProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	p.got = req
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}}, "ok", llm.Usage{}, nil
}

func imgMsg(role llm.Role, n int) llm.Message {
	blocks := []llm.ContentBlock{
		llm.TextBlock("图"),
		llm.ImageBlock("image/png", []byte{0x89, 0x50, 0x4E, 0x47, byte(n)}),
	}
	return llm.Message{Role: role, Content: blocks}
}

func toolResultImgMsg(n int) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Type:      llm.BlockToolResult,
		ToolUseID: "t1",
		Content: []llm.ContentBlock{
			llm.TextBlock("工具回执"),
			llm.ImageBlock("image/png", []byte{0x89, 0x50, 0x4E, 0x47, byte(n)}),
		},
	}}}
}

// 不支持视觉：递归剥离（含 tool_result 内嵌），文本原样保留。
func TestVisionGuardStripsWhenOff(t *testing.T) {
	inner := &captureProvider{}
	prov := VisionGuard(inner, false)
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hello")}},
		toolResultImgMsg(1),
		imgMsg(llm.RoleUser, 2),
	}
	_, _, _, err := prov.Complete(context.Background(), llm.CompletionRequest{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if countImages(inner.got.Messages) != 0 {
		t.Fatalf("图片应全部剥离，剩 %d 张", countImages(inner.got.Messages))
	}
	if !strings.Contains(inner.got.Messages[1].Content[0].Content[0].Text, "工具回执") {
		t.Fatal("tool_result 的文本必须保留")
	}
	if inner.got.Messages[2].Content[1].Text != visionElidedUnsupported {
		t.Fatalf("占位文案不符: %q", inner.got.Messages[2].Content[1].Text)
	}
	if msgs[2].Content[1].Type != llm.BlockImage {
		t.Fatal("守卫不得改写调用方传入的原始切片")
	}
}

// 支持视觉：保留最近 6 张（消息与块都从新往回数），更早的换占位。
func TestVisionGuardCapsToNewest(t *testing.T) {
	inner := &captureProvider{}
	prov := VisionGuard(inner, true)
	var msgs []llm.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, imgMsg(llm.RoleUser, i))
	}
	msgs = append(msgs, toolResultImgMsg(9)) // 最新的一张在 tool_result 里
	_, _, _, err := prov.Complete(context.Background(), llm.CompletionRequest{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if got := countImages(inner.got.Messages); got != 6 {
		t.Fatalf("总共 6 张时不应触发上限，得 %d", got)
	}

	// 8 张 → 保留最新 6 张：最后 5 条各 1 张 + tool_result 1 张，较早 2 条被占位。
	var many []llm.Message
	for i := 0; i < 7; i++ {
		many = append(many, imgMsg(llm.RoleUser, i))
	}
	many = append(many, toolResultImgMsg(7))
	inner2 := &captureProvider{}
	prov2 := VisionGuard(inner2, true)
	if _, _, _, err := prov2.Complete(context.Background(), llm.CompletionRequest{Messages: many}); err != nil {
		t.Fatal(err)
	}
	if got := countImages(inner2.got.Messages); got != visionMaxImagesPerRequest {
		t.Fatalf("保留数 = %d，期望 %d", got, visionMaxImagesPerRequest)
	}
	if inner2.got.Messages[0].Content[1].Text != visionElidedOverLimit {
		t.Fatalf("最旧的消息应被占位，得 %q", inner2.got.Messages[0].Content[1].Text)
	}
	if inner2.got.Messages[2].Content[1].Type != llm.BlockImage {
		t.Fatal("第 3 条应在保留窗口内（最新 6 张覆盖 i=2..6 与 tool_result）")
	}
	last := inner2.got.Messages[len(inner2.got.Messages)-1]
	if countImageBlocks(last.Content) != 1 {
		t.Fatal("最新的 tool_result 图片必须保留")
	}
}

// 纯文本请求：原对象原样透传（热路径零分配），保证默认行为逐字节不变。
func TestVisionGuardTextOnlyPassthrough(t *testing.T) {
	inner := &captureProvider{}
	for _, on := range []bool{false, true} {
		prov := VisionGuard(inner, on)
		msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("纯文本")}}}
		if _, _, _, err := prov.Complete(context.Background(), llm.CompletionRequest{Messages: msgs}); err != nil {
			t.Fatal(err)
		}
		if len(inner.got.Messages) != 1 || inner.got.Messages[0].Content[0].Text != "纯文本" {
			t.Fatal("纯文本请求必须原样转发")
		}
	}
}

// 守卫幂等：同设置重复包装返回原对象；改设置才重建外层。
func TestVisionGuardIdempotent(t *testing.T) {
	inner := &captureProvider{}
	a := VisionGuard(inner, true)
	if b := VisionGuard(a, true); b != a {
		t.Fatal("同设置应返回同一实例")
	}
	c := VisionGuard(a, false)
	if c == a {
		t.Fatal("改设置应重建外层")
	}
	if _, _, _, err := c.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{imgMsg(llm.RoleUser, 1)}}); err != nil {
		t.Fatal(err)
	}
	if countImages(inner.got.Messages) != 0 {
		t.Fatal("重建后应按新设置剥离")
	}
}

// 真实 wire 守门：默认（Vision=false）时 OpenAI 请求体里不得出现任何图片字段，
// 这是"旧配置行为逐字节不变"的最终证据；Vision=true 时才带上 image_url。
func TestNewProviderWireOmitsImagesWhenVisionOff(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	req := llm.CompletionRequest{Messages: []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.TextBlock("看图"),
			llm.ImageBlock("image/png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}),
		},
	}}}
	base := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m"}

	prov, err := base.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := prov.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("请求次数 = %d", len(bodies))
	}
	if strings.Contains(bodies[0], "image_url") || strings.Contains(bodies[0], "base64") {
		t.Fatalf("Vision=false 的请求体不得含图片: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], "图片已省略") {
		t.Fatalf("应留下占位文本: %s", bodies[0])
	}

	on := base
	on.Vision = true
	prov, err = on.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := prov.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], "image_url") {
		t.Fatalf("Vision=true 的请求体应带 image_url: %v", bodies)
	}
}
