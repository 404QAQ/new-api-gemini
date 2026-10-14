# new-api Gemini 自动与显式缓存版

本分支增加 Gemini 缓存的创建、查询、列表、续期和删除，并支持在原生 `generateContent` / `streamGenerateContent` 和 OpenAI `/v1/chat/completions` 请求中引用缓存。`gemini-cache.3` 增加按渠道开启的自动创建与复用：客户端照常发送完整对话，网关管理缓存前缀。缓存归属、固定渠道与密钥、创建和存储计费由 new-api 管理。

截至 2026-10-14，本地验证使用模拟上游；尚未用真实 Google API 验证，也未部署。

## 版本基线

| 对象 | 本次使用或核对的版本 |
| --- | --- |
| new-api 实现基线 | 官方 `main`：[`7de10f2c602b50122017556e8a09cb7c488c273c`](https://github.com/QuantumNous/new-api/commit/7de10f2c602b50122017556e8a09cb7c488c273c) |
| new-api 调研时最新 release | [`v1.0.0-rc.44`](https://github.com/QuantumNous/new-api/releases/tag/v1.0.0-rc.44)，提交 `633b4c5b190dc1af99a2814391354d86948e7943`；上述 main 比它多 5 个提交 |
| sub2api 参考版本 | [`Wei-Shaw/sub2api v0.2.16`](https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.16)，提交 [`01842576b3d5c1e2545b5a99ad555d36e28ed306`](https://github.com/Wei-Shaw/sub2api/commit/01842576b3d5c1e2545b5a99ad555d36e28ed306) |

本实现参考 sub2api 的显式缓存能力，沿用 new-api 的渠道选路、权限、计费表达式和数据库模型。Google 协议说明见 [Context caching](https://ai.google.dev/gemini-api/docs/caching) 与 [CachedContent API](https://ai.google.dev/api/caching)。

## 开源与构建

公开源码仓库：[404QAQ/new-api-gemini](https://github.com/404QAQ/new-api-gemini)。本扩展按原项目的 [AGPL-3.0](./LICENSE) 开源，保留 QuantumNous/new-api 的历史和归属；分发时同时保留 [NOTICE](./NOTICE) 与 [第三方许可说明](./THIRD-PARTY-LICENSES.md)。缓存实现依据接口契约重新编写，没有直接复制 sub2api 源码。

从本仓库构建需要 Go 1.26 和 Bun 1.4，使用已提交的依赖锁文件：

```sh
git clone https://github.com/404QAQ/new-api-gemini.git
cd new-api-gemini
(cd web && bun install --frozen-lockfile && bun run build:check)
GOWORK=off go build -trimpath \
  -ldflags '-X github.com/QuantumNous/new-api/common.Version=v1.0.0-rc.44-gemini-cache.3' \
  -o new-api .
./new-api --port 3000
```

浏览器访问 `http://127.0.0.1:3000` 完成初始化。渠道创建、模型定价和用户管理沿用 new-api 原有界面，缓存生命周期使用下文 API。

## 渠道与价格配置

支持范围如下。这里的“支持”表示已经实现并通过本地模拟协议测试，真实服务的模型可用性、最小 token 数和计费仍需使用测试账号核实。

| 上游 / 入口 | 缓存创建管理与引用 | 配置要求 |
| --- | --- | --- |
| AI Studio / Gemini Developer API | 支持 | Gemini 类型渠道；默认官方地址可直接参与缓存选路 |
| Vertex AI 服务账号 | 支持 | Vertex 类型渠道；服务账号 JSON；明确区域，如 `us-central1` |
| Gemini 原生协议中转 | 支持 | Gemini、OpenAI、NewAPI 或受限的 AdvancedCustom 类型；开启下述兼容开关 |
| OpenAI Chat 客户端 | 支持引用托管缓存 | 用统一缓存接口创建，再传 `extra_body.google.cached_content` |
| Vertex Express API Key | 不支持缓存创建管理 | 当前官方 REST Discovery 没有 Express 顶层 `cachedContents` 接口 |
| 仅提供 `/chat/completions` 的中转 | 不支持缓存创建管理 | 上游必须提供原生 Gemini 的计数、缓存和生成接口 |

Gemini 默认地址 `https://generativelanguage.googleapis.com` 可直接参与缓存选路；第三方地址和 OpenAI / NewAPI / AdvancedCustom 类型需要在渠道 `setting` JSON 内显式加入：

```json
{"gemini_explicit_cache_enabled": true}
```

渠道管理 API 的 `setting` 字段是 JSON 字符串，例如 `"setting": "{\"gemini_explicit_cache_enabled\":true}"`；保存时合并已有配置。该开关不证明第三方支持缓存，需由管理员确认其 `countTokens`、缓存 CRUD 和生成接口兼容。缓存渠道不能设置系统提示词、参数覆盖或请求头覆盖。

Gemini 类型向上游发送 `x-goog-api-key`；OpenAI / NewAPI 类型发送 `Authorization: Bearer <渠道 Key>`。这些兼容渠道的 Base URL 应为 API 根地址（可带网关路径前缀），不要追加 `/v1` 或 `/v1beta`；缓存和生成均走该地址下的 `/v1beta/...`。HTTP/SOCKS 代理沿用渠道代理设置。仅改变类型不会让上游自动支持缓存。

Vertex 服务账号从当前选中凭据取得项目，区域取渠道 `other`。可填写 `us-central1`，也可填写 `{"gemini-2.5-flash":"us-central1","default":"europe-west4"}`；按客户端模型名选区域。缓存使用 Vertex `v1` 项目区域接口，返回的项目数字标识会原样绑定。缺省/global 区域和 Express API Key 会被拒绝。自定义 Vertex Base URL 沿用原项目的规则：填写主机根或以 `/v1` 结尾的根，不填写 project/location。创建后改变项目、区域、凭据、认证模式或地址会使旧绑定不可用。

AdvancedCustom 仅接受匹配当前模型的标准原生生成与流式路由：`/v1beta/models/{model}:generateContent` 和 `/v1beta/models/{model}:streamGenerateContent`，上游路径使用相同模板（流式路由也可映射至 generateContent，由缓存适配器切换流式动作）。认证使用默认 Bearer，或固定的 `Authorization: Bearer {api_key}` / `x-goog-api-key: {api_key}` 请求头模板。任意绝对 URL、协议转换、无认证、查询参数认证或固定其他账号的模板均不受支持。OpenAI Chat 引用托管缓存时，使用这套原生路由。


除正常生成模型价格外，还必须为客户端使用的模型名配置两个独立计费名。若请求模型为 `gemini-2.5-flash`，在 `billing_setting.billing_mode` 中将以下两项设为 `tiered_expr`，并在 `billing_setting.billing_expr` 中保存对应表达式：

| 计费名 | 表达式 |
| --- | --- |
| `gemini-2.5-flash-cache-create` | `tier("cache_create", p * 0.30 + p * 1.00 * (param("cache_storage_hours") ?? 0))` |
| `gemini-2.5-flash-cache-storage` | `tier("cache_storage", p * 1.00 * (param("cache_storage_hours") ?? 0))` |

**这些数字是假设示例，不是已核实的 Google 官方价格。** 示例假设创建输入为每百万 token 0.30 美元，存储为每百万 token 每小时 1.00 美元；实际使用前应根据所用上游、模型及模态修改。不要额外除以一百万，表达式引擎会完成单位换算，再应用分组倍率。保留 `?? 0`，以通过缺少请求参数时的配置校验。

这两个计费名只用于计价，不要加入渠道模型列表或令牌模型权限。模型映射不改变计费名：以客户端请求名添加后缀。创建费用包含创建输入和本次有效期内的存储费用；续期只收取超过已付费截止时间的部分。缩短有效期或提前删除不退还已付费存储。缺价会拒绝请求；免费也必须显式配置零价表达式。

`p` 取上游实际缓存 token 数；`cache_storage_hours`、`cache_storage_seconds` 和 `cache_operation` 由服务端构造。创建前先用上游 `countTokens` 预扣，创建成功后按返回数量校正。后续生成仍使用正常模型的生成与缓存读取计费配置。

## 普通聊天自动创建缓存

自动模式默认关闭。先完成上面的渠道能力和缓存创建价格配置，再通过现有渠道管理 API 合并以下字段到渠道 `setting` JSON（这个 API 字段本身是 JSON 字符串）：

```json
{
  "gemini_explicit_cache_enabled": true,
  "gemini_auto_cache_enabled": true,
  "gemini_auto_cache_min_tokens": 4096,
  "gemini_auto_cache_ttl_seconds": 300
}
```

后两项可省略，默认分别为 **4096 token、300 秒**。最小 token 配置范围为 1–2,000,000，TTL 为 60–86,400 秒；这只是网关策略，上游仍执行自己的模型门槛。官方 Gemini 直连和受支持的 Vertex 渠道不要求 `gemini_explicit_cache_enabled`，第三方地址仍需要它声明原生缓存能力。关闭 `gemini_auto_cache_enabled` 即停止自动创建和使用，已创建资源按其到期时间结束存储，也可以通过显式接口管理。

客户端无需传缓存字段，也无需调用创建接口。下面的请求在稳定历史达到配置门槛时，自动缓存 system 和前两条历史消息，保留最后一个问题用于生成；`stream` 为 `false` 时同样适用：

```json
{
  "model": "gemini-2.5-flash",
  "stream": true,
  "messages": [
    {"role": "system", "content": "固定的系统指令"},
    {"role": "user", "content": "较长的材料或上一轮问题（此示例本身未达到缓存门槛）"},
    {"role": "assistant", "content": "上一轮回答"},
    {"role": "user", "content": "根据这些材料继续回答"}
  ]
}
```

具体行为：

- 缓存完整 system、tools、toolConfig 和历史消息前缀，只在真实用户轮次前切分；工具返回与对应调用保留在同一段，图片内容和思考签名不会被截断。首次只有一个用户问题且没有可缓存 system 时，正常生成。
- 先查找最长的已有匹配前缀。对话新增消息后仍可复用旧前缀，不因每轮历史增长而新建缓存。命中时不自动续期；过期后按新请求重新创建、重新计费。
- 按用户、令牌、实际分组、渠道、当前上游 key 指纹、端点和模型隔离。多 Key 轮询渠道按各自账号分别缓存，不能跨 Key 引用。
- 同一个完整前缀在多个网关进程之间通过数据库竞争，只由一个请求创建；其他并发请求可先正常生成。两个同时首次出现、长度不同的前缀可能分别创建，不保证整个会话只有一个缓存。
- 首次创建会增加计数和创建接口的等待时间。创建与存储费用单独记账；生成费用仍按真实上游 usage 结算。普通生成按完整输入预扣，不能用尚未创建的缓存提前减少预扣。订阅的缓存创建和生成使用独立的预留 ID。
- 自动生成的资源同样能通过 `/v1beta/cachedContents` 查询和管理，displayName 为 `new-api automatic cache`。数据库只保存摘要、绑定、过期时间和计费元数据，不保存提示词正文。

以下情况直接按原请求生成：关闭自动模式、短前缀、缺少缓存创建价格、创建额度不足、不支持缓存的渠道或模型、透传模式、Chat→Responses 路由、系统提示词/参数/请求头覆盖、已经显式引用缓存的请求，以及超出 32 MiB 或 256 条 contents 的候选请求。已有显式引用继续遵守它自己的权限和错误规则。

Gemini/Vertex 的原生生成转换可处理已支持的图文及工具内容。对于平时走 OpenAI 协议的中转，自动模式仅在请求可以保留语义地转换时生效；`n`、logprobs、penalty、未知扩展、多模态 Chat 内容等会保留原路径。AdvancedCustom 还必须具备当前客户端入口对应的正常路由，例如普通 Chat 请求需要 `/v1/chat/completions`，仅配置 Gemini 原生路由不够。原生 `fileData` 外部文件 URI 可能指向被覆盖的内容，因此自动模式跳过这类请求；需要固定文件快照时可显式创建缓存。

创建明确失败会进入短暂冷却。上游明确报告缓存过期或不存在时，网关使用完整原请求在同渠道回退一次；网络结果不明、5xx、HTTP 200 后读取/解析失败或已开始输出的流不会因此重新生成。创建结果或扣费状态不确定时，沿用预留额度和待对账记录，并保留自动创建占用，避免立即再次付费创建。过期一天以上的未发布占用会按小批次清理；已发布资源的元数据继续保留。

自动模式不保证每次节省费用，短时间内复用长前缀才可能抵消创建和存储成本。它不会让仅支持 Chat 的中转获得真实 Gemini 缓存，也不会开放本版本未支持的 Vertex Express/global。Google 当前文档说明 Gemini 4 Argon 及后续模型不支持显式缓存，它们继续使用上游的隐式缓存；本功能面向支持 `cachedContents` 的模型。依据：[generateContent 缓存文档](https://ai.google.dev/gemini-api/docs/generate-content/caching)（2026-10-14 核对）。

## API 示例

示例依赖 `curl` 和 `jq`。先启动本地 new-api、完成上述配置，并准备 `context.txt`；内容须达到所用上游模型的最低缓存 token 要求。`NEW_API_TOKEN` 是 new-api 令牌，不是 Google 密钥。

```sh
export NEW_API_BASE='http://127.0.0.1:3000'
export NEW_API_TOKEN='<替换为 new-api 令牌>'
export GEMINI_MODEL='gemini-2.5-flash'

# 创建：缓存静态上下文一小时，保存响应中的公开资源名。
CACHE_NAME="$(
  jq -n --arg model "models/$GEMINI_MODEL" --rawfile text context.txt \
    '{model:$model, displayName:"demo", contents:[{role:"user", parts:[{text:$text}]}], ttl:"3600s"}' |
  curl --fail-with-body -sS "$NEW_API_BASE/v1beta/cachedContents" \
    -H "x-goog-api-key: $NEW_API_TOKEN" -H 'Content-Type: application/json' \
    --data-binary @- |
  jq -er '.name'
)"

# 生成：只发送新的问题，并引用创建响应中的 name。
jq -n --arg cache "$CACHE_NAME" \
  '{cachedContent:$cache, contents:[{role:"user", parts:[{text:"请概括以上材料的重点"}]}]}' |
curl --fail-with-body -sS "$NEW_API_BASE/v1beta/models/$GEMINI_MODEL:generateContent" \
  -H "x-goog-api-key: $NEW_API_TOKEN" -H 'Content-Type: application/json' \
  --data-binary @-

# 查询单项与列表；列表仅包含当前令牌可访问、未过期的本地记录。
curl --fail-with-body -sS "$NEW_API_BASE/v1beta/$CACHE_NAME" \
  -H "x-goog-api-key: $NEW_API_TOKEN"
curl --fail-with-body -sS "$NEW_API_BASE/v1beta/cachedContents?pageSize=50" \
  -H "x-goog-api-key: $NEW_API_TOKEN"

# 续期：将到期时间设为现在之后两小时，并非在原到期时间上再加两小时。
curl --fail-with-body -sS -X PATCH \
  "$NEW_API_BASE/v1beta/$CACHE_NAME?updateMask=ttl" \
  -H "x-goog-api-key: $NEW_API_TOKEN" -H 'Content-Type: application/json' \
  --data '{"ttl":"7200s"}'

# 删除。
curl --fail-with-body -sS -X DELETE "$NEW_API_BASE/v1beta/$CACHE_NAME" \
  -H "x-goog-api-key: $NEW_API_TOKEN"
```

流式生成把生成 URL 改为 `:streamGenerateContent?alt=sse`，并给 curl 加 `-N`。分页时将列表响应的 `nextPageToken` 原样传入下一次请求的 `pageToken`。

同一个缓存也可从 OpenAI Chat 入口引用；创建、续期和删除仍使用上面的统一 `/v1beta/cachedContents` 接口：

```sh
jq -n --arg model "$GEMINI_MODEL" --arg cache "$CACHE_NAME" \
  '{model:$model, messages:[{role:"user",content:"请概括以上材料的重点"}], extra_body:{google:{cached_content:$cache}}}' |
curl --fail-with-body -sS "$NEW_API_BASE/v1/chat/completions" \
  -H "Authorization: Bearer $NEW_API_TOKEN" -H 'Content-Type: application/json' \
  --data-binary @-
```

将 JSON 中的 `stream` 设为 `true` 即返回 OpenAI 格式 SSE。OpenAI Python SDK 会展平调用参数 `extra_body`，所以 SDK 写法为 `extra_body={"extra_body":{"google":{"cached_content": CACHE_NAME}}}`；实际 HTTP JSON 使用上例的一层 `extra_body`。不要再次发送 system/developer 消息、tools/functions 或工具选择配置，这些不可变信息应在创建缓存时确定。工具调用结果可以作为新的对话内容继续发送。

## 绑定与限制

- 缓存绑定创建者、new-api 令牌 ID、实际分组、渠道、上游密钥指纹、Base URL 和模型映射。同一用户的另一枚令牌也不能使用；多密钥渠道调整顺序仍能找到原密钥，原密钥移除或禁用后拒绝使用，不能切换密钥或渠道重试。
- 本地托管缓存仅使用返回的 `cachedContents/...` 公开名称，Gemini API Key 渠道不能直接提交 Google 原始资源名。数据库保存资源绑定与用量元数据，不在缓存表保存提示词正文或明文上游密钥。查询和列表读取本地记录，不枚举上游账户的全部缓存。
- 创建默认有效期一小时；可指定 `ttl` 或 RFC3339 `expireTime`，两者不能并存。范围为 1 秒到 24 小时，向上取整至整秒。PATCH 仅允许修改这两个过期字段之一。
- 缓存请求体上限 32 MiB，缓存 token 数上限 2,000,000；上游还可能有更严格的模型、最小 token 数或模态限制。重复 JSON 字段、蛇形别名和服务端管理字段会被拒绝。
- 引用缓存时不能同时发送 `systemInstruction`、`tools` 或 `toolConfig`；这些配置应在创建时确定。托管缓存引用支持原生 Gemini 和 OpenAI Chat 的普通/流式生成，未扩展 Anthropic、Responses 或客户端缓存 `countTokens`。渠道或全局的请求体透传、Chat→Responses 设置不能绕过托管缓存的原生转换与最终校验。
- Vertex 通过本地缓存接口创建时，对客户端仍返回不透明的 `cachedContents/...` 名称，服务端转换为真实项目区域资源名。原生入口直接传入外部 Vertex 完整资源名仍保留既有路由行为，但不由本功能负责创建管理或存储计费。
- 原生入口在 NewAPI、AdvancedCustom 等其他类型渠道保留外部缓存引用；这些引用不会分配给 Gemini API Key 渠道。若资源名在本地缓存表存在，无论状态或归属如何，均必须通过本地权限校验，不能回退为外部引用。OpenAI Chat 入口只接受当前令牌创建的本地托管缓存。
- 自动前缀管理须按渠道开启，见上文；不管理 Google 隐式缓存，不做自动续期或跨令牌共享。

## 失败处理与验证边界

创建或续期失败会尽力删除新资源或恢复旧到期时间。补偿结果无法确认时，保留已预留额度，并保持资源不可用，等待人工对账；不能把网络超时当成上游操作一定失败。错误日志沿用 `Request failed`，通过 `other.cache_reconciliation_required=true`、公开 `cached_content` 和操作信息标记；管理员详情包含预留额度、原因和计费快照。

当前没有自动对账任务，也不提供严格的跨进程崩溃恢复或请求幂等保证。遇到上述日志或长期停留在 pending/mutating 状态的记录，应由管理员核对上游资源、额度预留及消费日志；不要直接解锁记录、反复创建或自行退款。

已完成的验证包括模拟上游生命周期、流式/非流式引用、权限隔离、固定密钥、计费及补偿路径，以及真实 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 16.13 的专项验证。该结果不代表已通过真实 Google API 或全部最低数据库版本验证。生产使用前仍需用独立测试账户核实真实上游的请求格式、可缓存模型、计费结果及异常补偿。

自动模式另覆盖五类 provider 的原生/OpenAI Chat 普通和流式生成、历史增长复用、同前缀并发去重、过期重建、其他令牌隔离、缺价/短前缀/拒绝/不确定创建回退、转换有损跳过和响应失败禁止重试。数据库验证包括从 `gemini-cache.2` 的已有缓存表升级、重复迁移不改变现有数据、创建竞争和清理，以及钱包/订阅的缓存与生成独立扣费。

`gemini-cache.3` 验证命令如下。三库专项使用本地隔离测试库：模型测试读取 `TEST_MYSQL_DSN` / `TEST_POSTGRES_DSN`，计费测试读取 `TEST_FIXED_MYSQL_DSN` / `TEST_FIXED_POSTGRES_DSN`。版本为 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 16.13；未配置这些环境变量时外部数据库用例会跳过，不能将跳过视为通过。

```sh
go test ./...
go vet ./...
go test -race ./router ./relay ./service -run 'TestGeminiAutoCache' -count=1
go test -v -race ./model ./service -run 'TestGeminiCachedContentDatabase|TestGeminiAutoCacheSlotValidation|TestGeminiCacheBillingDatabaseMatrix' -count=1
(cd relaykit && GOWORK=off go build ./... && GOWORK=off go test ./dto -run 'TestChannelSettingsValidateGeminiAutoCache' -count=1)
```

## 本次扩展的协议依据

Vertex 计数请求直接包含 `contents` / `systemInstruction` / `tools`，不使用 AI Studio 的 `generateContentRequest` 包装；缓存创建模型名使用完整的项目、区域和 publisher 路径。OAuth token 缓存按凭据指纹隔离，渠道多 Key 重排不会命中另一账号的旧 token；OAuth 使用固定 Google 地址、强制证书验证和禁止重定向。

- [Vertex 缓存服务](https://github.com/googleapis/googleapis/blob/master/google/cloud/aiplatform/v1/gen_ai_cache_service.proto)
- [Vertex countTokens 契约](https://github.com/googleapis/googleapis/blob/master/google/cloud/aiplatform/v1/prediction_service.proto)
- [Vertex REST Discovery](https://github.com/googleapis/google-api-go-client/blob/main/aiplatform/v1/aiplatform-api.json)
- [Gemini OpenAI compatibility](https://ai.google.dev/gemini-api/docs/openai)
- [Vertex OpenAI 缓存示例](https://github.com/GoogleCloudPlatform/generative-ai/blob/main/gemini/chat-completions/intro_chat_completions_api.ipynb)

认证变更参照 OWASP Authentication、Session Management、OAuth2 Cheat Sheets 和 ASVS 5.0 的 JWT、TLS 相关控制，回归覆盖 token 身份隔离、过期、重定向、全局 insecure 配置、敏感信息脱敏及取消；这不表示整个项目已经完成 ASVS 合规认证。
