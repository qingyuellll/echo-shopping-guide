# Echo 演出导购对接说明

本仓库是独立的演出导购服务，票务主站 Echo 作为外部依赖运行。导购通过查询接口读取票务数据；确认订单、选座、付款与评测发布由 Echo 登录后的页面处理。

## 使用

运行根目录 `start-guide.cmd`，打开 `http://127.0.0.1:8082`。首次使用先执行 `build-guide.cmd` 生成后端 `backend/guide.exe` 与前端 `frontend/dist`（两者为构建产物，不入库）。

Echo 后端需可访问 `http://127.0.0.1:8081`；购票页面默认在 `http://127.0.0.1:3002`。可以在 `guide.env.cmd` 中覆盖地址。此文件不提交版本控制。AI 配置使用 `AI_*` 环境变量，缺少密钥或模型暂时不可用时，页面根据真实演出与票档信息整理选票建议，不伪装成模型回复。

查看票档后点击「选择此票档，确认订单」，进入 Echo 的 `/guide/checkout/{ticketId}`；登录后核对数量、座位、规则和金额，再由用户确认下单。付款在原付款页单独确认；目前是模拟支付。Echo 登录状态由 Echo 自身维护，不把用户令牌传给导购。

「写评分与评测」进入 Echo 编辑页，由用户主动选评分、填写真实体验；可让导购整理可编辑草稿，预览并确认后才发布。购票与评测工作台位于 `http://127.0.0.1:3002/guide`。Echo 侧接口和测试约定见 Echo 仓库中的 `docs/GUIDE-COMMERCE.md`。

## 数据约定

- `GET /show-type/list`：演出类型名称。
- `GET /show/of/name?current=1&size=50`：按 Echo 返回的总数逐页读取全部公开演出，按编号去重，兼容服务端缩小每页条数。保留历史演出。异常重复页、缺失页或同步范围不足会明确提示。
- `GET /show/{id}`：票档查询时验证演出仍公开。历史演出可查看详情、评测与历史票价，但不能据此判断当前可购。
- `GET /ticket/list/{showId}`：读取票档、规则与库存。
- `GET /show-review/of/show?showId={id}&current=1&size=50`：完整分页读取演出的公开评测，保留纯评分、作者、正文、赞同数、讨论数和发表时间。
- `GET /show-review/hot?current=1&size=50`：按顶层 `total` 完整分页读取全站公开评测。关联演出已不可公开查看的评测仍展示原文，并明确标记状态，不加入演出购票推荐。
- 演出均价 `avgPrice` 单位为分，在导购端除以 100；票档 `payValueInYuan` 已经是元，不重复除以 100。
- 普通票优先使用 `remainingStock`，0 表示售罄；秒杀票优先使用 `seckillStock`，不根据普通票 `stock=0` 误判售罄。
- `is_seckill` 表示票档类型；`seckill_active` 同时考虑上架、余票和活动起止时间。尚未开始、已结束、已售罄的活动不会显示「抢票中」。
- 导购对外将演出和票档编号输出为字符串，保留大整数精度。
- 只提供 `GET` 查询与本地导购咨询；数据源报错时返回可重试的错误，不生成假演出与假库存。
- 可选的 `TICKETING_DSN` 只用于只读库查询降级，必须显式配置；数据库降级的秒杀库存标记为快照，购票前需在 Echo 确认。
- 演出时间按 `Asia/Shanghai` 解析，明确带时区的时间保持其实际时间点。
- 保留完整演出介绍、评测正文与票档规则，将富文本转换为普通文字显示。
- 演出与评测分别缓存 30 秒，减少重复读取；部分数据不完整时缩短至 5 秒，可手动刷新。每次演出同步最多 1,000 页、每个评测来源最多 2,000 页，达到范围而未读全时明确标记不完整。每场演出的评测、票档独立处理，一项失败不会丢掉整场演出。

## 口碑、比较与咨询

