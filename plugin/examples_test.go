package plugin_test

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/tokenmp/core/plugin"
)

// TestExamplesStayValid 保证 examples 目录里的插件与 fixture 始终可用。
func TestExamplesStayValid(t *testing.T) {
	root := filepath.Join("..", "examples", "plugins")
	loaded, err := plugin.LoadDir(root, plugin.DefaultLimits())
	if err != nil {
		t.Fatalf("加载示例插件失败: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("示例插件目录应当只有 1 个插件，实际 %d", len(loaded))
	}
	instance := loaded[0].Bind(map[string]any{"tag": "example"})

	fixture, err := plugin.LoadFixture(filepath.Join(root, "echo", "fixture.json"))
	if err != nil {
		t.Fatalf("读取示例 fixture 失败: %v", err)
	}
	if err := plugin.ValidateFixture(fixture); err != nil {
		t.Fatalf("示例 fixture 不合法: %v", err)
	}
	for _, result := range instance.Replay(fixture, slog.New(slog.DiscardHandler)) {
		if !result.Passed {
			t.Fatalf("示例用例 %s 未通过: %s", result.Name, result.Message)
		}
	}
}
