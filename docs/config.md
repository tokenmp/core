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

defaults:
  max_body_bytes: 33554432   # 32 MiB，客户端请求体的字节上限
  default_max_tokens: 4096   # 源请求未带 max_tokens 时给 Claude 补的值

retry:
  in_channel: "408,429,500,502,503,504"   # 渠道内原地重试的状态码
  failover: "408,401,403,429,500-599"     # 换下一条候选的状态码

channels:
  - name: openrouter
    dialect: openai   # openai | openai_responses | claude | gemini
    base_url: https://openrouter.ai
    auth:
      style: bearer   # bearer | x-api-key | query | custom | none
      key_env: OPENROUTER_API_KEY
    timeout: 120s
    max_retries: 2
    backoff:
      base: 250ms
      max: 2s
      max_retry_after: 120s
    headers:
      X-Title: tokenmp
    # 从客户端转发的请求头白名单，只放行列出的名字。
    pass_headers:
      - anthropic-beta
      - x-stainless-lang
      - x-app

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
凭据来源有两种：单账号的渠道写 `key_env`，引用顶层账号的渠道写 `accounts`。
两者互斥，同一条渠道只能写一个；`auth.style` 不是 `none` 时至少要有一个，都没有时校验失败。

同一个模型可以有多条候选。配置顺序就是优先级，在前的规则是首选，只有它不可用时才依次降到后面的候选。
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
| `defaults.max_body_bytes` | `32MiB` | 客户端请求体的字节上限 |
| `defaults.default_max_tokens` | `4096` | 源请求未带 `max_tokens` 时给 Claude 补的值 |
| `retry.in_channel` | `408,429,500,502,503,504` | 渠道内原地重试的状态码集合 |
| `retry.failover` | `408,401,403,429,500-599` | 换下一条候选的状态码集合，必须包含 `retry.in_channel` |
| `channels[].failover_statuses` | 空（沿用 `retry.failover`） | 覆盖全局的换候选集合 |
| `accounts[].name` | 必填 | 账号名，渠道按它引用 |
| `accounts[].key_env` | 必填 | 凭据所在的环境变量名 |
| `accounts[].headers` | 空 | 账号自带的请求头，展开时并入渠道头 |
| `channels[].auth.accounts` | 空 | 引用的顶层账号名，与 `auth.key_env` 互斥 |
| `channels[].timeout` | `120s` | 单次上游请求的整体超时 |
| `channels[].max_retries` | `0` | 失败后的重试次数 |
| `channels[].backoff.base` | `250ms` | 第一次重试前的等待时长 |
| `channels[].backoff.max` | `2s` | 指数退避的上限 |
| `channels[].backoff.max_retry_after` | `120s` | 上游 `Retry-After` 的上限，超过就不再重试 |
| `channels[].path` | 按方言 | 覆盖上游路径 |
| `channels[].pass_headers` | 空 | 从客户端转发的请求头白名单，空表示一个都不转发 |
| `models[].affinity` | 空 | 渠道亲和的组名，留空表示不参与 |
| `plugins.dir` | `plugins` | 插件目录 |
| `plugins.list[].order` | `0` | 执行顺序，越小越先执行 |

配置路径写成 `channels[].timeout` 这样的形式，嵌套用点号连接，
列表在字段名后加 `[]` 再写元素字段；元素是标量的列表路径就到字段名为止。
`config.Keys()` 返回整份键表的路径、类型与默认值，
由 `Config` 的字段与 yaml tag 现算，用来生成帮助、模板与字段清单，
不必再各自抄一份。

`defaults` 段放进程级的行为默认值。两个键都在装配时读一次，之后不再变。
`max_body_bytes` 不写时是 32 MiB，超限请求以 413 拒绝。
`default_max_tokens` 不写时是 4096。
两个值都必须是正整数。

