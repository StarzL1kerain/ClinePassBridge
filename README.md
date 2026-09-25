# ClinePassBridge

ClinePassBridge 是 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Cline Pass 插件。它把 Cline Pass API key 接入 CPA 的凭据系统，提供模型别名、Chat Completions 协议适配、真实 SSE 流及请求观测。首版面向 CLIProxyAPI v7.3.12，发布 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 五个平台的动态库。

> **本仓库说明**：本仓库是 [xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) 的独立维护分支（fork），不向上游提交合并请求，两边各自演进。相对上游新增/改进：
>
> - Cline Pass **账号登录**（WorkOS 设备码流程）与令牌自动续期；
> - **额度上报**（依赖宿主的插件额度能力；管理面板需打补丁，见下方「额度」一节）；
> - 插件错误提示中文化。
>
> 仓库公开、按 MIT 协议授权，谁想用谁用。

## 功能

- 支持两种凭据：**Cline Pass 账号登录**和 **API key**。账号登录走 WorkOS 设备码流程（与 Cline CLI 的 `cline auth` 同一条链路），在 CPA 的凭据管理里发起登录后打开页面给出的链接完成授权即可，插件自行换取并续期令牌；API key 仍可在插件管理页导入。
- 凭据交给 CPA 的 `auth-dir` 保存；插件状态目录不保存凭据。
- 通过 CPA 的额度能力展示 Cline Pass 的套餐、余额与三个滚动窗口的额度上限（**管理面板需支持插件额度**，官方版尚未支持，见下方「额度」一节），也可用插件自带的 `GET /v0/management/clinepassbridge/quota` 直接查看。
- 将客户端模型名映射为指定上游 ID，用户填写的两个名称均原样保存；默认别名 `deepseek-flash` 指向 `cline-pass/deepseek-v4.1-flash`，可在管理页维护其他映射。
- 非流式支持 `native`（解包 Cline 原生 `success/data`）、`native-fallback`（原生遇到空内容错误时尝试流式聚合）和 `stream-aggregate`（直接由 SSE 聚合）三种模式。默认 `stream-aggregate`，直接聚合上游 SSE 后返回 JSON，跳过原生非流式尝试。
- 流式请求转发为真正的 SSE，处理跨网络分块的事件、用量与终止信号；从上游响应元数据记录实际 provider，缺失时显示“未知”，不根据请求参数猜测。
- 管理页展示凭据、模型映射、请求状态、耗时、用量、实际 provider 和尝试记录。
- “添加模型”弹窗内可获取上游模型；已有的默认映射自动勾选，取消勾选后应用会删除该映射。仅当客户端名称与上游 ID 均完全匹配候选默认值时参与同步，自定义映射保持不变；同名但不同上游的候选会提示冲突。确认时若其他页面已经修改映射，会提示重新获取，避免覆盖。可取消全部默认映射；候选默认使用不带 `cline-pass/` 的客户端名称和目录中的完整上游 ID。之后编辑的名称原样保存，不自动补全或去重前缀。
- 模型添加和编辑使用弹窗，模型删除直接执行。界面跟随同源 CPA 主题，日志提供当前筛选结果的 token 和输入缓存率统计。
- 凭据使用添加与编辑弹窗，支持修改备注和 API key；编辑时留空 key 保持原值，页面不回显旧 key。

## 凭据

两种凭据在请求时都表示为一个 `Authorization: Bearer` 值，因此模型、流式转发与日志行为完全一致，区别只在获取与续期方式：

- **API key**：长期静态凭据，插件不续期，`NextRefreshAfter` 固定为一年后。
- **账号登录**：access token 是有效期约一小时的 JWT，refresh token 长期有效。插件在请求前若发现令牌进入 5 分钟续期窗口会先行续期，同时把续期时间交给 CPA，由宿主按需调用 `auth.refresh`。续期失败不会中断请求，真实失败会以上游错误记入请求日志。

上游只接受带 `workos:` 前缀的令牌；该前缀是 Cline 的存储标记，插件会原样保留。这两点以及续期接口的请求形状都由真实账号实测确认。

## 额度

额度来自三个只读元数据接口，均以同一枚令牌访问，实测只需三次请求：

