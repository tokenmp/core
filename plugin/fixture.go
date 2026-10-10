package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Fixture 是一组插件回放用例。形状与 new-api 的任务插件 fixture 一致，便于互相
// 搬运用例。
type Fixture struct {
	// UnixNow 固定插件内 host.now() 的取值，让签名与时间戳类钩子可复现。
	// 为 0 时用当前时间。
	UnixNow int64 `json:"unixNow"`
	// Cases 是用例列表。
	Cases []FixtureCase `json:"cases"`
}

// FixtureCase 是一条回放用例。
type FixtureCase struct {
	// Name 是用例名。
	Name string `json:"name"`
	// Hook 是要调用的钩子。
	Hook Hook `json:"hook"`
	// Member 指定只比较返回值的某个成员，与 Path 互斥。
	Member string `json:"member,omitempty"`
	// Path 是按点分路径定位返回值的比较目标，与 Member 互斥。数字段表示数组下标。
	Path string `json:"path,omitempty"`
	// Args 是钩子的参数列表，第一个参数即钩子的输入负载。
	Args []json.RawMessage `json:"args"`
	// Expected 是期望值，与 ExpectedError 互斥。
	Expected json.RawMessage `json:"expected,omitempty"`
	// ExpectedError 是期望的错误信息子串，与 Expected 互斥。
	ExpectedError string `json:"expectedError,omitempty"`
}

// CaseResult 是一条用例的结果。
type CaseResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
}

// LoadFixture 读取 fixture 文件。
func LoadFixture(path string) (Fixture, error) {
	//nolint:gosec // 路径来自调用方：fixture 由使用者指定。
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("plugin: 读取 fixture %s 失败: %w", path, err)
	}
	var fixture Fixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return Fixture{}, fmt.Errorf("plugin: 解析 fixture %s 失败: %w", path, err)
	}
	return fixture, nil
}

// ValidateFixture 校验 fixture 的结构。
func ValidateFixture(fixture Fixture) error {
	if len(fixture.Cases) == 0 {
		return fmt.Errorf("plugin: fixture 没有任何用例")
	}
	for index, item := range fixture.Cases {
		where := fmt.Sprintf("cases[%d]", index)
		if item.Name == "" {
			return fmt.Errorf("plugin: %s: name 不能为空", where)
		}
		if !item.Hook.Valid() {
			return fmt.Errorf("plugin: %s: hook %q 不是已知钩子，可用钩子为 %s", where, item.Hook, hookNames())
		}
		if item.Member != "" && item.Path != "" {
			return fmt.Errorf("plugin: %s: member 与 path 不能同时给出", where)
		}
		if len(item.Expected) > 0 && item.ExpectedError != "" {
			return fmt.Errorf("plugin: %s: expected 与 expectedError 不能同时给出", where)
		}
		if len(item.Expected) == 0 && item.ExpectedError == "" {
			return fmt.Errorf("plugin: %s: 必须给出 expected 或 expectedError", where)
		}
	}
	return nil
}

// Replay 回放 fixture 里的全部用例。
//
// 钩子返回 null 或 undefined 表示不改写，此时实际值按 null 比较 —— 这与转发路径
// 上「不改写就用原值」的语义不同，回放关心的是插件自己的返回值。
func (i *Instance) Replay(fixture Fixture, logger *slog.Logger) []CaseResult {
	results := make([]CaseResult, 0, len(fixture.Cases))
	for _, item := range fixture.Cases {
		results = append(results, i.replayCase(item, fixture.UnixNow, logger))
	}
	return results
}

// replayCase 回放一条用例。
func (i *Instance) replayCase(item FixtureCase, unixNow int64, logger *slog.Logger) CaseResult {
	result := CaseResult{Name: item.Name}
	input := json.RawMessage("null")
	if len(item.Args) > 0 {
		input = item.Args[0]
	}
	if !i.Has(item.Hook) {
		result.Message = fmt.Sprintf("插件未导出 %s", item.Hook)
		return result
	}
	session := Session{Logger: logger}
	if unixNow > 0 {
		session.Now = time.Unix(unixNow, 0).UTC()
	}
	output, changed, err := i.Call(context.Background(), item.Hook, input, session)
	if item.ExpectedError != "" {
		if err == nil {
			result.Message = fmt.Sprintf("期望报错 %q，实际成功", item.ExpectedError)
			return result
		}
		if !strings.Contains(err.Error(), item.ExpectedError) {
			result.Message = fmt.Sprintf("期望报错包含 %q，实际是 %q", item.ExpectedError, err.Error())
			return result
		}
		result.Passed = true
		return result
	}
	if err != nil {
		result.Message = fmt.Sprintf("调用失败: %v", err)
		return result
	}
	actual := json.RawMessage("null")
	if changed {
		selected, selectErr := selectValue(output, item.Member, item.Path)
		if selectErr != nil {
			result.Message = selectErr.Error()
			return result
		}
		actual = selected
	}
	equal, err := jsonEqual(actual, item.Expected)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	if !equal {
		result.Message = fmt.Sprintf("期望 %s，实际 %s", compact(item.Expected), compact(actual))
		return result
	}
	result.Passed = true
	return result
}

// selectValue 按成员名或点分路径从插件输出里取值。
func selectValue(output []byte, member, path string) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		return nil, fmt.Errorf("插件输出不是合法 JSON: %w", err)
	}
	switch {
	case member != "":
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("插件输出不是对象，无法取成员 %s", member)
		}
		selected, ok := object[member]
		if !ok {
			return nil, fmt.Errorf("插件输出没有成员 %s", member)
		}
		value = selected
	case path != "":
		selected, err := walkPath(value, path)
		if err != nil {
			return nil, err
		}
		value = selected
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("序列化取值失败: %w", err)
	}
	return encoded, nil
}

// walkPath 按点分路径在 JSON 值里导航，数字段表示数组下标。
func walkPath(value any, path string) (any, error) {
	current := value
	for _, segment := range strings.Split(path, ".") {
		if index, err := strconv.Atoi(segment); err == nil {
			array, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("路径 %s 的 %s 段不是数组下标", path, segment)
			}
			if index < 0 || index >= len(array) {
				return nil, fmt.Errorf("路径 %s 的下标 %d 越界", path, index)
			}
			current = array[index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("路径 %s 的 %s 段不在对象上", path, segment)
		}
		next, ok := object[segment]
		if !ok {
			return nil, fmt.Errorf("路径 %s 的 %s 段不存在", path, segment)
		}
		current = next
	}
	return current, nil
}

// jsonEqual 比较两段 JSON 的语义是否一致。数字与对象键顺序的差异不算差异。
func jsonEqual(left, right []byte) (bool, error) {
	normalize := func(data []byte) ([]byte, error) {
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("不是合法 JSON: %w", err)
		}
		return json.Marshal(value)
	}
	leftNormalized, err := normalize(left)
	if err != nil {
		return false, err
	}
	rightNormalized, err := normalize(right)
	if err != nil {
		return false, err
	}
	return string(leftNormalized) == string(rightNormalized), nil
}

// compact 把 JSON 压成一行，用于错误信息。
func compact(data []byte) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, data); err != nil {
		return string(data)
	}
	return buffer.String()
}
