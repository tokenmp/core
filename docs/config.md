# 配置

`github.com/tokenmp/core/config` 定义配置文件的内存模型、加载与校验。
格式是 YAML，解析用 `gopkg.in/yaml.v3`，解码开启 `KnownFields`，
拼错的字段在加载时报错，而不是被静默忽略。

## 完整示例

```yaml
listen: 127.0.0.1:8787

log:
  format: json        # json | text
  level: info         # debug | info | warn | error

channels:
  - name: openrouter
    dialect: openai   # openai | openai_responses | claude | gemini
    base_url: https://openrouter.ai
    auth:
      style: bearer   # bearer | x-api-key | query | custom | none
      key_env: OPENROUTER_API_KEY
    timeout: 120s
    max_retries: 2
    headers:
      X-Title: tokenmp

  - name: local
    dialect: claude
    base_url: http://127.0.0.1:8000

models:
  # 客户端模型名，支持 * 与 ? 通配
  - match: "gpt-*"
    channel: openrouter
    upstream: openai/gpt-5
  - match: "claude-*"
    channel: local

plugins:
  dir: plugins
  list:
    - key: echo
      enabled: true
      order: 10
      config:
        tag: local
```

## 条款

凭据只写环境变量名，配置文件里不出现密钥。
`auth.style` 不是 `none` 时必须给出 `key_env`，缺少时校验失败。

同一个模型可以有多条候选，按配置顺序依次尝试。
`upstream` 不写就沿用客户端请求的模型名。
`dialect` 可以在模型这一层覆盖渠道方言，用于同一个基址上的多种协议。

相对路径以配置文件所在目录为基准。
`Load` 会把插件目录解析成绝对路径，因此工作目录变化不会改变行为。

## 字段

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | 监听地址 |
| `log.format` | `json` | 日志格式 |
| `log.level` | `info` | 日志级别 |
| `channels[].timeout` | `120s` | 单次上游请求的整体超时 |
| `channels[].max_retries` | `0` | 失败后的重试次数 |
| `channels[].path` | 按方言 | 覆盖上游路径 |
| `plugins.dir` | `plugins` | 插件目录 |
| `plugins.list[].order` | `0` | 执行顺序，越小越先执行 |
| 请求体上限 | `32 MiB` | 客户端请求体的字节上限，不是配置项 |

`timeout` 写成 `120s` 这样的时长，也可以写成纯数字，按秒解释。

`max_retries` 只覆盖两类失败：请求没能建立，以及上游返回 408、429、5xx。
取值上限是 10：转发是同步路径，重试次数乘以超时直接等于客户端等待时长。

重试有两件已知的取舍。一是重试会重发请求体，上游返回 5xx 时那个请求可能已经被
处理了一半，重发会让它被处理两次；只有在上游保证按请求幂等时才安全。
二是上游给出的 `Retry-After` 不参与退避，退避按固定指数增长。

超时的分工要分开说。非流式请求的整体超时覆盖到响应体读完为止。
流式请求不加整体超时，只压到收到响应头，之后靠调用方的 context 结束。

`path` 的规则要单独说一句。
默认情况下上游 URL 是 `base_url` 加方言路径，例如
`https://api.openai.com` 加 `/v1/chat/completions`。
`base_url` 已经带路径时（比如 `https://openrouter.ai/api/v1`）必须显式给出
`path`，取值从该路径之后算起，否则校验失败。
Gemini 的路径含模型名，不支持覆盖。

## 校验

`Validate` 一次报出全部问题，不是发现第一条就返回。
它检查日志取值、渠道名重复、方言是否已知、基址协议、超时与重试数量、
鉴权字段是否齐全、模型规则是否引用了存在的渠道。

插件目录里是否存在被启用的插件不在校验范围内，由装配阶段检查。
启用但缺失的插件会让服务启动失败，因为静默降级会让「插件没生效」变成一个
只能靠读日志发现的问题。

## 加载

```go
cfg, err := config.Load("/path/to/config.yaml")
if err != nil {
    return err
}
routes := cfg.Resolve("gpt-4o")
```

`Load` 依次做四件事：解码、填默认值、把相对路径归一、校验。
`Parse` 从 `io.Reader` 读取，跳过路径归一那一步，适合测试与嵌入场景。

## 路由解析

`Config.Resolve` 返回该模型名的候选路由，按配置顺序排列。
命中多条时全部返回，调用方按顺序尝试。没有命中时返回空切片。

通配符只有两个：`*` 匹配任意长度的一段（含 `/` 与 `:`），`?` 匹配一个字符。
`openai/*` 因此能匹配 `openai/gpt-5`。
