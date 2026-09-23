# ClinePassBridge C ABI 入口

本目录将 `internal/bridge.Service` 接到 CLIProxyAPI v7.3.12 的原生插件 ABI v1。编译目标是 Linux amd64 共享库：

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o ClinePassBridge.so ./cmd/passbridge
```

`main.go` 导出 `cliproxy_plugin_init`，并向宿主提供 `call`、`free_buffer`、`shutdown` 函数表。所有插件方法原样传给 `Service.Handle(method string, raw json.RawMessage) (any, error)`，返回值序列化为 `{ "ok": true, "result": ... }`。错误转成 `{ "ok": false, "error": { "code": ..., "message": ..., "http_status": ... } }`；服务错误可实现 `StatusCode() int`、`Code() string`、`Retryable() bool` 保留分类。适配器会回收自己分配的 C 内存及宿主回调返回的缓冲区。

`Service.SetHost(func(method string, payload any, out any) error)` 注入宿主回调。它序列化请求、调用宿主函数表，检查返回码和 JSON 信封，再将 `result` 解码到 `out`。业务层可使用 `host.http.do`；真流式路径使用 `host.http.do_stream`、`host.http.stream_read`、`host.http.stream_close`，向客户端输出使用 `host.stream.emit` 和 `host.stream.close`。`executor.execute_stream` 请求包含小写的 `stream_id` 和 `host_callback_id`。异步处理时要保留这两个 ID，并在任何结束路径关闭宿主 HTTP 流和客户端流。字节字段交给 `encoding/json` 编成 base64。

注册结果须返回 `schema_version`、`metadata`、`capabilities`。元数据字段与 SDK Go 结构体保持 `Name`、`Version`、`Author` 等大小写；执行器响应使用 `Payload`、`Headers`，管理处理响应使用 `StatusCode`、`Headers`、`Body`。菜单资源由 `management.register` 的 `resources` 返回，CPA 暴露为 `/v0/resource/plugins/<插件ID>/<资源路径>`；需要管理认证的路由由 `routes` 返回。`management.handle` 请求来自宿主的 Go 结构体 JSON，含 `Method`、`Path`、`Headers`、`Query`、`Body` 与小写 `host_callback_id`。

此入口仅负责 ABI 和信封转换。业务行为、模型路由、HTTP 代理选择、流内容处理及管理页面由 `internal/bridge` 实现。
