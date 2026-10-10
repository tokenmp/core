package config

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Matcher 是一个已校验的模型名匹配器。
type Matcher struct {
	pattern string
}

// CompileMatch 校验匹配式并返回匹配器。
//
// 支持两个通配符：* 匹配任意长度的一段（含 / 与 :），? 匹配一个字符。
// 其余字符按字面比较。模型名按 UTF-8 字符逐个比较。
func CompileMatch(pattern string) (Matcher, error) {
	if strings.TrimSpace(pattern) == "" {
		return Matcher{}, errors.New("匹配式不能为空")
	}
	if !utf8.ValidString(pattern) {
		return Matcher{}, fmt.Errorf("匹配式 %q 不是合法的 UTF-8", pattern)
	}
	return Matcher{pattern: pattern}, nil
}

// Match 报告名字是否命中该模式。模式不合法时返回 false。
func Match(pattern, name string) bool {
	matcher, err := CompileMatch(pattern)
	if err != nil {
		return false
	}
	return matcher.Match(name)
}

// Pattern 返回原始匹配式。
func (m Matcher) Pattern() string { return m.pattern }

// Match 报告名字是否命中。按 rune 比较，通配符不跨字符。
func (m Matcher) Match(name string) bool {
	return wildcardMatch(m.pattern, name)
}

// wildcardMatch 是带回溯的通配匹配：* 记下位置，失配时回退一格。
// 时间复杂度为 O(模式长度 × 名字长度)，模式来自配置且很短。
func wildcardMatch(pattern, name string) bool {
	patternRunes := []rune(pattern)
	nameRunes := []rune(name)
	patternIndex, nameIndex := 0, 0
	starIndex, backtrack := -1, 0
	for nameIndex < len(nameRunes) {
		switch {
		case patternIndex < len(patternRunes) &&
			(patternRunes[patternIndex] == '?' || patternRunes[patternIndex] == nameRunes[nameIndex]):
			patternIndex++
			nameIndex++
		case patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*':
			starIndex = patternIndex
			backtrack = nameIndex
			patternIndex++
		case starIndex >= 0:
			patternIndex = starIndex + 1
			backtrack++
			nameIndex = backtrack
		default:
			return false
		}
	}
	for patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(patternRunes)
}
