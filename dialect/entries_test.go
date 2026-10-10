package dialect_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tokenmp/core/dialect"
)

// turnPair 是一段会话的连续两轮请求。第二轮的条目序列必须以第一轮的为前缀，
// 前缀链的查找全靠这条性质。
type turnPair struct {
	name  string
	first string
	next  string
}

// toolTurnPairs 里的每一对都带一次工具调用，用来确认工具轮不会打断前缀关系。
var toolTurnPairs = []turnPair{
	{
		name: "openai",
		first: `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],
			"messages":[{"role":"system","content":"sys"},{"role":"user","content":"天气"}]}`,
		next: `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],
			"messages":[{"role":"system","content":"sys"},{"role":"user","content":"天气"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"晴"},
			{"role":"assistant","content":"晴天"}]}`,
	},
	{
		name:  "openai_responses",
		first: `{"model":"m","instructions":"sys","input":"天气"}`,
		next: `{"model":"m","instructions":"sys","input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"天气"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"晴"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"晴天"}]}]}`,
	},
	{
		name: "claude",
		first: `{"model":"m","max_tokens":9,"system":[{"type":"text","text":"sys"}],
			"messages":[{"role":"user","content":"天气"}]}`,
		next: `{"model":"m","max_tokens":9,"system":[{"type":"text","text":"sys"}],
			"messages":[{"role":"user","content":"天气"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"晴"}]},
			{"role":"assistant","content":[{"type":"text","text":"晴天"}]}]}`,
	},
	{
		name:  "gemini",
		first: `{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"天气"}]}]}`,
		next: `{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[
			{"role":"user","parts":[{"text":"天气"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"v":"晴"}}}]},
			{"role":"model","parts":[{"text":"晴天"}]}]}`,
	},
}

// TestEntriesKeepPreviousTurnAsPrefix 确认上一轮的条目序列是这一轮的前缀。
//
// 这是前缀链的全部依据。不成立的话，查表会查不到上一轮写下的指纹，兜底逻辑等于
// 没写。工具调用是最容易打断这条关系的地方，因此每一对都带一次。
func TestEntriesKeepPreviousTurnAsPrefix(t *testing.T) {
	for _, pair := range toolTurnPairs {
		t.Run(pair.name, func(t *testing.T) {
			name := dialect.Name(pair.name)
			first, err := dialect.EntriesOf(name, []byte(pair.first))
			if err != nil {
				t.Fatalf("第一轮切分失败: %v", err)
			}
			next, err := dialect.EntriesOf(name, []byte(pair.next))
			if err != nil {
				t.Fatalf("第二轮切分失败: %v", err)
			}
			if len(next) <= len(first) {
				t.Fatalf("第二轮应当更长: 第一轮 %d 条，第二轮 %d 条", len(first), len(next))
			}
			for index, entry := range first {
				if !bytes.Equal(entry, next[index]) {
					t.Fatalf("第 %d 个条目对不上:\n第一轮 %s\n第二轮 %s", index, entry, next[index])
				}
			}
		})
	}
}

// TestEntriesIgnoreKeyOrder 确认客户端换 JSON 键顺序不会改变切分结果。
//
// 客户端重发同一段历史时不保证键顺序稳定。不规范化的话，指纹每次都变，前缀链
// 永远查不中。
func TestEntriesIgnoreKeyOrder(t *testing.T) {
	pairs := map[string][2]string{
		"openai": {
			`{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"一"}]}`,
			`{"messages":[{"content":"sys","role":"system"},{"content":"一","role":"user"}],"model":"m"}`,
		},
		"claude": {
			`{"model":"m","max_tokens":9,"system":{"type":"text","text":"sys"},"messages":[{"role":"user","content":"一"}]}`,
			`{"messages":[{"content":"一","role":"user"}],"max_tokens":9,"system":{"text":"sys","type":"text"},"model":"m"}`,
		},
		"gemini": {
			`{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"一"}]}]}`,
			`{"contents":[{"parts":[{"text":"一"}],"role":"user"}],"systemInstruction":{"parts":[{"text":"sys"}]}}`,
		},
		"openai_responses": {
			`{"model":"m","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"一"}]}]}`,
			`{"input":[{"content":[{"text":"一","type":"input_text"}],"role":"user","type":"message"}],"instructions":"sys","model":"m"}`,
		},
	}
	for name, pair := range pairs {
		t.Run(name, func(t *testing.T) {
			first, err := dialect.EntriesOf(dialect.Name(name), []byte(pair[0]))
			if err != nil {
				t.Fatalf("切分失败: %v", err)
			}
			second, err := dialect.EntriesOf(dialect.Name(name), []byte(pair[1]))
			if err != nil {
				t.Fatalf("切分失败: %v", err)
			}
			if len(first) != len(second) {
				t.Fatalf("条目数不同: %d 与 %d", len(first), len(second))
			}
			for index := range first {
				if !bytes.Equal(first[index], second[index]) {
					t.Fatalf("第 %d 个条目对不上:\n%s\n%s", index, first[index], second[index])
				}
			}
		})
	}
}

