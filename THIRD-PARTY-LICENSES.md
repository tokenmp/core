# 第三方许可

本仓库的依赖清单。版本以 `go.mod` 为准。

## 运行时依赖

这些模块会被编译进本模块，使用者按依赖关系装取。

| 模块 | 版本 | 许可 |
| --- | --- | --- |
| github.com/QuantumNous/new-api/relaykit | v0.3.0 | AGPL-3.0 |
| github.com/Calcium-Ion/moejs | v0.1.0-alpha.6 | Apache-2.0 |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/samber/lo | v1.53.0 | MIT |
| github.com/tidwall/gjson | v1.19.0 | MIT |
| github.com/tidwall/match | v1.1.1 | MIT |
| github.com/tidwall/pretty | v1.2.0 | MIT |
| github.com/tidwall/sjson | v1.2.5 | MIT |
| golang.org/x/text | v0.35.0 | BSD-3-Clause |
| gopkg.in/yaml.v3 | v3.0.1 | MIT 与 Apache-2.0 双许可 |

`github.com/tidwall/sjson` 是直接依赖：[relay](relay) 用它把上游请求报文
顶层的 `model` 字段做逐字节定点改写。只改一个字段而不重建报文，
未建模字段与多余空白才能原样保留。`gjson`、`match`、`pretty` 是它拉进来的
传递依赖，因此仍标为间接。

## 只出现在 go.mod 里的测试依赖

`go.mod` 还记着 `github.com/kr/text v0.2.0`。它不属于本模块的编译图，
`go list -deps ./...` 里找不到它。它被记下来是因为 `gopkg.in/yaml.v3` 自己的测试
依赖链要经过它，而 go 的模块裁剪会把它算进 `go.mod`。许可是 MIT，
版权归 Keith Rarick（2012）。

`go list -m all` 还会列出更多模块，那些是上面这些模块自己的测试与工具依赖，
使用者装取本模块时不会被下载，因此不在这里逐条登记。

## 版权行

```
github.com/QuantumNous/new-api/relaykit
  Copyright (c) new-api 项目及其贡献者
  GNU Affero General Public License v3.0

github.com/Calcium-Ion/moejs
  Copyright (c) Calcium-Ion 及 moejs 项目贡献者
  Apache License 2.0

github.com/google/uuid
  Copyright (c) 2009, 2014 Google Inc.
  BSD 3-Clause License

github.com/samber/lo
  Copyright (c) 2022-2025 Samuel Berthe
  MIT License

github.com/tidwall/gjson, github.com/tidwall/sjson
  Copyright (c) 2016 Josh Baker
  MIT License

github.com/tidwall/match
  Copyright (c) 2016 Josh Baker
  MIT License

github.com/tidwall/pretty
  Copyright (c) 2017 Josh Baker
  MIT License

golang.org/x/text
  Copyright 2009 The Go Authors.
  BSD 3-Clause License

gopkg.in/yaml.v3
  Copyright (c) 2006-2011 Kirill Simonov
  Copyright (c) 2011-2019 Canonical Ltd
  MIT License 与 Apache License 2.0 双许可

github.com/kr/text
  Copyright 2012 Keith Rarick
  MIT License
```

## 说明

RelayKit 按 AGPL-3.0 发布，本项目整体因此按 AGPL-3.0 发布，
许可口径与上游一致，详见 [LICENSE](LICENSE)。

go 标准库按 BSD-3-Clause 发布，版权归 The Go Authors 所有。

各依赖的完整许可全文不在本仓库内，可按上表的模块路径从
`https://proxy.golang.org/<模块路径>/@v/<版本>.zip` 取得。