- 「全部演出」默认包含公开的历史场次；支持名称 / 城市 / 场馆搜索、类型、城市、开演日期、评分、票价与余票筛选，每页 12 场。
- 详情展示实际评分均值、1–5 星分布、4–5 星占比、有效评分数和文字评测数。纯评分正常计入统计；无正文时不会生成评价文本。可阅读完整评测、按文字 / 星级筛选及按时间、赞同数、低分排序。
- `show.score` 是 Echo 演出展示分，`rating.average` 是根据有效公开评测计算的精确均分。两者分别展示；`show.comments` 不用于推算评测条数。
- 口碑榜先按有效评分数量分层：5 个及以上 → 3–4 个 → 2 个 → 1 个 → 无评分；同一层内再按 `(评分总和 + 5 × 3.5) / (有效评分数 + 5)` 排序。分层避免 1 个五星压过样本更多的演出；3.5 分与 5 个评分是导购固定参数，不是 Echo 给出的分数，也不是置信区间。卡片与详情仍展示真实均分，少于 5 个有效评分提示样本较少。
- 条件过窄（日期、城市、类型、预算、余票）导致没有结果时，助手会说明是哪一项限制了结果，并放宽日期或预算给出最多 2 个备选，每条标注条件差异并仍可进入确认页；备选不会被表述成「完全满足需求」。
- 城市筛选只接受真实城市（`*市` 或已知城市），不把上游 `area` 里的区县、场馆和占位值当成城市；`area`/`address` 中的香港、澳门、台湾显示为中国香港、中国澳门、中国台湾，筛选与搜索仍命中原值。
- 「观众评测」提供独立的全站评测浏览与搜索，不受当前演出筛选影响。公开演出关联评测数与全站公开评测总数分别标明，避免混淆。
- 收藏与对比清单保存在当前浏览器；同时可对比 2–4 场演出的状态、价格、均分、样本数、高低分原文与选座要求。
- 助手结合当前筛选、预算与对话偏好查询真实数据，引用评测作者与编号，提示样本不足、不同意见和历史状态。要求「能买票」的咨询排除历史场次并筛选预算内有余票的票档。模型收到事实和评测依据；模型不可用时自动返回数据整理结果。推荐卡片的票档直接链接到 Echo 订单确认页。
- `POST /api/v1/ticketing/review-draft` 只接收用户自己的笔记与评分，返回待审核草稿，不发布也不自动决定评分。正常流程经 Echo 已登录且通过 CSRF 校验的 `/guide/reviews/draft` 转发；模型不可用时原样保留用户笔记。

导购对外新增的查询入口：`GET /api/v1/ticketing/shows`、`GET /shows/{id}/detail`、`GET /shows/{id}/reviews`、`GET /rankings`、`GET /review-feed`、`GET /compare?ids=1,30`（后五项同样带 `/api/v1/ticketing` 前缀）。咨询使用 `POST /api/v1/ticketing/assist`，或流式 `POST /api/v1/ticketing/assist/stream`：SSE 先推 `meta`（推荐/备选/需求）与数据版回答逐段输出，模型可用时再接续流式润色并推 `final`；模型失败时数据回答保留。推荐条目带 `keywords`（评测中反复出现的词，相近合并）与 `related`（与提问词相关的评测原文引用）。筛选支持 URL 参数（`view/q/type/city/lc/sort/score/max/avail/from/to`），刷新与分享保留视图。

## 助手管理端

导购自带一个只管理「选票助手」的轻量后台，入口在导购页页脚「导购管理 ↗」，也可直接访问 `http://127.0.0.1:8082/#admin`。设置 `GUIDE_ADMIN_TOKEN` 后启用；未设置时页脚不显示入口，接口返回 503。

- 口令支持两种配置：`GUIDE_ADMIN_TOKEN`（明文）或 `GUIDE_ADMIN_TOKEN_SHA256`（SHA-256 十六进制，逗号分隔可配多个，便于轮换）。所有管理接口要求请求头 `X-Guide-Admin-Token`，常量时间比较。模型密钥不会返回给页面。
- `GET /api/v1/guide-admin/session`：返回是否启用、当前是否已认证、模型名称、提示词版本与启动时间，不需要口令，用于渲染门禁。
- `GET /api/v1/guide-admin/overview`：公开演出覆盖、评分与票档缺失清单、城市与类型数量、最近 20 条咨询（问题、模式、耗时、推荐数与备选数）、模型回退次数、数据更新时间。
- `GET|PUT /api/v1/guide-admin/prompt`：读取 / 保存助手提示词。保存、恢复默认与回滚都必须在请求体里再次携带管理口令（`confirm` 字段，与 `X-Guide-Admin-Token` 常量时间比较），防止离开座位时被顺手改动。保存需要 60–8000 字，版本号递增，写入程序同目录 `guide-prompt.json`；`{"restore":true}` 恢复出厂文本，`{"version":N}` 回滚到历史版本，两者同样需要确认。每次改动记入修改记录（最近 20 条，含版本、时间、动作与全文）并随响应返回保存后的巡检结果，改坏规划或预算折算当场可见；出厂版本不进记录，用「恢复默认」回到初始文本。
- `GET /api/v1/guide-admin/catalog`：城市与票档分析。按城市汇总演出数、可购数、有效评分与均分、评测数、票价区间、票档数与需选座数，并标出缺票档/评测未读完整；类型分布同页给出。票档侧按票档粒度汇总总数、可售、售罄、秒杀、选座/免选座、快照库存、缺退改规则、异常低价与余票总量，按价格带统计占比，并逐档列出演出、城市、价格、余票与状态。票档读取失败或超时不写入缓存，避免把不完整结果当作全量。
- `POST /api/v1/guide-admin/ask`：管理端对话试验台。输入访客会说的话，复用真实的 `resolvePlan` 与筛选链路，返回解析出的人数、总预算、单张预算、日期、城市与座位偏好，命中的演出/票档（含评分样本、缺票档、缺退改规则等标记），以及空结果时的备选。也可以问数据问题：缺票档、缺退改规则、异常低价、评分样本、城市分布、价格带，会走确定性统计而不调用模型。请求体 `{"message":"…","plan":{…},"use_model":false}`，`use_model` 默认关闭。
- `GET|POST|DELETE /api/v1/guide-admin/cases`：自定义巡检用例。POST 传 `{message, expect:["party_size=2",…]}`，支持的键见接口返回的 `keys`；保存到程序同目录 `guide-cases.json`，巡检时与内置 7 条一起执行。
- `POST /api/v1/guide-admin/draft`：粘贴演出描述，模型整理成演出 + 票档草稿 JSON 与 CSV（带 BOM）；只生成草稿，不写入 Echo，录入前需核对。
- 咨询统计持久化在程序同目录 `guide-stats.jsonl`，重启不丢；概览的健康项数量会以红色徽章显示在导航上。城市与票档表格支持一键导出 CSV。
- `POST /api/v1/guide-admin/eval`：用真实演出数据回放 7 条固定需求（另加自定义用例）（人数与总预算折算、城市与类型、人均预算、座位续接、预算不限、口碑排序、历史场次）并逐条给出判断依据。巡检关闭模型，只验证条件解析与数据，失败会指出具体哪一条不成立。

