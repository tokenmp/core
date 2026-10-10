package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokenmp/core/plugin"
)

const echoPlugin = `export const meta = {
  key: "echo",
  name: "回显",
  version: "0.1.0",
  apiVersion: "v1",
  description: "把插件标记写进请求",
};

export function onRequest(input) {
  if (!input.body || !Array.isArray(input.body.messages)) return null;
  input.body.messages.push({ role: "system", content: "tag=" + host.config.tag });
  return { body: input.body };
}

export function onEvent(input) {
  if (input.event === "ping") return null;
  return { data: input.data };
}

export function onStreamEnd(input) {
  return { events: input.events };
}

export function onResponse(input) {
  input.body.plugin = host.plugin.key;
  return { body: input.body };
}
`

const asyncPlugin = `export const meta = { key: "async", name: "异步", version: "1", apiVersion: "v1" };
export async function onRequest(input) { return null; }
`

const endlessPlugin = `export const meta = { key: "endless", name: "死循环", version: "1", apiVersion: "v1" };
export function onRequest() { for (;;) {} }
`

const fetchPlugin = `export const meta = { key: "fetch", name: "网络", version: "1", apiVersion: "v1" };
export function onRequest(input) { fetch("https://example.com"); return null; }
`

const legacyPlugin = `export const meta = { key: "legacy", name: "旧版本", version: "1", apiVersion: "v2" };
export function onRequest(input) { return null; }
`

// writePlugin 在临时目录里放一个插件。
func writePlugin(t *testing.T, key, source string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, key, plugin.PluginFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("写入插件失败: %v", err)
	}
	return dir
}

// loadEcho 编译回声插件并绑定配置。
func loadEcho(t *testing.T) *plugin.Instance {
	t.Helper()
	dir := writePlugin(t, "echo", echoPlugin)
	loaded, err := plugin.LoadFile(filepath.Join(dir, "echo", plugin.PluginFile), plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	return loaded.Bind(map[string]any{"tag": "local"})
}

func TestCompileReadsManifestAndHooks(t *testing.T) {
	instance := loadEcho(t)
	manifest := instance.Manifest()
	if manifest.Key != "echo" || manifest.Name != "回显" || manifest.APIVersion != plugin.APIVersion {
		t.Fatalf("自述信息不符: %+v", manifest)
	}
	for _, hook := range plugin.Hooks() {
		if !instance.Has(hook) {
			t.Fatalf("应当导出 %s", hook)
		}
		if !manifest.Has(hook) {
			t.Fatalf("自述信息里应当记录 %s", hook)
		}
	}
}

func TestCompileRejectsBadPlugins(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		source  string
		keyword string
	}{
		{"语法错误", "broken", "export const meta = {", "编译"},
		{"缺少 meta", "nometa", "export function onRequest() { return null; }", "meta"},
		{"契约版本不支持", "legacy", legacyPlugin, "apiVersion"},
		{"没有导出任何钩子", "noop", `export const meta = { key: "noop", version: "1", apiVersion: "v1" };`, "钩子"},
		{"钩子不是函数", "notfn", `export const meta = { key: "notfn", version: "1", apiVersion: "v1" }; export const onRequest = 1;`, "不是函数"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			dir := writePlugin(t, item.key, item.source)
			_, err := plugin.LoadFile(filepath.Join(dir, item.key, plugin.PluginFile), plugin.DefaultLimits())
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), item.keyword) {
				t.Fatalf("错误信息应包含 %q，实际 %v", item.keyword, err)
			}
		})
	}
}

func TestLoadFileChecksKeyAgainstDirectory(t *testing.T) {
	dir := writePlugin(t, "other-name", echoPlugin)
	_, err := plugin.LoadFile(filepath.Join(dir, "other-name", plugin.PluginFile), plugin.DefaultLimits())
	if err == nil {
		t.Fatal("meta.key 与目录名不一致应当报错")
	}
	if !strings.Contains(err.Error(), "meta.key") {
		t.Fatalf("错误信息应指出不一致: %v", err)
	}
}

