# Veil v0.3

Go TCP 代理，提供可嵌入的核心、SOCKS5 CONNECT、HTTP/HTTPS CONNECT 与固定目标转发入口、TLS 1.3/REALITY 服务端、业务鉴权、连接复用与双向半关闭。支持有界连接池、背压、取消和超时。当前为实验原型，不提供 TUN、UDP 或移动端 UI；流量特征与回落行为尚未通过抗识别验收。


v0.3 修正 REALITY 的签名协商：使用客户端已提供的 ECDSA P-256/SHA-256，将连接绑定的 HMAC 放入证书 SubjectKeyIdentifier 扩展，保留正常的证书自签名和 TLS CertificateVerify。浏览器模板的签名算法列表不变。AUTH 版本及 exporter 域同步升级，两端需同时升级，不兼容 v0.1、v0.2 或上游 REALITY 证书认证。独立实现见[完整线协议规范](../PROTOCOL.md)及 [Rust 互通验证端](../interop/rust/README.md)。这项修正解决签名协商和第三方互通，不代表已通过抗识别验收；证书签名的握手成本需另行测量。

v0.2 引入真正的并发多路复用：每条物理 TLS 连接最多承载 8 条独立逻辑流。每流有目标拨号结果、接收额度、FIN、RST 和完成确认；一条流的取消、目标失败或慢读不会占住其他流的应用层接收循环。认证版本及 TLS exporter 域已升级，两端需同时升级，不兼容 v0.1。

首次 AUTH 与 OPEN 仍同批发送，不增加认证往返；远端实际连接成功后才返回 OPENED。流帧头为 8 字节（类型、32 位流 ID、24 位长度），DATA 最多 32 KiB，物理写入最多批量 128 KiB。客户端使用不重复的递增奇数 ID。每流双向 FIN 后，服务端等待两个转发任务退出，再发送 DONE，防止目标写入尚未完成就关闭连接；不设跨流完成屏障。

客户端优先混合真实并发业务，在已有持续大流或 4 条活动流时可增加第二条物理连接，减少大下载影响新交互。连接数仍受 `max_connections` 限制；所有连接满 8 流时可继续扩展至该上限。调度轮转访问就绪流，每次发送份额在范围内采样，不等计时器、不产生定期假流量。TCP 丢包引起的连接级队头阻塞仍然存在。

TLS 与 REALITY 两端均关闭 Go TLS 的自适应记录长度递增：大块写入从开始即可使用 16 KiB 明文记录，小块写入仍立即发送，不等待凑满、不额外填充。这样消除随连接前段写入次数增长的固定切片阶梯，继续使用现有填充预算。记录长度不是 TCP 包长；较大记录在拥塞窗口小或丢包时可能延后首批明文交付，本机 CPU 成绩不能替代这项弱网延迟评估。

## 构建与运行

本文命令均在仓库的 `veil-core/` 目录执行；仓库根目录的 Makefile 也可转发常用构建与检查目标。

使用 Go 1.26.3 和 Python 3。原生构建不依赖 cgo 或 OpenSSL。请用 Makefile 或 `scripts/build.py` 构建：所有后端均需要版本校验的 REALITY 回落接口补丁，直接 `go build` 不包含该接口，会编译失败。

```sh
make build
.build/veil -keygen
.build/veil -config server.json
.build/veil -config client.json
```

从 `examples/` 复制配置，替换全部占位符。两端使用相同业务 `secret` 和 `short_id`；REALITY 私钥只放服务端，公钥放客户端。配置文件包含凭据，应限制读取权限。

REALITY 的 `cover_address` 指向兼容所用浏览器模板的 TLS 1.3 参考站点，两端 `server_name` 一致。普通 TLS 模式使用 `certificate` 和 `private_key_file`，客户端通过 `ca_file` 增加自有 CA；证书和主机名验证始终启用。

普通 TLS 服务端可配置 `http_fallback`，将未通过 Veil AUTH 的解密数据交给管理员指定的网站后端。可选择明文 `http1`（可选 `h2c`），或经过证书验证的 `https` 后端，两种配置互斥。没有 ALPN 的连接使用 HTTP/1；协商到 `h2` 的连接在 TLS 握手后直接交给网站，不等待 Veil AUTH。目标地址固定，不从请求 Host、URL 或 CONNECT 中提取；该配置与 REALITY 的 `cover_address` 分开。已通过 AUTH 的 Veil 流继续按 OPEN 目标转发。见 [TLS 网站回落示例](examples/server.tls-fallback.json)。

