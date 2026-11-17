# go-zenfs-config

Go 1.23+ 实现的 [zen-fs-config](https://github.com/zen-fs/config) 兼容库。字节级
对齐现有 JavaScript 实现，可以直接读写同一套「同步组」仓库：Go 写、JS 读，
或 JS 写、Go 读，无需迁移。

- 模块路径：`github.com/weijia/go-zenfs-config`
- 版本：`v0.1.0`（git tag，可直接 `go get`）
- 依赖：仅标准库（Gitee 后端用标准库 HTTP，本地目录后端用 `os`）
- 许可证：MIT

## 能力

- config-sync 组：`/{appId}/...` 业务配置读写、`.version` sidecar、墓碑；
- data-sync 组：真实文件读写、`/.meta/versions/{encoded}.json` 版本记录、墓碑；
- app-data-group 注册表：`.config/app-data-groups/{appId}/{id}.json` 引导副本，
  数据组自己的 `.meta/backends/{id}.json` 为权威；
- 统一 `Connect`：自动读组类型标记并路由到 config repo 或 data group，组类型不符时
  返回 `ErrGroupTypeMismatch`；
- Push/Pull 镜像：本地快照与远端双向同步，含墓碑传播。

## 为什么需要它

zen-fs-config 把配置存在一个 Git 仓库里，目录结构由 JavaScript 实现定义。
如果 Go 端按普通 JSON 序列化写文件，会出现：

- 字段顺序不同 → 每次同步都产生假变更；
- 缩进/结尾换行不同 → 字节不同，`hash` 变化，版本号无意义地增长；
- 元数据路径编码不同 → JS 端找不到墓碑、版本或后端描述符。

本库复刻了这些细节，并用真实仓库样本做了 golden 测试。

## 与 JS 实现对齐的字节契约

| 项目 | 规则 |
| --- | --- |
| JSON 缩进 | 2 空格 |
| 结尾换行 | 无 |
| 字段顺序 | 描述符按固定字段序；未知字段按 UTF-16 码元升序 |
| `options` | 递归按键排序 |
| `backends` | 按 `id` 排序 |
| 非 ASCII | 原样输出 UTF-8，不转义 |
| `& < >` | 不转义（与 `JSON.stringify` 一致） |
| 浮点 | JS 指数格式：`1e-7`、`1e+21` |
| 业务值 | `JSON.stringify(value, null, 2)`，保留对象插入序 |
| 版本 sidecar | `<file>.version`，字段序 `version,hash,author,timestamp`，`hash` 为内容原始字节 SHA-256 |
| 墓碑 | `.meta/tombstones/<encoded>.json`，旧版 `.meta/.deleted/` 只读兼容 |
| 路径编码 | 去前导 `/`，`/` → `__`，`.` → `++` |
| 组类型 | `.config/group-type` 优先，`.meta/group-type` 回退，最后看 `.meta/group.json` 的 `kind` |
| app-data 引导副本 | `.config/app-data-groups/{appId}/{id}.json`，旧版 `.meta/` 只读回退 |
| app-data 字段序 | `id, groupType, name?, features?, backends[]`；`backends` 按 `id` 升序 |
| app-data 幂等 | 投影字节比较忽略 `createdAt`；写入时 `createdAt` 取 min |

真实样本（`weijia432/configs`，config 组）：

- `group.json` 197 字节，sha256 前 16 位 `85017CB42A8E042D`
- `.meta/backends/gitee.json` 198 字节
- `.version` 180 字节
- 墓碑 142 字节
- `.config/app-data-groups/dir-struct-gen/default.json` 679 字节（2 个后端，
  含 `description`、`createdAt`；Go 反序列化后再序列化字节完全一致）

这些字节都已固化进 `testdata/` 与 golden 测试。

## 安装

```bash
go get github.com/weijia/go-zenfs-config@v0.1.0
```

## 快速开始

```go
package main

import (
	"context"
	"log"

	zc "github.com/weijia/go-zenfs-config"
	"github.com/weijia/go-zenfs-config/backend/gitee"
)

func main() {
	ctx := context.Background()
	store, err := gitee.New(gitee.Config{
		Owner: "you", Repo: "configs", Branch: "master",
		Token: os.Getenv("GITEE_TOKEN"),
	})
	if err != nil {
		log.Fatal(err)
	}
	kind, err := zc.ProbeGroupKind(ctx, store)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("group kind:", kind)

	repo, err := zc.Open(ctx, store, "my-app")
	if err != nil {
		log.Fatal(err)
	}
	repo.Author = "my-app"
	changed, err := repo.SetConfig(ctx, "settings/site", map[string]any{
		"title": "Hello",
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Println("changed:", changed) // 值相同则不写、不升版本
}
```

读取：

```go
data, err := repo.ReadConfig(ctx, "settings/site")
```

删除（写墓碑）：

```go
err = repo.DeleteConfig(ctx, "settings/site")
```

节点级配置（不带 app 前缀）：

```go
path := repo.NodeConfigPath("node-1", "env") // /nodes/node-1/env.json
```

## 自动判断组类型：`Connect`

调用方不需要预先知道远端是 config-sync 还是 data-sync：

```go
res, err := zc.Connect(ctx, zc.ConnectOptions{
	AppID:         "openlist",
	Storage:       remote,          // 远端组
	ConfigStorage: localConfig,     // data 组的注册宿主（data-sync 时通常是本地目录）
	DataGroupName: "OpenList",
	Backend: &zc.BackendDescriptor{
		ID: "gitee-primary", Type: "gitee",
		Options: map[string]any{"owner": "you", "repo": "openlist-data"},
	},
})
switch {
case res.IsConfig():
	// 现有 config-sync 流程：res.Repo.SetConfig(...)
case res.IsData():
	// 数据组流程：res.Data.WriteFile(...)；引导副本已写入 res.Repo
}
```

探测顺序：`/.config/group-type` → `/.meta/group-type` → `.meta/group.json` 的
`kind`。前两个标记必须精确等于 `config-sync` / `data-sync`；遇到未知值返回
`ErrUnknownGroupType`，不会静默当成 config。没有任何标记时由 `GroupType` 选项或
默认 `config-sync` 决定。

`ExpectType` 可用于拒绝错误类型的仓库：

```go
res, err := zc.Connect(ctx, zc.ConnectOptions{
	AppID: "openlist", Storage: remote, ExpectType: zc.GroupTypeConfig,
})
// 远端是 data 组时 err 为 ErrGroupTypeMismatch
```

## 应用数据组（app-data group）

数据组的**权威后端列表**在它自己的 `.meta/backends/{id}.json`；配置组里的
`.config/app-data-groups/{appId}/{id}.json` 只是「引导副本」，让新设备仅凭配置组
凭据就能发现数据组。

```go
repo, _ := zc.Open(ctx, configStore, "openlist")

// 列出 / 读取引导副本
groups, _ := repo.ListAppDataGroups(ctx)
d, _ := repo.GetAppDataGroupDescriptor(ctx, "default")

// 创建数据组（会同时写数据组自己的 group.json 与 .meta/backends）
dg, _ := repo.CreateAppDataGroup(ctx, "default",
	[]zc.BackendDescriptor{{ID: "gitee-primary", Type: "gitee",
		Options: map[string]any{"owner": "you", "repo": "pim-store"}}},
	zc.CreateAppDataGroupOptions{Storage: dataStore, Name: ptr("PIM")})

// 改写引导副本；投影字节相同则不写，createdAt 取 min
changed, _ := repo.SaveAppDataGroupDescriptor(ctx, zc.AppDataGroupDescriptor{
	ID: "default", GroupType: zc.GroupTypeData,
	Backends: dg.Backends(),
})
```

数据组文件 API（`/.meta`、`/.config` 对应用层不可见）：

```go
dg.WriteFile(ctx, "/notes/todo.json", data) // 内容相同返回 changed=false
dg.ReadFile(ctx, "/notes/todo.json")
dg.DeleteFile(ctx, "/notes/todo.json")      // 墓碑 + 删除 + 清版本记录
dg.List(ctx, "/notes")
```

- `features.versions=true` 时写 `/.meta/versions/{encoded}.json`，
  字段序 `version,hash,author,timestamp`；
- 数据组同步区排除 `/.config`、`/nodes`、`/shared`，`.meta` 仍同步；
- 两个组不允许指向完全相同的远端（type + options），`CreateAppDataGroup` 会拒绝。

## 后端

### 本地目录 `backend/folder`

```go
b, err := folder.New(`C:\data\group`)
```

目录不存在时按需创建。适合单元测试、离线快照、只读检查。

### Gitee `backend/gitee`

基于 Gitee OpenAPI v5 的 Contents API：

- 读：文件内容、目录列表、递归 tree；
- 写：创建/更新（带 `sha`）、删除（带 `sha`）；
- `branch` 为空时使用仓库 `default_branch`；
- 空仓库首次写入自动用 `.keep` 引导；
- `.mtime` 为纯数字 sidecar，值未变则不重复写；
- 错误信息中的 token 自动打码。

已知坑：**Gitee 对不存在的路径返回 `200 []` 而不是 404**。后端按响应体形状判断
（JSON 对象 = 文件，数组 = 目录或不存在），不能只看状态码。

## CLI

```bash
# 只读探测（组类型、标记是否存在、文件清单）
zfc -dir ./snapshot probe
zfc -gitee-owner you -gitee-repo configs -gitee-token TOKEN probe

# 只打印探测到的组类型
zfc -gitee-owner you -gitee-repo pim-store -gitee-token TOKEN detect

# 应用数据组
zfc -gitee-owner you -gitee-repo configs -gitee-token TOKEN -app my-app appdata-list
zfc -gitee-owner you -gitee-repo configs -gitee-token TOKEN -app my-app appdata-get default

# 数据组文件读写（真实 data-sync 仓库）
zfc -gitee-owner you -gitee-repo pim-store -gitee-token TOKEN -app my-app data-put default /notes/a.txt hello
zfc -gitee-owner you -gitee-repo pim-store -gitee-token TOKEN -app my-app data-get default /notes/a.txt
zfc -gitee-owner you -gitee-repo pim-store -gitee-token TOKEN -app my-app data-rm  default /notes/a.txt

# 初始化一个 config 组
zfc -gitee-owner you -gitee-repo new-repo init

# 读写业务配置
zfc -dir ./snapshot -app my-app get settings/site
zfc -dir ./snapshot -app my-app set settings/site '{"title":"Hello"}'

# 镜像：本地 → 远端 / 远端 → 本地
zfc -dir ./snapshot -gitee-owner you -gitee-repo configs push --prefix /my-app
zfc -dir ./snapshot -gitee-owner you -gitee-repo configs pull --prefix /my-app
```

Token 通过 `-gitee-token` 或环境变量 `GITEE_TOKEN` 传入，不要写进仓库。
注意：PowerShell 每次命令都是新进程，`$env:GITEE_TOKEN` 不会跨调用保留，
要么在同一条命令里设置，要么始终显式传 `-gitee-token`。

## 镜像同步（Push/Pull）

`Push`（源覆盖目标）与 `Pull`（反过来）用于把一棵本地快照树同步到远端：

- 复制新增/变化的文件，并携带 `.version` sidecar；
- 源端不存在的目标文件会被删除，并写入墓碑；
- 墓碑传播：目标端在 `deletedAt + 3000ms` 之后重建的文件视为「复活」，保留文件并退掉墓碑；
- 不镜像运行时元数据：`.meta/tombstones`、`.meta/.deleted`、`.meta/conflicts`、
  `.meta/versions`、`*.mtime`、`.keep`；
- 可用 `Prefixes` 把作用范围限制在某个 app 目录。

**当前限制（未实现）**：JS 版的冲突归档（`.meta/conflicts/{ts}_{path}` 与
`source/target/meta.json`）。镜像遇到双方都改时按「源胜出」，不会保留冲突副本。

## 在 OpenList 中使用

OpenList 侧的适配层位于 `OpenList/internal/configsync`，依赖本库的已发布版本
`v0.1.0`（`go.mod` 不再有本地 `replace`）：

- 自动探测组类型：`config-sync` 走镜像 + `/{prefix}/settings/*.json`；
  `data-sync` 直接通过数据组 API 读写同一批路径，不把数据组镜像到本地；
- 导出设置：站点、样式、预览、索引、流量五组（`SITE/STYLE/PREVIEW/INDEX/TRAFFIC`），
  自动跳过敏感键；
- 可选导出存储挂载快照（`storages.json`），**只导出，不自动导入**；
- 启动时拉取远端 `/{appId}` 下的设置并应用，然后把当前状态推回去；
- 设置变更后 debounce 推送。

启用方式：在 OpenList 数据目录创建 `data/configsync.json`：

```json
{
  "enabled": true,
  "gitee": {
    "owner": "you",
    "repo": "configs",
    "branch": "master",
    "token": "GITEE_TOKEN"
  },
  "app_id": "openlist",
  "prefix": "/openlist",
  "include_storages": true,
  "group_type": "auto",
  "data_group": "",
  "data_group_name": "OpenList",
  "app_data": {
    "enabled": false,
    "repo": "openlist-data",
    "branch": "master",
    "group_id": "default",
    "name": "OpenList"
  },
  "push_debounce_seconds": 5
}
```

- `group_type`：`auto`（默认，按远端标记）/ `config-sync` / `data-sync`；
- `data_group`：data-sync 组的 id，留空时读 `.meta/group.json`；
- `app_data`：在 config 组里登记一个数据组，供其他设备发现；
- 未创建该文件时功能完全关闭。Token 也可以只放在环境变量 `GITEE_TOKEN` 里。

## 测试

```bash
go test ./...                 # 离线：golden 字节 + 仓库语义 + 目录后端 + 镜像
```

真机测试默认跳过，需要显式开启：

```powershell
$env:GITEE_LIVE='1'
$env:GITEE_TOKEN='...'
$env:GITEE_OWNER='...'
$env:GITEE_REPO='...'
$env:GITEE_BRANCH='go-sync-test'
go test ./backend/gitee -run TestLiveRoundTrip -v
```

覆盖：首次写入、读回一致、幂等不写、二次写版本递增、删除后墓碑存在。

## 目录

```
canonjson/        有序 JSON 编码器（2 空格、UTF-16 排序、JS 浮点格式）
backend/folder/   本地目录 Storage
backend/gitee/    Gitee Contents API Storage
cmd/zfc/          CLI
model.go          组/后端/版本/墓碑描述符
paths.go          元数据路径编解码
repo.go           仓库操作（读写配置、版本、墓碑）
appdata.go        应用数据组注册表与数据组文件 API
connect.go        组类型探测与统一 Connect
mirror.go         Push/Pull 镜像
```

## 安全

- 不要把 token 提交到仓库；示例配置里只写环境变量名。
- Gitee 后端错误信息会对 token 打码，但调用方仍应避免把原始请求 URL 打进日志。
- 私有仓库中的 backend 描述符按 JS 实现会包含明文 token；这是格式本身的行为，
  请确保仓库为私有。
