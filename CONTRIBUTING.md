# 参与开发

## 环境

工具链版本写在 `.mise.toml` 里，本机、CI 与容器共用同一份声明。

```
make tools      # mise install，装齐 go 与 golangci-lint
```

## 门禁

```
make check
```

这个目标依次跑四件事，任意一件失败即非零退出：

```
go build ./...          编译检查
go test -race ./...     单测，带竞态检测
golangci-lint run       静态检查
golangci-lint fmt --diff 格式检查
```

`make check` 只依赖 Go 与 golangci-lint。
它不需要网络、数据库或 node，因此任何改动都能在没有外部服务的机器上验证。

提交前请在本地跑一遍。CI 的 `check` job 跑的是同一个目标。

## 测试约定

新增行为要带测试。三类测试各有分工：

- 转换矩阵在 `relay/engine_test.go` 里遍历全部方言组合。
  改转换逻辑时先看这张矩阵是否仍然全绿，再看快照是否需要更新。
- 插件与配置的测试覆盖失败路径：超时、异步钩子、超限、加载不到的插件。
- `server` 的测试用 `httptest` 起进程内假上游，断言上游收到的字节与客户端收到的字节。
  不要在这类测试里访问真实网络。

## 分支与提交

不直接推送 `main`，分支保护已开启。一切改动走分支加 PR。

提交信息用 Conventional Commits：

```
<type>(<scope>): <描述>
```

常见的 `type`：

| type | 用途 |
| --- | --- |
| `feat` | 新增功能 |
| `fix` | 修复缺陷 |
| `docs` | 只改文档 |
| `refactor` | 不改行为的重构 |
| `test` | 只改测试 |
| `chore` | 依赖、构建、工具链 |

`scope` 用包名，比如 `relay`、`plugin`、`server`。

描述写做了什么，不写怎么做的。合并用 squash，PR 标题即 `main` 上的提交信息。

## 发布

本仓库是库，按语义化版本打 tag，形如 `v0.1.0`。
`go.mod` 的模块路径不带主版本后缀。

发布前确认三件事：`make check` 全绿，CHANGELOG 无遗留条目，
公开面的变更已经写进 release notes。

## 许可

本项目按 AGPL-3.0 发布。
新增源文件不需要逐个加许可头，仓库根目录的 [LICENSE](LICENSE) 覆盖全部内容。

引入新依赖前先确认它的许可与 AGPL-3.0 兼容，并把条目加进
[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md)。