固定的二进制 AUTH 前缀不等同于 HTTP 语法：只发送部分有效 AUTH 前缀时，服务端可能等待认证超时，而网站已返回 HTTP 错误，因而仍可通过响应时序区分。

例如，先由 Caddy 在本机提供实际网站；`site/` 中放入自己的 `index.html` 和其他资源：

```caddyfile
{
    admin off
    auto_https off
    servers {
        protocols h1 h2c
    }
}
http://127.0.0.1:8080 {
    bind 127.0.0.1
    root * ./site
    file_server
}
```

运行 `caddy run --config Caddyfile` 后，在普通 TLS 服务端 JSON 增加：

```json
"http_fallback": {
  "http1": "127.0.0.1:8080",
  "h2c": "127.0.0.1:8080"
}
```

后端仅提供 HTTP/1 时，省略 `h2c` 即可。明文后端应放在适合该访问方式的网络中。部分 h2c 网站会等待客户端前言才发送 SETTINGS；若要沿用网站 HTTPS/H2 的主动发送行为，可将上述配置替换为：

```json
"http_fallback": {
  "https": {
    "address": "127.0.0.1:8444",
    "server_name": "site.example",
    "ca_file": "site-ca.pem",
    "http2": true
  }
}
```

在此地址运行自己的 HTTPS 网站，证书须匹配 `server_name`；`ca_file` 可添加私有 CA，使用系统信任的证书时省略它。`http2: true` 明确启用前端 H2，后端必须能协商相同的 ALPN；省略时前端仅提供 HTTP/1。Veil 不改写 HTTP 数据，也不生成替代网站的 SETTINGS。网站的证书、ALPN 或连接验证失败会结束该回落连接。

`dial_seconds` 限制后端拨号及 TLS 握手的总时间，网站回落的双向无进展期限为 `idle_seconds` 与 120 秒中的较小值，回落与已认证连接共同受 `max_connections` 限制。服务停止会关闭两端并等待转发结束。前端 TLS 握手及 HTTP/1 上的 AUTH 读取仍受 `handshake_seconds` 限制；H2 在握手后使用网站回落的期限。

REALITY 客户端只有验证 Veil 证书证明后才交给核心发送 AUTH。若收到普通网站证书，客户端另行校验证书链、有效期和 `server_name`，通过后在同一 TLS 连接上按 ALPN 发一次 HTTP/1.1 或 HTTP/2 `GET /`，随后关闭并报告认证失败；请求不携带业务凭据或目标地址，不跟随重定向、不重拨连接。网站信任使用系统 CA，可通过 `ca_file` 或 `ca_pem` 增加私有 CA。此分支的期限为父 context 剩余时间与 2 秒中的较短者，发送/接收应用字节预算分别为 16/64 KiB；TLS 握手和有界的记录预读另计。正常 Veil 连接不执行这段 HTTP 请求。该行为使用 Go HTTP 客户端，并不复现浏览器的完整 HTTP 指纹。

REALITY 客户端可用 `tls.fingerprint` 指定 `chrome120`、`chrome131`、`chrome133` 或 `chrome149`；省略时及旧名称 `chrome` 均使用 133。也可将该字段替换为 `"fingerprints": ["chrome131", "chrome133", "chrome149"]`：每条新建的物理 TLS 连接等概率选一个模板，连接复用期间保持不变。两个字段不能同时设置；空列表、未知名称和重复模板会在启动时拒绝。程序调用者可在 `transport.Client(Settings)` 中指定模板或候选列表，选择只发生在返回的握手函数被调用时。

120/131/133 对应固定 uTLS 版本中的模板；131/133 保留其 X25519MLKEM768 密钥份额，不再为减少冷连接开销而删除它。149 基于 Linux Chrome for Testing 149.0.7827.55 的新建连接抓包，在 133 基础上加入空 `trust_anchors` 向量并重新打乱扩展顺序；这里只匹配 ClientHello，不实现浏览器的信任锚重试、DNS 策略或 HTTP 行为。所有模板在本地禁用 TLS 重新协商，这不改变该扩展的线上字节。每条连接独立生成密钥、随机数、GREASE 与扩展顺序。

两端分别配置本地发送策略。`traffic` 描述允许的范围，核心只校验和执行；省略时使用默认范围。`service.GenerateTrafficProfile()`、`veil -profilegen` 和 `veilctl profilegen` 在应用层生成一份新范围配置，可由外层随客户端连接信息分发，再放入配置的 `traffic` 字段。没有预置模板目录，也没有核心自动下载或接受对端任意策略的接口。服务端的下行策略由其自己的配置控制，客户端的上行配置不替代它。

