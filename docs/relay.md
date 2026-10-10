# 转换内核

`github.com/tokenmp/core/relay` 是纯函数式的协议转换内核。
它不发网络请求、不读配置，输入一段报文，输出一段报文。

上游地址、鉴权、重试与超时由 [channel](../channel) 负责，装配与路由由
[server](../server) 负责。这样拆开之后，转换矩阵可以脱离网络单独测，
`go test ./relay/...` 里跑的正是这张矩阵。

## 四种方言

| 方言 | 取值 | 客户端路径 |
| --- | --- | --- |
| OpenAI Chat Completions | `openai` | `POST /v1/chat/completions` |
| OpenAI Responses | `openai_responses` | `POST /v1/responses` |
| Anthropic Claude Messages | `claude` | `POST /v1/messages` |
| Google Gemini generateContent | `gemini` | `POST /v1beta/models/{model}:generateContent` |

任意两种方言之间都能转换，请求、非流式响应、流式分片共用同一张矩阵。
Gemini 的模型名写在路径上，因此它的流式端点由
`:streamGenerateContent?alt=sse` 区分，请求体里没有 `stream` 字段。

## 透传与转换

两方言相同时内核不重建报文。
`PrepareRequest` 只读出模型名与 `stream` 字段用于选路，随后返回 `Passthrough` 为真
的结果，`Body` 就是客户端原始字节。未建模的字段因此逐字节保留，同方言转发也不
受协议实现覆盖面影响。

两方言不同时内核才解码并重建报文。
`Prepared.Result` 里带着转换用的转换器 ID、质量等级、实际步骤与诊断。
诊断与用量用的是 RelayKit 自己的类型，见下文「与 RelayKit 的关系」。

## 请求与响应

```go
engine := relay.NewEngine(relay.Options{})

prepared, err := engine.PrepareRequest(ctx, relay.Request{
    From:          dialect.ClaudeMessages, // 客户端方言
    To:            dialect.OpenAIChat,     // 渠道方言
    Body:          body,
    OriginModel:   "client-model",
    UpstreamModel: "upstream-model",
})
if err != nil {
    return err
}
send(prepared.Body, prepared.Stream)

converted, err := engine.PrepareResponse(ctx, relay.Response{
    From:          dialect.OpenAIChat, // 渠道方言
    To:            dialect.ClaudeMessages,
    Body:          upstreamBody,
    OriginModel:   "client-model",
    UpstreamModel: "upstream-model",
})
```

`PrepareResponse` 是唯一的响应入口：
上游返回流式时不要用它，改用下面的流式会话。

## 流式会话

每条上游流对应一个 `relay.Stream`，不可并发复用。
会话结束前必须调用一次 `Finalize`，否则部分转换路径不会补出终止帧与最终用量。

```go
stream, err := engine.NewStream(
    dialect.OpenAIChat,       // 渠道方言
    dialect.ClaudeMessages,   // 客户端方言
    "client-model",
    "upstream-model",
    relay.StreamOptions{Model: "client-model", IncludeUsage: true},
)
if err != nil {
    return err
}

for _, data := range upstreamSSE {
    events, err := stream.Convert(ctx, data)
    if err != nil {
        return err
    }
    for _, event := range events {
        writeSSE(event.Event, event.Data)
    }
}

final, err := stream.Finalize(ctx)
if err != nil {
    return err
}
for _, event := range final {
    writeSSE(event.Event, event.Data)
}
```

`Convert` 收到的内容为空时返回空切片，调用方跳过即可。
内核不读写 SSE，只处理每条事件的 `data` 负载；`event:` 行由
[channel](../channel) 的 `sse.go` 负责。

两方言相同时 `NewStream` 返回 `relay.ErrSameDialect`。
那种情况应当按字节管道转发，经过 JSON 往返会丢掉未建模字段。

## 诊断与损耗策略

每条转换损耗写成一条诊断，带代码、字段路径、级别与源目标格式。
`Options.ToolLossPolicy` 决定请求阶段怎么处理：

| 取值 | 行为 |
| --- | --- |
| `allow` | 默认。照常完成转换，每条损耗都记进诊断 |
| `safe` | `error` 级诊断时把转换判为失败 |
| `strict` | 任何诊断都把转换判为失败 |

响应与流式转换在任何策略下都完成转换并返回诊断，不会因为损耗失败。

## 模型名尾缀

`-thinking`、`-nothinking`、`-high` 这类尾缀默认不解释，模型名原样发出。
打开 `Options.ModelSuffix` 之后，`Engine.ParseModel` 会剥掉尾缀并返回推理状态。

剥掉的时机在选路之前：调用方应当用返回的基础模型名去匹配路由规则，
再把推理状态交给内核渲染到目标请求上。

## 媒体解析

跨协议转换遇到图片时可能需要下载 URL 内容或解析 data URL。
这类能力由宿主提供，且只能在进程启动时注册一次：

```go
relayconvert.SetMediaResolver(relayconvert.MediaResolver{
    GetBase64Data: func(
        ctx context.Context,
        source types.FileSource,
        reason ...string,
    ) (string, string, error) {
        return downloadAndEncode(ctx, source)
    },
    DecodeBase64FileData: func(data string) (string, string, error) {
        return decodeDataURL(data)
    },
})
```

没有注册回调时，需要媒体解析的转换会明确报错，内核本身不会发起网络请求。
这个注册是进程级副作用，改了它会影响同一进程内所有转换调用。

## 与 RelayKit 的关系

协议转换由 `github.com/QuantumNous/new-api/relaykit` 承担，本项目没有重写它。
转换结果的诊断与用量直接使用它的类型：`types.ConversionDiagnostic` 与
`dto.Usage`。这是刻意的取舍：包装一层只会让调用方多绕一次，而信息量不变。

代价是 RelayKit 处于 v0.x，它的小版本调整会传导到本模块的公开面。
`go.mod` 精确锁定版本，升级由单独的 PR 完成，见 [AGENTS.md](../AGENTS.md) 的
「升级上游」一节。

需要图片或 data URL 解析时按上面的方式注册回调。
本项目不转发 `relayconvert` 的全部入口，只暴露 `relay.Engine`。

## 上游路径

`dialect.Name.UpstreamPath` 按方言与模型名拼出上游路径，由
[channel](../channel) 使用。四方言的路径形状写在
[dialect/dialect.go](../dialect/dialect.go) 里，只有 Gemini 需要模型名。