// parallelTurn 是带两次并行工具调用的同一轮请求，用来量条目数。
//
// 同一次工具调用在四种方言里占的条目数不同，这是前缀链不能靠「减固定条数」实现的
// 原因。单次工具调用看不出来（四种方言都是四条），并行才显形。
var parallelTurn = map[dialect.Name]string{
	dialect.OpenAIChat: `{"model":"m",
		"messages":[{"role":"user","content":"天气"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"晴"},
		{"role":"tool","tool_call_id":"c2","content":"阴"},
		{"role":"assistant","content":"晴转阴"}]}`,
	dialect.OpenAIResponses: `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"天气"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"晴"},
		{"type":"function_call_output","call_id":"c2","output":"阴"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"晴转阴"}]}]}`,
	dialect.ClaudeMessages: `{"model":"m","max_tokens":9,
		"messages":[{"role":"user","content":"天气"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"f","input":{}},
			{"type":"tool_use","id":"t2","name":"g","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"晴"},
			{"type":"tool_result","tool_use_id":"t2","content":"阴"}]},
		{"role":"assistant","content":[{"type":"text","text":"晴转阴"}]}]}`,
	dialect.GeminiGenerate: `{"contents":[
		{"role":"user","parts":[{"text":"天气"}]},
		{"role":"model","parts":[
			{"functionCall":{"name":"f","args":{}}},
			{"functionCall":{"name":"g","args":{}}}]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"f","response":{"v":"晴"}}},
			{"functionResponse":{"name":"g","response":{"v":"阴"}}}]},
		{"role":"model","parts":[{"text":"晴转阴"}]}]}`,
}

// TestEntriesCountParallelToolRound 记录两次并行工具调用在各方言里占几个条目。
//
// 两个渠道之间最多差两条：Responses 把 function_call 与 function_call_output 都
// 摆在顶层，Chat 一个调用占一条 tool 消息，Claude 与 Gemini 把多个块装进同一条。
// 一次工具调用四条时四种方言一致，并行才分岔，所以这个测试必须用并行夹具。
func TestEntriesCountParallelToolRound(t *testing.T) {
	// 头条目加对话条目。对话条目数即并行调用带来的差异所在。
	wants := map[dialect.Name]int{
		dialect.ClaudeMessages:  5, // 头条目 + 用户、助手（两个块）、用户（两个结果）、助手
		dialect.GeminiGenerate:  5, // 同上，块换成 part
		dialect.OpenAIChat:      6, // 头条目 + 用户、助手、两条 tool 消息、助手
		dialect.OpenAIResponses: 7, // 头条目 + 用户、两条 function_call、两条结果、助手
	}
	for name, want := range wants {
		entries, err := dialect.EntriesOf(name, []byte(parallelTurn[name]))
		if err != nil {
			t.Fatalf("%s 切分失败: %v", name, err)
		}
		if len(entries) != want {
			t.Fatalf("%s 的条目数不符: 期望 %d，实际 %d", name, want, len(entries))
		}
	}
}

// toolHeadBodies 在四种方言里都声明同一个工具，用来确认工具被收进头条目。
var toolHeadBodies = map[dialect.Name]string{
	dialect.OpenAIChat: `{"model":"m","tools":[{"type":"function","function":{"name":"查天气"}}],
		"messages":[{"role":"system","content":"sys"},{"role":"user","content":"一"}]}`,
	dialect.OpenAIResponses: `{"model":"m","instructions":"sys","tools":[{"type":"function","name":"查天气"}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"一"}]}]}`,
	dialect.ClaudeMessages: `{"model":"m","max_tokens":9,"system":"sys","tools":[{"name":"查天气"}],
		"messages":[{"role":"user","content":"一"}]}`,
	dialect.GeminiGenerate: `{"systemInstruction":{"parts":[{"text":"sys"}]},
		"tools":[{"functionDeclarations":[{"name":"查天气"}]}],
		"contents":[{"role":"user","parts":[{"text":"一"}]}]}`,
}