```sh
.build/veil -profilegen
```

参数都是包含端点的整数范围。以下为默认值；外层生成器在这些字段的安全上限内随机生成不同的区间：

```json
"traffic": {
  "version": 1,
  "quantum_bytes": {"min": 8192, "max": 32768},
  "startup_bytes": {"min": 512, "max": 2048},
  "startup_writes": {"min": 2, "max": 6},
  "padding_limit": {"min": 64, "max": 256},
  "padding_budget": {"min": 512, "max": 1024},
  "credit_blocks": {"min": 16, "max": 64},
  "control_padding_limit": {"min": 16, "max": 96}
}
```

每条新建物理连接复制策略，在每个范围内选中心并缩窄到中心附近（约 ±25%，不越过配置边界），之后按操作重新采样。`quantum_bytes` 控制多流调度份额，`startup_bytes` / `startup_writes` 控制单流启动段的写入尺寸与次数；单流持续传输恢复大批量，多流用真实数据交织。启动尺寸是数据写入目标；完整控制帧、认证前缀和 TLS 填充可能使实际记录超过该值，它不表示 TCP 包长。返还接收额度的阈值也在 `credit_blocks` 内变化，避免固定更新周期。

开启 `tls.record_padding: true` 才执行 TLS 记录填充；省略时不填充，范围调度与启动分片仍有效。`padding_limit` 为单记录上限，`padding_budget` 为每次单流激活的单向总预算；记录已满时不填充，但消耗窗口名额。默认每次激活单向最多 1 KiB，生成器可能选到 1536 字节，硬上限为 4096 字节。仅控制帧的写入另有 `control_padding_limit` 小预算（硬上限每次 128 字节）；持续传输中的额度返还也计入这部分，不能把整条连接的总填充说成仅 1 KiB。没有固定上下行比例，填充不影响握手、未认证回落或告警。

安全界限：调度份额 4096～32768 字节、启动写入 128～8192 字节 / 0～12 次、单记录填充 0～512 字节、激活预算 0～4096 字节、额度返还阈值 16～64 块、控制填充 0～128 字节；非法范围在创建或更新时拒绝。短流的相对开销可能较大。范围组合多不等于不可学习：分布、方向、往返依赖、复用寿命和 TCP 行为仍可能被分类。

客户端 `inbound` 选择 `socks`、`http` 或 `mixed`；省略时默认 `socks`。`mixed` 在同一端口识别 SOCKS5 和 HTTP，共用 Veil 连接池，见 [client.mixed.json](examples/client.mixed.json)。HTTP 支持普通 HTTP 转发与 HTTPS CONNECT，正文流式传输；普通 HTTP 每个本地连接处理一个请求，CONNECT 保持长连接与双向半关闭。入口均无本地用户认证，默认监听回环地址；供局域网使用时绑定受信任的 LAN 地址。超时单位为秒。

客户端增加 `"target": "example.com:443"` 后，监听端口接受原始 TCP 数据，经过 Veil 连接服务端，再由服务端连接该固定目标，见 `examples/client.forward.json`。`target` 与 `inbound` 互斥；省略两者时提供 SOCKS5 入口。这里的转发入口不代替中转机上的裸 TCP 透传服务。

## 复用核心

`core` 负责握手、鉴权、流生命周期和连接池；`inbound` 负责 SOCKS5、HTTP、混合入口、固定目标转发与有界监听循环；`service` 将现有 CLI JSON 配置组合成这些组件，并提供 `Parse`、`Validate`、`Runtime.Start/Stop/Restart/Close` 和状态快照。每条逻辑流有两个独立转发任务；物理连接由一个解码器和一个调度写入者管理，不引入进程间数据转发。

两个转发任务各自复用略小于 128 KiB 的工作缓冲，为单流满批保留四个 DATA 帧头的位置，避免满缓冲产生很小的尾部写入。解码器逐段发布通过 TLS 认证的 DATA，应用读取会合并已到达的块，但不会等待整帧或凑满缓冲。每流最多 256 个未消费 DATA 块，按需分配，单流载荷上限 8 MiB、满 8 流物理连接上限 64 MiB；控制队列最多 64 项，服务端每条物理连接最多 8 个目标处理任务。平台层应按设备内存设置连接数，不能把协议上限当作建议配置。

三个后端共用 TLS 批读，只读取已经缓冲的完整记录，不等待未来数据。native 保持原生记录写入，batch/OpenSSL 另启用批量写入。物理连接有常驻读取任务和写入批缓冲，空闲连接仍有内存成本；池超时后释放整条连接。

