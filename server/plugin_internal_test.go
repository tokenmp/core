package server

import (
	"encoding/json"
	"testing"
)

// TestMergeRequestHeaders 覆盖 onRequest 合并上游头的语义：链上后面的插件覆盖同名项，
// 头名为空的项忽略，只改头时也算一次改写。
func TestMergeRequestHeaders(t *testing.T) {
	payload := []byte(`{"dialect":"openai","model":"m","stream":false,"body":{"model":"m"},` +
		`"headers":{"X-Keep":"1","X-Win":"first"}}`)
	rewrite := []byte(`{"headers":{"X-Win":"second","":"散落的","X-New":"3"}}`)

	merged, err := mergeRequest(payload, rewrite)
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if merged == nil {
		t.Fatal("只改头也应当算一次改写")
	}

	var out requestPayload
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("解析合并结果失败: %v", err)
	}
	want := map[string]string{"X-Keep": "1", "X-Win": "second", "X-New": "3"}
	if len(out.Headers) != len(want) {
		t.Fatalf("头数量不符: %v", out.Headers)
	}
	for name, value := range want {
		if out.Headers[name] != value {
			t.Fatalf("头 %s 不符: %q", name, out.Headers[name])
		}
	}
}

// TestMergeRequestWithoutHeaders 确认没写 headers 的改写不会凭空造出一个头集合，
// 否则下游每次请求都会多一次「改写了」的判断。
func TestMergeRequestWithoutHeaders(t *testing.T) {
	payload := []byte(`{"dialect":"openai","model":"m","stream":false,"body":{"model":"m"}}`)
	rewrite := []byte(`{"headers":{}}`)

	merged, err := mergeRequest(payload, rewrite)
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if merged != nil {
		t.Fatalf("空 headers 不应当算改写: %s", merged)
	}
}
