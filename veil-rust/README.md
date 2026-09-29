# 实验性 Rust 核心

独立的 **Veil v0.3 / T** 实现：可复用异步库与可运行代理程序，使用 Tokio、rustls/ring；不调用 Go、不依赖 `interop/rust` 的最小 TLS 编码器。

- TLS 1.3 身份验证、exporter 绑定 AUTH、AUTH 与首个 OPEN 合并。
- 每会话最多 8 条双向流，256 帧流控窗口，按流背压与轮转调度；发送队列有界，接收内存按需分配。
- 范围随机化的调度份额、启动切分、CREDIT 批次；128 KiB 写合并，不等待未来数据来凑批次。
- 持久连接池；并发达到 4 条或已有大流时优先建立第二条通道，超过单会话容量继续扩展。
- FIN / DONE / RESET、目标拒绝与拨号取消、部分 DATA 及时交付、物理断线传播、双向活动超时。
- 客户端混合 SOCKS5 CONNECT / HTTP CONNECT 入口，服务端支持 IPv4、IPv6 和服务端 DNS 解析。

该目录用于独立核心与性能实验。传输档位为标准 TLS 1.3，通过显式 CA 配置验证服务端身份；`record_padding` 固定为 `false`，填充预算固定为零。HTTP 入口提供 CONNECT 隧道。对超出本轮范围的配置明确报错。

## 构建与运行

需要 Rust 1.96.0；无需 OpenSSL 开发库。互通测试生成临时证书时使用 `openssl` 命令。

```sh
cargo build --manifest-path veil-rust/Cargo.toml --release --locked
veil-rust/target/release/veil-rust -config server.json
veil-rust/target/release/veil-rust -config client.json
```

服务端配置：

```json
{
  "role": "server",
  "listen": "127.0.0.1:8443",
  "secret": "填入独立生成的32字节base64url密钥",
  "tls": {
    "mode": "tls",
    "server_name": "proxy.example.com",
    "certificate": "/path/server-chain.pem",
    "private_key_file": "/path/server-key.pem"
  }
}
```

客户端配置：

```json
{
  "role": "client",
  "listen": "127.0.0.1:1080",
  "server": "proxy.example.com:8443",
  "secret": "与服务端相同的密钥",
  "tls": {
    "mode": "tls",
    "server_name": "proxy.example.com",
    "ca_file": "/path/ca.pem"
  }
}
```

支持用 `ca_pem` 代替 `ca_file`。可选的 `max_connections`、`max_idle`、`idle_seconds`、`pool_seconds` 默认分别为 64、8、120、20。TLS / AUTH 和目标拨号期限为 10 秒。`traffic` 格式见[线协议](../PROTOCOL.md)，但本实现的三个 padding 范围必须为 `[0,0]`；其他范围与 Go 默认值相同。密钥、nonce 与 TLS 密码运算使用密码学安全随机源，尺寸调度另用普通随机源。

库的边界：`wire` 定义字节与 AUTH，`mux` 接受已认证的 `AsyncRead + AsyncWrite` 并导出同样实现这两个 trait 的流，`transport` 完成 TLS，`proxy::Client` 管理连接池，`proxy::serve` 管理监听器。库内没有系统代理、平台服务或 UI 策略。

## 正确性验证

```sh
cargo fmt --manifest-path veil-rust/Cargo.toml --check
cargo clippy --manifest-path veil-rust/Cargo.toml --locked --all-targets -- -D warnings
cargo test --manifest-path veil-rust/Cargo.toml --locked
VEIL_GO_BINARY=/absolute/path/to/veil-go-batch \
  cargo test --manifest-path veil-rust/Cargo.toml --locked --test interop -- --nocapture
```

未设置 `VEIL_GO_BINARY` 时，真实 Go/Rust 进程互通测试会跳过。其他协议测试不依赖 Go 或实际网络监听。互通测试仅启动临时回环监听器与子进程，退出时清理子进程、证书和配置。

## 复现性能对比

从仓库根目录执行：

```sh
mkdir -p .build/rust-evaluation
python3 veil-core/scripts/build.py batch --output "$PWD/.build/rust-evaluation/veil-go-batch"
python3 veil-core/scripts/build.py openssl --output "$PWD/.build/rust-evaluation/veil-go-openssl"
python3 veil-core/scripts/build.py native --package ./cmd/benchpeer --output "$PWD/.build/rust-evaluation/benchpeer"
cargo build --manifest-path veil-rust/Cargo.toml --release --locked
python3 veil-rust/scripts/compare.py --output .build/rust-evaluation/run \
  --cpus 14,15,16,17 --size 2147483648 --rounds 5000 --repeats 5
```

CPU 顺序为服务端、客户端、目标、负载。选择本机允许使用的四个独立物理核，避免 SMT 兄弟核；代理各用一个核，Go 设置 `GOMAXPROCS=1`，Rust 使用单线程运行时。默认通过 `sudo -n perf` 读取两个代理的 CPU 时间、周期和指令数；无权限时可用 `--no-perf`，但 `/proc` 的 CPU 时间只有时钟滴答精度，短场景不宜用它得出效率结论。

负载由现有 Go `benchpeer` 产生，Python 仅负责进程编排。U/D 分别为上传/下载，1/8 为并发数，E 为已建立流上的 64 B 往返，S 为连接池上新建逻辑流的串行短请求，包含 SOCKS、OPEN 与正常半关闭。测量在预热后开始；不包括 TLS 握手成本。两种实现使用相同范围参数、关闭 TLS 填充，并记录实际物理会话数。Go/Go 保留原默认的混合密钥交换，含 Rust 的组合使用 X25519；由于握手在预热中完成，不能用此测试比较握手耗时。

每个场景保留原始结果、二进制 SHA-256、环境信息及 perf CSV。结果见 [BENCHMARK.md](BENCHMARK.md)。这里比较具体实现的数据路径，不能把结果泛化为语言优劣或 REALITY 线上性能。

实现参考：[Tokio 双向复制与半关闭语义](https://docs.rs/tokio/1.53.1/tokio/io/fn.copy_bidirectional.html)、[tokio-rustls 的显式 flush 要求](https://docs.rs/tokio-rustls/0.26.4/tokio_rustls/)。每批发送完成后都会 flush；单方向 EOF 后继续反方向转发。
