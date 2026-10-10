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
`upstream` 不写就沿用客户端请求的模型名；写了就用配置里的名字，
上游收到的顶层 `model` 字段随之改写。
`dialect` 可以在模型这一层覆盖渠道方言，用于同一个基址上的多种协议。

`upstream` 可以写一个 `${model}` 占位符，取值是客户端请求里、经插件改写
之后真正参与匹配的那个模型名。加前缀写在规则旁边即可：

```yaml
models:
  - match: "deepseek-*"
    channel: deepseek
    upstream: "deepseek/${model}"
```

`$` 只在紧跟 `{` 时才有含义。`$$` 是两个普通美元符号，
单独的 `}` 也没有特殊含义。`${` 开了头却没有配对的 `}`，
或配对了但名字不是 `model`，都在校验时报错，并指出是第几条规则。

不做捕获占位符：`${1}` 这类写法一律按未知占位符报错。
要剥掉名字里的一段，用多条规则表达。只做 `${model}` 是有意的取舍，
捕获要多定 `?` 是否也捕获、多个 `*` 怎么编号、`*` 匹配空串时取值、
以及字面 `${` 怎么转义这四件事。任何一件没定清，
配置读起来就会有两种解释。

相对路径以配置文件所在目录为基准。
`Load` 会把插件目录解析成绝对路径，因此工作目录变化不会改变行为。

## 字段

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | 监听地址 |
| `log.format` | `json` | 日志格式 |
| `log.level` | `info` | 日志级别，同时控制访问日志 |
| `channels[].timeout` | `120s` | 单次上游请求的整体超时 |
| `channels[].max_retries` | `0` | 失败后的重试次数 |
| `channels[].path` | 按方言 | 覆盖上游路径 |
| `models[].affinity` | 空 | 渠道亲和的组名，留空表示不参与 |
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

`headers` 是渠道级的附加请求头，每个请求都带上，与 `Auth` 一起构成默认值。
宿主在中间件里用 `server.WithUpstreamHeader` 设的同名头按请求覆盖它，其余渠道头
照旧。客户端自带的请求头不会被转发，只有宿主在这里明确设置的头才会上游。

## 访问日志

每个进入服务的请求留两行，一行记请求进入，一行记请求完成，
与两端方言是否相同、上游有没有给出用量都无关。

进入行在请求进入路由之前写出，恒记 INFO，字段是
`request_id`、`method`、`path`、`client`、`user_agent`。
`client` 是去掉端口之后的客户端地址，`user_agent` 是请求头里的 `User-Agent`；
这两个字段只有进入行有，完成行不再重复。

完成行在处理器返回之后写出。级别口径按状态码定，默认的 `log.level` 是 `info`。
状态码小于 400 记 INFO，因此服务一跑起来就能看见每个请求，
包括客户端与渠道同方言的透传这条最常见的路径；状态码 400 及以上记 WARN，
与既有的失败日志同级，一起由 `log.level` 控制。

失败路径上原有的 WARN 行保留，与进入行、完成行共享同一个 `request_id`，
一次请求的几条日志靠它串起来。

完成行的字段有 `request_id`、`method`、`path`、`status`、`duration`、
`model`、`channel`、`attempts`。跨方言转发且上游给出用量时另有
`prompt_tokens`、`completion_tokens`、`total_tokens`，有缓存命中时加
`cached_tokens`，有缓存写入时加 `cache_write_tokens`。
`prompt_tokens` 已经含缓存读与缓存写，命中率的分母直接用它即可。
缓存读取自归一之后的 `prompt_tokens_details.cached_tokens`
或 `input_tokens_details.cached_tokens`，两个都有值时取较大者；
只把缓存读报在顶层 `prompt_cache_hit_tokens` 的上游先由转换层搬进 details，
两种写法在这里是同一个数。
缓存写把 Claude 的 `cached_creation_tokens` 与 OpenAI 的
`cache_write_tokens` 合并成一个数，两者都有值时同样取较大者；
同方言透传时原样转发、不解析用量，所以完成行里也就不会有这些 token 字段。

`method`、`path`、`user_agent` 与 `model` 的值来自请求，超过 256 字节时截断，
末尾的省略号算在这 256 字节之内。`attempts` 是这条请求试过的候选条数，
转换失败与连接失败都算一次，`model` 是插件改写之后的模型名。

`duration` 是 slog 的时长属性：`text` 格式下形如 `12.3ms`，`json` 下是纳秒整数。

