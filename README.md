# 冷库门告警接收与回放链路

食品冷库夜间频繁开门、中控浏览器偶尔断网。本项目解决两个痛点：

- **重复告警**：设备网关重试回调时，同一事件不会重复入库、重复弹窗；
- **漏告警**：中控断线期间到达的事件，重连后按服务端序号完整补发，边界时刻到达的事件也**不重不漏**。

链路：

```
设备网关 ──POST JSON──▶ Go API ──写入──▶ SQLite（严格递增、永不复用的服务端序号）
                          │
                          ├──SSE 告警流──▶ Vue 中控页面（按序号显示，断线带最后序号重连）
Vue 中控页面 ──GET /api/doors/active──▶ Go API（未关闭门：接班加载、实时/补发后校准）
```

## 快速开始（Docker Compose）

```bash
docker compose up -d --build     # 起 api 与 web
# 中控页面： http://localhost:${WEB_PORT:-8081}
# API：      http://localhost:${API_PORT:-8080}

docker compose run --rm verify   # 一次性端到端验收，通过后退出码 0
```

端口可用环境变量覆盖（容器内固定监听，宿主机映射随之改变）：

```bash
WEB_PORT=9000 API_PORT=9090 docker compose up -d --build
```

## 本地开发（不用 Docker）

```bash
# 终端 1：API（Go 1.23+，SQLite 为纯 Go 驱动，无需 cgo）
cd backend
go run .                       # 默认 :8080，DB 在 /data/alarm.db
DB_PATH=./alarm.db API_PORT=8080 go run .   # 本地常用写法

# 终端 2：前端
cd frontend
npm install
API_PORT=8080 npm run dev      # http://localhost:5173，/api 自动代理到 API

# 后端测试（存储 + SSE 续接，含竞态检测）
cd backend && go test -race ./...

# 端到端测试（需要先在 API_PORT 上跑着 API）
cd frontend && npx playwright install chromium
npx playwright test
```

---

## 接口

### 1. 设备回调：`POST /api/events`

设备网关提交一条 JSON，必填 `event_id`、`door_id`、`occurred_at`、`kind`：

| 字段 | 说明 |
| --- | --- |
| `event_id` | **全局唯一**。相同 id 的重复回调返回既有记录，不新增事件、不分配新序号（幂等） |
| `door_id` | 冷库门编号，必填 |
| `occurred_at` | 设备侧事件时间，RFC3339（如 `2026-09-14T22:00:00Z`）。**只用于展示，不参与排序** |
| `kind` | 只能是 `OPEN_TOO_LONG`、`FORCED_OPEN`、`CLOSED` |

首次接受返回 `201 Created`，响应头 `X-Deduplicated: false`：

```bash
curl -X POST http://localhost:8080/api/events \
  -H 'Content-Type: application/json' \
  -d '{
    "event_id": "gw-20260914-0001",
    "door_id": "door-A1",
    "kind": "OPEN_TOO_LONG",
    "occurred_at": "2026-09-14T22:03:10Z"
  }'
```

```json
{
  "seq": 1,
  "event_id": "gw-20260914-0001",
  "door_id": "door-A1",
  "kind": "OPEN_TOO_LONG",
  "occurred_at": "2026-09-14T22:03:10Z",
  "received_at": "2026-09-14T14:03:10.482911Z"
}
```

相同 `event_id` 重发（哪怕重试时其它字段变了）返回 `200 OK`、`X-Deduplicated: true`，
响应体仍是**第一次**那条记录，`seq` 不变：

```bash
curl -X POST http://localhost:8080/api/events \
  -H 'Content-Type: application/json' \
  -d '{"event_id":"gw-20260914-0001","door_id":"door-A1","kind":"FORCED_OPEN","occurred_at":"2026-09-14T22:03:10Z"}'
# HTTP 200, X-Deduplicated: true, 仍是 seq=1、kind=OPEN_TOO_LONG
```

非法 JSON、缺字段、非法 `kind`、非法时间一律 `400`，**不分配序号**：

```bash
curl -i -X POST http://localhost:8080/api/events -d '{not json'
# HTTP 400  {"error":"invalid JSON body: ..."}

curl -i -X POST http://localhost:8080/api/events \
  -d '{"event_id":"x","door_id":"d","kind":"OPEN","occurred_at":"2026-09-14T22:00:00Z"}'
# HTTP 400  {"error":"kind must be one of OPEN_TOO_LONG, FORCED_OPEN, CLOSED"}
```

### 服务端序号 `seq`

- 由 SQLite `INTEGER PRIMARY KEY AUTOINCREMENT` 分配，**严格递增、永不复用**，重启后继续增大；
- 页面排序、补发、去重只认 `seq`；设备时间 `occurred_at` 即使乱序也不影响顺序。

### 2. 事件流：`GET /api/events/stream`（SSE）

#### 重连方式（关键）

连接时带上**最后已显示序号** `last_seq`：

```
GET /api/events/stream?last_seq=42
```

服务端处理顺序：

