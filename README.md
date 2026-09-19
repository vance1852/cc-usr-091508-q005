# floodair — 洪水救援现场临时空域授权服务

面向应急测绘无人机与救援直升机同场作业的临时空域授权（Temporary Airspace
Authorization）服务。协调员按**任务区域 + 有效时段 + 高度层 + 飞行器能力**申请；
空管依据救援直升机航线与避让规则**批准、缩小或撤销**；系统在范围相交、飞行器
失联或救援航线更新时**立即冻结凭证**并向持有人留下**必须确认**的通知。

- 语言/框架：Go 1.19+、[Echo v4](https://github.com/labstack/echo)
- 存储：SQLite（CGO `mattn/go-sqlite3`，WAL 模式），每版多边形、决策来源、
  凭证状态机、航段与通知/确认全部留痕，可直接用 sqlite3 核查
- 几何：GeoJSON Polygon/MultiPolygon 校验（闭合、简单不自交）、平面求交、
  冲突点定位、耳切三角化与栅格化"避让安全区"建议

## 快速开始

```bash
go run .
# 或
go build -o floodair . && ./floodair
# FLOODAIR_ADDR=:8080  FLOODAIR_DB=floodair.db（可用环境变量覆盖）
```

首次启动会写入种子用户（两家承包单位 ALPHA/BRAVO、各自飞手与协调员、救援
指挥、空管）和一条救援直升机走廊 `rt-rescue-main`（v1）。

| 角色 | API Key | 可见范围 |
|---|---|---|
| 空管 ATC | `atc-key-0001` | 全部授权的完整处理（批准/缩小/拒绝/撤销、航线维护、审计） |
| 救援指挥 | `rescue-key-0001` | 冲突概况看板、航线维护、审计；**不返回凭证 token** |
| ALPHA 协调员 | `coord-alpha-0001` | 仅 ALPHA 单位的申请、飞行器与凭证 |
| BRAVO 协调员 | `coord-bravo-0001` | 仅 BRAVO 单位 |
| 飞手 A1/A2/B1 | `pilot-alpha-a1` 等 | 仅本人飞行器的许可、凭证、通知与航段 |

## 核心规则如何落地

1. **版本化边界与决定来源**：每次申请/批准/缩小/拒绝/撤销都在
   `permit_versions` 新增一行（`seq` 递增、`source` 为 APPLICATION/
   ATC_DECISION/ROUTE_VERSION/FREEZE_ENGINE/LINK_REPORT），同一时刻
   **恰好一个 active 版本**；`events` 表是完整审计流。
2. **一次性凭证**：批准/缩小即签发 256-bit 随机 token，绑定具体版本与飞行器；
   兑换时条件 UPDATE（`status='ISSUED'`）保证原子单次使用，重复兑换返回 409。
   缩小/撤销会冻结旧凭证并签发新凭证。
3. **立即冻结引擎**（申请、裁决、航线更新后原子重算）：
   - 许可多边形相交 **且** 时间窗重叠 **且** 高度层重叠 → 双方 ISSUED 凭证冻结；
   - 许可压救援航线走廊 → 该许可凭证冻结；航线以版本号管理，v2 生效时旧冲突
     关闭、按新走廊重新判定并冻结；
   - 冻结是**粘性**的：几何分开也不自动解冻，必须由空管缩小/重批签发新凭证；
   - 已在飞（REDEEMED）的不回改状态，改为推送"立即避让并如实报备"告警。
4. **飞行器失联**：ISSUED 凭证立即 FROZEN(`LINK_LOST`)；在飞的收到专门通知，
   其航迹偏离按实际登记。
5. **偏航如实登记、不可回写**：航段关闭后 append-only，重复/更正回执返回 409；
   是否偏航由**服务端**依据该凭证绑定的精确版本多边形计算（忽略客户端自称），
   给出越界点数、最远距离与首个越界点。
6. **断网延迟/重复回执**：所有写操作要求 `Idempotency-Key` 头；相同键+相同
   请求体重放唯一原始响应（响应头 `Idempotent-Replayed: true`），同键不同体
   返回 409。
7. **角色与承包单位隔离**：跨单位引用飞行器 403；越权读许可返回 404（不存在
   而非拒绝，避免侧信道）；token 仅空管、本单位协调员、持有人飞手可见。
8. **授权详情**始终给出：唯一有效版本、版本历史、空间冲突**点位**、每个凭证
   的使用情况（ISSUED/FROZEN/REDEEMED + 冻结原因/兑换航段）、以及**仍待响应
   的人**（通知是否已被持有人飞手确认）。

裁决若与航线或许可冲突，空管会收到 `422`：精确冲突点 + 自动生成的
`suggestedSafePolygon`（保持约 35 m 避让余量的简单多边形，可直接作为缩小
范围的参考；最终以空管提交的多边形为准，且缩小范围必须包含于上一版授权）。

## API 概览（均在 `/api` 下，需 `X-API-Key`）

| 方法 路径 | 角色 | 说明 |
|---|---|---|
| `POST /permits` | 协调员 | 提交申请（幂等） |
| `GET  /permits` | 全部 | 按角色投影的清单 |
| `GET  /permits/:id` | 相关方 | 授权详情（版本/冲突/凭证/待确认） |
| `POST /permits/:id/decisions` | 空管 | approve/shrink/deny/revoke（幂等） |
| `GET  /aircraft` · `POST /aircraft` | 协调员等 | 飞行器注册/清单 |
| `POST /aircraft/:id/link` | 协调员/飞手 | `{"status":"ok|lost"}` 失联上报 |
| `GET  /routes` · `POST /routes` · `PUT /routes/:id` | 空管/指挥 | 救援航线版本化维护 |
| `POST /credentials/:token/redeem` | 持有人飞手 | 一次性兑换，建立航段（幂等） |
| `POST /segments/:id` | 持有人飞手 | 上报航迹，服务端判定偏航（幂等，关闭后不可改） |
| `GET  /notifications` · `POST /notifications/:id/ack` | 相关方 | 通知清单与确认（幂等） |
| `GET  /dashboard/rescue` | 指挥/空管 | 冲突概况、冻结统计、待响应名单 |
| `GET  /events` | 指挥/空管 | 审计流（决策来源） |

## 示例

```bash
# 申请（多边形避开种子航线走廊 x[113.00,113.20] y[23.10,23.16]）
curl -sX POST localhost:8080/api/permits \
  -H "X-API-Key: coord-alpha-0001" -H "Idempotency-Key: p-1" \
  -H "Content-Type: application/json" -d '{
    "aircraftId":"uav-a1","missionName":"一号堤段水情",
    "polygon":{"type":"Polygon","coordinates":[[[113.0,23.20],[113.05,23.20],[113.05,23.25],[113.0,23.25],[113.0,23.20]]]},
    "validFrom":"2026-09-19T10:00:00Z","validUntil":"2026-09-19T18:00:00Z",
    "altitudeMin":10,"altitudeMax":90,"requiredCapabilities":["optical","thermal"]}'

# 空管批准 -> 返回一次性 token
curl -sX POST localhost:8080/api/permits/<id>/decisions \
  -H "X-API-Key: atc-key-0001" -H "Idempotency-Key: d-1" \
  -d '{"action":"approve","reason":"航线以北，准予"}'

# 飞手兑换（只能一次；重复回执安全重放）
curl -sX POST localhost:8080/api/credentials/<token>/redeem \
  -H "X-API-Key: pilot-alpha-a1" -H "Idempotency-Key: takeoff-1" -d '{}'

# 航段报备（服务端自行判定是否偏航）
curl -sX POST localhost:8080/api/segments/<segmentId> \
  -H "X-API-Key: pilot-alpha-a1" -H "Idempotency-Key: close-1" \
  -d '{"track":[[113.01,23.21],[113.03,23.30]],"disposition":"避让后归位"}'
```

## 测试

```bash
go test -race ./...
```

覆盖：几何校验/相交/安全区切分；能力门槛与跨单位拒绝；航线冲突 422 与
安全区建议；凭证签发/一次性兑换/幂等重放/越权兑换；航段服务端偏航判定与
关闭后不可回写；航线 v2 冻结 + 通知 + 确认 + 越权确认；缩小发新凭证且
唯一 active 版本；失联冻结；待审批申请不冻结相邻已批许可；撤销冻结；
指挥看板与审计流；过期时间窗拒绝兑换；无凭证 401。

## 设计取舍

- 几何用本地等距柱状投影做平面运算，尺度限于县城救援场景（公里级），冲突
  点仍以经纬度返回；安全区建议是**保守栅格**结果（约 35 m 网格与避让余量），
  宁小勿越界，供空管参考，不自动生效。
- SQLite 写操作经单把互斥锁串行成事务，WAL 下读连接并发；冻结级联在同一
  事务内完成，不会出现"凭证已冻、通知未落"的中间态。
- API Key 为现场预置的共享密钥（种子表），便于堤段飞手离线使用；接入真实
  身份系统时替换 `authed` 中间件即可。