宿主要在每次请求结束时记自己的指标，用 `server.WithRequestDone` 拿原始量，
不必解析这两行。要整个接管记录就用 `Options.Middleware`、`Options.DisableAccessLog`
与 `Options.OnRequestDone`（回调才是拿到内核数据的入口），详见
[README](https://github.com/tokenmp/core#readme)。

## 校验

`Validate` 一次报出全部问题，不是发现第一条就返回。
它检查日志取值、渠道名重复、方言是否已知、基址协议、超时与重试数量、
鉴权字段是否齐全、模型规则是否引用了存在的渠道、`upstream` 里的占位符写法。

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

## 渠道亲和

同一个模型配了多条候选渠道时，可以让同一段会话的连续请求粘在同一个渠道上。
上游的提示缓存按账号隔离，会话换渠道就意味着缓存要重建，粘住能省钱。

组名相同的候选组成一个池，池里的渠道被当作可互换的：

```yaml
models:
  - {match: "claude-*", channel: account-a, affinity: accounts}
  - {match: "claude-*", channel: account-b, affinity: accounts}
  - {match: "*",        channel: fallback}
```

请求先在池里按会话标识选一条，池外的候选保持配置顺序，排在池后面兜底。
上例里 `fallback` 没有写组名，因此它只当降级路径，不会被选成首选。
池里只有一个成员时亲和不起作用，行为等于配置顺序。

会话标识按下面的顺序取，取到第一个非空的：

1. 请求头 `X-TokenMP-Session`。
2. 请求体里的 `prompt_cache_key`，只有 OpenAI 的两个方言有这个字段。
3. 都没有时看请求内容，认出上一轮发过的前缀。

前两条是纯函数，同一个标识永远算到同一个渠道，进程重启不变，
多实例部署也一致。

## 前缀链兜底

前两条都没拿到时，core 不看标识，改看内容。它把请求切成一条条目序列，
算出累积指纹，因此上一轮的整条链就是这一轮的链的前缀。
查「上一轮走了哪个渠道」不需要知道上一轮有几个条目，
从末尾往回查第一个命中的下标即可。

这条路径需要进程内记住「前缀对应哪条渠道」，因此它有三件事要交代：

- 记忆是本进程的。多实例部署时不同实例记的不一样，客户端带了标识才跨实例一致。
- 记的是实际服务而且成功的那条渠道。粘住的渠道限流降级之后，下一轮沿用真正
  写下了缓存的那条；上游返回 400 这类失败不记，否则这段会话会每轮都先撞坏渠道。
- 会话冷掉之后记忆会随 `ttl` 过期，重新分配。这正好对应上游缓存也冷了。

切分的方式是「工具定义加系统提示」作为第一个条目，后面依次是对话条目。
同一个工具调用在四种方言里占的条目数不一样，Responses 把 `function_call` 与
`function_call_output` 摆在顶层，Claude 与 Gemini 把多个块装进同一条，
所以不能靠「减掉尾部固定条数」实现，只能靠前缀关系。

## 亲和参数

三个参数都可选，不写就用默认值。

```yaml
affinity:
  table_size: 8192
  ttl: 10m
  min_prefix_bytes: 4096
```

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `affinity.table_size` | `8192` | 前缀链记忆的条数上限，不大于零时前缀链兜底关闭 |
| `affinity.ttl` | `10m` | 记忆的寿命，命中与写入都会续期；不写或写 `0` 都用默认值 |
| `affinity.min_prefix_bytes` | `4096` | 前缀的字节下限，显式写成 `0` 表示不判断 |

`ttl` 写 `0` 等于不写。要关掉前缀链兜底请用 `affinity.table_size: 0`。

`min_prefix_bytes` 是个启发式。上游对可缓存的最小长度有硬门槛，
按模型从 512 到 4096 tokens 不等，短到进不了缓存时粘住只剩负载集中的坏处。
core 数不了 token，只能按字节估，中英文混排下 4096 字节约合一到两千 tokens。
模型门槛高的时候应当调大它。

## 亲和的边界

亲和是偏好而不是钉子。粘住的渠道失败时后面的候选照旧兜底，
但只覆盖连接错误、408、401、403、429 与 5xx 这几种。
上游返回 400、404、422 时不会换渠道，因为那说明请求本身有问题，
换渠道也不会变好。

三件要知道的限制：

- 上游缓存按 workspace 或 organization 隔离。候选渠道属于同一个账号时，
  它们本来就共享缓存，此时亲和只是多了一层分配，不带来额外收益。
- 上游的机器落点不归 core 管。OpenAI 的文档写明缓存状态在单台机器上，
  每分钟超过 15 个请求会触发 overflow routing，即使渠道固定也可能落空。
- 请求头里的标识只在本进程内使用，不会转发给上游。
  从 `prompt_cache_key` 取的标识属于请求体，OpenAI 转 OpenAI 时会原样送上去。
