# 插件

`github.com/tokenmp/core/plugin` 是插件宿主。
它编译本地 JavaScript 插件，在四个时机调用它们，并对执行时长、并发、源码与负载
大小设限。

插件契约见 [v1.d.ts](../plugin-api/v1/v1.d.ts) 与
[v1.schema.json](../plugin-api/v1/v1.schema.json)，
运行时实现见 [host.go](../plugin/host.go)。

## 目录布局

```
plugins/
  echo/
    plugin.js      单文件同步 ECMAScript 模块
    icon.svg       可选，宿主不读取
```

也接受平铺写法 `plugins/<key>.js`。
名字与 `meta.key` 必须一致，不一致时加载失败。

启用状态与执行顺序写在配置文件的 `plugins.list` 段里，插件目录只放插件本身。

## 一个插件

```js
export const meta = {
  key: "echo",
  name: "回显",
  version: "0.1.0",
  apiVersion: "v1",
};

export function onRequest(input) {
  input.body.messages.push({
    role: "system",
    content: "tag=" + host.config.tag,
  });
  return { body: input.body };
}

export function onResponse(input) {
  input.body.plugin = host.plugin.key;
  return { body: input.body };
}
```

`meta` 必填，四个钩子按需导出。
钩子返回 `null` 或 `undefined` 表示不改写，宿主沿用前一个插件的输出。

## 四个钩子

| 钩子 | 时机 | 输入 | 输出 |
| --- | --- | --- | --- |
| `onRequest` | 选路之前 | `{dialect, model, stream, body}` | `{model?, body?}` |
| `onResponse` | 非流式响应写回之前 | `{dialect, model, body}` | `{body?}` |
| `onEvent` | 每条流式事件写回之前 | `{dialect, event, data}` | `{event?, data?}` |
| `onStreamEnd` | 流结束、终止帧写出之前 | `{dialect, model, usage, events}` | `{events?}` |

三条边界要写清楚。

`onRequest` 在选路之前执行，此时还没有渠道，因此它不能按渠道限定。
它改写 `model` 会改变后续选路。

多个插件同时启用时，下一个插件拿到的是上一个插件改写之后的完整负载，
而不是只拿到它返回的那几个字段，因此 `dialect`、`model`、`stream` 始终可读。

`onEvent` 在同方言透传与跨方言转换两条路径上都执行。
没有插件订阅它时，透传路径按字节搬运，一个字节都不会被改写。

`onStreamEnd` 只在流式转换路径上执行。
它拿到的是收尾阶段要补发的分片，返回新的分片列表即替换。

## 宿主提供的能力

模块内可以用一个全局变量 `host`：

| 成员 | 说明 |
| --- | --- |
| `host.config` | 配置里该插件的 `config` 段，未配置时是空对象 |
| `host.plugin` | `{key, name, version, apiVersion}` |
| `host.now()` | Unix 秒。fixture 回放时是固定值 |
| `host.log(message)` | 写一条宿主日志 |

宿主刻意不提供网络请求、文件系统、`require`、动态 `import`、`eval`、
`Function` 构造器与进程环境变量。
需要凭据或外部地址的插件应当把它们写进自己的 `config` 段。

## 限额

| 限额 | 默认值 | 含义 |
| --- | --- | --- |
| 源码 | 1 MiB | 单个插件文件的字节上限 |
| 负载 | 8 MiB | 单次调用的输入与输出上限 |
| 超时 | 50 ms | 单次调用的上限，超时中断插件 |
| 并发 | 8 | 同一插件同时执行的钩子数 |

超时取 50 毫秒：本地改写的正常耗时在微秒级，超过这个量级的计算应当在转发路径
之外完成。钩子必须是同步函数，返回 Promise 会被拒绝。

超时同时覆盖模块求值。每次调用都在全新的运行环境里求值一次模块，顶层代码因此
每次都会重跑，加载与检查插件时也会执行一次。把顶层逻辑写成一次性准备工作会拖慢
每个请求，写到死循环则连插件都加载不了。

宿主不限制插件的内存占用：moejs 没有提供这个开关。限额拦的是失控的插件，
拦不住故意吃内存的插件，见下文的「安全边界」。

## 失败语义

插件失败不影响转发可用性：

- `onRequest` 该插件的改写作废，客户端原始报文照常转发；链上前面插件的改写保留。
- `onEvent` 该条事件原样写出。
- `onStreamEnd` 不补发插件给出的分片。
- `onResponse` 原样写出上游响应。

失败记进日志，也记在 `Store.Apply` 返回的 `Outcome.Failures` 里。

## 检查与回放

```go
issues := plugin.LintSource("plugins/echo/plugin.js", source, plugin.DefaultLimits())
```

`LintSource` 返回致命结论与提示两类。
致命结论让插件不可用，比如语法错误、缺少 `meta`、契约版本不支持、钩子不是函数。
提示只说事实，比如源码里出现了 `fetch(`，以及宿主不提供网络访问。

回放沿用 new-api 任务插件的 fixture 形状：

```json
{
  "unixNow": 1735689600,
  "cases": [
    {
      "name": "写入插件标记",
      "hook": "onRequest",
      "args": [{"body": {"model": "m", "messages": [{"role": "user", "content": "你好"}]}}],
      "path": "body.messages.1.content",
      "expected": "tag=local"
    },
    {
      "name": "坏输入",
      "hook": "onRequest",
      "args": [{}],
      "expectedError": "messages"
    }
  ]
}
```

一条用例有 `name`、`hook`、互斥的 `member` 与 `path`、`args` 数组，以及互斥的
`expected` 与 `expectedError`。
`unixNow` 固定 `host.now()`，让签名与时间戳类钩子可复现。

钩子返回空值表示不改写，回放时按 `null` 比较 —— 这与转发路径上「不改写就用原值」
的语义不同，回放关心的是插件自己的返回值。

```go
results := instance.Replay(fixture, logger)
```

## 安全边界

加载插件等同于管理员级信任决定，与新配一个渠道凭据同级。
插件在转发进程内执行，能读到流经它的请求与响应内容。

限额拦的是失控的插件，拦不住恶意的插件。
发布或分发插件前请把它们当作可信代码审阅。
