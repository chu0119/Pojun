package agent

// P2-3：工具调用 span。agent 级 span 在 planner/worker/mainagent 入口创建；
// 这里包住每次 CoreTool.Call，记录工具名、错误与结果大小。默认 no-op provider
// 时不产生任何网络/性能负担。

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/chu0119/pojun/telemetry"
)

type tracedTool struct{ actool.CoreTool }

func (t *tracedTool) AcceptsRawInput() bool {
	if r, ok := t.CoreTool.(interface{ AcceptsRawInput() bool }); ok {
		return r.AcceptsRawInput()
	}
	return false
}

func (t *tracedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	ctx, span := telemetry.Start(ctx, "execute_tool."+t.Name(),
		attribute.String("tool.name", t.Name()),
	)
	defer span.End()
	res, err := t.CoreTool.Call(ctx, in, tc)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.Bool("tool.is_error", true))
		return res, err
	}
	span.SetAttributes(
		attribute.Bool("tool.is_error", res.IsError),
		attribute.Int("tool.result_chars", len([]rune(res.Flatten()))),
	)
	if res.IsError {
		span.SetStatus(codes.Error, "tool returned error result")
	}
	return res, nil
}
