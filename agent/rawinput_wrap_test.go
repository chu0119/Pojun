package agent

import (
	"context"
	"encoding/json"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// 写入类工具靠 normalizeToolInput 修模型写歪的入参，前提是 harness 跳过 schema
// 校验（Spec.RawInput）。运行时还会套 untrustedTool/guardedTool 两层包装——它们
// 必须把 AcceptsRawInput 透传出去，否则容错逻辑永远跑不到。
func TestRawInputOptOutSurvivesWrappers(t *testing.T) {
	run := func(context.Context, json.RawMessage) (actool.Result, error) { return actool.Result{}, nil }
	schema := map[string]any{"type": "object", "required": []any{"name"}}

	raw := rawWriteTool("raw", "d", schema, run)
	plain := writeTool("plain", "d", schema, run)
	if r, ok := raw.(interface{ AcceptsRawInput() bool }); !ok || !r.AcceptsRawInput() {
		t.Fatal("rawWriteTool must opt out of schema validation")
	}
	if r, ok := plain.(interface{ AcceptsRawInput() bool }); !ok || r.AcceptsRawInput() {
		t.Fatal("writeTool must keep schema validation")
	}
	wrapped := guardPanic(&untrustedTool{CoreTool: raw})
	if r, ok := wrapped.(interface{ AcceptsRawInput() bool }); !ok || !r.AcceptsRawInput() {
		t.Fatal("wrappers dropped the raw-input opt-out")
	}
	wrappedPlain := guardPanic(&untrustedTool{CoreTool: plain})
	if r, ok := wrappedPlain.(interface{ AcceptsRawInput() bool }); !ok || r.AcceptsRawInput() {
		t.Fatal("wrappers must not invent the opt-out for ordinary tools")
	}
}
