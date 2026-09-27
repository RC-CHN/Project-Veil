# 双 HTTPS 连接实验

**结论：不将这个原型并入 Veil 核心。** 分离上下行改变了单条连接的外观，但两条连接合并观察时，内层 TLS 握手的长度和往返依赖仍然存在；本实现还显著降低 CPU 效率。本次否决的是“仅拆双连接”的方案，不是所有双通道加调度、填充的组合。

实验日期：2026-09-28。基线 Veil：`0077946`。没有修改运行中的代理、部署配置、主机路由或主机防火墙。

## 实际实现

使用 Go 标准库的真实 HTTP/2 和 TLS 1.3，校验自有测试 CA；内层 HTTPS 始终端到端加密。每条业务流由一条 streaming POST 上传、一条 streaming GET 下载，随机 ID 配对；TCP 目标连接成功后才回复 SOCKS 成功。HTTP/2 负责帧格式、流控、背压和连接复用，不手写仿 HTTP 字节。

对照组：

| 组 | 外层传输 |
| --- | --- |
| HTTPS | Chrome 直连同一个受控 HTTPS 网站 |
| Veil | 当前 Veil，普通 TLS，开启现有记录填充 |
| H2-shared | GET/POST 共用一条长期 HTTP/2 TLS 连接 |
| H2-split | GET/POST 分别使用两条长期 HTTP/2 TLS 连接 |

shared 与 split 使用同一套代码，唯一传输配置区别是是否共用 `http.Transport`，用于区分 HTTP/2 成本和拆连接本身的影响。连续及并发业务流复用同一条/对物理连接；每次浏览器试验均核验连接数。原型没有额外填充或独立发送调度，也没有借用 REALITY，以免同时改变多个变量。

这只是受控夹具：程序仅允许在独立 Linux 网络命名空间运行，监听、服务器和目标限于回环地址。实验 Bearer 配对认证没有作为正式协议设计；Go 的外层 TLS/HTTP2 行为也不等于 Chrome。它不提供正式部署入口。

## 指纹结果

真实 Chrome 149.0.7827.55 访问自有 Node HTTPS 网站。分别测试网站 HTTP/2 长连接、HTTP/1.1 主动关闭，以及串行/并行获取六个不同大小的资源。无额外延迟 8 轮、单向 10ms netem 延迟 4 轮，共 **192 次浏览器试验、1344 个页面/资源响应**，全部通过状态码、协议及正文核验。

所有 TCP 连接使用同一个单调时钟；同时分析每条外层连接和合并后的记录序列。另一个自有抓取点提供内层 TLS 真值，仅用于泄漏核验。保存记录长度、方向、完成接收的时间，不保存 TLS 原始字节。

最明确的发现是：**两个 H2 组中，内层 ClientHello 以及服务器首个握手 flight 都保留了精确长度关系**。

| 组 | 无额外延迟：ClientHello / 服务端 flight | 20ms RTT：ClientHello / 服务端 flight |
| --- | --- | --- |
| H2-shared | 128/128，128/128 | 64/64，64/64 |
| H2-split | 128/128，128/128 | 64/64，64/64 |

关系为：`外层 TLS 记录长度 = 内层 ClientHello 或首个服务端 flight 长度 + 31`，其中 31 来自 9 字节 HTTP/2 DATA 帧头和 22 字节 TLS 1.3 记录开销。上下行分别匹配，每条外层记录最多使用一次，匹配限制在真值附近的时间窗口。20ms RTT 组中上传对应记录到内层抓取点的时间差中位数约 10.13ms。

**这些比例不是识别率。** 核验知道内层真值，真实观察者不知道；只证明这个原型保留了可关联的结构。没有训练分类器，也没有测得真实 GFW 的规则、误报率或抗阻断能力。

无额外延迟时，合并观察的加密记录方向变化次数中位数如下。统计从每条连接的客户端 Finished 开始，包含 HTTP/2 控制记录和结束记录，不等于握手 RTT 数，也不是越多越易识别的评分。

| 网站负载 | HTTPS | Veil | H2-shared | H2-split |
| --- | ---: | ---: | ---: | ---: |
| HTTP/2 串行 | 15 | 23 | 31 | 33 |
| HTTP/2 并行 | 5 | 14 | 25 | 26 |
| HTTP/1.1 关闭、串行 | 15 | 57 | 71 | 72 |
| HTTP/1.1 关闭、并行 | 14 | 44 | 62 | 63 |

例如 HTTP/2 并行负载下，split 单条连接的方向变化中位数为 8，合并后为 26。只按单条连接观察会高估拆分效果。延迟组中某些计数降低，但上面的精确长度关联依然成立。

20ms RTT 下，四种网站负载的文档首字节中位数范围：直连 HTTPS **64.3–64.7ms**、Veil **109.3–111.9ms**、H2-shared **110.2–110.9ms**、H2-split **130.7–132.5ms**。这些是每次新启动浏览器的冷路径；不能据此推断已预建隧道的开销。

## 性能与语义

