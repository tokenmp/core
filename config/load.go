package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load 读取并校验配置文件。
//
// 解码开启 KnownFields：拼错的字段在加载时报错，而不是被静默忽略。相对路径一律
// 以配置文件所在目录为基准，因此工作目录变化不会改变行为。
func Load(path string) (*Config, error) {
	//nolint:gosec // 路径来自调用方：配置文件位置由使用者给出。
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: 打开配置文件失败: %w", err)
	}
	defer func() { _ = file.Close() }()

	cfg, err := Parse(file)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: 解析配置文件路径失败: %w", err)
	}
	cfg.Path = absolute
	cfg.resolvePaths(filepath.Dir(absolute))
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse 从阅读器解析配置并校验。相对路径保留原样，调用方需要时自行归一。
//
// 顶层账号是加载期形态，Parse 会自己调一次 Expand，因此返回值里只剩下展开后的
// 渠道名，没有账号引用。校验跑两次：第一次看原始写法，账号引用关系（账号是否
// 存在、有没有人引用）只有在展开之前才看得清；第二次看平铺形态，用来抓展开名
// 与手写渠道名撞车这类展开之后才出现的重名。见 Config.Expand 的注释。
func Parse(reader io.Reader) (*Config, error) {
	var raw Config
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("配置文件为空")
		}
		return nil, err
	}
	raw.Defaults()
	if err := raw.Validate(); err != nil {
		return nil, err
	}
	cfg := raw.Expand()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolvePaths 把相对路径归一为绝对路径。base 是配置文件所在目录。
func (c *Config) resolvePaths(base string) {
	if c.Plugins.Dir == "" || filepath.IsAbs(c.Plugins.Dir) {
		return
	}
	c.Plugins.Dir = filepath.Join(base, c.Plugins.Dir)
}
