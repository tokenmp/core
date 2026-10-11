package config

import (
	"reflect"
	"strings"
)

// Key 是配置键表里的一条。CLI 的帮助、新配置模板与 `config path` 输出都从这份
// 表生成，同一份字段信息不再抄三份。
type Key struct {
	// Path 是配置路径。
	//
	// 嵌套用点号连接，列表在字段名后加 [] 再写元素字段，例如 channels[].timeout。
	// 不写成 channels.timeout：后者读起来像 channels 自己有个 timeout 字段，而实际
	// 字段在列表元素上；[] 把这一层明写出来。元素本身是标量的列表（如
	// channels[].auth.accounts）路径就到字段名为止，类型名里的 [] 已经说明了它是列表。
	Path string
	// Type 是类型名，如 string、int、duration、map[string]string。
	Type string
	// Default 是默认值的可读形式。没有默认值的字段写「必填」或「空」。
	Default string
}

// keyDefaults 按配置路径给出默认值的可读形式。
//
// 路径与类型都由 reflect 从 Config 的字段与 yaml tag 现算，只有可读形式写在这里：
// 32MiB 与 33554432 是同一个数，从 Go 值反推不出文档想写的那个形式，只能人工描述。
// 这是键表里唯一会漂移的部分，TestKeysMatchConfigFields 拿反射出的路径集合与它
// 对照，加字段、删字段或改字段名时漏改这里会失败。
var keyDefaults = map[string]string{
	"listen":                      DefaultListen,
	"log.format":                  DefaultLogFormat,
	"log.level":                   DefaultLogLevel,
	"defaults.max_body_bytes":     "32MiB",
	"defaults.default_max_tokens": "4096",

	"accounts[].name":    "必填",
	"accounts[].key_env": "必填",
	"accounts[].headers": "空",

	"channels[].name":     "必填",
	"channels[].dialect":  "必填",
	"channels[].base_url": "必填",
	"channels[].path":     "按方言",

	"channels[].auth.style":    "none",
	"channels[].auth.key_env":  "空",
	"channels[].auth.accounts": "空",
	"channels[].auth.header":   "空",
	"channels[].auth.query":    "空",

	"channels[].timeout":      "120s",
	"channels[].max_retries":  "0",
	"channels[].headers":      "空",
	"channels[].pass_headers": "空",

	"channels[].backoff.base":            "250ms",
	"channels[].backoff.max":             "2s",
	"channels[].backoff.max_retry_after": "120s",
	"channels[].failover_statuses":       "空（沿用 retry.failover）",

	"models[].match":    "必填",
	"models[].channel":  "必填",
	"models[].upstream": "空（沿用请求的模型名）",
	"models[].dialect":  "空（随渠道）",
	"models[].affinity": "空",

	"plugins.dir":            DefaultPluginDir,
	"plugins.list[].key":     "必填",
	"plugins.list[].enabled": "false",
	"plugins.list[].order":   "0",
	"plugins.list[].config":  "空",

	"affinity.table_size":       "8192",
	"affinity.ttl":              "10m",
	"affinity.min_prefix_bytes": "4096",

	"retry.in_channel": DefaultInChannelStatuses,
	"retry.failover":   DefaultFailoverStatuses,
}

// Keys 返回配置键表，按 Config 的字段声明顺序排列。
//
// 路径与类型由 Config 的字段与 yaml tag 现算，字段改名或加减字段时表跟着变，
// 不存在第二份手抄的字段清单。表里没有「是否可 set」这一列：那是 CLI `config set`
// 的策略，不是配置本身的事实，钉进 core 的公开面等于把一条命令行的策略写进库的
// 兼容承诺；CLI 自己维护可写白名单更合适。
func Keys() []Key {
	keys := make([]Key, 0, len(keyDefaults))
	collectKeys(reflect.TypeOf(Config{}), "", &keys)
	return keys
}

// collectKeys 递归收集 t 上的叶子字段。prefix 是当前已经走出来的路径片段。
func collectKeys(t reflect.Type, prefix string, keys *[]Key) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name := yamlName(field)
			if name == "" {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			collectKeys(field.Type, path, keys)
		}
	case reflect.Slice:
		element := t.Elem()
		for element.Kind() == reflect.Pointer {
			element = element.Elem()
		}
		// 元素是结构体时才展开成元素字段，例如 accounts[].name；标量列表是叶子，
		// 路径就是字段本身，类型名里的 [] 已经说明了它是个列表。
		if element.Kind() == reflect.Struct {
			collectKeys(element, prefix+"[]", keys)
			return
		}
		*keys = append(*keys, Key{
			Path:    prefix,
			Type:    typeNameOf(t),
			Default: keyDefaults[prefix],
		})
	default:
		*keys = append(*keys, Key{
			Path:    prefix,
			Type:    typeNameOf(t),
			Default: keyDefaults[prefix],
		})
	}
}

// yamlName 返回字段的 yaml 名。`yaml:"-"` 与没有 tag 的字段都返回空，两者都不该
// 进键表：前者是加载期字段，后者不进 YAML。
func yamlName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
	if name == "" || name == "-" {
		return ""
	}
	return name
}

// typeNameOf 返回叶子类型的可读名。Duration 这类自带编解码的别名要说成本名，
// 否则会露出 int64 这个实现细节。
func typeNameOf(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(Duration(0)) {
		return "duration"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int:
		return "int"
	case reflect.Bool:
		return "bool"
	case reflect.Interface:
		return "any"
	case reflect.Slice:
		return "[]" + typeNameOf(t.Elem())
	case reflect.Map:
		return "map[" + typeNameOf(t.Key()) + "]" + typeNameOf(t.Elem())
	default:
		return t.String()
	}
}