func TestCallRewritesPayload(t *testing.T) {
	instance := loadEcho(t)
	input := []byte(`{"dialect":"openai","model":"m","body":{"model":"m","messages":[{"role":"user","content":"你好"}]}}`)
	output, changed, err := instance.Call(context.Background(), plugin.HookRequest, input, plugin.Session{})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !changed {
		t.Fatal("插件应当给出新值")
	}
	if !strings.Contains(string(output), "tag=local") {
		t.Fatalf("改写未生效: %s", output)
	}
}

func TestCallTreatsNullAsUnchanged(t *testing.T) {
	instance := loadEcho(t)
	input := []byte(`{"dialect":"openai","body":{"model":"m"}}`)
	output, changed, err := instance.Call(context.Background(), plugin.HookRequest, input, plugin.Session{})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if changed {
		t.Fatal("返回 null 应当表示不改写")
	}
	if string(output) != string(input) {
		t.Fatalf("不改写时应原样返回输入: %s", output)
	}
}

func TestCallRejectsAsyncHook(t *testing.T) {
	dir := writePlugin(t, "async", asyncPlugin)
	loaded, err := plugin.LoadFile(filepath.Join(dir, "async", plugin.PluginFile), plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	instance := loaded.Bind(nil)
	_, _, err = instance.Call(context.Background(), plugin.HookRequest, []byte(`{}`), plugin.Session{})
	if !errors.Is(err, plugin.ErrAsyncHook) {
		t.Fatalf("应当拒绝异步钩子，实际 %v", err)
	}
}

func TestCallInterruptsEndlessHook(t *testing.T) {
	dir := writePlugin(t, "endless", endlessPlugin)
	limits := plugin.DefaultLimits()
	limits.Timeout = 100 * time.Millisecond
	loaded, err := plugin.LoadFile(filepath.Join(dir, "endless", plugin.PluginFile), limits)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	instance := loaded.Bind(nil)
	start := time.Now()
	_, _, err = instance.Call(context.Background(), plugin.HookRequest, []byte(`{}`), plugin.Session{})
	if err == nil {
		t.Fatal("死循环应当被中断")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("中断耗时过长: %s", elapsed)
	}
}

func TestHostProvidesNoNetworkAccess(t *testing.T) {
	dir := writePlugin(t, "fetch", fetchPlugin)
	loaded, err := plugin.LoadFile(filepath.Join(dir, "fetch", plugin.PluginFile), plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	instance := loaded.Bind(nil)
	if _, _, err := instance.Call(context.Background(), plugin.HookRequest, []byte(`{}`), plugin.Session{}); err == nil {
		t.Fatal("插件不应能访问网络")
	}
}

func TestCallRejectsOversizedPayload(t *testing.T) {
	dir := writePlugin(t, "echo", echoPlugin)
	limits := plugin.DefaultLimits()
	limits.PayloadBytes = 64
	loaded, err := plugin.LoadFile(filepath.Join(dir, "echo", plugin.PluginFile), limits)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	instance := loaded.Bind(nil)
	big := `{"body":{"messages":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}}`
	if _, _, err := instance.Call(context.Background(), plugin.HookRequest, []byte(big), plugin.Session{}); !errors.Is(err, plugin.ErrPayloadTooLarge) {
		t.Fatalf("应当因超限失败，实际 %v", err)
	}
}

func TestLint(t *testing.T) {
	clean := plugin.LintSource("plugins/echo/plugin.js", []byte(echoPlugin), plugin.DefaultLimits())
	for _, issue := range clean {
		if issue.IsError() {
			t.Fatalf("可用插件不应有致命结论: %s", issue)
		}
	}
	broken := plugin.LintSource("plugins/legacy/plugin.js", []byte(legacyPlugin), plugin.DefaultLimits())
	if !hasError(broken) {
		t.Fatal("契约版本不支持的插件应当报致命结论")
	}
	mismatched := plugin.LintSource("plugins/echo/plugin.js", []byte(strings.Replace(echoPlugin, `key: "echo"`, `key: "other"`, 1)), plugin.DefaultLimits())
	if !hasError(mismatched) {
		t.Fatal("key 与目录不一致应当报致命结论")
	}
	withFetch := plugin.LintSource("plugins/fetch/plugin.js", []byte(fetchPlugin), plugin.DefaultLimits())
	if !hasWarning(withFetch) {
		t.Fatal("用到网络访问应当有提示")
	}
}

func hasError(issues []plugin.Issue) bool {
	for _, issue := range issues {
		if issue.IsError() {
			return true
		}
	}
	return false
}

func hasWarning(issues []plugin.Issue) bool {
	for _, issue := range issues {
		if issue.Severity == plugin.SeverityWarning {
			return true
		}
	}
	return false
}

func TestReplayFixture(t *testing.T) {
	instance := loadEcho(t)
	fixture := plugin.Fixture{
		UnixNow: 1735689600,
		Cases: []plugin.FixtureCase{
			{
				Name: "写入插件标记",
				Hook: plugin.HookRequest,
				Args: []json.RawMessage{json.RawMessage(
					`{"body":{"model":"m","messages":[{"role":"user","content":"你好"}]}}`)},
				Path:     "body.messages.1.content",
				Expected: json.RawMessage(`"tag=local"`),
			},
			{
				Name:     "不改写时返回 null",
				Hook:     plugin.HookRequest,
				Args:     []json.RawMessage{json.RawMessage(`{"body":{"model":"m"}}`)},
				Expected: json.RawMessage(`null`),
			},
		},
	}
	if err := plugin.ValidateFixture(fixture); err != nil {
		t.Fatalf("fixture 应当合法: %v", err)
	}
	results := instance.Replay(fixture, slog.New(slog.DiscardHandler))
	for _, result := range results {
		if !result.Passed {
			t.Fatalf("用例 %s 未通过: %s", result.Name, result.Message)
		}
	}
}

func TestReplayFixtureReportsMismatch(t *testing.T) {
	instance := loadEcho(t)
	fixture := plugin.Fixture{
		Cases: []plugin.FixtureCase{{
			Name:     "期望值写错",
			Hook:     plugin.HookRequest,
			Args:     []json.RawMessage{json.RawMessage(`{"body":{"model":"m"}}`)},
			Expected: json.RawMessage(`{"body":{"model":"m"}}`),
		}},
	}
	results := instance.Replay(fixture, slog.New(slog.DiscardHandler))
	if len(results) != 1 || results[0].Passed {
		t.Fatalf("应当报告不通过: %+v", results)
	}
}

func TestValidateFixture(t *testing.T) {
	cases := []struct {
		name    string
		fixture plugin.Fixture
	}{
		{"没有用例", plugin.Fixture{}},
		{
			"钩子名不对",
			plugin.Fixture{Cases: []plugin.FixtureCase{{Name: "a", Hook: "onNope", Expected: json.RawMessage(`null`)}}},
		},
		{
			"member 与 path 同时给出",
			plugin.Fixture{Cases: []plugin.FixtureCase{
				{Name: "a", Hook: plugin.HookRequest, Member: "m", Path: "p", Expected: json.RawMessage(`null`)},
			}},
		},
		{
			"expected 与 expectedError 同时给出",
			plugin.Fixture{Cases: []plugin.FixtureCase{
				{Name: "a", Hook: plugin.HookRequest, Expected: json.RawMessage(`null`), ExpectedError: "x"},
			}},
		},
		{
			"既没有 expected 也没有 expectedError",
			plugin.Fixture{Cases: []plugin.FixtureCase{{Name: "a", Hook: plugin.HookRequest}}},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if err := plugin.ValidateFixture(item.fixture); err == nil {
				t.Fatal("应当报错")
			}
		})
	}
}

func TestLoadFixtureFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.json")
	content := `{"unixNow":1735689600,"cases":[{"name":"a","hook":"onRequest","args":[{}],"member":"body","expected":null}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	fixture, err := plugin.LoadFixture(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if fixture.UnixNow != 1735689600 || len(fixture.Cases) != 1 {
		t.Fatalf("fixture 解析不符: %+v", fixture)
	}
	if _, err := plugin.LoadFixture(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("文件不存在应当报错")
	}
}

func TestStoreInvokeChainAndFailureIsolation(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	good := loadEcho(t)
	dir := writePlugin(t, "broken", `export const meta = { key: "broken", name: "坏", version: "1", apiVersion: "v1" };
export function onRequest() { throw new Error("故意失败"); }`)
	brokenLoaded, err := plugin.LoadFile(filepath.Join(dir, "broken", plugin.PluginFile), plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	store, err := plugin.NewStore([]plugin.Entry{
		{Instance: brokenLoaded.Bind(nil), Enabled: true, Order: 1},
		{Instance: good, Enabled: true, Order: 2},
	}, logger)
	if err != nil {
		t.Fatalf("构造集合失败: %v", err)
	}
	input := []byte(`{"body":{"model":"m","messages":[{"role":"user","content":"你好"}]}}`)
	outcome := store.Invoke(context.Background(), plugin.HookRequest, input, nil, plugin.Session{})
	if len(outcome.Failures) != 1 || outcome.Failures[0].Key != "broken" {
		t.Fatalf("应当记录一次失败: %+v", outcome.Failures)
	}
	if len(outcome.Applied) != 1 || outcome.Applied[0] != "echo" {
		t.Fatalf("失败之后的插件应当继续执行: %+v", outcome.Applied)
	}
	if !strings.Contains(string(outcome.Data), "tag=local") {
		t.Fatalf("改写未生效: %s", outcome.Data)
	}
}

func TestStoreDisabledAndFiltered(t *testing.T) {
	good := loadEcho(t)
	store, err := plugin.NewStore([]plugin.Entry{{Instance: good, Enabled: false, Order: 1}}, nil)
	if err != nil {
		t.Fatalf("构造集合失败: %v", err)
	}
	if store.HasAny(plugin.HookRequest) {
		t.Fatal("停用的插件不应算作参与")
	}
	store, err = plugin.NewStore([]plugin.Entry{{Instance: good, Enabled: true, Order: 1}}, nil)
	if err != nil {
		t.Fatalf("构造集合失败: %v", err)
	}
	input := []byte(`{"body":{"model":"m","messages":[]}}`)
	outcome := store.Invoke(context.Background(), plugin.HookRequest, input, []string{"other"}, plugin.Session{})
	if outcome.Changed() {
		t.Fatal("过滤条件不匹配时不应改写")
	}
	if missing := store.Missing([]string{"echo", "nope"}); len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("缺失列表不符: %+v", missing)
	}
}

func TestLoadDirScansSubdirectoriesAndFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "flat"), 0o750); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o750); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	files := map[string]string{
		filepath.Join(dir, "flat.js"):             strings.Replace(echoPlugin, `key: "echo"`, `key: "flat"`, 1),
		filepath.Join(dir, "nested", "plugin.js"): strings.Replace(echoPlugin, `key: "echo"`, `key: "nested"`, 1),
		filepath.Join(dir, "README.md"):           "不是插件",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	loaded, err := plugin.LoadDir(dir, plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("应当加载 2 个插件，实际 %d", len(loaded))
	}
	keys := map[string]bool{}
	for _, item := range loaded {
		keys[item.Key()] = true
	}
	if !keys["flat"] || !keys["nested"] {
		t.Fatalf("插件 key 不符: %+v", keys)
	}
}
