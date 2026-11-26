package llmpresets

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		format, in, want string
		changed          bool
		confident        bool
	}{
		{FormatOpenAI, "https://dashscope.aliyuncs.com/compatible-mode", "https://dashscope.aliyuncs.com/compatible-mode/v1", true, true},
		{FormatOpenAI, "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions", "https://dashscope.aliyuncs.com/compatible-mode/v1", true, true},
		{FormatOpenAI, "https://api.openai.com", "https://api.openai.com/v1", true, true},
		{FormatOpenAI, "https://api.openai.com/v1/", "https://api.openai.com/v1", true, false},
		{FormatOpenAI, "https://x.test/api/paas/v4", "https://x.test/api/paas/v4", false, false},
		{FormatOpenAI, "https://x.test/v1?foo=1#bar", "https://x.test/v1", true, false},
		{FormatOpenAIResponses, "https://api.openai.com/v1/responses", "https://api.openai.com/v1", true, true},
		{FormatAnthropic, "https://relay.test/v1", "https://relay.test", true, true},
		{FormatAnthropic, "https://api.deepseek.com/anthropic", "https://api.deepseek.com/anthropic", false, false},
		{FormatAnthropic, "https://relay.test/v1/messages", "https://relay.test", true, true},
		{FormatOpenAI, "ftp://x.test", "ftp://x.test", false, false},
		// 已正确的地址必须零变化（默认行为不变）。
		{FormatOpenAI, "https://api.openai.com/v1", "https://api.openai.com/v1", false, false},
		{FormatAnthropic, "https://api.anthropic.com", "https://api.anthropic.com", false, false},
		// 完整 /models 端点同样去掉。
		{FormatOpenAI, "https://relay.test/v1/models", "https://relay.test/v1", true, true},
	}
	for _, c := range cases {
		got := NormalizeBaseURL(c.format, c.in)
		if got.Normalized != c.want {
			t.Errorf("%s %s: got %q want %q", c.format, c.in, got.Normalized, c.want)
		}
		if got.Changed != c.changed {
			t.Errorf("%s %s: changed=%v want %v", c.format, c.in, got.Changed, c.changed)
		}
		if got.Confident != c.confident {
			t.Errorf("%s %s: confident=%v want %v (note=%q)", c.format, c.in, got.Confident, c.confident, got.Note)
		}
	}
}

func TestBaseCandidatesAddsV1(t *testing.T) {
	got := BaseCandidates(FormatOpenAI, "https://relay.test/openai")
	if len(got) != 1 || got[0] != "https://relay.test/openai/v1" {
		t.Fatalf("结尾 /openai 应归一为 /openai/v1: %v", got)
	}
	raw := BaseCandidates(FormatOpenAI, "https://relay.test/llm")
	if len(raw) < 2 || raw[1] != "https://relay.test/llm/v1" {
		t.Fatalf("非标准路径应追加 /v1 候选: %v", raw)
	}
	// Anthropic：归一后没有 /v1 时也补一个候选（部分网关实际挂在 /v1 下）。
	ant := BaseCandidates(FormatAnthropic, "https://relay.test")
	if len(ant) < 2 || ant[1] != "https://relay.test/v1" {
		t.Fatalf("Anthropic 候选应包含 /v1: %v", ant)
	}
	if empty := BaseCandidates(FormatOpenAI, ""); len(empty) != 0 {
		t.Fatalf("空地址不应产生候选: %v", empty)
	}
}

func TestProtocolHint(t *testing.T) {
	if p, _ := ProtocolHint(FormatAnthropic, "https://dashscope.aliyuncs.com/compatible-mode/v1", "deepseek-v4"); p != FormatOpenAI {
		t.Fatalf("compatible-mode 应提示 OpenAI，得到 %q", p)
	}
	if p, _ := ProtocolHint(FormatOpenAI, "https://relay.test/anthropic", "claude-x"); p != FormatAnthropic {
		t.Fatalf("/anthropic 应提示 Anthropic，得到 %q", p)
	}
	if p, _ := ProtocolHint(FormatOpenAI, "https://relay.test/v1", "claude-sonnet-4-5"); p != FormatAnthropic {
		t.Fatalf("claude 模型应提示 Anthropic，得到 %q", p)
	}
	if p, _ := ProtocolHint(FormatAnthropic, "https://relay.test/v1", "gpt-5"); p != FormatOpenAI {
		t.Fatalf("gpt 模型在 Anthropic 协议下应提示 OpenAI，得到 %q", p)
	}
	if p, _ := ProtocolHint(FormatOpenAI, "https://relay.test/v1", "deepseek-v4"); p != "" {
		t.Fatalf("无强信号时不应建议，得到 %q", p)
	}
	// 地址与模型给出一致信号时不重复建议；信号冲突时仍以强信号为准（此处
	// compatible-mode 是 OpenAI 地址、claude 模型名是 Anthropic 信号，后者胜出）。
	if p, _ := ProtocolHint(FormatOpenAI, "https://dashscope.aliyuncs.com/compatible-mode/v1", "deepseek-v4"); p != "" {
		t.Fatalf("协议已匹配时不应建议，得到 %q", p)
	}
	if p, _ := ProtocolHint(FormatOpenAI, "https://dashscope.aliyuncs.com/compatible-mode/v1", "claude-x"); p != FormatAnthropic {
		t.Fatalf("claude 模型名应提示 Anthropic，得到 %q", p)
	}
}

func TestLookupModelDefaults(t *testing.T) {
	d := LookupModelDefaults("deepseek-v4.1-flash", FormatOpenAI)
	if d.ContextK != 1024 || !d.Reasoning || d.Source != SourceMeta || d.MaxTokens != 8192 {
		t.Fatalf("deepseek-v4.1-flash 元数据缺失: %+v", d)
	}
	g := LookupModelDefaults("gpt-5", FormatOpenAI)
	if g.MaxTokensField != "max_completion_tokens" || g.ContextK != 400 {
		t.Fatalf("gpt-5 应使用 max_completion_tokens: %+v", g)
	}
	c := LookupModelDefaults("claude-sonnet-4-5", FormatAnthropic)
	if c.MaxTokens != 8192 || c.ContextK != 200 {
		t.Fatalf("claude 应补 max_tokens=8192: %+v", c)
	}
	// 同一个模型走 OpenAI 协议时不应套用 Anthropic 的 max_tokens 默认。
	cOpenAI := LookupModelDefaults("claude-sonnet-4-5", FormatOpenAI)
	if cOpenAI.MaxTokens != 8192 { // 输出上限来自元数据，与协议无关
		t.Fatalf("claude 在 openai 协议下输出上限来自 meta: %+v", cOpenAI)
	}
	if unknown := LookupModelDefaults("no-such-model-xyz", FormatOpenAI); unknown.Source != SourceNone || unknown.MaxTokens != 0 {
		t.Fatalf("未知模型应返回 none: %+v", unknown)
	}
	if empty := LookupModelDefaults("", FormatOpenAI); empty.Source != SourceNone || empty.VisionSource != VisionSourceDefault {
		t.Fatalf("空模型应返回 none/default: %+v", empty)
	}
}

func TestNormalizeBaseURLIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"https://api.openai.com/v1",
		"https://relay.test/v1/chat/completions",
		"https://api.deepseek.com/anthropic",
	} {
		first := NormalizeBaseURL(FormatOpenAI, in)
		second := NormalizeBaseURL(FormatOpenAI, first.Normalized)
		if second.Normalized != first.Normalized {
			t.Fatalf("%s: not idempotent: %q → %q", in, first.Normalized, second.Normalized)
		}
	}
}
