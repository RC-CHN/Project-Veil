# 模块边界与后续并行开发

正式源码位于仓库根目录。当前两个 Go 模块可以独立构建和测试，references 不参与产品构建。

| 工作范围 | 目录 | 对接边界 |
|---|---|---|
| 核心公开原语 | core/endpoint、transport、identity、model、telemetry | 值类型、所有权、取消及错误合同 |
| 会话与承载 | core/internal/session、behavior | 双端模型、身份与执行代次 |
| 流与预算 | core/internal/stream*、datagram、flightwindow | 消费信用、半关闭、报文和槽位生命周期 |
| 运行应用 | node/internal/config、lifecycle、compose | 配置输入、组件启停、公共 core API |
| 应用接入 | node/internal/ingress | transport.Dialer |
| Linux 系统实现 | node/internal/platform/linux | 消费方定义的小接口 |
| Windows 系统实现 | node/internal/platform/windows | 同一接口合同，独立原生验证 |
| 用户界面 | apps/windows、apps/linux | 后续 api/control 协议 |
| 交付与构建 | packaging、tools | 固定版本、源码摘要、产物清单 |

规则：

1. core 不依赖 node、GUI 或平台服务管理。node 不导入 core/internal。
2. 只有 compose 选择具体平台实现；共用业务逻辑通过构造参数获取接口。
3. 基础类型位于独立包，内部实现不反向导入 core 根包。
4. 新接口同时定义取消、关闭、所有权、预算和失败结果，附带有意义的合同测试。
5. 协议、节点配置、控制 API 和安装格式分别标记版本。安装代次不能重置协议预算。
6. 后续目录中的 README 明确表示待实现职责，不代表功能已经可用。

原始设计见 [跨平台原语](cross-platform-primitives.zh.md)。本轮先建立主线与可验证的直接调用接口；复杂平台事务的具体实现按后续模块逐项推进。