| 接口 | 用途 |
|---|---|
| `/users/me/plan` | 套餐名、当前周期、三个滚动窗口的上限 |
| `/users/me/plan/usage-limits` | 三个窗口的 `percentUsed` 与 `resetsAt`（已用比例由上游直接给出） |
| `/users/{id}/balance` | 余额 |

两个金额单位不同，均已实测确认：`balance` 的单位是 **1e-6 美元**（网页端把 `-32221` 显示为 `Credits: -0.0322`），`costUsd` 与 `inferenceCapThreshold` 的单位是 **1e-8 美元**（三个窗口上限换算后为 10 / 25 / 50 美元）。

窗口以额度桶（bucket）形式上报，`remainingFraction = 1 - percentUsed/100`，取值 **0~1 的小数**（不是百分数）。
**宿主会丢弃没有有效 `remainingFraction` 的桶**，所以这个字段不能省。

`quota.reset` 一律返回失败，因为 Cline 未提供重置额度的接口。

### 在管理面板里查看（注意面板版本）

插件按 CPA 的额度能力上报（`quota_provider: true`），宿主侧完整支持。但**官方管理面板目前不支持插件额度**——
它把额度分派写死在 7 个内置厂商上，没有插件分支（最新版 v1.24.2 实测同样如此）。
因此在**未打补丁**的面板上，凭据卡片上不会出现额度区块，也不会报错。

两条路：

1. **不依赖面板**：直接 `GET /v0/management/clinepassbridge/quota`，或打开控制台页面查看。
2. **让面板显示**：给管理面板套用插件额度补丁，卡片上即出现「点击此处刷新额度」，
   配额管理页多出「插件」分组。现成产物见 `../panel-plugin-quota/`，
   原理与部署步骤见 [`../docs/04-管理面板-额度显示与部署.md`](../docs/04-管理面板-额度显示与部署.md)。

补丁版面板会把 `window` token 翻译成标签，识别 `five_hour` / `weekly` / `seven_day` / `monthly`，
其余 token 原样显示；未识别时不会丢数据，只是标签不好看。

## 安装

