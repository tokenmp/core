# 第三方许可

本仓库的运行时依赖清单。版本以 `go.mod` 为准，测试与工具依赖不在此列。

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
```

## 说明

RelayKit 按 AGPL-3.0 发布，本项目整体因此按 AGPL-3.0 发布，
许可口径与上游一致，详见 [LICENSE](LICENSE)。

go 标准库按 BSD-3-Clause 发布，版权归 The Go Authors 所有。

各依赖的完整许可全文不在本仓库内，可按上表的模块路径从
`https://proxy.golang.org/<模块路径>/@v/<版本>.zip` 取得。
