// Package dialect 定义对外暴露的四种大模型文本协议方言，以及它们在本地端点与
// 上游端点上的路径形状。
//
// 方言只描述「客户端用哪种协议说话」，不描述「上游是哪家厂商」。同一个方言可以
// 对应多个渠道，渠道的 BaseURL 与鉴权方式由 channel 包负责。
package dialect

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/types"
)

// maxModelNameBytes 是写进 URL 路径的模型名上限。
const maxModelNameBytes = 256

// ValidateModelName 校验适合写进 URL 路径的模型名。
//
// Gemini 把模型名写在路径上，因此它必须是一段不含分隔符的名字。允许的字符是
// 字母、数字与 . _ - 三种符号：斜杠会把模型名拆成两段路径，问号会凭空添出查询
// 参数，百分号能绕开这段检查去构造前面两种情况。
func ValidateModelName(model string) error {
	if model == "" {
		return errors.New("dialect: 模型名不能为空")
	}
	if len(model) > maxModelNameBytes {
		return fmt.Errorf("dialect: 模型名超过 %d 字节", maxModelNameBytes)
	}
	if strings.Contains(model, "..") {
		return fmt.Errorf("dialect: 模型名 %q 含有两个连续的点", model)
	}
	for _, character := range model {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '_', character == '-':
		default:
			return fmt.Errorf("dialect: 模型名 %q 含有不被允许的字符 %q，只允许字母、数字与 . _ -",
				model, string(character))
		}
	}
	return nil
}

// Name 是方言标识，取值即配置文件里 dialect 字段的写法。
type Name string

const (
	// OpenAIChat 是 OpenAI Chat Completions。
	OpenAIChat Name = "openai"
	// OpenAIResponses 是 OpenAI Responses。
	OpenAIResponses Name = "openai_responses"
	// ClaudeMessages 是 Anthropic Claude Messages。
	ClaudeMessages Name = "claude"
	// GeminiGenerate 是 Google Gemini generateContent。
	GeminiGenerate Name = "gemini"
)

// Names 按固定顺序返回四种方言，供配置校验、文档与测试遍历。
func Names() []Name {
	return []Name{OpenAIChat, OpenAIResponses, ClaudeMessages, GeminiGenerate}
}

// Parse 解析方言名，大小写与首尾空白不计。第二个返回值表示是否识别。
func Parse(s string) (Name, bool) {
	n := Name(strings.ToLower(strings.TrimSpace(s)))
	for _, candidate := range Names() {
		if n == candidate {
			return candidate, true
		}
	}
	return "", false
}

// MustParse 与 Parse 相同，但不识别时 panic。只用于包内常量与测试。
func MustParse(s string) Name {
	n, ok := Parse(s)
	if !ok {
		panic(fmt.Sprintf("dialect: 未知方言 %q", s))
	}
	return n
}

// RelayFormat 返回该方言在 RelayKit 里的对应格式。
func (n Name) RelayFormat() types.RelayFormat {
	switch n {
	case OpenAIChat:
		return types.RelayFormatOpenAI
	case OpenAIResponses:
		return types.RelayFormatOpenAIResponses
	case ClaudeMessages:
		return types.RelayFormatClaude
	case GeminiGenerate:
		return types.RelayFormatGemini
	default:
		return ""
	}
}

// FromRelayFormat 把 RelayKit 的格式映射回方言。
func FromRelayFormat(format types.RelayFormat) (Name, bool) {
	switch format {
	case types.RelayFormatOpenAI:
		return OpenAIChat, true
	case types.RelayFormatOpenAIResponses:
		return OpenAIResponses, true
	case types.RelayFormatClaude:
		return ClaudeMessages, true
	case types.RelayFormatGemini:
		return GeminiGenerate, true
	default:
		return "", false
	}
}

// LocalPath 返回本地端点接收该方言的路径。Gemini 的模型名在路径上，因此用
// {model} 占位。
func (n Name) LocalPath() string {
	switch n {
	case OpenAIChat:
		return "/v1/chat/completions"
	case OpenAIResponses:
		return "/v1/responses"
	case ClaudeMessages:
		return "/v1/messages"
	case GeminiGenerate:
		return "/v1beta/models/{model}:generateContent"
	default:
		return ""
	}
}

// UpstreamPath 返回发往上游时应拼在渠道 BaseURL 之后的路径。stream 为真时
// 取流式端点。
//
// 只有 Gemini 把模型名写在路径上，必须由调用方给出；其余三方言的模型名在请求体
// 里，model 会被忽略。
func (n Name) UpstreamPath(model string, stream bool) (string, error) {
	model = strings.TrimSpace(model)
	switch n {
	case OpenAIChat:
		return "/v1/chat/completions", nil
	case OpenAIResponses:
		return "/v1/responses", nil
	case ClaudeMessages:
		// 是否流式写在请求体的 stream 字段里，路径与之一致。
		return "/v1/messages", nil
	case GeminiGenerate:
		if err := ValidateModelName(model); err != nil {
			return "", err
		}
		if stream {
			return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse", nil
		}
		return "/v1beta/models/" + model + ":generateContent", nil
	default:
		return "", fmt.Errorf("dialect: 未知方言 %q", n)
	}
}
