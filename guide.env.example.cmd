@echo off
rem 把本文件复制成 guide.env.cmd，再按本机情况填写；start-guide.cmd 会自动加载它。
set "GUIDE_ADDR=127.0.0.1:8082"
set "TICKETING_API_BASE=http://127.0.0.1:8081"
set "ECHO_WEB_BASE=http://127.0.0.1:3002"

rem 可选：模型接入（OpenAI 兼容协议）。不配置时全站可用，只是回答不做润色。
rem 以 DeepSeek 为例，去掉下面四行的 rem 并填入 Key：
rem set "AI_BASE_URL=https://api.deepseek.com/v1"
rem set "AI_API_KEY="
rem set "AI_SMALL_MODEL=deepseek-chat"
rem set "AI_LARGE_MODEL=deepseek-chat"

rem 可选：Echo 库的只读账号，仅在接口不可用时用于降级查询；平时不要配置。
rem set "TICKETING_DSN=guide_reader:password@tcp(127.0.0.1:3306)/echo?parseTime=true&loc=Asia%2FShanghai"

rem 助手管理端口令：配一个足够长的随机串后，#admin 与页脚入口才会启用。
rem set "GUIDE_ADMIN_TOKEN="
rem 也可以只配 SHA-256 哈希（支持逗号分隔多个，便于轮换），口令本身不落盘：
rem set "GUIDE_ADMIN_TOKEN_SHA256=<64位十六进制>"

rem 可选：Agent 运行期文件路径。默认写在 backend 目录；多实例或测试时改成独立路径。
rem set "GUIDE_AGENT_TRACE=agent-runs.jsonl"
rem set "GUIDE_AGENT_STATE=agent-state.json"
rem set "GUIDE_AGENT_BENCH=agent-bench.json"