Intel Xeon CPU Max 9470C，Linux 7.0.0-31-generic，Go 1.26.3。两端各固定一个 CPU，`GOMAXPROCS=1`；三组都使用现有 Go AES/TLS batch overlay。这里 Veil 两端均为 batch，**不是 batch 客户端加 OpenSSL 服务端的部署组合**。

独立回环网络、无抓取、无额外延迟，每项随机顺序配对 5 轮。U/D 为预热后单业务流 1GiB，E 为同流 1000 次 64B 回显，S 为 1000 次顺序短流并验证半关闭，C 为一次冷连接回显。`perf task-clock` 累加客户端和服务端 CPU 时间；不包括负载发生器和目标服务。

| 项目 | Veil：墙钟 / CPU ms | H2-shared：墙钟 / CPU ms | H2-split：墙钟 / CPU ms |
| --- | ---: | ---: | ---: |
| 上传 1GiB | 0.588s / 1091 | 0.852s / 1674 | 0.845s / 1660 |
| 下载 1GiB | 0.582s / 1072 | 1.027s / 1988 | 0.916s / 1789 |
| 同流回显 1000 次 | 0.077s / 60 | 0.091s / 74 | 0.094s / 78 |
| 短流 1000 次 | 0.280s / 250 | 0.460s / 518 | 0.454s / 516 |
| 冷连接一次 | 4.02ms / 4.16 | 4.51ms / 4.89 | 4.91ms / 6.11 |

按每轮配对比值取中位数，split 相对 Veil 的上传 CPU 效率为 **67.0%**，下载为 **59.9%**，下载墙钟吞吐为 **63.6%**。这是当前最小原型的结果，不是 HTTP/2 实现能达到的性能上限。shared 也明显落后，说明不能把全部成本都归因于多了一条 TCP 连接。

语义测试覆盖上传 EOF 后继续接收、服务端 FIN 后继续上传、RST 错误、取消、连接拒绝、下载背压期间继续上传、取消解阻塞、会话清理和物理连接复用。另以真实 HTTPS 302 后下载 64MiB、发送到一半暂停 30 秒再继续，逐组核对完整文件 SHA-256；这是功能检查，不把 Python 源站的速度当作代理性能。

局限：这是同机回环与受控延迟，浏览器抓取还经过用户态转发点；没有真实 WAN 的丢包、拥塞和重排，没有大规模正常流量库，CPU 结论也未在 ARM 上验证。1MiB HTTP/2 流接收窗口尚未为高带宽延迟积调优。没有改变内层握手必须等待对端应答这一事实。

## 复现与结果文件

在 `veil-core` 目录使用现有构建环境，先备齐 `.build/veil-native`（生成测试凭据）、`.build/veil-batch`、`.build/benchpeer`，再构建夹具：

```sh
python3 scripts/build.py batch --package ./experiments/splithttp --output .build/splithttp
```

下面命令需要在独立命名空间中执行，例如 `sudo unshare -n` 后启用 `lo`，降回普通用户并设置 `VEIL_ISOLATED_NETNS=1`；PATH 应包含 Go、Node、curl、openssl 和 perf。脚本只在该命名空间对测试端口设置 netem，退出时删除临时证书、配置和浏览器 profile。

```sh
python3 scripts/split_http.py shapes --out .build/split-shapes-final --browser /path/to/chrome --samples 8
python3 scripts/split_http.py shapes --out .build/split-shapes-delay --browser /path/to/chrome --samples 4 --delay-ms 10
python3 scripts/split_http.py perf --out .build/split-perf-final --samples 5 --rounds 1000
python3 scripts/split_http.py download --out .build/split-download-final --delay-ms 10 --pause-seconds 30
python3 scripts/check.py batch --race
```

输出目录必须不存在。性能脚本默认绑核为服务端 14、客户端 22、目标 17、发生器 18；换机器复现时先调整为本机可用的不同物理核心，并单独执行性能测试。每组目录保留 `samples.json`、二进制 SHA-256 元数据和日志；原始记录只保存在本地忽略的 `.build`，不提交测试凭据或下载内容。

离线重新汇总（不需要提权或网络）：

```sh
python3 scripts/split_http_report.py .build/split-shapes-final .build/split-shapes-delay --perf .build/split-perf-final
```

实测夹具 SHA-256：`4a3fbe69ed6cde90fae151301faaa652d51287f376e701488bea4250fc5fedf4`；Veil：`c46c1beb4276ef679d4e5c65956cbd5d437bc6db9511562be774aeb4c5ad2ced`。

后续若继续研究，值得单独验证的是**发送调度与业务 write 解耦**：设定流量/延迟预算，检查批量合并、分片及必要的掩护数据能否削弱关联，同时保留性能退出标准。[TLS-in-TLS 研究 §8](https://censoredplanet.org/assets/tls_in_tls.pdf)讨论过分离方向与调度，但不能将仅拆连接等同于完整防御；[HTTP/2 标准](https://www.rfc-editor.org/rfc/rfc9113.html)规定的真实帧与流控也不会自动隐藏应用握手依赖。该方向尚未实现或验收。
