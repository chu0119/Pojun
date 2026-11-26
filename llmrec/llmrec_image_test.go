package llmrec

import (
	"strings"
	"testing"
)

// 图片 base64 不许进审计：一张截图 ~1MB，落库会把 llm_records 撑爆，也会拖慢
// 删任务时的清理。清洗必须发生在 all body modes（含 full）之前。
func TestApplyBodyModeScrubsImageBase64(t *testing.T) {
	b64 := strings.Repeat("A", 4096)
	anthropic := `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}]}`
	openai := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`

	for _, raw := range []string{anthropic, openai} {
		req, resp, rawReq, rawResp := applyBodyMode("full", "ok", raw, raw, raw, raw)
		for name, body := range map[string]string{"req": req, "resp": resp, "raw_req": rawReq, "raw_resp": rawResp} {
			if strings.Contains(body, b64) {
				t.Fatalf("%s 未清洗 base64（长度 %d）", name, len(body))
			}
			if !strings.Contains(body, "omitted") && !strings.Contains(body, "省略") {
				t.Fatalf("%s 缺少省略标记: %q", name, body)
			}
		}
	}
}

// 小图（例如 64×64 探测图）的 base64 也可能只有几百字符：只要它是 image 块的
// data 字段，就必须清洗，不能靠「长度 ≥512」的阈值放行。
func TestScrubImageB64SmallImageBlock(t *testing.T) {
	short := strings.Repeat("C", 120)
	for _, raw := range []string{
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + short + `"}}`,
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + short + `"}}`,
	} {
		got := scrubImageB64(raw)
		if strings.Contains(got, short) {
			t.Fatalf("小图 base64 未清洗: %q", got)
		}
		if !strings.Contains(got, "omitted") {
			t.Fatalf("缺少省略标记: %q", got)
		}
	}
}

// 普通文本与短数据不能被误伤（阈值 512 字符）；decoded 长度也在 marker 里给出。
func TestScrubImageB64LeavesTextAlone(t *testing.T) {
	text := `{"type":"text","text":"hello world"}`
	if got := scrubImageB64(text); got != text {
		t.Fatalf("纯文本被改写: %q", got)
	}
	short := `{"data":"abc"}`
	if got := scrubImageB64(short); got != short {
		t.Fatalf("短字段被改写: %q", got)
	}
	long := `{"data":"` + strings.Repeat("B", 600) + `"}`
	got := scrubImageB64(long)
	if strings.Contains(got, strings.Repeat("B", 600)) {
		t.Fatal("长 base64 未清洗")
	}
	if !strings.Contains(got, "image omitted 600 bytes") {
		t.Fatalf("省略标记应带长度: %q", got)
	}
}