空闲计时使用单调时钟，双向读取返回的部分帧及写入返回的实际字节都计为进展；缓慢但持续传输不会因为整帧尚未收齐而被误判为空闲。真正双向无进展时仍按配置结束，不实施最低下载速率策略。底层单次 I/O 尚未返回时，核心无法观察内核里的字节进展。

以下片段使用 `"veil/core"` 导入路径（包名为 `core`）：

```go
client, err := core.NewClient(core.ClientConfig{
    Config: core.Config{Secret: secret, TLS: tlsConfig},
    Server: serverAddress,
})
if err != nil { return err }
defer client.Close()

stream, err := client.Open(ctx, "example.com:443")
if err != nil { return err }
defer stream.Close()
// 此时远端已返回 OPENED，适配器才可向本地客户端报告连接成功。
return stream.Relay(localConn)
```

同一个 `*core.Client` 可同时传给 `inbound.SOCKS5(client, timeout)` 、`inbound.HTTP(client, timeout)`、`inbound.Mixed(client, timeout)` 和 `inbound.Forward(client, target)`，各自用 `inbound.Serve` 监听。停止其中一个监听器只取消其连接；应用在全部入口退出后调用 `client.Close()`。服务端通过 `core.NewServer` 创建，用同一监听循环调用 `server.Handle`；直接调用 `Handle` 的应用自行限制并发。

`Open` 的 context 覆盖整条流，不能在 Open 返回后立即取消。`Stream.Relay` 接管并关闭本地连接，该连接须实现 `CloseWrite`；每条 Stream 只能 Relay 一次。双向 FIN/DONE 确认一条流正常完成；提前 Close、取消、目标拨号失败和目标 RST 只重置该逻辑流。外层 TLS/帧解析错误会关闭物理连接并通知所有流。旧 Stream 再次 Close 是安全的。`Client.Close` 中止拨号和流并停止池维护；调用方负责等待自己的处理任务退出。`PoolStats` 统计物理连接（含拨号预留）及空闲数。

物理连接的常驻读取任务持续处理 TLS 关闭和控制帧，失效连接在下次 OPEN 前淘汰。关闭和任务回收不占用池锁。不发送心跳，不能证明黑洞链路可用，也不重放已经发送的 OPEN 或业务字节。

等待 OPEN 回复超过核心预算，或双向 FIN 后未按时收到 DONE 时，该物理连接转为排空：已有流继续运行，新流选择其他连接，最后一条流结束后关闭。目标明确返回的拨号失败、DNS 错误、超时，以及调用方自己的取消或较短期限，都不会触发此策略。排空连接仍计入 `MaxConnections`，`PoolStats.Draining` 报告其数量；达到总上限时返回连接池上限错误，不通过无限建连绕过限制。等待其他请求拨号受 `HandshakeTimeout` 约束；可选的第二条连接拨号失败后，本次请求会重新检查已有连接。

物理连接的 TCP/TLS 建连失败后，同一连接池共享短暂退避，首轮 100–200 ms，连续失败指数增加，最多 1–2 s，区间内随机取值。退避期间优先使用已有连接；没有可用连接时立即返回最近的建连错误。下一次业务请求在退避结束后重新尝试，成功后清除退避；调用方取消不触发退避。`PoolStats.RetryAfterMillis` 报告剩余等待时间，核心不主动发送重试流量。

`core.OpError` 支持 `errors.As`/`errors.Is`，包含本地/隧道的读写操作；`core.ErrIdleTimeout` 和 `core.ErrWriteStall` 的错误还报告两个转发任务的等待位置。`core.TargetError` 表示远端拨号原因。CLI 输出这些操作错误，`service.Runtime.Snapshot` 的 `last_connection_error` 保留本次运行最近一条操作错误，重启实例后清空。正常 EOF 通过 FIN 半关闭；目标 RST 重置一条流，外层截断使物理连接失效；本地错误不保证与原始 TCP RST 报文完全相同。

`ClientConfig.DialContext` 可接平台的 socket 保护/绑定逻辑，`ServerConfig.DialContext` 可接目标访问策略；两者须遵守 context。没有实现这些钩子时，保持普通 TCP 拨号。配置在构造时复制。`Client.SetTrafficProfile(profile)` / `Server.SetTrafficProfile(profile)` 原子替换后续物理连接使用的策略，已有连接沿用自己的快照，错误更新不覆盖有效策略。其他配置通过创建新实例生效。公共控制服务尚未提供独立的在线策略更新命令，保存配置后仍按原来的 restart 流程应用。核心不修改系统路由、DNS、防火墙或服务状态。

