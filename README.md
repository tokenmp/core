# core

TokenMP 的转发内核，Go 库，模块 `github.com/tokenmp/core`。
它对外暴露四种大模型文本协议的端点，对内转发到配置好的上游渠道，
跨协议时转换，并按配置加载本地 JavaScript 插件。

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

## 许可

本项目按 AGPL-3.0 发布，全文见 [LICENSE](LICENSE)，不加 `-or-later`。
转换内核来自 new-api 的 RelayKit，它同样按 AGPL-3.0 发布，
版权与许可列在 [NOTICE](NOTICE) 与 [第三方许可](THIRD-PARTY-LICENSES.md)。

AGPL-3.0 第 13 条要求：通过网络与本程序交互的用户，有权拿到对应版本的源码。
把它部署成对外服务时，请一并提供源码位置。

本项目与 new-api、QuantumNous 没有隶属关系。