// TestEntriesPutToolsAndSystemInHead 确认工具定义与系统提示都进了第一个条目。
//
// 四种方言把这二者放在不同位置，统一收进头条目之后，序列结构才在四种方言下一致，
// 也才只因为「客户端改了工具或系统提示」而分叉。
//
// 断言要写出具体的工具名，不能只查是否存在 tools 这个键：头条目是个固定结构，
// 没有工具时那个键也在，值为 null，那样的断言恒真。
func TestEntriesPutToolsAndSystemInHead(t *testing.T) {
	for name, body := range toolHeadBodies {
		entries, err := dialect.EntriesOf(name, []byte(body))
		if err != nil {
			t.Fatalf("%s 切分失败: %v", name, err)
		}
		head := string(entries[0])
		if !strings.Contains(head, "查天气") {
			t.Fatalf("%s 的头条目里没有工具名: %s", name, head)
		}
		if !strings.Contains(head, "sys") {
			t.Fatalf("%s 的头条目里没有系统提示: %s", name, head)
		}
		if len(entries) < 2 {
			t.Fatalf("%s 应当还有一个对话条目", name)
		}
		if strings.Contains(string(entries[1]), "查天气") {
			t.Fatalf("%s 的工具名不该出现在对话条目里: %s", name, entries[1])
		}
	}
}

// TestEntriesNoToolsStillHasHead 确认没有工具时头条目仍然存在，只是值为空。
func TestEntriesNoToolsStillHasHead(t *testing.T) {
	entries, err := dialect.EntriesOf(dialect.OpenAIChat,
		[]byte(`{"model":"m","messages":[{"role":"user","content":"一"}]}`))
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应当有一个头条目加一条对话，实际 %d 条", len(entries))
	}
	if !strings.Contains(string(entries[0]), `"tools":null`) {
		t.Fatalf("没有工具时头条目的 tools 应当是 null: %s", entries[0])
	}
}

// TestEntriesNormalizeEquivalentSpellings 确认同一段内容的两种写法算同一段条目。
//
// 多模态内容既允许写成纯字符串，也允许写成只含一个文本块的数组；Responses 的
// input 还允许整段写成纯字符串。这些写法表达同一段内容，不归一的话客户端换一种
// 写法前缀就断了，前缀链永远查不中。
func TestEntriesNormalizeEquivalentSpellings(t *testing.T) {
	cases := []struct {
		name     string
		dialect  dialect.Name
		spelling [2]string
	}{
		{
			name:    "openai 消息内容",
			dialect: dialect.OpenAIChat,
			spelling: [2]string{
				`{"model":"m","messages":[{"role":"user","content":"一"}]}`,
				`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"一"}]}]}`,
			},
		},
		{
			name:    "claude 系统提示",
			dialect: dialect.ClaudeMessages,
			spelling: [2]string{
				`{"model":"m","max_tokens":9,"system":"sys","messages":[{"role":"user","content":"一"}]}`,
				`{"model":"m","max_tokens":9,"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"一"}]}`,
			},
		},
		{
			name:    "claude 消息内容",
			dialect: dialect.ClaudeMessages,
			spelling: [2]string{
				`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":"一"}]}`,
				`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":[{"type":"text","text":"一"}]}]}`,
			},
		},
		{
			name:    "responses 整段简写",
			dialect: dialect.OpenAIResponses,
			spelling: [2]string{
				`{"model":"m","input":"一"}`,
				`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"一"}]}]}`,
			},
		},
		{
			name:    "responses 消息内容",
			dialect: dialect.OpenAIResponses,
			spelling: [2]string{
				`{"model":"m","input":[{"type":"message","role":"user","content":"一"}]}`,
				`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"一"}]}]}`,
			},
		},
		{
			name:    "responses 助手消息内容",
			dialect: dialect.OpenAIResponses,
			spelling: [2]string{
				`{"model":"m","input":[{"type":"message","role":"assistant","content":"一"}]}`,
				`{"model":"m","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"一"}]}]}`,
			},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			first, err := dialect.EntriesOf(item.dialect, []byte(item.spelling[0]))
			if err != nil {
				t.Fatalf("第一种写法切分失败: %v", err)
			}
			second, err := dialect.EntriesOf(item.dialect, []byte(item.spelling[1]))
			if err != nil {
				t.Fatalf("第二种写法切分失败: %v", err)
			}
			if len(first) != len(second) {
				t.Fatalf("条目数不同: %d 与 %d", len(first), len(second))
			}
			for index := range first {
				if !bytes.Equal(first[index], second[index]) {
					t.Fatalf("第 %d 个条目对不上:\n%s\n%s", index, first[index], second[index])
				}
			}
		})
	}
}

// TestEntriesRejectUnknownDialect 确认未知方言明确报错。
func TestEntriesRejectUnknownDialect(t *testing.T) {
	if _, err := dialect.EntriesOf(dialect.Name("nope"), []byte(`{}`)); err == nil {
		t.Fatal("未知方言应当报错")
	}
}