平台分工如下，公共控制服务与启动模板见 [veil-service](../veil-service/README.md)：

| 平台 | 平台层负责的工作 | 复用方式 |
| --- | --- | --- |
| OpenWrt / LuCI / CLI | UCI 配置、procd 生命周期、路由和防火墙 | Go CLI/核心；LuCI 管理配置和状态 |
| systemd Linux | unit、权限、配置应用、网络配置 | 同一 Go CLI/核心 |
| OPNsense | FreeBSD 服务、配置与 pf 集成 | FreeBSD CLI/核心 |
| Windows CLI、Windows/Linux 桌面 | 进程管理、权限、系统代理/路由 | CLI；Tauri 界面通过受限控制接口管理本地进程 |
| Android | VpnService、socket protect、TUN/DNS、应用生命周期 | 原生壳加 Go 绑定；后续需用户态 TCP 栈与 UDP 方案 |

界面只走控制通道，数据直接走核心；保存配置和应用配置应为不同操作。Android 的 TUN 数据不是 `net.Conn`，本轮 TCP 接口不能直接宣称已有完整 VPN 支持。桌面 Tauri、LuCI、OPNsense 插件和 Android 绑定均为后续工作。

当前公开包可通过仓库内的 Go module 引用复用，仍需构建器生成的补丁 uTLS 模块；仓库尚未发布可直接 `go get` 的独立 SDK。移动端绑定也须沿用此构建链。native 与 batch 均无 cgo；OpenSSL 后端用于 Linux 服务端。编译检查：

```sh
python3 scripts/cross_check.py
```

检查 Linux amd64/arm64/ARMv7/MIPS/MIPSLE、FreeBSD amd64、Windows amd64/arm64 CLI，以及 Android arm64 的 core/inbound 包。结果在 `.build/cross/results.json`；交叉编译通过不代表设备、安装包、VPN 权限或平台网络集成已验证。

## 检查

```sh
make test
make race
make vet
```

TLS/REALITY 集成测试使用本机 `openssl s_server`，只连接自有回环端点。

下载回归还使用真实 curl 检查上游提前 EOF/RST、单向下载的空闲计时，以及下载被背压阻塞时上传仍能推进。可在隔离网络中设置 `VEIL_LONG_DOWNLOAD_TEST=1`，通过相同构建 flags 单独执行 `go test ./service -run TestLongHTTPSDownloadPause -v`：它模拟 HTTPS 302 后的 104 MiB 下载，中途保持连接静默 30 秒，再续传并校验哈希。测试不连接 HF 或现有部署。

`idle_seconds` 按两个方向合计的活动计时，默认 1800 秒（30 分钟），允许已建立连接较长时间静默。`pool_seconds` 默认 300 秒（5 分钟），保留最近使用过的 TLS 隧道，让间歇访问继续复用。显式配置的旧值仍生效；多层中转应逐层检查这两个参数，任一层较短的期限都可能提前结束内层连接。待转发数据持续无进展时，逻辑流和共享 TLS 写入仍采用至多 120 秒的阻塞预算（显式更短的 `idle_seconds` 优先）；逻辑流诊断为 `write_stall`。这些都是超时预算，不是最低下载速度检查。收到 EOF/RST 与链路黑洞不同：没有关闭报文的丢包可能要等 TCP 重传或应用空闲超时。30 秒停顿测试通过不能排除特定运营商、CDN 或部署版本下的长连接问题，也不能替代现场日志。

长时间稳定性检查需要显式启用，普通 CI 会跳过 `TestStabilitySoak`。它默认用 128 并发经过两层 REALITY，混合 SOCKS/HTTP CONNECT、长传输、半关闭、RST 和取消，核对传输字节与资源释放。`VEIL_STRESS_WORKERS=1..512` 可调整并发；日志包含错误分类、每个失败事务已校验的字节数、GC、堆、FD 和池状态：

```sh
make prepare
VEIL_STRESS_DURATION=60m GOCACHE="$PWD/.build/cache" GOMODCACHE="$PWD/.build/mod" \
  go test -tags=with_utls -modfile=.build/native/build.mod ./service -run '^TestStabilitySoak$' -timeout=70m
```

`TestPoolOverloadRecovery` 在同一隧道保留健康连接，将其余槽位填满或阻塞在目标拨号，再反复注入并发请求、取消并恢复，检查连接复用和资源归还。

