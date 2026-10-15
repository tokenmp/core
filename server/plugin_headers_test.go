package server_test

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/tokenmp/core/plugin"
	"github.com/tokenmp/core/server"
)

// upstreamHeaderPlugin 给上游加两个头，其中一个与渠道 headers 同名，用来验优先级。
const upstreamHeaderPlugin = `export const meta = { key: "upstream-header", name: "上游头", version: "1", apiVersion: "v1" };
export function onRequest() {
  return { headers: { "X-Plugin-Note": "插件加的", "X-Title": "from-plugin" } };
}
`

// pluginHeadersYAML 是一条渠道带静态 headers 与放行名单的配置。
func pluginHeadersYAML(upstreamURL string) string {
	return `
channels:
  - name: upstream
    dialect: openai
    base_url: "` + upstreamURL + `"
    pass_headers: [x-client-note]
    headers:
      X-Title: from-config
      X-Stays: keep
models:
  - {match: "client-model", channel: upstream}
`
}

// withPlugins 把一份插件集合装进装配参数。宿主直接调 server.New 时走 Options.Plugins，
// 这里用同一个字段，免得为测试另加一个公开选项。
func withPlugins(store *plugin.Store) server.Option {
	return func(options *server.Options) { options.Plugins = store }
}

// pluginStore 编译一个插件源码并装成只有它的集合。
func pluginStore(t *testing.T, source string) *plugin.Store {
	t.Helper()
	loaded, err := plugin.Compile("upstream-header.js", []byte(source), plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("编译插件失败: %v", err)
	}
	store, err := plugin.NewStore(
		[]plugin.Entry{{Instance: loaded.Bind(nil), Enabled: true, Order: 1}},
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatalf("构造插件集合失败: %v", err)
	}
	return store
}

// TestPluginHeadersReachUpstream 覆盖插件加的上游头，以及它与另外三处的优先级：
// 客户端按名单转发的头与渠道 headers 在插件之前，宿主按请求设的头在插件之后。
func TestPluginHeadersReachUpstream(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, pluginHeadersYAML(upstreamServer.URL),
		withPlugins(pluginStore(t, upstreamHeaderPlugin)),
		server.WithMiddleware(withUpstreamHeaders([][2]string{{"X-Plugin-Note", "宿主覆盖的"}})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{"X-Client-Note": "客户端带的"})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	for name, want := range map[string]string{
		"X-Client-Note": "客户端带的",
		"X-Stays":       "keep",
		"X-Title":       "from-plugin",
		"X-Plugin-Note": "宿主覆盖的",
	} {
		if values := got.Values(name); len(values) != 1 || values[0] != want {
			t.Fatalf("头 %s 不符，期望 %q，实得 %v", name, want, values)
		}
	}
}
