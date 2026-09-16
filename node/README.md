# node

模块：`veil.local/node`。通过 core 的公开 API 运行客户端或服务端。

```text
cmd/veild       → internal/compose → core
                         ├───────→ ingress/socks5 → core/transport
                         ├───────→ config
                         └───────→ lifecycle
平台选择文件   → platform/linux 或 platform/windows → 实现 FileReader
cmd/veilctl     → 配置检查、模型生成、版本和能力查询
```

- config 定义自己的 FileReader 接口，解析应用配置并加载模型、身份和信任材料。
- ingress/socks5 处理 SOCKS TCP/UDP，保持半关闭、来源约束和报文边界。它不导入 core/internal。
- lifecycle 在组件失败或取消后等待全部组件退出。
- compose 是具体实现的装配点；host_linux.go、host_windows.go 隔离文件访问和信号处理。
- platform 目前实现安全文件读取。服务注册、TUN、路由修改和持久状态提交尚未加入。

配置见 ../examples。新配置版本为 1，和 core 线路版本分别演进。当前使用 `replace veil.local/core => ../core` 支持仓库内独立构建；对外发布模块时再绑定可获取的正式版本。
