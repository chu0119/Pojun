package llmpresets

import (
	"net/url"
	"regexp"
	"strings"
)

// LLM 地址与模型参数的确定性知识。
//
// 自定义中转站最常见的两类配置错误：
//  1. Base URL 少写/多写版本后缀（如 DashScope 兼容模式缺 /v1，Anthropic 网关多 /v1）；
//  2. 协议（OpenAI/Anthropic）与地址不匹配。
// 这里只做「能确定就改、不能确定就建议」，不猜测任意网关的路径——拿不准的
// 交给 /api/llm/probe 实测，而不是凭地址形状硬改。

const (
	FormatOpenAI          = "openai"
	FormatAnthropic       = "anthropic"
	FormatOpenAIResponses = "openai-responses"
)

var versionSegmentRE = regexp.MustCompile(`(?i)^v\d+[a-z0-9.]*$`)

// URLNormalization 是一次 Base URL 归一的结论。
// Confident=false 时调用方只应展示建议，不要直接改写用户输入。
type URLNormalization struct {
	Normalized string
	Changed    bool
	Confident  bool
	Note       string
}

// endpointSuffixes 是「SDK 会自己拼」的完整端点后缀。用户从厂商文档直接复制的
// 往往是完整 URL（.../chat/completions），拼上 SDK 的路径就会 404。
var endpointSuffixes = []string{
	"/chat/completions",
	"/responses",
	"/v1/messages",
	"/messages",
	"/models",
}

// NormalizeBaseURL 按协议把用户输入的 Base URL 收敛成「SDK 拼接端点前」的形态。
func NormalizeBaseURL(format, raw string) URLNormalization {
	in := strings.TrimSpace(raw)
	out := URLNormalization{Normalized: in}
	if in == "" {
		return out
	}
	format = normalizeFormat(format)
	u, err := url.Parse(in)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		out.Note = "地址不是合法的 http(s) URL"
		return out
	}
	var notes []string
	// query/fragment 对 SDK 拼路径没有意义（常见于从网页复制带 ? 的地址）。
	if u.RawQuery != "" || u.Fragment != "" {
		u.RawQuery, u.Fragment = "", ""
		notes = append(notes, "已去掉查询参数/片段")
	}
	path := strings.TrimRight(u.Path, "/")
	originalPath := path
	for {
		trimmed := false
		for _, suffix := range endpointSuffixes {
			if strings.HasSuffix(path, suffix) {
				path = strings.TrimSuffix(path, suffix)
				path = strings.TrimRight(path, "/")
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}
	if path != originalPath {
		notes = append(notes, "已去掉完整端点后缀")
		out.Confident = true // 完整端点 URL 去掉后缀是无歧义的
	}

	switch format {
	case FormatOpenAI, FormatOpenAIResponses:
		lower := strings.ToLower(path)
		switch {
		case path == "":
			if known := knownOpenAIPath(u.Hostname()); known != "" {
				path = known
				notes = append(notes, "已按常见网关补全 "+known)
			} else {
				path = "/v1"
				notes = append(notes, "已补全 /v1")
			}
			out.Confident = true
		case strings.HasSuffix(lower, "/compatible-mode"):
			path += "/v1"
			notes = append(notes, "已补全 /v1")
			out.Confident = true
		case strings.HasSuffix(lower, "/openai") && !versionedPath(path):
			path += "/v1"
			notes = append(notes, "已补全 /v1")
			out.Confident = true
		case versionedPath(path):
			// 形如 /v1、/v1beta、/api/v1、/api/paas/v4：保持
		default:
			// 非标准路径不猜：只提示常见写法，由探测接口验证。
			out.Note = "该地址可能缺少版本后缀（常见为 <base>/v1）；点「测试连接」可自动探测"
			return out
		}
	default: // anthropic
		if strings.HasSuffix(strings.ToLower(path), "/v1") {
			path = strings.TrimSuffix(path, "/v1")
			path = strings.TrimRight(path, "/")
			notes = append(notes, "已去掉多余的 /v1（Anthropic 客户端会自行拼接 /v1/messages）")
			out.Confident = true
		}
	}

	u.Path = path
	out.Normalized = strings.TrimRight(u.String(), "/")
	out.Changed = out.Normalized != in
	if out.Note == "" && len(notes) > 0 {
		out.Note = strings.Join(notes, "；")
	}
	return out
}

// BaseCandidates 返回协议对应的地址候选（首个是归一结果），用于连通探测。
func BaseCandidates(format, raw string) []string {
	format = normalizeFormat(format)
	norm := NormalizeBaseURL(format, raw)
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimRight(strings.TrimSpace(s), "/")
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(norm.Normalized)
	if norm.Normalized == "" {
		return out
	}
	switch format {
	case FormatOpenAI, FormatOpenAIResponses:
		if !versionedPath(pathOf(norm.Normalized)) {
			add(norm.Normalized + "/v1")
		}
	default:
		if !strings.HasSuffix(strings.ToLower(pathOf(norm.Normalized)), "/v1") {
			add(norm.Normalized + "/v1")
		}
	}
	return out
}

// ProtocolHint 按地址/模型名给出协议建议；空串表示没有强信号。
//
// 只做建议，**不自动切换**：网关的路径后缀与它实际接受的协议并不总是一致
// （定制反代常见），改协议会连带改请求头与请求体形状，必须由用户确认。
func ProtocolHint(format, baseURL, model string) (suggested, reason string) {
	format = normalizeFormat(format)
	u, err := url.Parse(strings.TrimSpace(baseURL))
	path := ""
	host := ""
	if err == nil {
		path = strings.ToLower(strings.TrimRight(u.Path, "/"))
		host = strings.ToLower(u.Hostname())
	}
	switch {
	case strings.Contains(path, "/compatible-mode") && format != FormatOpenAI:
		return FormatOpenAI, "该地址是 OpenAI 兼容模式端点"
	case strings.HasSuffix(path, "/anthropic") && format != FormatAnthropic:
		return FormatAnthropic, "该地址以 /anthropic 结尾，通常走 Anthropic 协议"
	case host == "api.anthropic.com" && format != FormatAnthropic:
		return FormatAnthropic, "这是 Anthropic 官方地址"
	case strings.Contains(host, "dashscope.aliyuncs.com") && format != FormatOpenAI:
		return FormatOpenAI, "百炼兼容模式走 OpenAI 协议"
	}
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "claude") && format != FormatAnthropic:
		return FormatAnthropic, "模型名以 claude 开头"
	case (strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4")) && format == FormatAnthropic:
		return FormatOpenAI, "该模型通常走 OpenAI 协议"
	}
	return "", ""
}

