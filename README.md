# 无人机临时空域授权

本项目面向应急测绘队与空管席位，用于协调临时管制空域中的无人机任务和救援航线。

系统需要处理空间相交、授权撤销和现场回执，确保飞手拿到的凭证与最新安全边界一致。

## 技术栈

- Go + Echo（HTTP API）
- SQLite（WAL 模式，单文件库，默认 `./airspace.db`）
- 平面几何自实现（`internal/geo`）：多边形相交 / 包含 / 交点计算

## 运行

```bash
# 环境变量（均可选）
export AIRSPACE_DB=./airspace.db   # SQLite 路径，":memory:" 表示纯内存
export ADDR=:8080                  # 监听地址

go run ./cmd/server
# 或
./scripts/run.sh                   # 自动使用仓库内 .toolchain 的 Go（如有）
```

首次启动空库时自动写入演示数据：两个测绘承包单位、各自协调员/飞手/飞行器、
空管席、指挥所，以及救援直升机航线「生命线一号」走廊。

演示令牌（生产部署前必须更换）：

| 令牌 | 角色 | 单位 |
|---|---|---|
| `tok-atc` | 空管员 | — |
| `tok-command` | 救援指挥 | — |
| `tok-coord-alpha` / `tok-coord-beta` | 协调员 | 曙光测绘队 / 堤防快测队 |
| `tok-pilot-a1` / `tok-pilot-a2` / `tok-pilot-b1` | 飞手 | 曙光 / 曙光 / 堤防 |

调用方式：`Authorization: Bearer <令牌>`。

## 测试

```bash
go test ./...
```

`internal/app/app_test.go` 是覆盖完整应急流程的集成测试：申请穿航线 → 批准即冻结 →
幂等回执 → 缩小范围 → 一次性凭证 → 航段登记 → 失联冻结 → 范围相交冻结 →
航线更新冻结 → 指挥概况 → 数据隔离。

## 核心规则

- **每一版边界都留痕**：每次批准 / 缩小 / 撤销追加一行 `auth_versions`
  （多边形、决定人、决定来源 `manual|rule:avoidance`、理由），当前唯一有效版本
  即最高版本号，历史永不改写。
- **凭证一次性**：每个有效版本为每架被指派的飞行器签发一枚凭证
  （`active → consumed` 原子跃迁，重复使用返回 409 与当前状态）；
  版本更替时旧凭证作废。凭证令牌仅持有人（飞手）与空管可见。
- **立即冻结**（与触发操作同一事务，不会被观察到中间态）：
  - 授权范围与在飞的救援航线走廊相交（`route_conflict`）
  - 飞行器失联（`link_lost`）
  - 两个时间窗重叠的在飞授权范围相交——后签发的凭证冻结，先批准者保留优先
    （`mutual_intersection`）
  冻结即向持有人写入可确认通知。系统只冻不解；解冻只能由空管显式操作。
- **回执幂等**：`POST /api/notifications/:id/ack` 携带客户端回执号；
  断网重发、延迟到达、重复投递都返回已存储的确认状态；
  同一回执号用于另一条通知才返回 409。
- **航段只增不改**：已起飞航段登记后没有任何修改/删除入口，
  不能回写成未执行；偏航必须同时登记原因与处置。
- **授权详情一屏看全**：`GET /api/requests/:id` 返回唯一有效版本、
  全部历史版本、空间冲突位置（交点坐标）、各凭证使用情况、
  仍待响应的人（未回执通知）。
- **数据隔离**：飞手只看自身许可与凭证；协调员只看本单位；
  承包单位之间互不可见（越权访问一律 404，不泄露存在性）；
  救援指挥只有冲突概况（不含令牌与通知正文）；空管掌握完整授权。

## API 一览

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| POST | `/api/requests` | 协调员 | 按任务区域/有效时段/能力申请，指派本单位飞行器与飞手 |
| GET | `/api/requests` | 各角色 | 按角色过滤的申请列表 |
| GET | `/api/requests/:id` | 可见范围内 | 授权详情（版本/冲突/凭证/待回执/航段） |
| POST | `/api/requests/:id/decisions` | 空管 | `approve`/`shrink`/`revoke`，缩小校验包含关系 |
| GET/POST | `/api/routes` | 空管(写) | 救援航线走廊维护，更新即触发冻结评估 |
| POST | `/api/routes/:id` | 空管 | 更新走廊或启停 |
| GET | `/api/aircraft` | 各角色 | 按单位隔离的飞行器列表 |
| POST | `/api/aircraft/:id/link` | 空管/本单位协调员 | 链路 `ok`/`lost`，失联即冻结 |
| GET | `/api/credentials/mine` | 飞手 | 本人凭证 |
| POST | `/api/credentials/use` | 飞手 | 起飞核销（一次性，校验有效时段） |
| POST | `/api/credentials/:id/unfreeze` | 空管 | 人工解除冻结 |
| GET | `/api/notifications/mine` | 各角色 | 本人通知 |
| POST | `/api/notifications/:id/ack` | 通知对象 | 幂等回执 |
| POST | `/api/segments` | 飞手 | 登记已飞航段（只增不改） |
| GET | `/api/segments/mine` · `/api/segments` | 飞手/空管 | 航段查询 |
| GET | `/api/overview` | 指挥/空管 | 冲突概况、凭证状态统计、待响应人员 |

多边形 JSON 形如 `{"vertices":[{"lat":31.0,"lng":121.0}, ...]}`，至少 3 个顶点，
首尾自动闭合；时间一律 RFC3339。
