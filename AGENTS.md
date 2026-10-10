# AGENTS.md

在本仓库工作的规则。

## 项目

TokenMP 的转发内核，Go 库，模块 `github.com/tokenmp/core`。
本仓库不产出可执行文件。

```
dialect/            四种方言的标识、路径与编解码
relay/              转换内核，纯函数式，不发网络请求
channel/            上游渠道客户端与 SSE 读写
plugin/             插件宿主
config/             配置模型、加载与校验
server/             HTTP 服务与装配
plugin-api/v1/      插件接口定义
docs/               转换、插件、配置三份规范
examples/           可直接复制的一份配置与一个插件
```

## 命令

```
make check      # 门禁：编译、单测（-race）、静态检查、格式检查
make build      # 只做编译检查
make test       # 单测
make lint       # 静态检查
make fmt        # 格式化
make tools      # 按 .mise.toml 装齐工具链
```

改动后必须跑 `make check` 并保持全绿。该目标只依赖 Go 与 golangci-lint。

## 公开面

`dialect`、`relay`、`channel`、`plugin`、`config`、`server` 六个包可以被外部导入，
其余都不存在。新增公开包要在本文里登记，并说明它为什么不能留在内部。

公开面一旦发布就是兼容承诺。改公开函数的签名、删字段、改默认值都算破坏性变更，
需要新的 tag 与升级说明。

## 分层红线

依赖必须单向向下：

- `dialect` 不依赖本仓库任何包。
- `relay` 只依赖 `dialect`。
- `channel` 只依赖 `dialect`。
- `plugin` 不依赖本仓库任何包，只依赖 moejs。
- `config` 只依赖 `dialect`。
- `server` 依赖其余全部，别的包不依赖 `server`。

四条更细的红线：

- 转换内核不发网络请求、不读配置、不碰插件。它只做字节到字节的转换。
- 渠道客户端不解析协议内容。方言之间的转换全部在 `relay`。
- 配置只描述事实，不构造渠道与插件。装配只在 `server`。
- 插件宿主不感知 HTTP。它只认 JSON 负载与钩子名。

## 约定

- 注释与用户可见文案使用中文。
- 注释写取舍与原因，不复述代码在做什么。
- 错误必须处理，`context` 随请求传递，响应体必须关闭 —— 相关 linter 已开启。
- 工具链版本只在 `.mise.toml` 中声明，不在别处硬编码版本号。
- 依赖只加必需的。转换内核由 RelayKit 承担，不要在这里重写协议转换。
- 新增依赖要写清它替代了什么，以及为什么不能自己写或不用。

## 测试

- 转换矩阵由 `relay/engine_test.go` 遍历全部方言组合，请求、响应、流式三条路径各一遍。
  改转换行为后这张矩阵必须仍然全绿。
- `plugin/examples_test.go` 与 `config/config_test.go` 会跑 `examples/` 下的配置与插件，
  改了示例必须让测试继续通过。
- `server` 的测试用进程内假上游，不依赖真实网络。

## 文档

仓库内容默认公开，文档按职责分流：

| 文件 | 职责 |
| --- | --- |
| `README.md` | 项目定位、包清单、快速开始、许可 |
| `CONTRIBUTING.md` | 开发门禁与提交规范 |
| `AGENTS.md` | 本文件：工作规则 |
| `docs/` | 转换、插件、配置三份规范 |
| `plugin-api/v1/` | 插件接口定义，改动等同破坏性变更 |
| `NOTICE` | 上游版权声明 |
| `THIRD-PARTY-LICENSES.md` | 依赖清单与许可 |

规则：

- **不写本机与环境特定信息**：账号名、主机名、本机路径、登录状态、密钥、本地配置。
  文档描述仓库本身，不描述某台机器或某次环境。
- 只陈述仓库中已经存在的事实。不写计划、进度、待办清单、设计推演或决策记录。
- 功能先落地，再更新文档；包、命令、行为的变更与文档同步。
- 实现取舍写进对应源文件的注释，不写进公开文档。
- 文案与提交信息不使用对话式指代（你 / 我 / 咱们），只陈述代码与事实。
- 文档里的行内代码块与表格不受行长限制，正文每行控制在 40 到 60 个字。

## GitHub 协作

`gh` CLI 操作 Issue 与 PR，不经网页手工编辑：

- **不直接推送 `main`**：分支保护已开启，一切改动走分支。
- 取任务：`gh issue list` / `gh issue view <n>`；
  新建：`gh issue create -t "<type>: <标题>" -b "<现象、期望、验收条件>"`
- 实现走分支：`git checkout -b <type>/<短描述>`，完成后 `gh pr create`，
  标题符合 Conventional Commits，正文写 `Closes #<n>`
- 合并用 squash：`gh pr merge --squash --delete-branch`
- 合并前置：`make check` 全绿，CI 与 PR 标题检查通过，评审无未解决意见
- 不 force push `main`，不删除或改写已发布 tag，不批量关闭无关 Issue

## 提交

使用 Conventional Commits（`<type>(<scope>): <描述>`），完整规则见
[CONTRIBUTING.md](CONTRIBUTING.md)。

## 升级上游

RelayKit 与 moejs 的版本在 `go.mod` 里精确锁定，不用 `latest`。

升级 RelayKit 单独一个 PR，并跑一遍转换矩阵与 `make check`。
转换矩阵的快照变化说明协议输出变了，要么是预期行为，要么是上游引入的回归，
两种都要在 PR 里说清。

moejs 处于 alpha，升级同样单独一个 PR，并重点跑插件层的超时、异步拒绝与限额用例。
