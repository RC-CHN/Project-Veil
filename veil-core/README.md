# Veil v0.1

Go TCP 代理，提供可嵌入的核心、SOCKS5 CONNECT 与固定目标转发入口、TLS 1.3/REALITY 服务端、业务鉴权、连接复用与双向半关闭。支持有界连接池、背压、取消和超时。当前为实验原型，不提供 TUN、UDP 或移动端 UI；流量特征与回落行为尚未通过抗识别验收。

v0.1 将首次 AUTH 与 OPEN 同批发送，目标连接成功后返回 OPEN_OK；满 DATA 帧连同帧头为 128 KiB。从 v0 升至 v0.1 时两端需同时升级，v0 鉴权会被拒绝；当前仍兼容已有 v0.1 两端。OPEN_ERROR 沿用单字节载荷，细分普通失败、DNS 失败、拒绝连接与超时；旧对端仍会拒绝失败的 OPEN。没有新增握手往返。

REALITY 客户端在生成密钥前确定模板与扩展，并复用 TLS 库已解析的证书执行认证。服务端收到对端 FIN 后，若本地传输也结束，可在两个转发任务退出后将 FIN 与 DONE 同批写入；本地先结束时仍立即发送 FIN。帧格式不变，兼容已有 v0.1 对端。这些改动不代表已经消除流量指纹。

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

REALITY 客户端可用 `tls.fingerprint` 指定 `chrome120`、`chrome131`、`chrome133` 或 `chrome149`；省略时及旧名称 `chrome` 均使用 133。也可将该字段替换为 `"fingerprints": ["chrome131", "chrome133", "chrome149"]`：每条新建的物理 TLS 连接等概率选一个模板，连接复用期间保持不变。两个字段不能同时设置；空列表、未知名称和重复模板会在启动时拒绝。程序调用者可在 `transport.Client(Settings)` 中指定模板或候选列表，选择只发生在返回的握手函数被调用时。

120/131/133 对应固定 uTLS 版本中的模板；131/133 保留其 X25519MLKEM768 密钥份额，不再为减少冷连接开销而删除它。149 基于 Linux Chrome for Testing 149.0.7827.55 的新建连接抓包，在 133 基础上加入空 `trust_anchors` 向量并重新打乱扩展顺序；这里只匹配 ClientHello，不实现浏览器的信任锚重试、DNS 策略或 HTTP 行为。所有模板在本地禁用 TLS 重新协商，这不改变该扩展的线上字节。每条连接独立生成密钥、随机数、GREASE 与扩展顺序。

两端可分别设置 `tls.record_padding: true`。每个物理连接的发送方向独立选择单记录填充上限（128～256 字节）和窗口上限（4～8 条记录）；这些参数保持到连接结束。每条业务流在该范围内另选 2～窗口上限条应用记录，以及 512～1024 字节总预算，记录填充长度从 1～剩余上限随机选择，记录已满时不填充但消耗窗口名额。双向每流合计最多 2 KiB，窗口或预算耗尽即恢复批处理；没有填充消息、等待定时器或固定上下行比例。该选项省略时关闭，REALITY 示例显式开启。填充不作用于 TLS 握手、普通网站回落或告警，旧 v0.1 对端也能解码。

这层填充削弱精确记录长度特征，不能消除突发总量、方向、时序或嵌套 TLS 握手的往返依赖；短流的相对流量开销可能较大。多模板与填充均不等于通过抗识别验收。

SOCKS5 入口仅支持无认证 CONNECT，默认监听回环地址。按需修改监听地址、连接数和超时配置；超时单位为秒。

客户端增加 `"target": "example.com:443"` 后，监听端口接受原始 TCP 数据，经过 Veil 连接服务端，再由服务端连接该固定目标，见 `examples/client.forward.json`。省略 `target` 继续提供原有 SOCKS5 入口，已有配置无需修改。这里的转发入口不代替中转机上的裸 TCP 透传服务。

## 复用核心

`core` 负责握手、鉴权、流生命周期和连接池；`inbound` 负责 SOCKS5、固定目标转发与有界监听循环；`service` 将现有 CLI JSON 配置组合成这些组件，并提供 `Parse`、`Validate`、`Runtime.Start/Stop/Restart/Close` 和状态快照。数据仍沿用原转发循环，一方向一个写入者，无额外数据队列或进程间转发。

