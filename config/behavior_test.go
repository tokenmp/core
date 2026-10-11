package config_test

import (
	"strings"
	"testing"

	"github.com/tokenmp/core/config"
)

// TestBehaviorDefaultsUnsetMatchBuiltins 覆盖不写 defaults 段：两个取值都退回
// 内置默认，行为与没有这两个配置键时相同。
func TestBehaviorDefaultsUnsetMatchBuiltins(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader("listen: 127.0.0.1:9999\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := cfg.MaxBodyBytes(); got != config.DefaultMaxBodyBytes {
		t.Fatalf("未写 defaults.max_body_bytes 应取 %d，实际 %d", config.DefaultMaxBodyBytes, got)
	}
	if got := cfg.DefaultMaxTokens(); got != config.DefaultMaxTokens {
		t.Fatalf("未写 defaults.default_max_tokens 应取 %d，实际 %d", config.DefaultMaxTokens, got)
	}
}

// TestBehaviorDefaultsFromYAML 覆盖两个键都写了时的取值。
func TestBehaviorDefaultsFromYAML(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
defaults:
  max_body_bytes: 1048576
  default_max_tokens: 7777
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := cfg.MaxBodyBytes(); got != 1048576 {
		t.Fatalf("defaults.max_body_bytes 应为 1048576，实际 %d", got)
	}
	if got := cfg.DefaultMaxTokens(); got != 7777 {
		t.Fatalf("defaults.default_max_tokens 应为 7777，实际 %d", got)
	}
}

// TestValidateRejectsNonPositiveBehaviorDefaults 覆盖取值下限：零和负数都拦下。
func TestValidateRejectsNonPositiveBehaviorDefaults(t *testing.T) {
	for _, source := range []string{
		"defaults:\n  max_body_bytes: 0\n",
		"defaults:\n  max_body_bytes: -1\n",
		"defaults:\n  default_max_tokens: 0\n",
		"defaults:\n  default_max_tokens: -1\n",
	} {
		_, err := config.Parse(strings.NewReader(source))
		if err == nil || !strings.Contains(err.Error(), "必须为正") {
			t.Fatalf("%q 应当被拦下，实际 %v", source, err)
		}
	}
}