内核另有三个行为开关还没有出口：`tool_loss_policy`、`model_suffix`、
`thinking_adapter` 都没有对应的 YAML 键。
`model_suffix` 与 `thinking_adapter` 会改变哪个模型名参与匹配，
打开之后 `models[].upstream` 与 `${model}` 的语义跟着变，
要另写一节语义说明；`tool_loss_policy` 的取值口径也未定。
在它们有键之前，宿主可以在 Go 侧构造 `relay.Options` 注入内核。

`timeout` 写成 `120s` 这样的时长，也可以写成纯数字，按秒解释。

`max_retries` 只覆盖两类失败：请求没能建立，以及上游返回渠道内重试集合里的状态码。
集合默认是 `408,429,500,502,503,504`，由 `retry.in_channel` 给出。
取值上限是 10：转发是同步路径，重试次数乘以超时直接等于客户端等待时长。

换下一条候选的判据是另一组状态码，默认 `408,401,403,429,500-599`，由 `retry.failover` 给出；
渠道级 `failover_statuses` 覆盖全局值。两组都写成字符串，
每项要么是 100 到 599 的整数，要么是 `a-b` 且 `a <= b`。
`retry.failover` 必须包含 `retry.in_channel`：凡是在渠道内重试过的失败，
重试耗尽后还要能降级，否则会出现「重试了却不换候选」的死角。
不满足时加载失败，报错指到缺的那一项。
401 与 403 只在换候选一侧：它们说明凭据或权限有问题，同一条渠道原地重试不会变好。

渠道不认某个模型时，上游常用 400 或 404 表达。默认不把这两个纳入换候选集合，
因为「请求本身有问题」也走这两个码，纳入之后一次注定失败的请求会变成多次上游请求，
延迟与成本都上升。按自己的渠道实测确认之后，把 400 或 404 加进 `failover_statuses`：

```yaml
channels:
  - name: channel-a
    failover_statuses: "400,404,408,429,500-599"
```

上游请求数的上界是可算的：`(max_retries + 1) × 候选数`。它有限，因此不另加
`max_upstream_calls` 这类开关。

重试的等待取指数退避与上游 `Retry-After` 的较大者。
第 n 次重试等待 `base × 2^(n-1)`，超过 `max` 按 `max` 算；
429 与 503 若带 `Retry-After`，等待取两者中的较大值。
这个头两种写法都认：秒数与 HTTP 日期。缺它时退避与不配退避参数的版本逐条相同。

上游要求的等待超过 `max_retry_after` 时不再重试，把响应交给候选切换，并记一行 WARN。
不缩短成上限再重试，因为那会让请求早于上游允许的时刻发出，正是要避开的情况。
默认上限 120s，对齐 openai-python 的 `MAX_RETRY_AFTER_DELAY`。
`Retry-After` 照旧透传给客户端：客户端看到的是上游给的「什么时候再来」，
内核做的是「自己什么时候再试」。

退避不做抖动。抖动会改变同账号并发请求的重来时点，是另一个可观测的改动。

重试还会重发请求体。上游返回 5xx 时那个请求可能已经被处理了一半，
重发会让它被处理两次；只有在上游保证按请求幂等时才安全。

超时的分工要分开说。非流式请求的整体超时覆盖到响应体读完为止。
流式请求不加整体超时，只压到收到响应头，之后靠调用方的 context 结束。

`path` 的规则要单独说一句。
默认情况下上游 URL 是 `base_url` 加方言路径，例如
`https://api.openai.com` 加 `/v1/chat/completions`。
`base_url` 已经带路径时（比如 `https://openrouter.ai/api/v1`）必须显式给出
`path`，取值从该路径之后算起，否则校验失败。
Gemini 的路径含模型名，不支持覆盖。

