package config

import (
	"fmt"
	"strconv"
	"time"
)

// Duration 是配置里可读的时长，写法与 time.ParseDuration 一致，例如 120s、1m30s。
// 为兼容只写数字的配置，纯数字按秒解释。
type Duration time.Duration

// UnmarshalYAML 支持字符串与数字两种写法。
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	switch value := raw.(type) {
	case nil:
		*d = 0
		return nil
	case string:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("时长 %q 无法解析: %w", value, err)
		}
		*d = Duration(parsed)
		return nil
	case int:
		*d = Duration(time.Duration(value) * time.Second)
		return nil
	case float64:
		*d = Duration(time.Duration(value * float64(time.Second)))
		return nil
	default:
		return fmt.Errorf("时长 %v 的类型 %T 不受支持", raw, raw)
	}
}

// MarshalYAML 按 time.Duration 的字符串形式写出。
func (d Duration) MarshalYAML() (any, error) {
	return d.Duration().String(), nil
}

// Duration 返回标准库时长。
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// String 返回可读时长。
func (d Duration) String() string {
	return d.Duration().String()
}

// UnmarshalJSON 让 Duration 在 JSON 里也能工作，便于 CLI 输出配置快照。
func (d *Duration) UnmarshalJSON(data []byte) error {
	text := string(data)
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("时长 %s 无法解析: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}