性能基准分别覆盖串行新连接（`BenchmarkSerialShort`、`BenchmarkSerialSized`）、持久大流（`BenchmarkBulkEcho`）和不同业务大小后的空闲堆（`BenchmarkIdleStreams`、`BenchmarkIdleMediumStreams`）。空闲堆按 `-benchtime=1x` 且每个大小使用独立进程采样，避免前一次负载的缓存影响比较。

全窗口背压测试会塞满同一隧道内 8 条流的双向接收窗口，再交替恢复读取或重置部分流，检查数据哈希、FIN/DONE 和剩余流的进展。测试使用本机 TCP，按需启用：

```sh
VEIL_WINDOW_STRESS_DURATION=5m go test -race ./internal/mux -run '^TestWindowSaturationSoak$' -timeout=7m -v
```

## 优化构建

需要 Python 3。OpenSSL 后端另需 C 编译器、`pkg-config` 和 OpenSSL 开发库，验证版本为 3.5.5。

```sh
make optimized
python3 scripts/check.py openssl --race --stdlib
python3 scripts/demo.py
.build/veil-openssl -config server.json
.build/veil-batch -config client.json
```

`make optimized` 生成 `veil-native`、`veil-batch` 和 `veil-openssl`，均位于 `.build/`。推荐客户端使用批量记录后端，Linux 服务端使用 OpenSSL 后端；原生后端仍可独立使用。

构建器校验固定 Go/uTLS 源码摘要，通过 build overlay 和本地模块副本应用补丁，不修改系统工具链或下载缓存。版本不匹配时先审查补丁并执行回归。演示使用自有 HTTPS 端点验证完整 SOCKS5/REALITY 路径。

`patches/*.go.in` 是受版本控制的 Go 源码模板：部分是追加到 TLS 源文件的声明片段，部分复制为独立的 Go 文件。它们由 `scripts/build.py` 读取，不是实验中间文件；生成后的可编译源码在 `.build/`。保留模板可以集中审查改动，避免将整个上游 TLS 库复制进仓库。

生成代码、依赖、临时凭据、日志和二进制集中在 `.build/`，由 `make clean` 删除。

不要把运行中的服务部署在构建清理目录内；对正在使用的版本做改动时，应在独立工作树构建和验证，部署另行切换。

## 基准与本地探测

```sh
make optimized
python3 scripts/build.py native --package ./cmd/benchpeer --output .build/benchpeer
python3 scripts/build.py native --package ./cmd/probepeer --output .build/probepeer
python3 scripts/bench_build.py native
python3 scripts/bench_build.py batch
python3 scripts/bench_build.py openssl
python3 scripts/benchmark.py --out .build/bench-local --reps 7 --gib 3
python3 scripts/probe.py --out .build/probes-local
```

上述工具仅连接自有回环端点。基准依赖 Linux `perf`、`taskset` 和本机 `sudo -n perf` 权限；服务端固定 CPU 14，参考站点/目标 16–17，负载 18–21，客户端 22–25，其他机器需调整亲和性。AnyTLS 对照使用固定版本 sing-box，其优化组应用同一 TLS 补丁。

`core_regression.py` 比较重构前后的实际二进制：默认先验证同一协议版本的新旧两端交叉互通，再随机交替测 U/D 吞吐、E 热流回显和 S 短流复用。比较不同线协议版本时加 `--skip-interop`，因为线协议有意不兼容。两端各一个 CPU、同一 Chrome 149 模板和填充开关，用 perf 分别记录 CPU 时间、周期和指令数。旧目录需提供 `veil-client`（batch）、`veil-server`（openssl）和固定版本的 `benchpeer`；候选目录提供 `veil-batch`、`veil-openssl`、`veil-native`（生成临时密钥）。

```sh
sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/core_regression.py --before .build/baseline --peer .build/baseline/benchpeer --out .build/core-regression --cores 14,22,17,18'
```

测试强制私有网络命名空间，进程退出即清理测试监听；证书、配置、原始结果和二进制摘要保存在输出目录，未经授权不操作任何已部署服务。`summary.json` 保留逐配对差值及其中位数，不能只凭单次成绩宣称无回退。

`benchmark.py --modes E --rounds 10000` 测热连接延迟；`--modes C --connections 1 --rounds 1` 测冷连接。输出目录不可覆盖已有原始结果；测量数据不纳入版本控制。

