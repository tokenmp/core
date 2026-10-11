package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tokenmp/core/config"
)

// configLeafPaths 独立走一遍 Config 的字段与 yaml tag，返回全部叶子路径。
//
// 测试自己实现一份遍历，是为了让断言不绑在 Keys() 的实现上：两边都漏掉同一个
// 字段时才会同时失效，正常改动只会让其中一边先报出来。
func configLeafPaths(t *testing.T) map[string]bool {
	t.Helper()
	paths := make(map[string]bool)
	var walk func(current reflect.Type, prefix string)
	walk = func(current reflect.Type, prefix string) {
		for current.Kind() == reflect.Pointer {
			current = current.Elem()
		}
		switch current.Kind() {
		case reflect.Struct:
			for i := 0; i < current.NumField(); i++ {
				field := current.Field(i)
				if !field.IsExported() {
					continue
				}
				name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
				if name == "" || name == "-" {
					continue
				}
				path := name
				if prefix != "" {
					path = prefix + "." + name
				}
				walk(field.Type, path)
			}
		case reflect.Slice:
			element := current.Elem()
			for element.Kind() == reflect.Pointer {
				element = element.Elem()
			}
			if element.Kind() == reflect.Struct {
				walk(element, prefix+"[]")
				break
			}
			paths[prefix] = true
		default:
			paths[prefix] = true
		}
	}
	walk(reflect.TypeOf(config.Config{}), "")
	return paths
}

// TestKeysResolveToConfigFields 断言键表里每条路径都能在 Config 上找到对应字段。
func TestKeysResolveToConfigFields(t *testing.T) {
	fields := configLeafPaths(t)
	for _, key := range config.Keys() {
		if !fields[key.Path] {
			t.Errorf("键表里的 %s 在 Config 上找不到对应字段", key.Path)
		}
	}
}

// TestKeysMatchConfigFields 断言 Config 上每个叶子字段都在键表里，且类型与默认值
// 都不为空。字段改名或加减字段时这条会失败，逼着键表跟着改。
func TestKeysMatchConfigFields(t *testing.T) {
	keys := make(map[string]config.Key)
	for _, key := range config.Keys() {
		if _, duplicate := keys[key.Path]; duplicate {
			t.Errorf("键表里 %s 出现多次", key.Path)
		}
		keys[key.Path] = key
	}
	for path := range configLeafPaths(t) {
		key, ok := keys[path]
		if !ok {
			t.Errorf("Config 上的 %s 不在键表里", path)
			continue
		}
		if key.Type == "" || key.Default == "" {
			t.Errorf("键 %s 的类型或默认值为空: %+v", path, key)
		}
	}
}

// TestKeysCoverTopLevelScalars 断言顶层标量字段都进了键表。
func TestKeysCoverTopLevelScalars(t *testing.T) {
	keys := make(map[string]bool)
	for _, key := range config.Keys() {
		keys[key.Path] = true
	}
	typ := reflect.TypeOf(config.Config{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Struct, reflect.Slice:
			// 嵌套段与列表的元素字段由 TestKeysMatchConfigFields 逐条覆盖。
			continue
		}
		if !keys[name] {
			t.Errorf("顶层标量 %s 不在键表里", name)
		}
	}
}
