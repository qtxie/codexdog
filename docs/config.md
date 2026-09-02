• 当前版本 0.6.1 的 .codexdog 配置文件只在 codexdog start 时读取，位置是 -C 指定目录下的 .codexdog。推荐使用 TOML。

  ### 通用配置

   配置项                        类型          默认值 / 说明
  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━  ━━━━━━━━━━━━  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
   version                       整数          可选，目前只能是 1
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   codex                         字符串        codex，Codex 可执行文件
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   codex_config                  字符串数组    对应重复的 -c KEY=VALUE
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   state_dir                     字符串        状态和日志目录；默认使用平台目录
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   health_url                    字符串        旧版 HTTP 2xx 健康检查 URL；不能和 [health] 同时使用
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   probe_model                   字符串        恢复探测使用的模型；默认当前 Codex 模型
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   probe_timeout_ms              正整数        120000
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   error_grace_ms                正整数        5000
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   probe_successes               正整数        2，恢复前需要连续成功的探测次数
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   backoff_ms                    整数数组      [2000, 5000, 10000, 20000, 30000, 60000]
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   max_auto_resumes              正整数        5
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   stall_timeout_ms              非负整数      0，普通静默超时；0 表示禁用
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   stall_confirm_ms              正整数        30000
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   stall_interrupt_timeout_ms    正整数        15000
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   max_stall_resumes             正整数        2
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   tool_stall_timeout_ms         非负整数      0，工具执行静默超时；0 表示禁用
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   options                       字符串数组    原始 Codexdog 参数，放在 -- 之前
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   args                          字符串数组    完整参数数组，可包含 --
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   tui_args                      字符串数组    传给 Codex TUI 的参数，等同 -- 后的内容
  ────────────────────────────  ────────────  ──────────────────────────────────────────
   session                       字符串        telegram.alias 的别名

  ### 健康检查配置

  推荐使用 `[health]` 和 `[[health.sources]]` 配置能够识别模型状态的状态页：

  ```toml
  probe_model = "gpt-5.6-sol"

  [health]
  policy = "any"                 # any 或 all
  unknown_policy = "canary"      # canary 或 block
  max_age_ms = 180000

  [[health.sources]]
  type = "uptime_kuma"
  url = "https://status.ciii.club/status/codex"
  name = "ciii"
  # model = "gpt-5.6-sol"        # 默认自动使用 probe_model 或当前线程模型
  # provider = "ciii"            # 可选，仅匹配这个 Codex model_provider
  ```

  Input.im 的配置为：

  ```toml
  [health]
  policy = "any"
  unknown_policy = "canary"
  max_age_ms = 180000

  [[health.sources]]
  type = "input_im"
  url = "https://status.input.im/"
  ```

  `type` 支持 `http`、`uptime_kuma` 和 `input_im`。`http` 保留旧版 HTTP
  状态码语义；另外两种类型会读取 JSON API、选择目标模型并检查数据新鲜度。
  `healthy` 只允许继续执行真实 Codex canary，不会直接恢复线程；`unhealthy`
  会跳过本次 canary；`unknown` 默认继续执行 canary，避免第三方状态页故障阻塞恢复。

  多个 source 只有在它们监控同一条 provider 路由时才应一起使用。`policy =
  "any"` 表示任一 source 健康即可，全部明确不健康时才阻止；`policy = "all"`
  要求全部健康。source 的 `model` 优先级高于 `probe_model`，后者优先级高于当前
  失败线程的实际模型。

  ### Telegram 配置

  放在 [telegram] 表中：

   配置项              类型           默认值 / 说明
  ━━━━━━━━━━━━━━━━━━  ━━━━━━━━━━━━━  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
   token               字符串         Bot Token
  ──────────────────  ─────────────  ────────────────────────────────
   token_file          字符串         Token 文件路径，推荐使用
  ──────────────────  ─────────────  ────────────────────────────────
   chat_id             整数           单个允许的聊天 ID
  ──────────────────  ─────────────  ────────────────────────────────
   chat_ids            整数数组       允许的聊天 ID 列表
  ──────────────────  ─────────────  ────────────────────────────────
   user_id             整数           单个允许的用户 ID
  ──────────────────  ─────────────  ────────────────────────────────
   user_ids            整数数组       允许的用户 ID 列表
  ──────────────────  ─────────────  ────────────────────────────────
   poll_timeout_sec    1-50 的整数    30
  ──────────────────  ─────────────  ────────────────────────────────
   no_notify           布尔值         true 禁用主动通知
  ──────────────────  ─────────────  ────────────────────────────────
   notify              布尔值         false 等同于 no_notify = true
  ──────────────────  ─────────────  ────────────────────────────────
   alias               字符串         多会话 Telegram Hub 中的会话名
  ──────────────────  ─────────────  ────────────────────────────────
   session             字符串         alias 的别名
  ──────────────────  ─────────────  ────────────────────────────────
   disabled            布尔值         true 禁用 Telegram 配置

  配置了 Token 时，至少需要一个 chat_id 或 chat_ids。chat_id 与 chat_ids、user_id 与 user_ids 可以同时使用，值会合并。

  ### WeChat 配置

  放在 [wechat] 表中：

   配置项               类型           默认值 / 说明
  ━━━━━━━━━━━━━━━━━━━  ━━━━━━━━━━━━━  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
   user_ids             字符串数组     允许的 iLink 用户 ID
  ───────────────────  ─────────────  ───────────────────────────────
   poll_timeout_sec     1-50 的整数    35
  ───────────────────  ─────────────  ───────────────────────────────
   login_timeout_sec    正整数         480
  ───────────────────  ─────────────  ───────────────────────────────
   no_browser           布尔值         true 时登录不自动打开浏览器
  ───────────────────  ─────────────  ───────────────────────────────
   no_notify            布尔值         true 禁用主动通知
  ───────────────────  ─────────────  ───────────────────────────────
   notify               布尔值         false 等同于 no_notify = true
  ───────────────────  ─────────────  ───────────────────────────────
   disabled             布尔值         true 禁用 WeChat 控制器

  ### 完整示例

  version = 1

  codex = "codex"
  codex_config = [
    'model="gpt-5.6-sol"',
    'model_reasoning_effort="high"',
  ]

  # 相对路径相对于 .codexdog 所在目录
  # state_dir = ".codexdog-state"

  probe_model = "gpt-5.6-sol"
  probe_timeout_ms = 120000
  error_grace_ms = 5000
  probe_successes = 2
  backoff_ms = [2000, 5000, 10000, 20000, 30000, 60000]
  max_auto_resumes = 5

  stall_timeout_ms = 0
  stall_confirm_ms = 30000
  stall_interrupt_timeout_ms = 15000
  max_stall_resumes = 2
  tool_stall_timeout_ms = 0

  # 二选一即可
  tui_args = ["resume", "-s", "danger-full-access"]
  # args = ["--probe-timeout-ms", "120000", "--", "resume"]

  [health]
  policy = "any"
  unknown_policy = "canary"
  max_age_ms = 180000

  [[health.sources]]
  type = "uptime_kuma"
  url = "https://status.ciii.club/status/codex"
  name = "ciii"

  [telegram]
  alias = "api"
  token_file = "secrets/telegram-token.txt"
  chat_ids = [-1001234567890]
  user_ids = [123456789]
  poll_timeout_sec = 30
  notify = true

  [wechat]
  user_ids = ["your-ilink-user-id"]
  poll_timeout_sec = 35
  login_timeout_sec = 480
  notify = true

  ### 顶层兼容写法

  Telegram 也支持以下顶层键：

  telegram_token = "..."
  telegram_token_file = "secrets/telegram-token.txt"
  telegram_chat_id = 123456789
  telegram_chat_ids = [123456789]
  telegram_user_id = 123456789
  telegram_user_ids = [123456789]
  telegram_poll_timeout_sec = 30
  telegram_no_notify = true
  telegram_alias = "api"
  telegram_disabled = true

  WeChat 支持：

  wechat_user_ids = ["user-id"]
  wechat_poll_timeout_sec = 35
  wechat_login_timeout_sec = 480
  wechat_no_browser = true
  wechat_no_notify = true
  wechat_disabled = true

  另外还支持 JSON 对象、JSON 参数数组和逐行 CLI 参数文件；JSON 使用 camelCase，例如 codexConfig、probeTimeoutMs、telegram.chatIds。

  配置优先级为：内置默认值 < 环境变量 < .codexdog < 命令行参数。相对的 state_dir、telegram.token_file 和带路径的 codex 会相对于配置文件目
  录解析。配置文件不能设置 -C 或 --cwd，最大大小为 128 KiB。
