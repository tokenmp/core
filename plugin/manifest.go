// Package plugin 是插件宿主：编译、加载、按钩子调用本地 JavaScript 插件，并提供
// 校验与回放工具。
//
// 插件是单文件同步 ECMAScript 模块，导出 meta 与若干钩子函数。契约见
// plugin-api/v1/v1.d.ts 与 docs/plugin.md。
//
// 安全边界：宿主不提供网络、文件系统、require、import 与动态代码执行；插件的执行
// 时长、并发数、源码大小与单次输入大小都有限额。加载插件等同于管理员级信任决定。
package plugin

import (
	"errors"
	"fmt"
	"time"
)

// APIVersion 是当前支持的插件契约版本。
const APIVersion = "v1"

// Hook 是钩子名，即插件要导出的函数名。
type Hook string

const (
	// HookRequest 在选路与转发之前改写请求。
	HookRequest Hook = "onRequest"
	// HookEvent 在每条流式事件写回客户端之前改写它。
	HookEvent Hook = "onEvent"
	// HookStreamEnd 在流结束前改写补发的分片。
	HookStreamEnd Hook = "onStreamEnd"
	// HookResponse 在非流式响应写回客户端之前改写它。
	HookResponse Hook = "onResponse"
)

// hookOrder 是钩子按请求生命周期的固定顺序，供文档与遍历使用。
var hookOrder = []Hook{HookRequest, HookEvent, HookStreamEnd, HookResponse}

// Hooks 按生命周期顺序返回四个钩子。
func Hooks() []Hook {
	return append([]Hook(nil), hookOrder...)
}

// Valid 报告是否是已知钩子。
func (h Hook) Valid() bool {
	for _, candidate := range hookOrder {
		if h == candidate {
			return true
		}
	}
	return false
}

// Manifest 是插件的自述信息，取自模块导出的 meta 对象。
type Manifest struct {
	// Key 是插件标识，须与目录名一致。
	Key string `json:"key"`
	// Name 是展示名。
	Name string `json:"name"`
	// Version 是插件自己的版本号。
	Version string `json:"version"`
	// APIVersion 是插件声明的契约版本，目前只支持 v1。
	APIVersion string `json:"apiVersion"`
	// Description 是可选说明。
	Description string `json:"description,omitempty"`
	// Hooks 是实际导出的钩子，由宿主在编译后填入，插件不必自己写。
	Hooks []Hook `json:"hooks,omitempty"`
}

// Has 报告插件是否导出了该钩子。
func (m Manifest) Has(hook Hook) bool {
	for _, candidate := range m.Hooks {
		if candidate == hook {
			return true
		}
	}
	return false
}

// Validate 校验自述信息。key 与目录名的比对由加载器完成。
func (m Manifest) Validate() error {
	if m.Key == "" {
		return errors.New("plugin: meta.key 不能为空")
	}
	if m.APIVersion != APIVersion {
		return fmt.Errorf("plugin: meta.apiVersion 必须是 %q，实际是 %q", APIVersion, m.APIVersion)
	}
	if m.Version == "" {
		return errors.New("plugin: meta.version 不能为空")
	}
	return nil
}

// Limits 是插件执行的限额。零值不可用，请用 DefaultLimits。
type Limits struct {
	// SourceBytes 是单个插件源码的字节上限。
	SourceBytes int
	// PayloadBytes 是单次钩子调用输入与输出的字节上限。
	PayloadBytes int
	// Timeout 是单次钩子调用的上限。
	Timeout time.Duration
	// Concurrency 是同一插件同时执行的钩子上限。
	Concurrency int
}

// 默认限额。
const (
	DefaultSourceBytes  = 1 << 20
	DefaultPayloadBytes = 8 << 20
	DefaultTimeout      = 50 * time.Millisecond
	DefaultConcurrency  = 8
)

// DefaultLimits 返回默认限额。默认超时取 50 毫秒：本地改写的正常耗时在微秒级，
// 超过这个量级的插件应当在转发路径之外完成。
func DefaultLimits() Limits {
	return Limits{
		SourceBytes:  DefaultSourceBytes,
		PayloadBytes: DefaultPayloadBytes,
		Timeout:      DefaultTimeout,
		Concurrency:  DefaultConcurrency,
	}
}

// fill 补上未设置的限额。
func (l Limits) fill() Limits {
	defaults := DefaultLimits()
	if l.SourceBytes <= 0 {
		l.SourceBytes = defaults.SourceBytes
	}
	if l.PayloadBytes <= 0 {
		l.PayloadBytes = defaults.PayloadBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = defaults.Timeout
	}
	if l.Concurrency <= 0 {
		l.Concurrency = defaults.Concurrency
	}
	return l
}

// Failure 是一次失败的钩子调用。钩子失败不影响转发可用性，只记录事实。
type Failure struct {
	// Key 是插件 key。
	Key string
	// Hook 是钩子名。
	Hook Hook
	// Err 是失败原因。
	Err error
}

// Error 实现 error。
func (f Failure) Error() string {
	return fmt.Sprintf("plugin: %s 的 %s 执行失败: %v", f.Key, f.Hook, f.Err)
}

// Unwrap 返回底层错误。
func (f Failure) Unwrap() error { return f.Err }

// Outcome 是一次钩子链调用的结果。
type Outcome struct {
	// Data 是链路走完后的负载。
	Data []byte
	// Applied 是实际改写了负载的插件 key，按执行顺序。
	Applied []string
	// Failures 是执行失败的插件。
	Failures []Failure
}

// Changed 报告是否有插件改写过负载。
func (o Outcome) Changed() bool { return len(o.Applied) > 0 }
