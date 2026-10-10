package dialect

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
)

// EntriesOf 返回请求里的内容条目序列，每一段都已经规范化成稳定的字节。
//
// 序列的第一个条目固定是「工具定义加系统提示」，后面依次是对话条目。这个划分让
// 同一段会话在连续几轮里的条目序列保持前缀关系：只有尾部在增长。上游的提示缓存
// 按前缀工作，调用方要靠这个关系认出「当前请求里的哪一段是上一轮发过的」。
//
// 规范化走「解码再序列化」而不是截取原始字节。客户端重发同一段历史时 JSON 键
// 顺序不保证稳定，直接哈希原始字节会让同一个前缀算出不同的指纹。
//
// 没有对话条目时返回的序列只有头条目，不是空序列。
func EntriesOf(name Name, body []byte) ([][]byte, error) {
	decoded, err := DecodeRequest(name, body)
	if err != nil {
		return nil, err
	}
	switch typed := decoded.(type) {
	case *dto.GeneralOpenAIRequest:
		return chatEntries(typed)
	case *dto.OpenAIResponsesRequest:
		return responsesEntries(typed)
	case *dto.ClaudeRequest:
		return claudeEntries(typed)
	case *dto.GeminiChatRequest:
		return geminiEntries(typed)
	default:
		return nil, fmt.Errorf("%w: %s / %T", ErrUnsupportedPayload, name, decoded)
	}
}

// 条目里反复用到的字段名。
const (
	itemTypeKey = "type"
	roleKey     = "role"
	contentKey  = "content"
)

// headEntry 把工具定义与系统提示合成序列的第一个条目。
//
// 四种方言把系统提示放在不同位置：Chat 与 Responses 是条目数组里的一条，Claude
// 与 Gemini 各有独立字段。统一抽出来放在最前面，序列的结构在四种方言下就一致了。
func headEntry(tools, system any) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		Tools  any `json:"tools"`
		System any `json:"system"`
	}{Tools: tools, System: system})
	if err != nil {
		return nil, fmt.Errorf("dialect: 序列化头条目失败: %w", err)
	}
	return encoded, nil
}