`traffic_shapes.py --before .build/previous --after .build --out .build/shapes --samples 12` 必须在私有网络命名空间运行。两组 Veil 使用同一 Chrome 149 模板并开启填充，与受控 Python/OpenSSL HTTPS 比较新连接和连续六条业务流：记录 TLS 长度、方向、转发突发和时间，不保存原始流量。参考 HTTPS 在一条 TLS 连接上复用 HTTP，Veil 在一条外层连接中承载六次内层 TLS 握手；这项差异有意保留以展示嵌套握手特征。20 ms 夹具间隔、用户态转发时间均不代表真实 TCP 包时序，工具不提供抗审查分类结论。

`browser_shapes.py` 提供负载一致的真实浏览器对照：同一 Chrome 页面访问自有 Node HTTPS 网站，比较直连、旧版/新版 SOCKS 代理，以及 Chrome 直接访问新版 REALITY 端口的未认证回落。覆盖 HTTP/2 长连接、HTTP/1.1 主动关闭，每种分别串行和并行获取六个资源；逐条校验正文、状态码与协商协议。每次使用新浏览器配置，仅对临时测试证书放行 SPKI；后台域名在独立网络空间内直接失败，不进入代理。需要 Node.js 与本机 Chromium/Chrome，不安装浏览器依赖。

```sh
sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/browser_shapes.py --browser /path/to/chrome --before .build/previous --after .build --out .build/browser-shapes --samples 6'
```

两组二进制目录均须含 `veil-batch` 与 `veil-openssl`，构建目录另需 `veil-native` 生成测试密钥。输出 `samples.json`、`summary.json`、二进制及工具摘要；记录全部物理连接、TLS 长度、方向、关闭结果与浏览器连接复用信息，不保存 TLS 密文或正文。浏览器临时配置自动删除，测试证书及 Veil 临时配置在结果目录的 `fixture/` 中。可指定 `--delay-ms 5` 在每次转发前增加延迟；它是用户态敏感性检查，不是固定 RTT 或丢包仿真。浏览器退出可能重置连接，代理计数中的失败还包含监听就绪探测，因此不将这些计数等同于页面下载失败。

以下是 v0.1 的历史测量，不代表当前版本性能。2026-09-27 使用 Chrome 149.0.7827.55、Node 24.16.0，与 `adab5bd` 对照：最终 96 组中，旧版 23/24 出现相邻记录增加 1186 字节的阶梯，新版、直连及未认证回落均为 0/24；补充 5 ms 转发延迟的 24 组中，旧版、新版、直连分别为 8/8、0/8、0/8。24 组真实浏览器回落共完成 168 个页面/资源请求，含 HTTP/2 和 HTTP/1.1。此计数只针对已定位的 Go TLS 递增模式，不是通用分类器准确率。三种后端的真实密文记录回归与 race 检查通过；新测试对旧配置失败。五轮 batch 客户端/OpenSSL 服务端配对 CPU 效率中位数变化：上传 +3.53%、下载 −0.73%、热流回显 −0.37%、短流复用 +7.78%；新旧两端交叉互通通过。性能数据为本机回环结果。

该轮没有消除嵌套 TLS：当时 HTTP/2 串行负载的方向突发中位数仍为直连 20、旧版 28、新版 28；并行为 10、18、18。AUTH/OPEN、内层握手、控制帧、连接寿命与复用行为仍可能提供判别信息。保持远端连接成功后才报告 SOCKS 成功，不用提前报成功来掩盖额外往返；也不强制流量比例或加入周期性假包。