两个转发任务各自通过 `sync.Pool` 复用 128 KiB 工作缓冲，退出后归还。接收端验证帧头后逐段转发已通过 TLS 认证的 DATA，无需等整帧到齐；读操作严格受剩余帧长限制，不越过 FIN/DONE。所有后端共用 TLS 批读逻辑，覆盖 `Conn.Read` 和浏览器客户端的 `UConn.Read`，只读取已经缓冲的完整记录，不等未来数据。TLS 读缓冲容纳两批记录以减少底层读取，写入仍按最多八条记录一批；共享缓存最多保留 8 个约 260 KiB 的空缓冲，总上限约 2 MiB。只在没有待处理输入时回收，闲置连接不持有该缓冲。native 保持原生记录写入，batch/OpenSSL 另启用批量写入。

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
// 此时远端已返回 OPEN_OK，适配器才可向本地客户端报告连接成功。
return stream.Relay(localConn)
```

同一个 `*core.Client` 可同时传给 `inbound.SOCKS5(client, timeout)` 和 `inbound.Forward(client, target)`，各自用 `inbound.Serve` 监听。停止其中一个监听器只取消其连接；应用在全部入口退出后调用 `client.Close()`。服务端通过 `core.NewServer` 创建，用同一监听循环调用 `server.Handle`；直接调用 `Handle` 的应用自行限制并发。

`Open` 的 context 覆盖整条流，不能在 Open 返回后立即取消。`Stream.Relay` 接管并关闭本地连接，该连接须实现 `CloseWrite`；每条 Stream 只能 Relay 一次。只有双向 FIN 和 DONE 都完成才回池；提前 Close、取消、目标失败或协议错误丢弃连接。旧 Stream 在连接复用后再次 Close 是安全的。`Client.Close` 中止拨号和流并停止池维护；调用方负责等待自己的处理任务退出。`PoolStats` 提供连接总数（含拨号预留）与空闲数，`Stats` 提供原有计数。

复用空闲超过 1 秒的连接前，以 2 ms I/O 截止时间检查待接收的 TLS 关闭、错误或非法应用字节；失效连接在发送 OPEN 前淘汰。热复用不增加检查开销，关闭连接不占用池锁。该检查不发送心跳，不能证明黑洞链路可用，也不重放已经发送的 OPEN 或业务字节。

`core.OpError` 支持 `errors.As`/`errors.Is`，包含本地/隧道的读写操作；`core.ErrIdleTimeout` 的错误还报告两个转发任务的等待位置。`core.TargetError` 表示远端拨号原因。CLI 输出这些操作错误，`service.Runtime.Snapshot` 的 `last_connection_error` 保留本次运行最近一条操作错误，重启实例后清空。正常 EOF 通过 FIN 半关闭；RST/截断使整条隧道失效，本地错误不保证与原始 TCP RST 报文完全相同。

`ClientConfig.DialContext` 可接平台的 socket 保护/绑定逻辑，`ServerConfig.DialContext` 可接目标访问策略；两者须遵守 context。没有实现这些钩子时，保持普通 TCP 拨号。配置在构造时复制，变更通过创建新实例生效。核心不修改系统路由、DNS、防火墙或服务状态。

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

`idle_seconds` 按两个方向合计的活动计时，默认 120 秒；它不是最低下载速度检查。收到 EOF/RST 与链路黑洞不同：没有关闭报文的丢包可能要等 TCP 重传或应用空闲超时。30 秒停顿测试通过不能排除特定运营商、CDN 或部署版本下的长连接问题，也不能替代现场日志。

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

`core_regression.py` 比较重构前后的实际二进制：先验证新旧两端交叉互通，再随机交替测 U/D 吞吐、E 热流回显和 S 短流复用。两端各一个 CPU、同一 Chrome 149 模板和填充设置，用 perf 分别记录 CPU 时间、周期和指令数。旧目录需提供 `veil-client`（batch）、`veil-server`（openssl）和固定版本的 `benchpeer`；候选目录提供 `veil-batch`、`veil-openssl`、`veil-native`（生成临时密钥）。

```sh
sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/core_regression.py --before .build/baseline --peer .build/baseline/benchpeer --out .build/core-regression --cores 14,22,17,18'
```

测试强制私有网络命名空间，进程退出即清理测试监听；证书、配置、原始结果和二进制摘要保存在输出目录，未经授权不操作任何已部署服务。`summary.json` 保留逐配对差值及其中位数，不能只凭单次成绩宣称无回退。

`benchmark.py --modes E --rounds 10000` 测热连接延迟；`--modes C --connections 1 --rounds 1` 测冷连接。输出目录不可覆盖已有原始结果；测量数据不纳入版本控制。

`traffic_shapes.py --before .build/previous --after .build --out .build/shapes --samples 12` 必须在私有网络命名空间运行。两组 Veil 使用同一 Chrome 149 模板并开启填充，与受控 Python/OpenSSL HTTPS 比较新连接和连续六条业务流：记录 TLS 长度、方向、转发突发和时间，不保存原始流量。参考 HTTPS 在一条 TLS 连接上复用 HTTP，Veil 在一条外层连接中承载六次内层 TLS 握手；这项差异有意保留以展示嵌套握手特征。20 ms 夹具间隔、用户态转发时间均不代表真实 TCP 包时序，工具不提供抗审查分类结论。

REALITY 回落回归以同一受控 HTTPS 网站为对照，覆盖 TLS 1.2/1.3、ALPN、分片 ClientHello、普通网站会话票据恢复、畸形握手和明文 HTTP；另测半关闭、长 HTTP 传输、静默目标与取消。普通网站回落的票据恢复不等于 Veil 启用了认证会话恢复或 0-RTT。

`benchpeer -mode S` 测反复建立 SOCKS 流、64 字节双向回显与半关闭，底层 TLS 会话可复用。`handshakebench` 则每次重新建立 REALITY/TLS 会话，并完成 AUTH、OPEN、回显与 FIN/DONE；其延迟统计截止 OPEN_OK，CPU 计数覆盖整个事务。构建时使用与被测客户端相同的后端：

```sh
python3 scripts/build.py batch --package ./cmd/handshakebench --output .build/handshakebench-batch
python3 scripts/reality_benchmark.py --before .build/previous --out .build/reality-cpu
python3 scripts/reality_fingerprint.py --before .build/previous --out .build/reality-shapes
```

对照目录中的 `veil-openssl`、`veil-batch` 和 `handshakebench-batch` 必须来自同一旧版源码，握手测量工具本身使用相同源码。CPU 对照采用相邻随机 A/B 配对，分别测两端 `perf task-clock`、指令数与周期数，并记录物理连接数；默认两端各固定一个 CPU。指纹工具比较归一化 ClientHello 和 TLS 记录序列，保留随机字段的摘要用于检查重复，不保存原始捕获内容；它不执行抗审查分类验收。

这两个工具支持 `--fingerprint chrome149 --record-padding`，仅修改候选组配置，原版组保持不变。`browser_hello.py --browser /path/to/chrome --out .build/browser-hello` 使用独立临时浏览器配置向自有回环端点发送 ClientHello，保存归一化字段后退出；指纹工具通过 `--browser-result .build/browser-hello/result.json` 比较实测浏览器。静态字段匹配不代表扩展顺序分布、完整浏览器行为或实际抗审查能力相同。

重建前可将旧二进制保存到 `.build/previous/`，用 `veil-opt-reality-previous` 变体与候选同时测试；对应目录必须包含 `veil-openssl` 和 `veil-batch`。测量元数据记录两套二进制摘要。

`probe.py` 使用 OpenSSL ECDSA、OpenSSL RSA 和 Go TLS 三类参考端点，比较直连、裸上游 REALITY 与 Veil。正常 HTTPS、延迟请求、异常输入、重放、参考站点故障和 TLS 记录形态分别记录。为防止上游未传播 TCP EOF 阻塞单线程参考站点，每次裸 REALITY 异常探测后重置参考进程。

REALITY 收到参考站点响应并选择回落后，改用 `idle_seconds` 控制双向共享的空闲期限，单向传输也会续期；此前及代理连接的握手、业务鉴权阶段仍受 `handshake_seconds` 约束。回落传播 TCP 半关闭，错误与服务停止会关闭两端。空闲期限仍可能与参考站点不同；满数据帧对齐也不消除短读和控制帧的长度特征。

20 ms RTT 模拟需 `iproute2` 和一次性网络命名空间。将 `USER` 替换为具有本机测试权限的普通用户：

```sh
sudo unshare -n -- sh -c 'ip link set lo up && exec runuser -u USER -- env VEIL_ISOLATED_NETNS=1 python3 scripts/delayed_benchmark.py'
```

`delayed_benchmark.py` 支持 `--out`、`--variants`、`--modes E,C`、`--delay-ms 10` 和 `--loss-percent 0.1`，并记录命名空间内 TCP 重传与 qdisc 丢弃计数。

`network_smoke.py` 提供两台自有主机上的低流量 `server` / `client` 测试角色，验证半关闭完整性、复用、四连接回显、目标失败和取消，不测带宽上限。将脚本、待测二进制、`-keygen` 输出的测试用 `config.json` 与 `cert.pem`/`key.pem` 放入专用临时目录；客户端只需公钥、业务凭据与证书。参数见 `--help`。服务端在标准输入关闭时停止，两个角色均清理自己启动的进程；运行后删除两端临时目录。

## 许可

本项目使用 GPL-3.0-or-later；第三方代码来源见 [THIRD_PARTY.md](../THIRD_PARTY.md) 和 [LICENSE](../LICENSE)。
