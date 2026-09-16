# node

模块：`veil.local/node`。通过 core 的公开 API 运行客户端或服务端。

```text
cmd/veild       → internal/compose → core
                         ├───────→ ingress/socks5 → core/transport
                         ├───────→ config
                         └───────→ lifecycle
平台选择文件   → platform/linux 或 platform/windows → 实现 FileReader
cmd/veilctl     → 配置检查、模型生成、私有材料准备、回显探测、版本和能力查询
```

- config 定义自己的 FileReader 接口，解析应用配置并加载模型、身份和信任材料。
- ingress/socks5 处理 SOCKS TCP/UDP，保持半关闭、来源约束和报文边界。它不导入 core/internal。
- lifecycle 在组件失败或取消后等待全部组件退出。
- compose 是具体实现的装配点；host_linux.go、host_windows.go 隔离文件访问和信号处理。
- platform 目前实现安全文件读取。服务注册、TUN、路由修改和持久状态提交尚未加入。

engineering2 增加 Linux `veilctl prepare` 初始双端材料准备、跨平台 `veilctl probe` TCP/UDP 回显探测，以及 `veild --status-interval` 周期状态。prepare 的写入后端在 Linux 中以私有暂存目录和拒绝覆盖的发布操作实现；它不是通用 PrivateStore、安装器或换证事务。Windows 暂未实现准备工具的私有目录写入，能力查询明确报告不支持。

节点配置的可选 `limits.stream_bytes`、`limits.carrier_bytes` 分别控制流和承载的累计字节预算，缺省仍为 8 MiB / 64 MiB；对端按较小值协商。窗口、队列、并发和模型等待语义保持原约束。`configcheck` 输出本地配置报告，不建立网络连接，不声称知道对端协商值。详见 [操作说明](../docs/operations/linux-process.zh.md)。

服务管理是外部适配层：运行与诊断不依赖 systemd、Docker 或 Python。首轮 systemd 集成后续实现，未来 OpenWrt 可接 procd；DNS、路由和保护规则也应单独适配，不将本机 UID 策略当作 LAN 转发策略。

配置见 ../examples。新配置版本为 1，和 core 线路版本分别演进。当前使用 `replace veil.local/core => ../core` 支持仓库内独立构建；对外发布模块时再绑定可获取的正式版本。