`headers` 是渠道级的附加请求头，每个请求都带上。宿主在中间件里用
`server.WithUpstreamHeader` 设的同名头按请求覆盖它，覆盖后只发一条，其余渠道头
照旧；覆盖是按名字整个替换，宿主读不到这里的原值，想两个值都保留得自己拼。
鉴权头是例外，它由 `Auth` 最后写入，覆盖不了。一个名字只带一个值，名字按
`http.CanonicalHeaderKey` 归一后比较，重复字段的头表达不了。

## 客户端请求头

渠道构造上游请求时不碰客户端发来的请求头，`authorization`、`x-api-key` 这类
凭据因此不会漏给上游。但 `anthropic-beta`、`OpenAI-Beta`、`X-Codex-*` 这些头
按请求变化，静态的 `headers` 表达不了，漏掉它们会让上游按不同功能开关对待请求。
渠道级键 `pass_headers` 是一份白名单，名单里的头按请求跟上去，名单外的照旧不转发：

```yaml
channels:
  - name: anthropic
    dialect: claude
    base_url: https://api.anthropic.com
    pass_headers: [anthropic-beta, x-stainless-lang, x-app]
```

名单放在渠道级，不放在方言级。放行的信任边界是「这条上游渠道可以收到哪些客户端
头」，不是「客户端是什么方言」。按方言放行会把 Claude 客户端的特性开关带给一个
它没打算对话的 OpenAI 上游。名单为空时行为与没有这个键时逐条相同。

只能写白名单，不能反过写成黑名单：新出的协议头默认不转发，要放行得显式加名字。

这五个头永远不放行，写进 `pass_headers` 会在校验期报错，报错指到那一项：
`authorization`、`x-api-key`、`cookie`、`anthropic-workspace-id`、`content-type`。
前四个是凭据与计费归属，放行等于让客户端决定用谁的钱；`content-type` 会破坏
跨方言，请求体经过转换之后形状已经变了，客户端给的那个对不上。判断按
`http.CanonicalHeaderKey` 归一后的名字做，`Authorization` 与 `authorization`
一视同仁。要按请求给上游加 `anthropic-workspace-id`，用宿主侧的
`server.WithUpstreamHeader`，或写进顶层账号的 `headers`。

同名头的优先级从低到高是：客户端按名单转发来的头、渠道 `headers`、
宿主 `WithUpstreamHeader` 设的头。也就是越具体越晚写，宿主赢，渠道 `headers`
赢过客户端转发的同名头。转发按 `http.Header` 的多值语义走，名单里的头把客户端的
所有值都带上，`Cookie` 这类重复字段的头是合法写法。

两份可以直接复制的推荐名单，数据来自中转层 new-api 的按客户端透传清单。
Claude Code：

```yaml
pass_headers:
  - anthropic-beta
  - anthropic-version
  - anthropic-dangerous-direct-browser-access
  - x-app
  - user-agent
  - x-stainless-arch
  - x-stainless-lang
  - x-stainless-os
  - x-stainless-package-version
  - x-stainless-retry-count
  - x-stainless-runtime
  - x-stainless-runtime-version
  - x-stainless-timeout
```

Codex：

```yaml
pass_headers:
  - originator
  - session_id
  - thread_id
  - session-id
  - thread-id
  - x-client-request-id
  - user-agent
  - x-codex-beta-features
  - x-codex-turn-state
  - x-codex-turn-metadata
  - x-codex-window-id
  - x-codex-parent-thread-id
  - x-openai-subagent
  - x-openai-memgen-request
  - x-responsesapi-include-timing-metrics
  - x-openai-internal-codex-responses-lite
```

不要把 `accept-encoding` 放进名单。Go 只在它自己加这个头时才自动解压响应，显式
转发会让上游压缩，而流式读取拿到的是压缩字节。