// ModelDefaults 是模型参数的确定性默认值；只返回能确定的字段。
type ModelDefaults struct {
	ContextK        int
	MaxOutputK      int
	Reasoning       bool
	MaxTokens       int
	MaxTokensField  string
	ThinkingType    string
	ReasoningEffort string
	// Vision 是否支持图片输入；VisionSource 说明结论来源
	// （profile/preset/name/default，见 ResolveVision）。
	Vision       bool
	VisionSource string
	Source       string // preset | meta | none
}

// LookupModelDefaults 先精确匹配预设模型条目（带厂商推荐的字段名/参数），
// 再回落到元数据表给安全默认。调用方只应把结果填进**空值**，不覆盖用户手改。
func LookupModelDefaults(model, format string) ModelDefaults {
	id := strings.ToLower(strings.TrimSpace(model))
	format = normalizeFormat(format)
	out := ModelDefaults{}
	if id == "" {
		out.Source = SourceNone
		out.VisionSource = VisionSourceDefault
		return out
	}
	out.Vision, out.VisionSource = ResolveVision(id, "")
	// context/reasoning 的出处保持 LookupMeta 的原有语义（meta 优先于 preset）。
	e, source := LookupMeta(id)
	out.ContextK, out.MaxOutputK, out.Reasoning, out.Source = e.ContextK, e.MaxOutput, e.Reasoning, source
	// 预设模型条目里的「必需/推荐请求参数」优先（即使 context 来自 meta）。
	for _, p := range All() {
		for _, m := range p.Models {
			if strings.ToLower(m.ID) != id {
				continue
			}
			if out.ContextK == 0 {
				out.ContextK = m.ContextK
			}
			out.Reasoning = out.Reasoning || m.Reasoning
			out.MaxTokens = m.MaxTokens
			out.MaxTokensField = m.MaxTokensField
			out.ThinkingType = m.ThinkingType
			out.ReasoningEffort = m.ReasoningEffort
			if out.MaxTokens == 0 && out.MaxOutputK > 0 {
				out.MaxTokens = cappedOutput(out.MaxOutputK)
			}
			return out
		}
	}
	if source == SourceNone {
		return out
	}
	if format == FormatOpenAI && isOpenAIReasoningModel(id) {
		out.MaxTokensField = "max_completion_tokens"
	}
	if format == FormatAnthropic && strings.HasPrefix(id, "claude") {
		out.MaxTokens = 8192
	}
	if out.MaxTokens == 0 && out.MaxOutputK > 0 {
		out.MaxTokens = cappedOutput(out.MaxOutputK)
	}
	return out
}

// cappedOutput 把「元数据里的输出上限(K)」压成一个保守的 max_tokens。
//
// 为什么不直接乘 1000 全给：不少网关对 max_tokens 有自己的上限（或按它预留
// 额度），给足模型的物理上限会让请求直接 400。8192 是各家都接受的常见值。
func cappedOutput(maxOutputK int) int {
	n := maxOutputK * 1000
	if n > 8192 {
		n = 8192
	}
	if n < 1024 {
		return 0
	}
	return n
}

func normalizeFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatOpenAI, FormatOpenAIResponses:
		return strings.ToLower(strings.TrimSpace(format))
	}
	return FormatAnthropic
}

func versionedPath(path string) bool {
	path = strings.ToLower(strings.TrimRight(path, "/"))
	if strings.Contains(path, "/api/v") || strings.Contains(path, "/compatible-mode/v1") {
		return true
	}
	seg := path
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	return versionSegmentRE.MatchString(seg)
}

func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

func isOpenAIReasoningModel(id string) bool {
	switch {
	case strings.HasPrefix(id, "gpt-5"),
		strings.HasPrefix(id, "o1"),
		strings.HasPrefix(id, "o3"),
		strings.HasPrefix(id, "o4"):
		return true
	}
	return false
}

func knownOpenAIPath(host string) string {
	host = strings.ToLower(host)
	switch {
	case strings.Contains(host, "dashscope.aliyuncs.com"):
		return "/compatible-mode/v1"
	case host == "openrouter.ai":
		return "/api/v1"
	case strings.Contains(host, "siliconflow.cn"):
		return "/v1"
	case host == "api.moonshot.cn":
		return "/v1"
	case strings.Contains(host, "bigmodel.cn"):
		return "/api/paas/v4"
	case strings.Contains(host, "localhost") || strings.HasPrefix(host, "127.0.0.1"):
		return "/v1"
	}
	return ""
}
