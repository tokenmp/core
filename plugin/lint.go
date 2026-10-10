package plugin

import (
	"fmt"
	"os"
	"strings"
)

// 检查结论的级别。
const (
	// SeverityError 表示插件不可用。
	SeverityError = "error"
	// SeverityWarning 表示插件可用但值得注意。
	SeverityWarning = "warning"
)

// Issue 是一条检查结论。
type Issue struct {
	// Path 是插件文件路径。
	Path string `json:"path"`
	// Severity 取 error 或 warning。
	Severity string `json:"severity"`
	// Message 是结论说明。
	Message string `json:"message"`
}

// IsError 报告这条结论是否致命。
func (i Issue) IsError() bool { return i.Severity == SeverityError }

// String 返回可读的一行。
func (i Issue) String() string {
	return fmt.Sprintf("%s: %s: %s", i.Severity, i.Path, i.Message)
}

// LintFile 读取并检查一个插件文件。返回的 error 只表示读文件失败。
func LintFile(path string, limits Limits) ([]Issue, error) {
	source, err := readSource(path, limits.fill().SourceBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return []Issue{{Path: path, Severity: SeverityError, Message: "文件不存在"}}, nil
		}
		return nil, err
	}
	return LintSource(path, source, limits), nil
}

// LintSource 检查插件源码，返回全部结论。
func LintSource(path string, source []byte, limits Limits) []Issue {
	limits = limits.fill()
	var issues []Issue
	add := func(severity, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Severity: severity, Message: fmt.Sprintf(format, args...)})
	}
	if len(source) > limits.SourceBytes {
		add(SeverityError, "源码 %d 字节，超过上限 %d", len(source), limits.SourceBytes)
	}
	loaded, err := Compile(path, source, limits)
	if err != nil {
		add(SeverityError, "%s", err.Error())
		return issues
	}
	if expected := keyFromPath(path); expected != "" && loaded.manifest.Key != expected {
		add(SeverityError, "meta.key 是 %q，与文件所在位置要求的 %q 不一致", loaded.manifest.Key, expected)
	}
	issues = append(issues, sourceWarnings(path, source)...)
	sortIssues(issues)
	return issues
}

// forbiddenPatterns 是宿主不提供的能力。插件用了它们会在调用时报错，这里提前提示。
var forbiddenPatterns = []struct {
	Pattern string
	Hint    string
}{
	{"fetch(", "宿主不提供网络访问"},
	{"require(", "宿主不提供 require"},
	{"import(", "宿主不提供动态 import"},
	{"eval(", "宿主关闭了动态代码执行"},
	{"new Function(", "宿主关闭了动态代码执行"},
}

// sourceWarnings 扫描源码里用到了宿主不提供的能力的地方。
func sourceWarnings(path string, source []byte) []Issue {
	text := string(source)
	var issues []Issue
	for _, item := range forbiddenPatterns {
		if strings.Contains(text, item.Pattern) {
			issues = append(issues, Issue{
				Path:     path,
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("源码出现 %s：%s", item.Pattern, item.Hint),
			})
		}
	}
	if !strings.Contains(text, "export function") && !strings.Contains(text, "export const") {
		issues = append(issues, Issue{
			Path:     path,
			Severity: SeverityWarning,
			Message:  "没有找到函数导出，确认导出写法",
		})
	}
	return issues
}

// sortIssues 把致命结论排在前面。
func sortIssues(issues []Issue) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0 && issues[j].IsError() && !issues[j-1].IsError(); j-- {
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}
