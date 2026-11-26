package llmpresets

import "testing"

// 三态优先级：用户显式选择永远压过一切推断。
func TestResolveVisionTriState(t *testing.T) {
	cases := []struct {
		model, mode string
		want        bool
		source      string
	}{
		{"deepseek-v4.1-flash", "", false, VisionSourceDefault},
		{"deepseek-v4.1-flash", "on", true, VisionSourceProfile},
		{"deepseek-v4.1-flash", "off", false, VisionSourceProfile},
		{"claude-opus-4-8", "", true, VisionSourceName},
		{"claude-opus-4-8", "off", false, VisionSourceProfile},
		{"totally-unknown-model-xyz", "", false, VisionSourceDefault},
		{"totally-unknown-model-xyz", "ON", true, VisionSourceProfile},
	}
	for _, c := range cases {
		got, src := ResolveVision(c.model, c.mode)
		if got != c.want || src != c.source {
			t.Fatalf("ResolveVision(%q,%q) = %v/%s，期望 %v/%s", c.model, c.mode, got, src, c.want, c.source)
		}
	}
}

// 模型名启发：多模态家族命中，纯文本 mini 显式排除，未知名字不给结论。
func TestVisionFromName(t *testing.T) {
	yes := []string{
		"qwen2.5-vl-72b", "glm-4v-plus", "gpt-4o", "gpt-4.1-mini",
		"claude-sonnet-4-5", "gemini-2.5-pro", "llava-1.6", "internvl2",
		"o1", "o3-pro", "o4-mini", "pixtral-12b", "nova-pro",
	}
	for _, m := range yes {
		if v, known := VisionFromName(m); !known || !v {
			t.Fatalf("%q 应判为视觉模型，得 %v/%v", m, v, known)
		}
	}
	no := []string{"o1-mini", "o3-mini", "deepseek-v4.1-flash", "gpt-3.5-turbo", "mystery-model"}
	for _, m := range no {
		if v, known := VisionFromName(m); known && v {
			t.Fatalf("%q 不应判为视觉模型", m)
		}
	}
	if _, known := VisionFromName("mystery-model"); known {
		t.Fatal("未知模型名不应给出结论")
	}
	if _, known := VisionFromName(""); known {
		t.Fatal("空模型名不应给出结论")
	}
}

// 指纹：format/base_url/model 任一变化即失效；大小写与末尾斜杠归一化；key 不参与。
func TestVisionSignature(t *testing.T) {
	a := VisionSignature("openai", "https://api.example.com/v1/", "GPT-4o")
	b := VisionSignature("OPENAI", "https://api.example.com/v1", "gpt-4o")
	if a != b {
		t.Fatalf("归一化后指纹应相同：%s vs %s", a, b)
	}
	if a == VisionSignature("anthropic", "https://api.example.com/v1", "gpt-4o") {
		t.Fatal("format 变化应改变指纹")
	}
	if a == VisionSignature("openai", "https://api.example.com/v2", "gpt-4o") {
		t.Fatal("base_url 变化应改变指纹")
	}
	if a == VisionSignature("openai", "https://api.example.com/v1", "gpt-4.1") {
		t.Fatal("model 变化应改变指纹")
	}
}

// 探测结果只在指纹匹配时生效；用户三态始终优先。
func TestResolveVisionStored(t *testing.T) {
	sig := VisionSignature("openai", "https://api.example.com/v1", "unknown-model")

	if v, src := ResolveVisionStored("unknown-model", "", "on", sig, sig); !v || src != VisionSourceProbe {
		t.Fatalf("指纹匹配的 on 探测应生效，得 %v/%s", v, src)
	}
	if v, src := ResolveVisionStored("unknown-model", "", "off", sig, sig); v || src != VisionSourceProbe {
		t.Fatalf("指纹匹配的 off 探测应生效，得 %v/%s", v, src)
	}
	// 指纹不匹配（配置改过）→ 结果作废，回落到名字/默认。
	if v, src := ResolveVisionStored("claude-opus-4-8", "", "off", sig, "other-sig"); !v || src != VisionSourceName {
		t.Fatalf("指纹不匹配的探测应被忽略，得 %v/%s", v, src)
	}
	// 用户显式选支持时，探测的 off 不得覆盖。
	if v, src := ResolveVisionStored("unknown-model", "on", "off", sig, sig); !v || src != VisionSourceProfile {
		t.Fatalf("用户三态应压过探测，得 %v/%s", v, src)
	}
	// 空探测结果 → 不算结论。
	if v, src := ResolveVisionStored("unknown-model", "", "", "", sig); v || src != VisionSourceDefault {
		t.Fatalf("空探测不应产生结论，得 %v/%s", v, src)
	}
}