研究依据需区分实测与假设：[GFW Report 的 V2Ray 分析](https://gfw.report/blog/v2ray_weaknesses/en/)展示了重放、错误处理和关闭差异的探测价值；其 [2022 年 TLS 工具阻断文章](https://gfw.report/blog/blocking_of_tls_based_circumvention_tools/en/)明确将具体 TLS 指纹机制列为未实测的推测。[封装 TLS 握手研究](https://censoredplanet.org/assets/tls_in_tls.pdf)说明 ClientHello 模板和有限填充不能自动消除内层握手的长度、方向和往返依赖。上述本地回归以及正常网站回落成功，均不等于已经通过真实 GFW 抗识别验收。

REALITY 回落回归以同一受控 HTTPS 网站为对照，覆盖 TLS 1.2/1.3、ALPN、分片 ClientHello、普通网站会话票据恢复、畸形握手和明文 HTTP；另测半关闭、长 HTTP 传输、静默目标与取消。普通网站回落的票据恢复不等于 Veil 启用了认证会话恢复或 0-RTT。

`benchpeer -mode S` 测反复建立 SOCKS 流、64 字节双向回显与半关闭，底层 TLS 会话可复用。`handshakebench` 则每次重新建立 REALITY/TLS 会话，并完成 AUTH、OPEN、回显与 FIN/DONE；其延迟统计截止 OPENED，CPU 计数覆盖整个事务。构建时使用与被测客户端相同的后端：

```sh
python3 scripts/build.py batch --package ./cmd/handshakebench --output .build/handshakebench-batch
python3 scripts/reality_benchmark.py --before .build/previous --out .build/reality-cpu
python3 scripts/reality_fingerprint.py --before .build/previous --out .build/reality-shapes
```

对照目录中的 `veil-openssl`、`veil-batch` 和 `handshakebench-batch` 必须来自同一旧版源码，握手测量工具本身使用相同源码。CPU 对照采用相邻随机 A/B 配对，分别测两端 `perf task-clock`、指令数与周期数，并记录物理连接数；默认两端各固定一个 CPU。指纹工具比较归一化 ClientHello 和 TLS 记录序列，保留随机字段的摘要用于检查重复，不保存原始捕获内容；它不执行抗审查分类验收。

这两个工具支持 `--fingerprint chrome149 --record-padding`，仅修改候选组配置，原版组保持不变。`browser_hello.py --browser /path/to/chrome --out .build/browser-hello` 使用独立临时浏览器配置向自有回环端点发送 ClientHello，保存归一化字段后退出；指纹工具通过 `--browser-result .build/browser-hello/result.json` 比较实测浏览器。静态字段匹配不代表扩展顺序分布、完整浏览器行为或实际抗审查能力相同。

重建前可将旧二进制保存到 `.build/previous/`，用 `veil-opt-reality-previous` 变体与候选同时测试；对应目录必须包含 `veil-openssl` 和 `veil-batch`。测量元数据记录两套二进制摘要。

`probe.py` 使用 OpenSSL ECDSA、OpenSSL RSA 和 Go TLS 三类参考端点，比较直连、裸上游 REALITY 与 Veil。正常 HTTPS、延迟请求、异常输入、重放、参考站点故障和 TLS 记录形态分别记录。为防止上游未传播 TCP EOF 阻塞单线程参考站点，每次裸 REALITY 异常探测后重置参考进程。

REALITY 收到参考站点响应并选择回落后，改用 `idle_seconds` 与 120 秒中的较小值控制双向共享的空闲期限，单向传输也会续期；此前及代理连接的握手、业务鉴权阶段仍受 `handshake_seconds` 约束。回落传播 TCP 半关闭，错误与服务停止会关闭两端。空闲期限仍可能与参考站点不同；满数据帧对齐也不消除短读和控制帧的长度特征。

20 ms RTT 模拟需 `iproute2` 和一次性网络命名空间。将 `USER` 替换为具有本机测试权限的普通用户：

```sh
sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/delayed_benchmark.py'
```

`delayed_benchmark.py` 支持 `--out`、`--variants`、`--modes E,C`、`--delay-ms 10` 和 `--loss-percent 0.1`，并记录命名空间内 TCP 重传与 qdisc 丢弃计数。

`network_smoke.py` 提供两台自有主机上的低流量 `server` / `client` 测试角色，验证半关闭完整性、复用、四连接回显、目标失败和取消，不测带宽上限。将脚本、待测二进制、`-keygen` 输出的测试用 `config.json` 与 `cert.pem`/`key.pem` 放入专用临时目录；客户端只需公钥、业务凭据与证书。参数见 `--help`。服务端在标准输入关闭时停止，两个角色均清理自己启动的进程；运行后删除两端临时目录。

## 许可

本项目使用 GPL-3.0-or-later；第三方代码来源见 [THIRD_PARTY.md](../THIRD_PARTY.md) 和 [LICENSE](../LICENSE)。

空闲/丢包回归也可单独运行：`VEIL_QUIET_DURATION=3m` 启用 `TestQuietDoubleHopDefaults`，核对双层连接静默三分钟后仍为同一流；`VEIL_WRITE_STALL_TEST=1` 启用核心包的 `TestLongIdleDoesNotExtendBlockedWrite`，核对长空闲预算不会放宽阻塞写入期限。Linux 上的 `TestRecoverableNetworkPauses` 要求在独立、名称以 `veil-test-` 开头的 network namespace 中设置 `VEIL_NETEM_PAUSES=5s,15s,30s`，测试用 `tc netem` 丢弃全部包，再验证原连接恢复和数据完整性。应为这些手动测试设置足够的 `go test -timeout`，普通 CI 不执行等待数分钟的用例。
