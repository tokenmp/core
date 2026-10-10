# core

TokenMP 的转发内核，Go 库，模块 `github.com/tokenmp/core`。
它对外暴露四种大模型文本协议的端点，对内转发到配置好的上游渠道，
跨协议时转换，并按配置加载本地 JavaScript 插件。

同方言转发时不重建报文，报文字节原样送上游；只有规则写了 `upstream` 且与
客户端模型名不同时，才用定点改写把顶层 `model` 换成上游名，其余字节不动。
跨方言才解码重建。诊断与用量沿用 RelayKit 的类型，见
[转换内核](docs/relay.md) 的「与 RelayKit 的关系」。

本仓库不产出可执行文件。
命令行与管理界面各自有仓库，它们按 tag 依赖这里的公开包。

## 包

| 包 | 职责 |
| --- | --- |
| [dialect](dialect) | 四种方言的标识、路径形状，以及请求、响应、流式分片的编解码 |
| [relay](relay) | 转换内核：同方言字节透传，跨方言请求、响应、流式转换 |
| [channel](channel) | 上游渠道：基址拼接、鉴权样式、超时、重试、SSE 读写 |
| [plugin](plugin) | 插件宿主：编译、加载、四个钩子、限额、检查与 fixture 回放 |
| [config](config) | YAML 配置模型、加载、校验与路由解析 |
| [server](server) | HTTP 服务：把上面五个包装配起来并监听 |

依赖单向向下。
`server` 依赖其余全部，`config` 只依赖 `dialect`，`relay` 依赖 `dialect`。
公开面一旦发布就是兼容承诺。

支持的四种方言是 OpenAI Chat Completions、OpenAI Responses、
Anthropic Claude Messages 与 Google Gemini generateContent，
任意两种之间都能转换。

## 起一个服务

```go
package main

import (
	"context"
	"log/slog"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/server"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		panic(err)
	}
	logger := slog.Default()
	service, err := server.FromConfig(cfg, logger)
	if err != nil {
		panic(err)
	}
	if err := service.Run(context.Background()); err != nil {
		panic(err)
	}
}
```

装配要用的渠道凭据从环境变量读，变量名写在配置的 `auth.key_env` 里。

服务给每个进入的请求记两行访问日志（进入与完成），用传进来的 logger 写出，
级别按配置的 `log.level` 定。字段与级别口径见[配置规范](docs/config.md)的「访问日志」一节。

宿主想在每次请求结束时记自己的指标，用 `WithRequestDone` 拿原始量：

```go
service, err := server.FromConfigWith(cfg, logger, server.WithRequestDone(func(outcome server.Outcome) {
    // 口径由宿主定，core 只交原始量。
    logger.Info("请求结束",
        "ttft", outcome.FirstByte,
        "duration", outcome.Duration,
        "bytes", outcome.BytesWritten,
        "streaming", outcome.Streaming,
        "attempts", len(outcome.Attempts))
}))
```

`Outcome` 带首字节时间、写出字节数、每次候选尝试的耗时与状态、用量、模型与渠道。
三件事要留意：

- `FirstByte` 是首次写出**响应体**的时长。流式请求里它就是要算的 TTFT；
  非流式响应是一次写完的，这个值约等于 `Duration`，看不出 TTFT。
- `Usage` 只在跨方言转发且上游给出用量时非空。同方言透传按字节转发、不解析，
  流式响应的 TPS 因此算不出来。
- 回调同步跑在请求的 goroutine 上，宿主在里面做的耗时工作会算进客户端等待时间里。
  它 panic 会被吞掉并记一行 ERROR，不连累别的请求。

只用转换内核、不起服务也是支持的：

```go
engine := relay.NewEngine(relay.Options{})
prepared, err := engine.PrepareRequest(ctx, relay.Request{
    From: dialect.ClaudeMessages,
    To:   dialect.OpenAIChat,
    Body: body,
})
```