## 选票 Agent

除了 `/assist` 的一次性回答，还提供多步推理入口：

- `POST /api/v1/ticketing/agent`：一次性返回 `answer / mode / intent / steps / items / plan / run_id`；
- `POST /api/v1/ticketing/agent/stream`：SSE，与 `/assist/stream` 一样挂在 65 秒超时包装之外。

请求体与 `/assist` 相同，额外接受 `session_id`（缺省时服务端生成并在响应里返回）。多轮条件由服务端记忆保存，前端只需带同一个 `session_id`。

事件序列：

| 事件 | 内容 |
|---|---|
| `agent_start` | `session_id`、`mode`（`agent-react` / `agent-rule`） |
| `step` | `{index, phase, title, detail, tool, ok, ms}`；phase 为 `planner` / `tool` / `observe` / `check` |
| `delta` | `{source:"agent", text}`，逐字输出 |
| `phase` | `answer`，表示开始成文 |
| `meta` | 跑完后推送：`items`（票务卡片）、`plan`、`intent`、`mode` |
| `final` | 最终 `answer`、`note`、`run_id`、`steps`（步数）、`elapsed_ms` |

工具（全部只读，按意图下发白名单）：

| 工具 | 用途 |
|---|---|
| `search_shows` | 按城市、类型、日期、单张预算、评分、余票筛选 |
| `show_detail` | 单场演出的票档、余票、秒杀、选座、退改规则 |
| `list_reviews` | 评测原文与评分分布，可带关注点做相关度排序 |
| `compare_shows` | 2–4 场并排比较 |
| `venue_tips` | 内置观演常识库（入场、迟到、儿童、选座、交通、礼仪等），会标注「通用建议」 |
| `budget_plan` | 人数 × 票价、剩余预算、是否超支（金额一律由代码算） |
| `add_watchlist` | 写操作：把演出加入观演清单，只生成待确认动作 |
| `save_preference` | 写操作：记住城市/类型/预算/座位偏好，只生成待确认动作 |

护栏：工具白名单、最多 4 步、单步 20 秒、总量 75 秒、单条观察截断 1400 字、同一步协议解析失败 2 次即转规则链；决策上下文超过 6000 字时压缩为工具摘要；答案自检命中「已下单/已支付/已支付成功/已锁座/已出票」时，整段回退为数据版答案并附说明。

写操作采用「提案 → 确认 → 执行」：

| 接口 | 作用 |
|---|---|
| `POST /api/v1/ticketing/agent/actions/{id}/confirm` | 执行待确认动作；已处理过返回 409 与当前状态 |
| `POST /api/v1/ticketing/agent/actions/{id}/reject` | 取消动作 |
| `GET /api/v1/ticketing/agent/actions?profile_id=` | 该设备的提案历史（刷新后恢复卡片） |
| `GET /api/v1/ticketing/watchlist?profile_id=` | 已确认的观演清单 |

写操作与长期偏好按 `profile_id`（设备级，前端存 localStorage）归属；请求不带时退回 `session_id`。提案 10 分钟未确认自动失效，同一目标重复提案会复用现有 pending。状态与审计落 `agent-state.json`，可用 `GUIDE_AGENT_STATE` 覆盖路径。

