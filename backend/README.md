# backend · Echo 演出导购服务

一个可执行文件、一个端口（默认 `:8082`）就是全部：HTTP API、SSE 流式咨询、静态页面托管、助手管理端。

```bash
cd backend
go run ./cmd/guide          # 或 go build -o guide.exe ./cmd/guide
```

启动后：

- 导购页 `http://127.0.0.1:8082`
- 助手管理端 `http://127.0.0.1:8082/#admin`（需先设置 `GUIDE_ADMIN_TOKEN`）
- 健康检查 `GET /api/v1/health`

Echo 票务主站需要单独运行（默认 `127.0.0.1:8081`），用户购票页默认 `127.0.0.1:3002`。

## 目录

```
cmd/guide/          进程入口：装配路由、托管 frontend/dist、优雅退出
src/agent/          Agent 运行时：规划、8 个工具、多轮记忆、自检、并行、断点续跑、向量检索
src/guide/          导购业务：发现/筛选、需求解析、对比、口碑、SSE、管理端
src/ticketing/      Echo 只读数据适配器：HTTP 优先、MySQL 只读降级、快照缓存
src/llm/            OpenAI 兼容模型客户端，仅用于润色与草稿
```

四者的关系只有一条主线：

```
Echo 只读接口 → ticketing 聚合快照 → agent 规划并调用工具 → guide 还原卡片 → llm 润色(可选) → 浏览器
```

- `POST /api/v1/ticketing/assist`：固定链路的确定性回答；
- `POST /api/v1/ticketing/agent`：Agent 版本（JSON），`/agent/stream` 为 SSE；
- 没有模型 Key 时 Agent 走规则编排（`agent-rule`），功能不降。

`guide` 不写 Echo 的数据库，也不持有 Echo 用户令牌：下单、选座、支付、评测全部由 Echo 完成。
写工具产生的动作先进入待确认队列（本地 `agent-state.json`），用户确认后才落到本地的观演清单与偏好表；交易类操作只生成跳转 Echo 的确认链接。

## 配置

全部走环境变量，示例见根目录 `guide.env.example.cmd`（本地复制成 `guide.env.cmd`，`start-guide.cmd` 会自动加载）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `GUIDE_ADDR` | `127.0.0.1:8082` | 监听地址 |
| `TICKETING_API_BASE` | `http://127.0.0.1:8081` | Echo 后端地址 |
| `TICKETING_DSN` | 空 | 可选：Echo 库的只读账号，接口不可用时降级查询 |
| `ECHO_WEB_BASE` | `http://127.0.0.1:3002` | 生成票档确认链接 |
| `GUIDE_ADMIN_TOKEN` / `GUIDE_ADMIN_TOKEN_SHA256` | 空 | 管理端口令（明文或 SHA-256）；都不配则管理端 503 |
| `GUIDE_AGENT_TRACE` | 程序目录 `agent-runs.jsonl` | Agent 轨迹文件路径（测试与多实例用） |
| `GUIDE_AGENT_STATE` | 程序目录 `agent-state.json` | 待确认动作、观演清单与长期偏好的落盘路径 |
| `GUIDE_AGENT_BENCH` | 程序目录 `agent-bench.json` | 跑分用例文件；不存在时用内置 8 条 |
| `GUIDE_AGENT_CHECKPOINTS` | 程序目录 `agent-checkpoints.jsonl` | 断点快照落盘路径（重启后仍可续跑） |
| `AI_EMBEDDING_MODEL` | DashScope embedding 模型 | 配了 Key 时常识检索用它向量化；没 Key 用离线 n-gram 向量 |
| `AI_BASE_URL` / `AI_API_KEY` | DashScope 兼容地址 / 空 | 模型接入；没有 Key 时全站可用，只是少了润色 |
| `AI_SMALL_MODEL` / `AI_LARGE_MODEL` | `qwen3.5-flash` / `qwen3.7-plus` | 小模型做草稿，大模型做回答 |

## 依赖

- 运行期只依赖 Echo（HTTP 或 MySQL 只读），没有中间件、没有消息队列、没有配置中心。
- Go 依赖只有 MySQL 驱动（`github.com/go-sql-driver/mysql`），且只在配置了 `TICKETING_DSN` 时才会建立连接。

## 测试

```bash
go build ./...
go vet ./...
go test ./...
```

真实 Echo 联调（需要 Echo 正在运行）：

```bash
TICKETING_INTEGRATION_TEST=1 go test ./src/ticketing
```

## 运行期落盘

程序同目录会生成下列运行期文件，均已在 `.gitignore` 中：

| 文件 | 内容 |
|---|---|
| `guide-prompt.json` | 助手提示词版本与修改记录（管理端可回滚） |
| `guide-stats.jsonl` | 每次咨询的问题、模式、意图、耗时，重启不丢 |
| `guide-cases.json` | 管理员自定义巡检用例 |
| `agent-runs.jsonl` | Agent 每次运行的意图、工具链、步骤与耗时 |
| `agent-state.json` | 待确认动作、观演清单、长期偏好与审批结果 |
| `agent-checkpoints.jsonl` | Agent 断点快照：每步推理后的决策上下文与已查事实 |
| `agent-bench.json` | 自定义跑分用例（可选，缺省用内置用例） |

## 接口与数据约定

完整接口清单、Echo 字段口径（金额单位、库存来源、秒杀状态）、口碑算法与已知边界见
[`../docs/ECHO-GUIDE.md`](../docs/ECHO-GUIDE.md)。
