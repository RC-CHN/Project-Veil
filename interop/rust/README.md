# Rust 互通验证端

独立实现 [Veil v0.3 线协议](../../PROTOCOL.md)的测试程序，不导入 Go、Veil 核心或其生成文件。密码运算使用 Rust OpenSSL 绑定；T 模式和 R 服务端使用普通 OpenSSL TLS 1.3，R 客户端自行编码最小 TLS 1.3 握手以构造认证 ClientHello。

需要 Rust（已验证 1.96.0）、C 编译器、pkg-config 和 OpenSSL 开发库（已验证 3.5.5）。`selftest` 另需 PATH 中的 `openssl` 命令，以及任意位置的 Veil v0.3 CLI 二进制。可把整个 `interop/rust/` 单独复制后构建：

```sh
cargo build --locked
cargo clippy --locked -- -D warnings
cargo run --locked -- vectors
cargo run --locked -- selftest /absolute/path/to/veil
```

`vectors` 输出规范中的固定 AUTH、exporter、REALITY ClientHello 和证书证明向量。`selftest` 在本机回环地址创建临时参考站点、回显目标和两种语言的协议端；退出时清理子进程和临时配置。无需外部服务器或修改系统代理。运行期间会有预期的 readiness/错误 AUTH 握手关闭日志；以退出码及 JSON `PASS` 结果判断成功。

覆盖范围：

| 方向 | 模式 | 检查 |
| --- | --- | --- |
| Rust 客户端 → Go 服务端 | T、R | 证书证明、CertificateVerify、Finished、exporter AUTH、合并 AUTH/OPEN、双向回显、FIN/DONE、串行复用、8 流并发、600 个 DATA 帧触发 CREDIT、FAILED/RESET 后继续复用、错误 exporter 的 AUTH 被拒 |
| Go 客户端 → Rust 服务端 | T、R | Go 的 Chrome 149 ClientHello 与普通 OpenSSL 的签名协商、AUTH、3 流串行复用、64 B/1 MiB/17 B 完整回显及半关闭 |

这是互通测试端，不是可部署的 Rust 代理。R 客户端仅实现 X25519/AES-128-GCM/SHA-256，不实现浏览器模板、证书压缩、KeyUpdate 或恢复。服务端只连接回环 IPv4 测试目标，没有未认证网站回落和通用双向转发调度；证书和畸形输入验证也不代替完整 TLS/PKI 实现审计。未覆盖的完整互通要求以规范为准。测试不评价抗识别能力或吞吐性能。