细节见 [转换内核](docs/relay.md)。

## 配置

格式是单个 YAML 文件，解码开启 `KnownFields`，拼错的字段在启动时报错。

```yaml
listen: 127.0.0.1:8787

channels:
  - name: openrouter
    dialect: openai
    base_url: https://openrouter.ai/api/v1
    path: /chat/completions
    auth:
      style: bearer
      key_env: OPENROUTER_API_KEY

models:
  - match: "gpt-*"
    channel: openrouter
    upstream: openai/gpt-5
```

字段清单与校验规则见 [配置](docs/config.md)，
可直接复制的一份在 [示例配置](examples/config.yaml)。

## 插件

插件是单文件同步 ECMAScript 模块，放在 `plugins/<key>/plugin.js`，
导出 `meta` 与需要的钩子。

```js
export const meta = {
  key: "echo",
  name: "回显",
  version: "0.1.0",
  apiVersion: "v1",
};

export function onRequest(input) {
  input.body.messages.push({ role: "system", content: "tag=" + host.config.tag });
  return { body: input.body };
}
```

宿主不提供网络、文件系统与动态代码执行，单次调用限时 50 毫秒。
插件失败不影响转发可用性。

契约见 [v1.d.ts](plugin-api/v1/v1.d.ts)，运行细节见 [插件](docs/plugin.md)，
示例见 [示例插件](examples/plugins/echo/plugin.js)。

## 命令

```
make check      # 门禁：编译、单测（-race）、静态检查、格式检查
make test       # 单测
make lint       # 静态检查
make tools      # 按 .mise.toml 装齐工具链
```

`make check` 只依赖 Go 与 golangci-lint，不需要网络、数据库或 node。

## 边界

本仓库只做本地转发。明确不做的事：

- 不做客户端鉴权，也不做配额与速率限制。默认只监听 `127.0.0.1`，
  对外开放时请自己加一层入口。
- 不做账户、余额、计费与结算，也没有数据库。
- 不做浏览器控制台。
- 不做异步任务类插件（视频、图片、轮询）。四个钩子只处理文本请求与响应。

## 渠道亲和

同一个模型配了多条候选渠道时，给这些规则写上同一个 `affinity` 组名，同一段会话的
连续请求就会粘在同一个渠道上，让上游的提示缓存有机会命中。

客户端肯配合的话，请求头带 `X-TokenMP-Session` 最准，请求体里的
`prompt_cache_key` 也行，但它只有 OpenAI 的两个方言有。两个都没带时 core 靠请求
内容认出上一轮发过的前缀，在进程内记着上一轮走了哪条渠道。

亲和默认关闭，要显式写组名才生效。它是偏好而不是钉子，粘住的渠道失败时后面的
候选照旧兜底。细节见[配置规范](docs/config.md)的「渠道亲和」一节。

## 已知边界

这几条是明确的取舍，不是待办。

重试会重发请求体。上游返回 5xx 时那个请求可能已经被处理了一半，
只有在上游保证按请求幂等时才安全。`Retry-After` 不参与退避。

插件没有内存上限。moejs 不提供这个开关，限额拦得住失控的插件，拦不住故意吃内存的。

默认不做客户端鉴权。服务只听 `127.0.0.1`，对外提供时必须自己加一层入口。

## 许可

本项目按 AGPL-3.0 发布，全文见 [LICENSE](LICENSE)，不加 `-or-later`。
转换内核来自 new-api 的 RelayKit，它同样按 AGPL-3.0 发布，
版权与许可列在 [NOTICE](NOTICE) 与 [第三方许可](THIRD-PARTY-LICENSES.md)。

AGPL-3.0 第 13 条要求：通过网络与本程序交互的用户，有权拿到对应版本的源码。
把它部署成对外服务时，请一并提供源码位置。

本项目与 new-api、QuantumNous 没有隶属关系。