两件事不在这个键的范围里。客户端的 query 串不转发，任意转发会把 `?api_key=`
这类凭据一起带上去，要做也得先定白名单口径。内核也不自己补
`anthropic-version` 这类「协议必需的头」，它已经能用渠道 `headers` 写死表达。

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
`model`、`channel`、`attempts`。上游给出用量时另有
`prompt_tokens`、`completion_tokens`、`total_tokens`，有缓存命中时加
`cached_tokens`，有缓存写入时加 `cache_write_tokens`。
`prompt_tokens` 已经含缓存读与缓存写，命中率的分母直接用它即可。
缓存读取自归一之后的 `prompt_tokens_details.cached_tokens`
或 `input_tokens_details.cached_tokens`，两个都有值时取较大者；
只把缓存读报在顶层 `prompt_cache_hit_tokens` 的上游先由转换层搬进 details，
两种写法在这里是同一个数。
缓存写把 Claude 的 `cached_creation_tokens` 与 OpenAI 的
`cache_write_tokens` 合并成一个数，两者都有值时同样取较大者。

同方言的流式透传也记用量：内核在按字节转发的旁路里只看 OpenAI 系两种方言分片的
`data`，命中 `usage` 才解析。Responses 把同一个数报在 `input_tokens` 与
`output_tokens` 上，记账时与 Chat 那套字段合成一组数。
同方言的非流式透传也记用量：响应字节原样返回，但先用子串 `usage`（Gemini 为
`usageMetadata`）预筛，命中才解码取用量。解码失败时不记用量，请求照常成功。

`method`、`path`、`user_agent` 与 `model` 的值来自请求，超过 256 字节时截断，
末尾的省略号算在这 256 字节之内。`attempts` 是这条请求试过的候选条数，
转换失败与连接失败都算一次，`model` 是插件改写之后的模型名。

`attempts` 只数候选条数，一条候选内部的重试不在其中。按
`channels[].max_retries` 重试时，每次重试另记一行 DEBUG，字段是 `retry`
（第几次重试，从 1 开始）、`status`（触发重试的上游状态码）与 `wait`
（这次重试之前等待的时长）；次数用尽时再记一行 DEBUG `渠道内重试次数已用尽`。
这些行只在 `log.level: debug` 下出现，`max_retries` 为 0 时一行都不写。

`duration` 是 slog 的时长属性：`text` 格式下形如 `12.3ms`，`json` 下是纳秒整数。

这个值的精度就是平台单调时钟的粒度，不另行记录：Linux 上是纳秒，
Windows 上只有系统时钟中断的粒度，通常 0.5 到 15 毫秒。
粗粒度平台上短于一个刻度的等待量出来是 `0`：`0` 只说明这一次没测出来，
不代表请求瞬时完成、也不代表请求失败。`Outcome` 的几个耗时字段取自同一个时间源，
精度同理；要分辨「整个请求没有写出过响应体」，看同一个 `Outcome` 里的
`BytesWritten` 是不是 0。