1. 先注册实时订阅；
2. 再从 SQLite 按 `seq` 升序补发所有 `seq > last_seq` 的历史事件（`event: alarm`）；
3. 补发结束发一帧 `event: replay-done`；
4. 之后持续实时推送新事件，并每 15s 发一帧心跳注释。

因为「先订阅、后查库」，订阅与补发交叠窗口内到达的事件可能同时出现在两处，
服务端按 `seq` 去重（`seq <= 已补发序号` 的实时帧丢弃），**每条告警恰好下发一次**。
前端同样只接受 `seq > 本地最后序号` 的帧，双重保险。

首次打开页面用 `last_seq=0` 全量补发。EventSource 自带重连不会携带查询参数，
因此前端在 `onerror` 时主动关闭并带上最新 `last_seq` 重新建连（指数退避，上限 10s）。

帧示例：

```
event: alarm
data: {"seq":43,"event_id":"gw-...","door_id":"door-A1","kind":"FORCED_OPEN","occurred_at":"...","received_at":"..."}

event: replay-done
data: {"last_seq":43}
```

直接观察：

```bash
curl -N "http://localhost:8080/api/events/stream?last_seq=0"
```

### 3. 未关闭门视图：`GET /api/doors/active`（只读）

值班员接班时不必逐条翻告警，直接拿到**仍处于异常开启、尚未 CLOSED** 的门：

```bash
curl http://localhost:8080/api/doors/active
```

```json
[
  {
    "door_id": "door-A1",
    "start_seq": 13,
    "last_seq": 16,
    "last_kind": "FORCED_OPEN",
    "last_occurred_at": "2026-09-14T22:11:00Z"
  }
]
```

- 每扇门一行：门号、异常**开始序号**、**最近序号**、最近告警类型、最近设备时间；
  序号直接复用服务端 `seq`，设备时间仍只用于展示。
- 按异常开始序号升序（最早异常的门排最前）；没有未关闭门时返回 `200 []`。
- 异常时段在与事件入库**同一 SQLite 事务**中维护：
  首次接受的 `OPEN_TOO_LONG`/`FORCED_OPEN` 开段，同门后续异常告警只刷新最近类型/序号，
  `CLOSED` 结束该时段（之后再开是新段）；重复 `event_id` 不改变任何结果。
- 服务启动时若发现旧库（升级前的库），会按 `seq` 从既有事件回算一次初始视图，
  回算结果幂等，之后由写入路径持续维护。
- 查询失败返回与事件接口一致的 JSON 错误结构（`{"error": ...}`）。

### 4. 健康检查：`GET /healthz` → `200 {"status":"ok"}`

---

## 中控页面

- 顶部徽标显示连接状态：`连接中 / 补发断线期间事件 / 实时 / 已断开，自动重连中`；
- 每条告警一张卡片，展示服务端序号 `#seq`、类型、门号与**设备时间**；
- 「序号连续 · 屏幕完整」自检：若本地序号出现空洞（例如补发异常），会明确列出缺失序号，值班员可立刻判断屏幕是否完整；
- 时间线旁的**未关闭门面板**显示仍异常开启的门数量与详情（门号、异常开始/最近序号、最近类型、最近设备时间）：
  接班时先加载一次，收到实时告警后刷新，断线补发完成（`replay-done`）后再校准一次；
  快照接口失败只在面板区域提示并可手动重试（也会自动重试），**不中断**告警流与完整性判断。

## 验收服务 `verify`

`docker compose run --rm verify`（或本地 `go run ./cmd/verify http://localhost:8080`）
对运行中的 API 真实执行：

1. 非法 JSON / 缺字段 / 非法 `kind` 全部返回 400，且随后的合法事件序号紧接（非法请求不占号）；
2. 打开 SSE 后连续提交 3 条，实时收到、序号严格 +1；
3. 重复回调返回 200 + `X-Deduplicated: true` + 既有记录，且不再推送；
4. **主动断开 SSE**，断线期间连续提交并夹杂重复回调，再带最后序号重连，断言补发事件序号连续、每条恰好一次；
5. 补发完成后的新事件仍实时到达，序号紧接；
6. 乱序的设备时间不影响服务端顺序。

可重复执行：每次运行使用唯一 `event_id` 前缀，不依赖空库，也不污染结论。

## 目录结构

```
backend/
  main.go          HTTP 接口、校验、SSE 补发+实时续传、未关闭门只读接口
  store.go         SQLite 存储、幂等写入、按 seq 补发、事务内维护未关闭门视图与启动回算
  broker.go        实时事件扇出（慢消费者踢除，强制其走补发恢复）
  store_test.go    存储/幂等/序号单调与重启不复用、未关闭门事务行为与历史回算
  sse_test.go      SSE 补发、实时、断线续传、边界恰好一次
  active_api_test.go 未关闭门接口的固定顺序、报文形状与 405
  cmd/verify/      一次性验收程序
frontend/
  src/             Vue3 页面、SSE 续传组合式函数、未关闭门快照组合式函数
  e2e/             Playwright：回调 → 页面全链路（含断网窗口、未关闭门面板）
docker-compose.yml api / web / verify 三服务，WEB_PORT、API_PORT 可覆盖
```
