# ClinePassBridge

ClinePassBridge 是 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Cline Pass 插件。它把 Cline Pass API key 接入 CPA 的凭据系统，提供模型别名、Chat Completions 协议适配、真实 SSE 流及请求观测。首版面向 CLIProxyAPI v7.3.12，发布 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 五个平台的动态库。

## 功能

- 在插件管理页导入 Cline Pass API key，凭据交给 CPA 的 `auth-dir` 保存；插件状态目录不保存 key。
- 将客户端模型名映射为 `cline-pass/*` 上游名。未带前缀的名称只补一次前缀；默认别名 `deepseek-flash` 指向 `cline-pass/deepseek-v4.1-flash`，可在管理页维护其他映射。
- 非流式支持 `native`（解包 Cline 原生 `success/data`）、`native-fallback`（原生遇到空内容错误时尝试流式聚合）和 `stream-aggregate`（直接由 SSE 聚合）三种模式。默认 `native-fallback`。
- 流式请求转发为真正的 SSE，处理跨网络分块的事件、用量与终止信号；从上游响应元数据记录实际 provider，缺失时显示“未知”，不根据请求参数猜测。
- 管理页展示凭据、模型映射、请求状态、耗时、用量、实际 provider 和尝试记录。
- 刷新模型先打开选择窗口，确认后才添加所选模型；候选默认使用不带 `cline-pass/` 的客户端名称和目录中的完整上游 ID。之后编辑的名称原样保存，不自动补全或去重前缀。
- 模型添加和编辑使用弹窗，模型删除直接执行。界面跟随同源 CPA 主题，日志提供当前筛选结果的 token 和输入缓存率统计。
- 凭据使用添加与编辑弹窗，支持修改备注和 API key；编辑时留空 key 保持原值，页面不回显旧 key。

## 安装

先在 CPA 配置中启用插件并添加本仓库的市场源。需要 CLIProxyAPI v7.3.12 或兼容的插件 ABI。以下是通用配置片段，按现有配置合并：

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - https://raw.githubusercontent.com/xiao-qiu-qiu/ClinePassBridge/main/marketplace/registry.json
```

CPA 的内置官方市场始终保留；`store-sources` 添加一个额外来源。安装 `v0.1.1` 或更高版本时，在 CPA 的插件市场找到 **ClinePassBridge** 并安装。市场读取 `marketplace/registry.json`，再从本仓库 Release 下载与宿主平台匹配的 ZIP 和 `checksums.txt`，核验 ZIP 的 SHA-256。各 ZIP 根目录分别是 `clinepassbridge.so`（Linux）、`clinepassbridge.dylib`（macOS）或 `clinepassbridge.dll`（Windows）；安装后的文件名带版本，但插件 ID 始终是 `clinepassbridge`。

市场安装会写入插件配置。插件加载后打开：

```text
/v0/resource/plugins/clinepassbridge/console
```

页面优先复用同源 CPA 管理中心通过“记住密码”保存的登录信息，并核对其服务器地址。未保存登录信息、存储不可用或管理中心跨域时，才提示输入 **CPA 管理密钥**；手动输入的密钥仅保存在当前页面内存。插件不会额外持久化管理密钥。CPA 的管理 API 必须已启用；接口鉴权仍由 CPA 控制。进入页面后点“添加凭据”，导入自己的 Cline Pass API key，再检查或选择模型映射。

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

非流式三种模式用于应对 Cline 返回格式和偶发空内容，`native-fallback` 可能产生第二次上游请求。SSE 一旦开始向客户端输出，就不进行透明重试。上游错误、订阅额度与模型可用性仍由 Cline 决定。

CPA v7.3.12 的 Chat Completions 流式接口由宿主封装 SSE，插件提交原始 JSON 并由宿主发送结束标记。标准 `/v1/messages` 路由的 Claude 转换器要求 SSE 输入，插件依据宿主传入的 `request_path` 适配；未携带此元数据的内部 Claude 调用尚未覆盖。

## 从源码构建

项目使用 Go 1.26、标准 C ABI 和 `gopkg.in/yaml.v3`。在仓库根目录执行：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

构建阶段使用 `golang:1.26-bookworm`，并以 `CGO_ENABLED=1`、`-buildmode=c-shared` 编译 `./cmd/passbridge`。输出 `dist/clinepassbridge.so`，目标为 Debian 12 兼容的 Linux amd64 动态库。本地 Windows 无需安装 C 编译器，构建交给 Docker。

[构建工作流](.github/workflows/release.yml) 在普通 push 和 PR 中分别测试并构建五个平台：Linux amd64 使用 Debian 12 Docker 构建，Linux arm64 在 `ubuntu-24.04-arm` 上使用同一 Go 镜像；macOS amd64/arm64 使用原生 `macos-15-intel`/`macos-15`；Windows amd64 使用 `windows-latest` 和 MSYS2 UCRT64 GCC。只有推送与源码版本一致的 `v<version>` 标签且五个作业全部通过时，工作流才汇总发布五个 ZIP 与统一的 `checksums.txt`。资产命名遵循 [CPA 官方插件市场规范](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)。

市场 registry 使用 CPA v7.3.12 的 `schema_version: 1`、`github-release` 安装类型。它是 CPA 商店入口，不是一个可直接安装的 `.so` URL；若使用自己的市场源，也必须托管符合该 schema 的 JSON registry，并提供对应 GitHub Release 资产。

## 许可与来源

本项目按 [MIT License](LICENSE) 发布。ClinePassBridge 为独立实现；[`cline-pass-switcher`](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 只作为公开协议行为的研究参考，没有复制其代码。Cline Pass 与 CLIProxyAPI 分别由各自项目维护。