// chatEntries 切分 OpenAI Chat 的条目。
//
// 系统提示混在消息数组里，必须挑出来归入头条目，否则客户端换一下位置就会改变
// 整个序列。
func chatEntries(request *dto.GeneralOpenAIRequest) ([][]byte, error) {
	system := make([]dto.Message, 0, len(request.Messages))
	conversation := make([]dto.Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		if isSystemRole(message.Role) {
			system = append(system, message)
			continue
		}
		conversation = append(conversation, message)
	}
	entries, err := startEntries(headEntry(request.Tools, system))
	if err != nil {
		return nil, err
	}
	for _, message := range conversation {
		message.Content = normalizeContent(message.Content, "text")
		if entries, err = appendEntry(entries, message); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// claudeEntries 切分 Claude Messages 的条目。
func claudeEntries(request *dto.ClaudeRequest) ([][]byte, error) {
	entries, err := startEntries(headEntry(request.Tools, normalizeContent(request.System, "text")))
	if err != nil {
		return nil, err
	}
	for _, message := range request.Messages {
		message.Content = normalizeContent(message.Content, "text")
		if entries, err = appendEntry(entries, message); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// geminiEntries 切分 Gemini generateContent 的条目。
func geminiEntries(request *dto.GeminiChatRequest) ([][]byte, error) {
	tools, err := canonicalValue(request.Tools)
	if err != nil {
		return nil, err
	}
	entries, err := startEntries(headEntry(tools, request.SystemInstructions))
	if err != nil {
		return nil, err
	}
	for _, content := range request.Contents {
		if entries, err = appendEntry(entries, content); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// responsesEntries 切分 OpenAI Responses 的条目。
//
// input 是扁平的条目数组而不是消息数组，因此逐个条目算一段，不做消息层面的合并。
// 工具调用在这些方言里本来就占独立的条目位置。
func responsesEntries(request *dto.OpenAIResponsesRequest) ([][]byte, error) {
	items, err := responsesItems(request.Input)
	if err != nil {
		return nil, err
	}
	system := make([]any, 0, len(items))
	conversation := make([]any, 0, len(items))
	for _, item := range items {
		canonical, canonicalErr := canonicalValue(normalizeResponsesItem(item))
		if canonicalErr != nil {
			return nil, canonicalErr
		}
		if isSystemRole(responsesRole(item)) {
			system = append(system, canonical)
			continue
		}
		conversation = append(conversation, canonical)
	}
	instructions, err := canonicalValue(request.Instructions)
	if err != nil {
		return nil, err
	}
	tools, err := canonicalValue(request.Tools)
	if err != nil {
		return nil, err
	}
	entries, err := startEntries(headEntry(tools, struct {
		Instructions any   `json:"instructions"`
		System       []any `json:"system"`
	}{Instructions: instructions, System: system}))
	if err != nil {
		return nil, err
	}
	for _, item := range conversation {
		if entries, err = appendEntry(entries, item); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// responsesItems 把 input 字段解析成条目数组。
//
// input 允许写成纯字符串，表示只有一条用户消息。这里把它展开成与数组形式完全
// 相同的结构，而不是自造一个形状：客户端从简写升级成完整写法时，前缀链才认得出
// 这是同一段历史。
func responsesItems(input json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("dialect: 解析 Responses 的 input 失败: %w", err)
		}
		// 造成与数组写法同形的消息条目，后面的归一化会把两者收敛到一起。
		item, err := json.Marshal(map[string]any{
			itemTypeKey: responsesMessageItemType, roleKey: "user", contentKey: text,
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{item}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("dialect: 解析 Responses 的 input 失败: %w", err)
	}
	return items, nil
}

// responsesRole 读出一个条目的 role 字段。读不出来时按空串处理。
func responsesRole(item json.RawMessage) string {
	var parsed struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(item, &parsed); err != nil {
		return ""
	}
	return parsed.Role
}

// canonicalValue 把一段 JSON 解成 any，交给编码器重新输出。
//
// 目的是让映射的键按照固定顺序输出。RawMessage 会原样保留客户端写下的键顺序，
// 同一个前缀在两次请求里键顺序不同就会算出不同的指纹。
//
// 用 UseNumber 保留数字的原始写法。解成 float64 会让超出精度的大整数与别的值撞
// 在一起，虽然碰撞的后果只是多一段会话共用渠道，能避免就避免。
func canonicalValue(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("dialect: 规范化条目失败: %w", err)
	}
	return value, nil
}

// normalizeContent 把写成纯字符串的内容收敛成单块数组。
//
// 多模态内容允许写成纯字符串，也允许写成只含一个文本块的数组，两种写法表达同一
// 段内容。不归一的话客户端从一种换成另一种时指纹就变了，前缀链认不出这是同一段
// 历史。textType 是该方言的文本块类型名。
func normalizeContent(value any, textType string) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	return []any{map[string]any{"type": textType, "text": text}}
}

// responsesMessageItemType 是 Responses 里消息条目的类型名。
const responsesMessageItemType = "message"

// normalizeResponsesItem 把消息条目里写成纯字符串的 content 展开成内容块数组。
//
// 块类型按角色取：助手回合是 output_text，其余是 input_text，与规范的写法一致。
//
// 读不出结构时原样返回。这个函数只服务于指纹，不是校验器：请求体的合法性由
// DecodeRequest 负责，这里认不出来的形状交给后面的规范化处理即可，报错反而会让
// 一次亲和计算失败升级成一次转发失败。
func normalizeResponsesItem(item json.RawMessage) json.RawMessage {
	var probe struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(item, &probe) != nil {
		return item
	}
	if probe.Type != responsesMessageItemType || len(probe.Content) == 0 || probe.Content[0] != '"' {
		return item
	}
	var text string
	if json.Unmarshal(probe.Content, &text) != nil {
		return item
	}
	decoder := json.NewDecoder(bytes.NewReader(item))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil {
		return item
	}
	blockType := "input_text"
	if role, _ := fields["role"].(string); role == "assistant" {
		blockType = "output_text"
	}
	fields["content"] = []any{map[string]any{"type": blockType, "text": text}}
	// 这里只装着字符串，编码不会失败。
	encoded, err := json.Marshal(fields)
	if err != nil {
		return item
	}
	return encoded
}

// isSystemRole 报告角色名是否属于系统提示。
func isSystemRole(role string) bool {
	return role == "system" || role == "developer"
}

// startEntries 组成序列的第一个条目。
func startEntries(head []byte, err error) ([][]byte, error) {
	if err != nil {
		return nil, err
	}
	return [][]byte{head}, nil
}

// appendEntry 用规范化后的形式追加一个条目。
func appendEntry(entries [][]byte, value any) ([][]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("dialect: 序列化条目失败: %w", err)
	}
	return append(entries, encoded), nil
}