宿主要在每次请求结束时记自己的指标，用 `server.WithRequestDone` 拿原始量，
不必解析这两行。要整个接管记录就用 `Options.Middleware`、`Options.DisableAccessLog`
与 `Options.OnRequestDone`（回调才是拿到内核数据的入口），详见
[README](https://github.com/tokenmp/core#readme)。

## 校验

`Validate` 一次报出全部问题，不是发现第一条就返回。
它检查日志取值、渠道名重复、方言是否已知、基址协议、超时与重试数量、
重试判据的取值范围与包含关系、鉴权字段是否齐全、账号名与账号引用是否合法、
`pass_headers` 里有没有禁止放行的项、模型规则是否引用了存在的渠道、
`upstream` 里的占位符写法。

`pass_headers` 的头名判断分两层：加载期只挡明显写错的（空项、含冒号或空白），
严格的 RFC 7230 token 判断在装配时做，不合格时服务启动失败。

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

`Load` 在 `Parse` 的基础上把相对路径归一成绝对路径。
`Parse` 从 `io.Reader` 读取，适合测试与嵌入场景，依次做五件事：
解码、填默认值、校验、调 `Config.Expand` 展开顶层账号、再校验一次。
第二次校验跑在平铺形态上，用来抓展开名与手写渠道名撞车这类展开之后才出现的重名。
`Expand` 返回展开后的副本，原对象不改，重复调用也不会把候选数按账号数相乘。

`Parse` 的解码开启 `KnownFields`，配置里出现二进制不认识的键会直接报错，
而不是被忽略。因此回滚到旧版本之前，要先删掉旧版本不认识的键，
否则新配置配旧二进制会启动失败。

## 路由解析

`Config.Resolve` 返回该模型名的候选路由，按配置顺序排列。
命中多条时全部返回，调用方按顺序尝试。没有命中时返回空切片。

通配符只有两个：`*` 匹配任意长度的一段（含 `/` 与 `:`），`?` 匹配一个字符。
`openai/*` 因此能匹配 `openai/gpt-5`。

两条规则同时命中时，候选顺序就是配置顺序，通配规则不会因为范围更大而被排到后面。
下面的配置把通配规则写在前面，`Config.Resolve("model-x")` 返回
`[channel-a(prefix/model-x), channel-b]`，首选是 `channel-a`：

```yaml
models:
  - match: "model-*"
    channel: channel-a
    upstream: "prefix/${model}"
  - match: model-x
    channel: channel-b
```

这两条规则看起来像「通用加例外」，实际语义是「主加备」。
`model-x` 同时命中通配规则与具体规则，通配规则排在前面，因此具体规则不是首选，
只在通配规则那条渠道失败之后才被用到。要让具体规则成为首选，把它写在通配规则前面：

```yaml
models:
  - match: model-x
    channel: channel-b
  - match: "model-*"
    channel: channel-a
    upstream: "prefix/${model}"
```

此时 `Config.Resolve("model-x")` 的候选顺序与上例相反，具体规则那条在前：

`[channel-b(model-x), channel-a(prefix/model-x)]`

顺序即降级链，所以「具体规则写在前面」不是可选的风格问题，而是这条路由能不能按预期生效的前提。

一条 `models` 规则同时是「这条渠道服务这些模型」的声明。
上游实际不认某个模型名时，core 读不到上游的模型清单，配置加载与请求过程都不会发现。
命中通配规则的模型名都会送到那条通配规则指的渠道，因此顺序写错不会在本地报错，
只能在请求发到上游之后暴露出来，只能靠把具体规则写在通配规则前面来避免。

配了 `affinity` 且同组候选不止一条时，候选顺序还会被亲和重排，规则见「渠道亲和」。
同组候选在池内按会话标识定序，池外的候选保持配置顺序，因此命令行按
`Config.Resolve` 试算出的「顺序 1」不必然等于实际首选。

## 顶层账号

`accounts` 是顶层的账号清单，每条描述一份凭据。`name` 是账号名，供渠道引用；
`key_env` 是凭据所在的环境变量名，`headers` 是账号自带的请求头，可以不写。

渠道用 `auth.accounts` 按名字引用账号，写成一个列表。
它与 `auth.key_env` 互斥：单账号直接写 `key_env`，引用账号的写 `accounts`。
`auth.style` 留在渠道上，因为同一个凭据字符串在上游怎么用由上游协议决定。

```yaml
accounts:
  - name: team-a
    key_env: ANTHROPIC_KEY_A
  - name: team-b
    key_env: ANTHROPIC_KEY_B
    headers:
      anthropic-workspace-id: ws-2

channels:
  - name: anthropic
    dialect: claude
    base_url: https://api.anthropic.com
    auth:
      style: x-api-key
      key_env: ANTHROPIC_KEY_A

  - name: anthropic-pool
    dialect: claude
    base_url: https://api.anthropic.com
    auth:
      style: x-api-key
      accounts: [team-a, team-b]

models:
  - match: "claude-*"
    channel: anthropic-pool
    affinity: anthropic-team
```

展开在 `config.Parse` 里做，顺序是先填默认值、校验原始写法、展开、再校验一次平铺形态。
引用了 N 个账号的渠道展开成 N 条渠道，名字是 `<渠道名>/<账号名>`。
模型规则引用这样的渠道时，那条规则就地展开成 N 条候选，顺序照 `auth.accounts` 的声明序。
展开之后 `Config.Resolve`、`server` 与 `channel` 都按平铺形态工作，不感知账号层。

展开之后 `Config` 里不再有账号层：顶层 `accounts` 与渠道的 `auth.accounts` 都不保留，
每条渠道只留自己的 `key_env`，因此 `Load` 出来的配置可以直接写回文件再次加载。
程序化构造带账号的配置之后要调一次 `config.Config.Expand`。
展开只在那里发生，`Validate` 不会替调用方做。

展开时把账号的 `headers` 并入渠道的 `headers`，同名时账号覆盖渠道，
展开出的多条渠道各自持有一份头，互不影响。
口径是越具体的越晚写，与 `server.WithUpstreamHeader` 按请求覆盖渠道头一致。

校验期会报出四类账号问题：

- 账号名为空，或两条账号同名。
- `key_env` 为空。
- `auth.accounts` 里写了不存在的账号名。
- `auth.accounts` 与 `auth.key_env` 同时写。

没人引用的账号不算错。账号与引用它的渠道写在同一份文件里，先建账号、再让渠道引用
它是自然顺序，中间那个「账号还在、没人引用」的状态必须能加载，否则一次只改一处的
工具就没法把这件事做完。没人引用的账号不参与展开，运行时什么都不影响。

`key_env` 对应的环境变量缺失会让服务启动失败，
与「启用了但目录里不存在的插件让启动失败」是同一条口径。
账号没有 `enabled` 字段，停用一个账号要先把账号名从引用它的每条 `auth.accounts` 里删掉，
再删顶层 `accounts` 里的那一行；只删顶层那一行会报「引用了不存在的账号」。

同一段会话会粘在同一个展开出来的渠道上，也就是粘在同一份凭据上。
上游缓存的隔离范围见「亲和的边界」：放进同一个亲和池的顶层账号应当同属一个上游 workspace。
否则亲和换来的是跨 workspace 的缓存丢失，收益为负。

一条模型规则展开成 N 条候选之后，重试的等待也会相乘。
客户端等待的时间上限是候选条数乘以 (1 + `max_retries`) 再乘以 `timeout`。
账号多的时候应当把每条的 `max_retries` 压到 0 或 1，靠换账号兜底，而不是让同一个账号反复重试。

## 渠道亲和

同一个模型配了多条候选渠道时，可以让同一段会话的连续请求粘在同一个渠道上。
上游的提示缓存按 workspace 或 organization 隔离，会话换渠道就意味着缓存要重建，粘住能省钱。

组名相同的候选组成一个池，池里的渠道被当作可互换的：

```yaml
models:
  - {match: "claude-*", channel: account-a, affinity: team}
  - {match: "claude-*", channel: account-b, affinity: team}
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
但覆盖的状态码由换候选集合决定，默认是连接错误、408、401、403、429 与 5xx。
上游返回 400、404、422 时默认不会换渠道，因为那说明请求本身有问题，
换渠道也不会变好；把 400 加进渠道的 `failover_statuses` 可以改这条默认。

三件要知道的限制：

- 上游缓存按 workspace 或 organization 隔离。候选渠道属于同一个账号、
  或引用同一个顶层账号时，它们本来就共享缓存，此时亲和只是多了一层分配，不带来额外收益。
- 上游的机器落点不归 core 管。OpenAI 的文档写明缓存状态在单台机器上，
  每分钟超过 15 个请求会触发 overflow routing，即使渠道固定也可能落空。
- 请求头里的标识只在本进程内使用，不会转发给上游。
  从 `prompt_cache_key` 取的标识属于请求体，OpenAI 转 OpenAI 时会原样送上去。
