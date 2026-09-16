# core

模块：`veil.local/core`。第一批公开包：

| 包 | 职责 |
|---|---|
| 根包 | Client/Server 的构造、运行与状态 |
| endpoint | 不丢失域名身份的规范目的地 |
| transport | Stream、Association、Dialer 合同 |
| model | 当前版本模型的生成、解析、ID 与文件摘要 |
| identity | 内存中的证书/签名器验证和身份快照 |
| policy | 完整目的地地址集合的策略判定 |
| telemetry | 公共状态、事件及有界缓冲区 |
| contracttest | 下游模块可复用的临时测试身份 |

Client.Run 管理承载；DialStream 和 OpenAssociation 直接对接内层多流，不创建 SOCKS 监听。Server.Serve 接管调用方提供的 listener。构造参数使用内存对象，文件加载属于 node。

```go
client, err := core.NewClient(options)
// 检查 err 后，在受监督的 goroutine 中执行 client.Run(runContext)。
// 等待 client.WaitReady，再通过 client.DialStream 打开业务流。
// Stream.CloseWrite 保留读取方向；退出时取消 runContext 并等待 Run 返回。
```

运行上下文管理流的总生命周期；DialStream 的上下文仅约束建立。UDP 每次调用对应一条报文，接收缓冲区过小时丢弃该条并返回 transport.ErrShortBuffer。

internal/session 从原 objecttransport 迁入，历史版本的内部兼容构造和文件夹具仍保留；当前公开 API 仅开放 Config5 路线。下一步拆分时以原有单测、线路向量和新的直接调用合同为边界，避免一次性改写状态机。

在本目录运行 `go test ./...`；根目录工具可以附加竞态检查和跨平台构建。
