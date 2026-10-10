package dialect_test

import (
	"bytes"
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

// TestEntriesAlwaysStartWithHead 确认头条目总是第一个，且带着工具与系统提示。
func TestEntriesAlwaysStartWithHead(t *testing.T) {
	for _, pair := range toolTurnPairs {
		name := dialect.Name(pair.name)
		entries, err := dialect.EntriesOf(name, []byte(pair.next))
		if err != nil {
			t.Fatalf("%s 切分失败: %v", name, err)
		}
		head := string(entries[0])
		if !bytes.Contains([]byte(head), []byte("tools")) {
			t.Fatalf("%s 的头条目里没有 tools: %s", name, head)
		}
		if !bytes.Contains([]byte(head), []byte("sys")) {
			t.Fatalf("%s 的头条目里没有系统提示: %s", name, head)
		}
	}
}

// TestEntriesRejectUnknownDialect 确认未知方言明确报错。
func TestEntriesRejectUnknownDialect(t *testing.T) {
	if _, err := dialect.EntriesOf(dialect.Name("nope"), []byte(`{}`)); err == nil {
		t.Fatal("未知方言应当报错")
	}
}
