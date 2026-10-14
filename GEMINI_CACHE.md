# new-api Gemini 显式缓存版

本分支增加 Gemini 缓存的创建、查询、列表、续期和删除，并支持在原生 `generateContent` / `streamGenerateContent` 请求中引用缓存。缓存归属、固定渠道与密钥、创建和存储计费由 new-api 管理。

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
  -ldflags '-X github.com/QuantumNous/new-api/common.Version=v1.0.0-rc.44-gemini-cache.1' \
  -o new-api .
./new-api --port 3000
```

浏览器访问 `http://127.0.0.1:3000` 完成初始化。渠道创建、模型定价和用户管理沿用 new-api 原有界面，缓存生命周期使用下文 API。

## 渠道与价格配置

使用 Gemini 类型渠道。默认 Google 地址 `https://generativelanguage.googleapis.com` 可参与缓存选路；第三方 Gemini 兼容地址需要在渠道 `setting` JSON 内显式加入：

```json
{"gemini_explicit_cache_enabled": true}
```

渠道管理 API 的 `setting` 字段是 JSON 字符串，例如 `"setting": "{\"gemini_explicit_cache_enabled\":true}"`；保存时合并已有配置。该开关不证明第三方支持缓存，需由管理员确认其 `countTokens`、缓存 CRUD 和生成接口兼容。缓存渠道不能设置系统提示词、参数覆盖或请求头覆盖。

除正常生成模型价格外，还必须为客户端使用的模型名配置两个独立计费名。若请求模型为 `gemini-2.5-flash`，在 `billing_setting.billing_mode` 中将以下两项设为 `tiered_expr`，并在 `billing_setting.billing_expr` 中保存对应表达式：

| 计费名 | 表达式 |
| --- | --- |
| `gemini-2.5-flash-cache-create` | `tier("cache_create", p * 0.30 + p * 1.00 * (param("cache_storage_hours") ?? 0))` |
| `gemini-2.5-flash-cache-storage` | `tier("cache_storage", p * 1.00 * (param("cache_storage_hours") ?? 0))` |

**这些数字是假设示例，不是已核实的 Google 官方价格。** 示例假设创建输入为每百万 token 0.30 美元，存储为每百万 token 每小时 1.00 美元；实际使用前应根据所用上游、模型及模态修改。不要额外除以一百万，表达式引擎会完成单位换算，再应用分组倍率。保留 `?? 0`，以通过缺少请求参数时的配置校验。

这两个计费名只用于计价，不要加入渠道模型列表或令牌模型权限。模型映射不改变计费名：以客户端请求名添加后缀。创建费用包含创建输入和本次有效期内的存储费用；续期只收取超过已付费截止时间的部分。缩短有效期或提前删除不退还已付费存储。缺价会拒绝请求；免费也必须显式配置零价表达式。

`p` 取上游实际缓存 token 数；`cache_storage_hours`、`cache_storage_seconds` 和 `cache_operation` 由服务端构造。创建前先用上游 `countTokens` 预扣，创建成功后按返回数量校正。后续生成仍使用正常模型的生成与缓存读取计费配置。

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

## 绑定与限制

- 缓存绑定创建者、new-api 令牌 ID、实际分组、渠道、上游密钥指纹、Base URL 和模型映射。同一用户的另一枚令牌也不能使用；多密钥渠道调整顺序仍能找到原密钥，原密钥移除或禁用后拒绝使用，不能切换密钥或渠道重试。
- 本地托管缓存仅使用返回的 `cachedContents/...` 公开名称，Gemini API Key 渠道不能直接提交 Google 原始资源名。数据库保存资源绑定与用量元数据，不在缓存表保存提示词正文或明文上游密钥。查询和列表读取本地记录，不枚举上游账户的全部缓存。
- 创建默认有效期一小时；可指定 `ttl` 或 RFC3339 `expireTime`，两者不能并存。范围为 1 秒到 24 小时，向上取整至整秒。PATCH 仅允许修改这两个过期字段之一。
- 缓存请求体上限 32 MiB，缓存 token 数上限 2,000,000；上游还可能有更严格的模型、最小 token 数或模态限制。重复 JSON 字段、蛇形别名和服务端管理字段会被拒绝。
- 引用缓存时不能同时发送 `systemInstruction`、`tools` 或 `toolConfig`；这些配置应在创建时确定。托管缓存引用仅支持原生 Gemini 的生成和流式生成，未扩展 OpenAI/Anthropic 协议或客户端缓存 `countTokens`。
- Vertex 的 `projects/.../locations/.../cachedContents/...` 完整资源引用保留既有 Vertex 路由行为；本版本不新增这些缓存的创建、管理和存储计费，生成仍走既有计费流程。
- NewAPI、AdvancedCustom 等其他类型渠道保留外部缓存引用；这些引用不会分配给 Gemini API Key 渠道。若资源名在本地缓存表存在，无论状态或归属如何，均必须通过本地权限校验，不能回退为外部引用。
- 不自动提取公共前缀创建缓存，不做隐式缓存管理、自动续期或跨令牌共享。

## 失败处理与验证边界

创建或续期失败会尽力删除新资源或恢复旧到期时间。补偿结果无法确认时，保留已预留额度，并保持资源不可用，等待人工对账；不能把网络超时当成上游操作一定失败。错误日志沿用 `Request failed`，通过 `other.cache_reconciliation_required=true`、公开 `cached_content` 和操作信息标记；管理员详情包含预留额度、原因和计费快照。

当前没有自动对账任务，也不提供严格的跨进程崩溃恢复或请求幂等保证。遇到上述日志或长期停留在 pending/mutating 状态的记录，应由管理员核对上游资源、额度预留及消费日志；不要直接解锁记录、反复创建或自行退款。

已完成的验证包括模拟上游生命周期、流式/非流式引用、权限隔离、固定密钥、计费及补偿路径，以及真实 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 16.13 的专项验证。该结果不代表已通过真实 Google API 或全部最低数据库版本验证。发布前仍需用独立测试账户核实真实上游的请求格式、可缓存模型、计费结果及异常补偿。
