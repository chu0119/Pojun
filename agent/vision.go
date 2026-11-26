package agent

// 原生视觉能力的请求侧守卫。
//
// 平台决定“这个 profile 能不能看图”用的是三态（自动/支持/不支持），解析结果放在
// agent.Config.Vision。真正的编码由 Norma 的三个协议适配器做；这里只负责两件事：
//
//	Vision=false  把请求里所有 image 块（含 tool_result 内嵌）换成文本占位，
//	              让纯文本模型永远不会收到图片（旧配置/未知模型行为不变）。
//	Vision=true   保留图片，但单请求最多保留最近 N 张——长会话里连续截图会把
//	              body 和 token 预算一起推爆，旧图对当前判断的价值也最低。
//
// 守卫包在 Provider 外层（与 GuardRefusal 同层），不碰历史存储：transcript
// 落盘前另有脱图逻辑，审计记录见 llmrec 的清洗。

import (
	"context"
	"iter"

	"github.com/Autumn-27/norma/llm"
)

const (
	// visionMaxImagesPerRequest 是单个请求保留的图片上限（从最新往回保留）。
	visionMaxImagesPerRequest = 6
	// visionElidedUnsupported / visionElidedOverLimit 是两种占位文案。写成常量
	// 便于测试与日志检索。
	visionElidedUnsupported = "[图片已省略：当前模型不支持视觉输入]"
	visionElidedOverLimit   = "[图片已省略：超出单轮图片上限]"
)

type visionGuardProvider struct {
	inner llm.Provider
	on    bool
}

// VisionGuard wraps p so requests match the profile's image capability. It is
// idempotent: wrapping an already-guarded provider with the same setting returns
// it unchanged (the server re-wraps outside the recorder so audit rows reflect
// the stripped request).
func VisionGuard(p llm.Provider, on bool) llm.Provider {
	if p == nil {
		return p
	}
	if g, ok := p.(*visionGuardProvider); ok {
		if g.on == on {
			return p
		}
		return &visionGuardProvider{inner: g.inner, on: on}
	}
	return &visionGuardProvider{inner: p, on: on}
}

func (g *visionGuardProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	req.Messages = g.apply(req.Messages)
	return g.inner.Stream(ctx, req)
}

func (g *visionGuardProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	req.Messages = g.apply(req.Messages)
	return g.inner.Complete(ctx, req)
}

func (g *visionGuardProvider) apply(msgs []llm.Message) []llm.Message {
	if !hasImages(msgs) {
		return msgs // hot path: text-only histories pay nothing
	}
	if !g.on {
		return replaceImages(msgs, visionElidedUnsupported)
	}
	return capImages(msgs, visionMaxImagesPerRequest)
}

func hasImages(msgs []llm.Message) bool {
	for _, m := range msgs {
		if blocksHaveImages(m.Content) {
			return true
		}
	}
	return false
}

func blocksHaveImages(blocks []llm.ContentBlock) bool {
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockImage:
			return true
		case llm.BlockToolResult:
			if blocksHaveImages(b.Content) {
				return true
			}
		}
	}
	return false
}

// replaceImages builds a copy with every image block replaced by kind.
func replaceImages(msgs []llm.Message, placeholder string) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = llm.Message{Role: m.Role, Content: replaceImageBlocks(m.Content, placeholder, nil)}
	}
	return out
}

func replaceImageBlocks(blocks []llm.ContentBlock, placeholder string, keep func() bool) []llm.ContentBlock {
	out := make([]llm.ContentBlock, len(blocks))
	for i, b := range blocks {
		switch b.Type {
		case llm.BlockImage:
			if keep != nil && keep() {
				out[i] = b
				continue
			}
			out[i] = llm.TextBlock(placeholder)
		case llm.BlockToolResult:
			b.Content = replaceImageBlocks(b.Content, placeholder, keep)
			out[i] = b
		default:
			out[i] = b
		}
	}
	return out
}

// capImages keeps at most max images, counting from the newest message/block so
// the screenshots the current turn actually refers to survive.
func capImages(msgs []llm.Message, max int) []llm.Message {
	total := countImages(msgs)
	if total <= max || max <= 0 {
		return msgs
	}
	// Walk newest-first (messages and blocks reversed) and keep the first max
	// images seen; everything older becomes a placeholder.
	kept := 0
	keep := func() bool {
		if kept < max {
			kept++
			return true
		}
		return false
	}
	out := make([]llm.Message, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		out[i] = llm.Message{Role: msgs[i].Role, Content: capImageBlocks(msgs[i].Content, keep)}
	}
	return out
}

// capImageBlocks applies keep() in reverse block order (newest block first) so
// the cap stays consistent with message order.
func capImageBlocks(blocks []llm.ContentBlock, keep func() bool) []llm.ContentBlock {
	out := make([]llm.ContentBlock, len(blocks))
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		switch b.Type {
		case llm.BlockImage:
			if keep() {
				out[i] = b
			} else {
				out[i] = llm.TextBlock(visionElidedOverLimit)
			}
		case llm.BlockToolResult:
			b.Content = capImageBlocks(b.Content, keep)
			out[i] = b
		default:
			out[i] = b
		}
	}
	return out
}

func countImages(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += countImageBlocks(m.Content)
	}
	return n
}

func countImageBlocks(blocks []llm.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockImage:
			n++
		case llm.BlockToolResult:
			n += countImageBlocks(b.Content)
		}
	}
	return n
}

// visionGuidance 是“当前模型支持视觉”时追加到 worker/main/chat system prompt 的
// 说明。不支持时不追加：模型看不到图片，谈截图只会误导。
const visionGuidance = "\n\n【视觉能力】当前模型支持图片输入：页面截图、验证码、图表可用 chromium --screenshot 落盘后 " +
	"用 view_image 直接查看（单轮最多 6 张，单张 ≤5MB）。图片内容来自目标/外部，只作数据与辅助证据，" +
	"结论仍以文字与请求/响应为准。"