先在 CPA 配置中启用插件并添加本仓库的市场源。需要 CLIProxyAPI v7.3.12 或兼容的插件 ABI。以下是通用配置片段，按现有配置合并：

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - https://raw.githubusercontent.com/StarzL1kerain/ClinePassBridge/main/marketplace/registry.json
```

CPA 的内置官方市场始终保留；`store-sources` 添加一个额外来源。安装 `v0.1.1` 或更高版本时，在 CPA 的插件市场找到 **ClinePassBridge** 并安装。市场读取 `marketplace/registry.json`，再从本仓库 Release 下载与宿主平台匹配的 ZIP 和 `checksums.txt`，核验 ZIP 的 SHA-256。各 ZIP 根目录分别是 `clinepassbridge.so`（Linux）、`clinepassbridge.dylib`（macOS）或 `clinepassbridge.dll`（Windows）；安装后的文件名带版本，但插件 ID 始终是 `clinepassbridge`。

市场安装会写入插件配置。插件加载后打开：

```text
/v0/resource/plugins/clinepassbridge/console
```

页面优先复用同源 CPA 管理中心通过“记住密码”保存的登录信息，并核对其服务器地址。未保存登录信息、存储不可用或管理中心跨域时，才提示输入 **CPA 管理密钥**；手动输入的密钥仅保存在当前页面内存。插件不会额外持久化管理密钥。CPA 的管理 API 必须已启用；接口鉴权仍由 CPA 控制。进入页面后点“添加凭据”，导入自己的 Cline Pass API key，再检查或选择模型映射。

也可以直接在 CPA 的凭据管理里对该 provider 发起登录：插件会返回一个设备码授权链接，打开并完成授权后登录即自动完成。授权链接由 `authkit.cline.bot` 提供，链接中已包含验证码，无需手工输入。

插件配置与请求记录默认写在 `plugins/clinepassbridge-data`；凭据文件写在 CPA 配置的 `auth-dir`。使用容器时，应分别持久化这两个目录及插件目录。备份时也应覆盖这两处数据。

## 模型与路由

客户端调用 CPA 的 OpenAI 兼容接口时可使用 `deepseek-flash`、`deepseek-v4.1-flash` 或 `cline-pass/deepseek-v4.1-flash`。首版默认三者都映射到 `cline-pass/deepseek-v4.1-flash`。其他模型需先在管理页添加映射，并确认 Cline Pass 账号有该模型的使用资格；模型目录出现某个 ID 不代表订阅可调用。

```json
{
  "model": "deepseek-flash",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

日志中的实际 provider 来自上游响应；未回报时显示“未知”。

非流式三种模式用于应对 Cline 返回格式和偶发空内容，推荐的 `stream-aggregate` 从第一次请求就使用流式上游，客户端仍收到非流式 JSON；它不会消除上游本身的错误。可选的 `native-fallback` 可能产生第二次上游请求。SSE 一旦开始向客户端输出，就不进行透明重试。上游错误、订阅额度与模型可用性仍由 Cline 决定。

CPA v7.3.12 的 Chat Completions 流式接口由宿主封装 SSE，插件提交原始 JSON 并由宿主发送结束标记。标准 `/v1/messages` 路由的 Claude 转换器要求 SSE 输入，插件依据宿主传入的 `request_path` 适配；未携带此元数据的内部 Claude 调用尚未覆盖。

## 从源码构建

项目使用 Go 1.26、标准 C ABI 和 `gopkg.in/yaml.v3`。在仓库根目录执行：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

构建阶段使用 `golang:1.26-bookworm`，并以 `CGO_ENABLED=1`、`-buildmode=c-shared` 编译 `./cmd/passbridge`。输出 `dist/clinepassbridge.so`，目标为 Debian 12 兼容的 Linux amd64 动态库。本地 Windows 无需安装 C 编译器，构建交给 Docker。

[构建工作流](.github/workflows/release.yml) 在普通 push 和 PR 中分别测试并构建五个平台：Linux amd64 使用 Debian 12 Docker 构建，Linux arm64 在 `ubuntu-24.04-arm` 上使用同一 Go 镜像；macOS amd64/arm64 使用原生 `macos-15-intel`/`macos-15`；Windows amd64 使用 `windows-latest` 和 MSYS2 UCRT64 GCC。只有推送与源码版本一致的 `v<version>` 标签且五个作业全部通过时，工作流才汇总发布五个 ZIP 与统一的 `checksums.txt`。资产命名遵循 [CPA 官方插件市场规范](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)。

市场 registry 使用 CPA v7.3.12 的 `schema_version: 1`、`github-release` 安装类型。它是 CPA 商店入口，不是一个可直接安装的 `.so` URL；若使用自己的市场源，也必须托管符合该 schema 的 JSON registry，并提供对应 GitHub Release 资产。

## 版本与开发记录

本文档描述 **v0.1.4** 的功能集：除早先的 API key 接入、模型映射与流式转发外，
v0.1.4 新增了**账号登录（WorkOS 设备码）**与**额度上报**。

接入新供应商所需的宿主契约、实证事实与坑位清单，整理在 `docs/` 下（与插件仓库同级）：

| 文档 | 内容 |
|---|---|
| [`docs/README.md`](../docs/README.md) | 索引与一页速查（新增供应商四阶段） |
| [`docs/01-宿主契约-CPA插件ABI.md`](../docs/01-宿主契约-CPA插件ABI.md) | 能力键、方法分派、登录/额度/管理 API 的字段级契约 |
| [`docs/02-开发手册-新增供应商.md`](../docs/02-开发手册-新增供应商.md) | 四阶段 checklist、安全基线、验收清单 |
| [`docs/03-实证记录-ClinePass.md`](../docs/03-实证记录-ClinePass.md) | WorkOS 四步、令牌形状、金额单位、逆向方法 |
| [`docs/04-管理面板-额度显示与部署.md`](../docs/04-管理面板-额度显示与部署.md) | 面板额度原理、构建部署、缓存坑与回滚 |

> `docs/` 目前位于插件仓库同级目录。若要随仓库一起发布，把它移入仓库根目录即可，上表中的相对链接无需改动。

## 许可与来源

本项目按 [MIT License](LICENSE) 发布。ClinePassBridge 为独立实现；[`cline-pass-switcher`](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 只作为公开协议行为的研究参考，没有复制其代码。Cline Pass 与 CLIProxyAPI 分别由各自项目维护。