跑分：`POST /api/v1/guide-admin/agent/bench` 用内置 8 条用例（推荐/口碑/对比/常识/预算/空结果/拒绝代下单/写操作）回放，返回 `passed / tool_hit_rate / intent_rate / avg_steps / avg_elapsed_ms`；用例可通过 `GUIDE_AGENT_BENCH` 指向自定义 JSON 覆盖。

`GET /api/v1/ticketing/config` 的 `agent` 字段会返回 `{enabled, mode, tools[]}`，前端据此显示「多步推理」开关。

管理端（需口令）：

- `GET /api/v1/guide-admin/agent/runs?limit=20`：最近的运行摘要（意图、模式、工具链、耗时、步数）；
- `GET /api/v1/guide-admin/agent/runs/{run_id}`：单次运行的完整步骤与答案。

轨迹文件默认写在程序同目录 `agent-runs.jsonl`，可用 `GUIDE_AGENT_TRACE` 覆盖路径。

## 断点续跑

推理被超时或断连打断时，运行不会作废：每一步决策后都落一份快照（按 `run_id`），包含决策上下文、已查到的事实、走到第几轮。前端在 `final` 事件里收到 `resumable: true`，展示「继续上次推理」按钮。

| 接口 | 作用 |
|---|---|
| `POST /api/v1/ticketing/agent/resume/{run_id}` | 从断点继续（JSON），`/stream` 后缀为 SSE |
| `GET /api/v1/ticketing/agent/checkpoints?profile_id=` | 当前设备可继续的运行列表 |
| `GET /api/v1/guide-admin/agent/checkpoints` | 管理端审计视图（需口令） |

语义约定：

- **续跑 ≠ 重试**：重试从头再来，续跑复用快照里的工具结果，不重复查询（SSE 里会推一条「从断点继续」的 step）；
- **跑完即清**：续跑成功后快照被删除，同一 `run_id` 再次续跑返回 404；
- **快照按设备隔离**：`owner_id` 不匹配的快照不出现在列表里；
- **TTL 30 分钟、上限 50 条**，落盘 `agent-checkpoints.jsonl`，重启后仍可续跑；
- 续跑与首轮共用同一套 finalize 与自检，答案质量不会因为「接着跑」而降级。

`GET /api/v1/ticketing/config` 的 `agent.resume` 返回是否启用，`agent.rag` 返回当前向量器名称。

## 观演常识检索（向量召回 + 重排）

`venue_tips` 工具的检索分三档，按部署自动选择：

| 策略 | 说明 |
|---|---|
| keyword | 纯词面（二元组匹配标题与正文），永远可用的基线 |
| vector | query 与 12 条常识做余弦相似度，召回 top 6 |
| hybrid+rerank | 向量召回后按「标题/别名命中 + 正文命中」加权重排，取前 3 |

向量器两套实现自动切换：配了 `AI_API_KEY` 时走 OpenAI 兼容 `/embeddings`（`AI_EMBEDDING_MODEL` 可覆盖模型名）；没有 Key 时用离线字符 n-gram 符号哈希向量（确定性、零依赖），整条链路仍然可跑。

`POST /api/v1/guide-admin/agent/rag-eval` 在 15 条标注问题上回放三种策略，输出 `recall@k / MRR@k / 平均耗时 / 未命中数`，并列出「基线漏、混合命中」的差异例子。离线向量器下的实测：keyword 0.87 / vector 0.93 / hybrid 1.00（recall@3），MRR 0.73 → 0.97。

## 开发与构建

`build-guide.cmd` 会运行前端类型检查和生产打包，再编译 Go 后端。前端 `npm run build` 保留 Vite 标准构建；`npm run build:direct` 使用同一源代码及 esbuild 打包，适合限制子进程管道的 Windows 环境。

开发模式：后端执行 `go run ./cmd/guide`，前端执行 `npm run dev`。前端默认 `127.0.0.1:5174`，代理到 `127.0.0.1:8082`；可通过前端 `.env.local` 的 `GUIDE_API_TARGET` 覆盖代理地址。

后端测试：`cd backend` 后执行 `go test ./...`（Agent 运行时在 `./src/agent`，导购行为在 `./src/guide`，票务契约与转换在 `./src/ticketing`）。票务接口契约和导购行为用本地测试服务器验证；真实 Echo 验证需设置 `TICKETING_INTEGRATION_TEST=1` 后再跑同一条命令。

## 命名

- Go module：`github.com/LYP-leo/echo-shopping-guide/backend`
- 服务名（健康接口返回）：`echo-shopping-guide`
- 前端包名：`echo-shopping-guide-frontend`
- 后端四个包分工：`src/guide`（导购业务）、`src/ticketing`（Echo 只读数据）、`src/agent`（Agent 运行时）、`src/llm`（模型调用）。
