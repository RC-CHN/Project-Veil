# Veil v0.3 线协议规范

状态：互通规范，文档修订 1，2026-09-28。

本文完整定义 Veil v0.3 的线上字节、认证计算和流状态机。实现者只需本文、所引用的公开密码/TLS 标准及相应基础库；不需要原实现的源代码、编程语言、构建补丁、配置文件或 SDK。这里的客户端、服务端是协议角色，不限定操作系统。

“必须 / MUST”“不得 / MUST NOT”“应 / SHOULD”“可以 / MAY”分别表示互通要求、禁止行为、通常应采用但可说明理由偏离的行为，以及可选行为。整数默认无符号、大端；区间包含两端；长度以字节计。`||` 表示字节拼接，`x[a:b]` 表示从 0 起、左闭右开的切片。十六进制中的空格和换行不在线上传输。文中的字符串按 ASCII 编码，不隐含结尾 NUL；只有明确写出的 `00` 才是 NUL。

## 1. 范围与互通档位

Veil 在一条 TCP 上承载一个经过认证的 TLS 1.3 会话，再在会话内复用最多 8 条独立的双向 TCP 字节流。每条流有自己的目标地址、流量额度、半关闭及取消状态。

定义两个由部署信息预先选定的传输档位：

| 档位 | TLS 服务端身份验证 | 后续 Veil 协议 |
| --- | --- | --- |
| T：标准 TLS | 证书链、信任锚和主机名验证 | 第 4～10 节完全相同 |
| R：REALITY | 第 3 节的 ClientHello 认证和证书 HMAC 验证 | 第 4～10 节完全相同 |

只实现 T 的端点必须声明“Veil v0.3 / T”；它不能连接只提供 R 的监听器。完整实现应支持 T 和 R。双方必须提前知道档位；没有在线自动探测或降级协商。T 可先用通常的 TLS 库完成互通；R 需要能够构造指定 ClientHello、取得对应临时 X25519 私钥并自定义证书验证的 TLS 实现。

本版本没有 UDP、IP 包封装、服务端主动创建流、在线策略下发、PING/PONG、GOAWAY、重连续传或应用数据重放。未知帧类型不是扩展通道。协议版本由 AUTH 中的 `03` 确定，没有版本协商；不得把 v0.1 或 v0.2 当作兼容版本。

TLS、AUTH、MUX 是三层不同的边界：

```text
TCP 字节流
  TLS 1.3（T 或 R）
    客户端方向：AUTH 一次，然后 MUX 帧连续排列
    服务端方向：直接是 MUX 帧；没有 AUTH 回复
      OPEN / OPENED / FAILED / DATA / FIN / RESET / CREDIT / DONE
```

SOCKS、系统路由、入口监听、连接池 API、进程控制及界面不属于线协议。应用数据即原始目标 TCP 字节；Veil 不解析内层 HTTP/TLS。

## 2. 连接信息与共同 TLS 要求

双方通过线外配置取得下列信息。表中的名字只是本文标识符，不要求使用某种 JSON 或接口。

| 标识符 | 长度 / 内容 | 哪一方需要 |
| --- | --- | --- |
| `endpoint` | 代理服务器 IP/域名及 TCP 端口 | 客户端 |
| `mode` | `T` 或 `R` | 双方 |
| `server_name` | TLS SNI 主机名 | 双方 |
| `K` | 独立、随机的 32 字节 Veil 共享认证密钥 | 双方 |
| `trust` | 系统或私有 CA 信任锚；对应 `server_name` | T 客户端 |
| `certificate, certificate_private_key` | 可供 TLS 1.3 使用的证书及私钥 | T 服务端 |
| `R_private` | 32 字节 X25519 私钥输入 | R 服务端 |
| `R_public` | `X25519(R_private, basepoint)`，32 字节 | R 客户端 |
| `short_id` | 恰好 8 字节 | R 双方 |
| `cover` | 用于普通连接回落的 HTTPS 地址及端口 | R 服务端 |

密钥的文本交换格式为 RFC 4648 无 `=` 尾部填充的 base64url；32 字节对应 43 个字符。解码后的字节直接参与密码运算，不对字符串再哈希。`short_id` 的文本格式为 16 个十六进制字符，解码为 8 字节；不是 16 字节 ASCII。`K` 与 REALITY 密钥用途不同，不得混用。客户端不需要 `R_private`。

共同要求：

1. 使用 TLS 1.3。实际成功的 Veil 会话不得协商 TLS 1.2。TLS ClientHello 的兼容字段仍按 TLS 标准编码。
2. 支持 `TLS_AES_128_GCM_SHA256`（`1301`）和 X25519（`001d`）即可建立本文的最小互通组合。还可协商 TLS 1.3 的其他受支持套件；AUTH 的 HMAC 始终是 SHA-256。
3. SNI 使用部署提供的 `server_name`。T 客户端发送 ALPN `http/1.1`；T 服务端选择它。R 的 ClientHello 可以同时提供 `h2` 和 `http/1.1`，但选定值不改变 Veil 应用帧格式：这里没有 HTTP 请求、HTTP/2 preface 或 Upgrade 步骤。ALPN 不是 Veil 版本协商。
4. 不发送 TLS early data。当前档位使用完整握手，不依赖 PSK/会话恢复。收到网站回落产生的票据，不得将其当作认证成功的 Veil 会话。
5. 必须完成 TLS 的 CertificateVerify 和 Finished 验证，再发送/接受 Veil AUTH。R 的自定义证书证明只替代证书信任判断，不替代 TLS 握手签名、Finished 或记录认证。
6. 必须能导出第 4 节的 TLS exporter。任何 TLS 库都可使用，但只暴露加密 socket 而不暴露 exporter 的接口不够用。
7. 一个 TLS 应用记录可以包含多个 Veil 帧；一个 Veil 帧也可以跨任意多个 TLS 记录。TCP 分段、TLS record、一次读写调用均不是帧边界。
8. 接收方必须正确处理 TLS 1.3 的零字节记录填充，以及没有应用内容的合法记录。解密后只把 TLS application-data 内容交给 Veil 解析器。

T 使用普通 TLS 服务器认证：客户端必须验证证书链和 `server_name`，不得仅因证书可解析就接受。私有 CA 必须通过线外配置明确受信任。

## 3. R 档位的完整认证定义

这一节定义与本版本 REALITY 传输互通所需的额外字节和密码计算。它不要求特定浏览器版本、库或 ClientHello 扩展顺序。模仿某浏览器的完整流量行为是另一项任务；通过本节互通不意味着流量不可识别。

### 3.1 ClientHello 的结构约束

客户端产生一次新的、密码学安全随机的 32 字节 `client_random`，以及临时 X25519 密钥对 `(x, X)`。`X` 必须作为 ClientHello 的 X25519 key_share 发送。最小实现只提供 X25519，避免 HelloRetryRequest；R 本版本不定义认证重试的重算流程，因此不得依赖 HelloRetryRequest 完成连接。

ClientHello 必须使用 32 字节 `legacy_session_id`，并支持 TLS 1.3、ECDSA P-256/SHA-256 握手签名方案 `0403`。为让真实 cover 能选择自己的证书，还应提供 cover 支持的 ECDSA/RSA-PSS 签名方案。不得声明自己不能处理的证书压缩等扩展。

令 `CH0` 为**完整 TLS Handshake ClientHello 消息**：包括 `01 || uint24(body_length)` 四字节握手头，不包括五字节 TLS record 头；将其中 `legacy_session_id` 的 32 字节全部置零，其余字节保持将要发送的最终值。

在此结构下，相对于 `CH0` 开头的偏移是：

| 偏移 | 长度 | 字段 |
| --- | --- | --- |
| 0 | 1 | Handshake 类型 `01` |
| 1 | 3 | ClientHello body 长度 |
| 4 | 2 | `legacy_version = 0303` |
| 6 | 32 | `client_random` |
| 38 | 1 | `legacy_session_id` 长度 `20` |
| 39 | 32 | `legacy_session_id`，计算 AAD 时全零 |
| 71 | 可变 | cipher_suites、compression_methods、extensions 等剩余 ClientHello |

AAD 使用实际完整消息，包括扩展的实际顺序、长度、GREASE、key_share 等。不得通过重新排序扩展或重新序列化一个“等价”ClientHello 来计算 AAD。ClientHello 若分散在多个 TCP/TLS 片段，先重组握手消息。

### 3.2 ClientHello 认证密钥和密文

```text
Z        = X25519(x, R_public)
PRK      = HMAC-SHA256(key = client_random[0:20], message = Z)
A        = HMAC-SHA256(key = PRK, message = ASCII("REALITY") || 01)
nonce    = client_random[20:32]
P        = 01 08 01 00 || uint32_be(unix_time_seconds) || short_id
SID      = AES-256-GCM-Seal(key=A, nonce=nonce, plaintext=P, AAD=CH0)
CH       = CH0，唯独将字节 [39:71] 替换为 SID
```

`PRK/A` 是 HKDF-SHA256 Extract/Expand、salt=`client_random[0:20]`、info=`REALITY`、输出 32 字节的完整展开式。`P` 恰好 16 字节；前三字节 `01 08 01` 是此认证布局的客户端版本字段，**不是 Veil 版本**；第四字节保留为零。时间戳单位是 UTC Unix 秒。AES-GCM nonce 恰好 12 字节，tag 恰好 16 字节；`SID` 是 16 字节 ciphertext 后接 16 字节 tag。不得传输明文 `P`。

X25519 必须按其标准执行标量处理并拒绝无效/全零共享秘密。`x` 和 `client_random` 不得跨新连接复用。替换 SID 后，TLS 的实际 transcript 必须使用 `CH`，不能使用用于认证 AAD 的 `CH0`。

服务端用收到的 X25519 公钥 `X` 和 `R_private` 计算相同 `Z`、`A`，从收到的原始 CH 复制得到 `CH0`，仅清零 SID，然后解密 SID。认证通过条件为：GCM tag 正确、SNI 被允许、`short_id` 精确匹配，以及服务器当前时间与 P 中时间的差值绝对值不超过 60 秒。当前服务端不额外限制三字节客户端版本；发送方使用上面的值以获得稳定互通。

浏览器模板还可能提供 X25519MLKEM768（`11ec`）。认证取 key_share 的规则是：优先使用独立 X25519（`001d`，32 字节）；只有不存在该份额时，才使用 `11ec` 的 1216 字节客户端 key_exchange 中最后 32 字节 X25519 公钥，前 1184 字节为 ML-KEM-768 公钥。客户端必须使用被这个规则选中的公钥对应的私钥计算 A。独立 X25519 与混合份额中的 X25519 可以是不同密钥，不得混淆。

最小互通实现无需实现混合组：客户端只提供 `001d`，服务端从当前客户端提供的 `001d` 份额选择它即可。若选择 `11ec`，必须另外完整实现该组的 TLS 密钥交换；REALITY 认证密钥 A 仍使用上述规则，不随 TLS 实际选择的组变化。

### 3.3 服务端证明和正常 TLS 密钥交换

REALITY 静态密钥只生成 A，不充当 TLS 的服务端临时 ECDHE 密钥。成功认证后，服务端使用新的 TLS 临时密钥按正常 TLS 1.3 完成密钥交换。

服务端产生或安全持有一个 ECDSA P-256 密钥对。发送的叶证书必须是可解析的 DER X.509，公钥算法为 id-ecPublicKey（OID `1.2.840.10045.2.1`），命名曲线为 prime256v1/secp256r1（OID `1.2.840.10045.3.1.7`），公钥点按未压缩形式编码。令 `SPKI` 为证书中完整的 SubjectPublicKeyInfo DER（包括其 SEQUENCE 标签和长度），计算：

```text
certificate_proof = HMAC-SHA256(
    key = A,
    message = ASCII("Veil-v0.3 server authentication") || 00 || SPKI
)  // 恰好 32 字节
```

证明放入叶证书的**非 critical SubjectKeyIdentifier 扩展**（OID `2.5.29.14`）。该扩展的值类型是 OCTET STRING：扩展的 extnValue 解包后为 `04 20 || certificate_proof`；再解包这个 OCTET STRING 才得到 32 字节证明。不得把证明塞进证书 signatureValue。证书必须以它自己的 P-256 私钥正常自签名，算法为 ecdsa-with-SHA256（OID `1.2.840.10045.4.3.2`）；证书 signatureValue 是标准 DER 编码的 ECDSA 签名。

客户端必须校验 P-256 公钥、上述非 critical 扩展、32 字节证明的常量时间相等性，以及叶证书的 SHA-256/ECDSA 自签名。缺失、重复或格式错误的扩展必须失败。**即使证书属于公开 CA 信任的网站，只要这个证明不符，也不得继续 Veil AUTH。** R 不把证书链、证书主机名或有效期当作这个证明的替代品；SNI 仍参与 ClientHello 的认证。证书的序列号、名称、有效期和其他扩展没有固定互通常量；它们不能代替上述证明。证明认证 SPKI；自签名则完整校验包含证明在内的 TBSCertificate。

TLS CertificateVerify 是另一项验证：服务端必须从客户端实际提供的 signature_algorithms 中选择 `0403`，用该证书的私钥按 TLS 1.3 签署服务器 CertificateVerify 输入；客户端正常验证它，然后验证 Finished。客户端没有提供 `0403` 时不得强行选择它。不得为本认证修改浏览器模板的签名算法列表以加入未提供的算法。两端由实际 TLS transcript 导出 traffic secrets 和 exporter；A 不直接用于加密 Veil 数据。

本认证是 Veil v0.3 的 R 档位，**不与上游 REALITY 的 Ed25519/HMAC-SHA512 证书认证互通**。保留 ClientHello 的认证布局不表示证书认证也相同；不得静默回退到旧证明格式。部署时两端必须同时升级。

### 3.4 cover、回落与兼容边界

未通过 ClientHello 认证的连接进入普通 TCP 回落，转给预先配置的 cover；已读出的原始字节也必须完整转发，不能向探测端插入 Veil AUTH 错误或伪造 Veil 成功响应。保持两个方向独立传输，传播半关闭；错误或超时可以终止连接。已经通过 REALITY/TLS 但 Veil AUTH 失败的连接直接关闭，不再切换到网站流。

成功握手也可以利用 cover 的 ServerHello 参数和首个服务端 flight 的记录尺寸：读取 cover 对原始 ClientHello 的响应，保留相容的 TLS 版本、套件、group、session-id echo 和服务端随机数，**替换服务器 key_share 为本端临时公钥**，自己完成第 3.3 节的证书、CertificateVerify、Finished 和 TLS 加密。不能把 cover 的加密握手或证书证明原样拼接进另一套密钥的握手。

与现有 R 服务端互通的 cover 应直接支持 TLS 1.3 和 X25519，并且不要求 HelloRetryRequest；其首个响应须为正常 ServerHello，后续握手布局须可用于记录尺寸仿照。不能假设任意 HTTPS 站点都适合作为 cover。最小验证环境可用一个支持 `1301`、X25519、`http/1.1` 的 TLS 1.3 站点。

记录尺寸仿照是服务端的发送策略，不是另一端解析 Veil 所需的状态。ECDSA 签名 DER 长度会变化；真实证书或握手消息大于 cover 对应尺寸时，应按合法 TLS 消息发出，不得截断或因无法做负数填充而拒绝连接。此时记录可比 cover 更大，不能声称完全复现其尺寸。独立服务端可以自己构造符合第 3.3 节的 TLS 1.3 flight 与当前客户端握手，无需复制某个实现的 cover 缓存或记录切分算法；它仍应实现未认证回落。不同策略的被动可识别性不能仅凭互通测试评定。

浏览器名称、版本号和扩展乱序不是认证输入的固定常量。普通 X25519 最小 ClientHello 可完成协议验证，但不得因此宣称已获得浏览器指纹。若提供证书压缩扩展，必须按协商算法解压后再做相同证书和 transcript 验证。

## 4. TLS 绑定的 Veil AUTH

TLS 握手完成后，双方计算：

```text
B = TLS-Exporter(label = ASCII("EXPORTER-Veil-v0.3"),
                 context = empty byte string,
                 length = 32)
```

必须使用 TLS 1.3 标准 exporter（RFC 8446 §7.5），不是 early exporter、tls-unique、Finished 字节或 TLS traffic secret。label 没有 NUL；context 是长度 0 的字节串，不是一个 `00` 字节。支持 context 开关的 API 在 TLS 1.3 下也必须得到空 context 对应结果。

为消除 TLS 库 API 的歧义，设 `Hash` 为所选套件的哈希，`EMS` 为正常 TLS exporter_master_secret，`ExpandLabel` 为 TLS 1.3 HKDF-Expand-Label：

```text
S = ExpandLabel(EMS, "EXPORTER-Veil-v0.3", Hash(empty), Hash.length)
B = ExpandLabel(S,   "exporter",          Hash(empty), 32)
```

ExpandLabel 内部会加标准 `tls13 ` 前缀；调用 API 时不得自行重复添加。若套件使用 SHA-384，上面两处 Hash 也使用 SHA-384，但 B 长度仍为 32。

客户端生成密码学安全随机的 16 字节 `N`：

```text
body  = 03 || N
label = ASCII("Veil-v0.3 client authentication") || 00
tag   = HMAC-SHA256(key=K, message=label || B || body)
AUTH  = 01 00 00 31 || body || tag
```

AUTH 结构：

| 偏移 | 长度 | 含义 |
| --- | --- | --- |
| 0 | 1 | AUTH 类型 `01` |
| 1 | 3 | payload 长度 `000031`，十进制 49 |
| 4 | 1 | Veil 版本 `03` |
| 5 | 16 | 随机 nonce N |
| 21 | 32 | tag |

AUTH 总长恰好 53 字节，**没有 Stream ID**。只在客户端到服务端方向、每个物理 TLS 会话开头发送一次。服务器先检查头及固定长度，再读取 payload，检查版本并常量时间验证 tag。任何错误直接结束物理会话；不得分配攻击者声明的超大 payload，不发送应用层认证失败说明。

服务端不发送 AUTH_OK。客户端应直接把第一个 OPEN 接在 AUTH 后面发送，两者可以在同一 TLS 写入或记录内；服务器验证 AUTH 成功后继续解析缓冲区中尚未消费的 MUX 字节。客户端若只发 AUTH 并等回复，会停住。

N 不含时间或用户 ID，也不需要在服务器维护跨连接 nonce 表。相同 AUTH 在另一条 TLS 会话上因 B 不同而失效。每次重连必须重新计算 AUTH；不得重放旧连接的应用数据。

## 5. MUX 帧编码

AUTH 之后，两个方向独立解析下列帧：

```text
+---------+--------------------+-------------------+----------------+
| type: 1 | stream_id: 4 (BE)  | length: 3 (BE)    | payload: length|
+---------+--------------------+-------------------+----------------+
```

头长 8 字节；length 只计算 payload，不含头。每个头的偏移为 type=0，stream_id=1..4，length=5..7。无校验和、无内层 AEAD、无额外对齐、无结构体内存填充。所有帧均有非零奇数 stream_id；0 和偶数非法。

| type | 本文名称 | 合法方向 | payload 长度 | 内容 |
| --- | --- | --- | --- | --- |
| `01` | OPEN | 客户端→服务端 | 1..512 | 第 6 节的目标地址 |
| `02` | OPENED | 服务端→客户端 | 0 | 真实目标连接已成功 |
| `03` | FAILED | 服务端→客户端 | 1 | 目标连接失败码 |
| `04` | DATA | 双向 | 1..32768 | TCP 有序字节 |
| `05` | FIN | 双向 | 0 | 本方向的 DATA 已结束 |
| `06` | RESET | 双向 | 0 | 取消整个逻辑流 |
| `07` | CREDIT | 双向 | 2 | uint16_be，返还 DATA 帧额度 |
| `08` | DONE | 服务端→客户端 | 0 | 服务端两个转发方向均已完成 |

`DONE` 是名称；线上只有 `08`，没有额外字符串。长度必须先验证再分配/读取。未知类型、非法 ID、长度不符、被截断的帧是物理会话级错误。

客户端分配 ID，从 1 开始、每次递增 2 是推荐方式。服务端要求每个 OPEN 的 ID 严格大于之前见过的所有 OPEN ID；允许跳过奇数，但永不复用已打开、失败、取消或完成的 ID。新连接重新从 1 开始。不得发生 uint32 回绕；应在 ID 到达 `fffffffd` 后停止在该会话新建流，排空后建立新的物理连接。没有 GOAWAY 帧。

每个会话最多 8 条未终结流，正在连接目标的 OPEN 也占用槽位。超过服务端可接收槽位时，它对该 OPEN 返回 FAILED(1)，保留其 ID 高水位，不影响其他流。实现必须能在等待任一目标拨号或目标写入时继续解析其他流。

### 5.1 旧 ID 和并发取消

发送 RESET、FAILED 或完成流后，另一方向可能仍有之前已经在途的帧。接收方必须读取完合法长度的迟到帧并丢弃，保持物理解析器对齐，不得为已终结 ID 重建流或回送 RESET 风暴。

当前接收规则可无歧义表述为：维护最后已分配/接受的 ID `high_water`。OPEN 以外的帧若 ID 大于 high_water，则是会话错误；ID 不大于 high_water 且已无活动状态时，验证通用头后消费并忽略 payload。这也涵盖被跳过、但低于高水位的奇数 ID。不得利用此容错发送新数据或扩展消息。

OPEN 无论如何都检查“方向正确且 ID 严格增加”。活动流上的帧还必须通过第 7 节状态检查。即便目标 ID 已终结，未知类型、非法帧长度、偶数/零 ID 仍不能被容错。

## 6. OPEN 目标地址与 FAILED

OPEN payload 精确为下表结构，不包含 SOCKS 版本、CONNECT 命令、保留字节或用户名：

| 第一字节 | 地址 | 端口 |
| --- | --- | --- |
| `01` | 4 字节 IPv4 网络序 | 2 字节大端 |
| `03` | 1 字节域名字节数 L，再跟 L 字节域名 | 2 字节大端 |
| `04` | 16 字节 IPv6 网络序 | 2 字节大端 |

端口范围 1..65535；域名字节长度 1..255。IPv4 payload 总长 7，IPv6 总长 19，域名总长 L+4。不得有尾随字节、NUL 结尾、IPv6 中括号或 zone ID。域名不得含字节 `00..20`、`7f`、冒号 `3a`、斜杠 `2f`、反斜杠 `5c`。发送方应使用 ASCII DNS 名称，国际化域名先转换为 ASCII A-label；接收方不得以 Unicode 重新编码已认证字节来猜测另一地址。域名由服务端解析。

无法解码地址或目标连接失败，服务端返回 FAILED，停止此流。错误码：

| 字节 | 含义 |
| --- | --- |
| `01` | 通用拨号失败、地址无效、容量不足或本地策略拒绝 |
| `02` | DNS 解析失败 |
| `03` | 连接被拒绝 |
| `04` | 拨号超时 |

发送方只发送 1..4。接收方必须把其他字节也当作此流的通用失败，绝不能解释为成功；不因未知失败码关闭其他流。FAILED 不带错误字符串。

服务端必须等实际目标 TCP 连接成功才发送 OPENED。目标拒绝/超时不能提前报告 OPENED。客户端必须等 OPENED 后才发送 DATA/FIN；这是本版本的同步连接确认，不支持 OPEN 中携带早期数据。

## 7. 流状态与关闭语义

每条流维护 `opened`、`local_fin`、`remote_fin`、`terminal`。两个 FIN 标志方向相互独立，必须独立推进读写。

### 7.1 建立

客户端：分配新 ID → 发送 OPEN → 等待 OPENED 或 FAILED。等待期间可发 RESET 取消。收到 OPENED 才进入可传输状态；收到 FAILED 终结。重复 OPENED、可传输后收到 FAILED 或服务端向客户端发 OPEN 均是协议错误。

服务端：接收 OPEN → 解析地址并拨号 → 成功发送 OPENED，失败发送 FAILED。拨号过程中收到 RESET 必须取消拨号；即使取消与拨号完成竞争，也不得把后续连接结果交给其他流。服务端不得在 OPENED 之前发送 DATA 或 FIN。

### 7.2 DATA 与 FIN

DATA 只可在成功建立后发送，且不得晚于同方向 FIN。每个方向最多发送一个 FIN。FIN 本身不消耗数据额度，允许在额度为零时关闭已发完的数据方向。

收到 FIN 的处理顺序是：继续把此前该方向的 DATA 全部交付目标/应用，再向对应本地 TCP socket 执行写半关闭。不能直接关闭整个 socket，也不能丢弃已缓冲的数据。反方向仍可任意长时间传输；单向长期下载不是协议错误。

收到 FIN 之前，部分 DATA payload 一旦经 TLS 完整记录认证且可读，就应及时向应用发布；不得为了填满 32768 字节或整个声明帧而永久扣住已到达数据。只有完整消费该 DATA 帧后，才能返还对应额度。

### 7.3 DONE

服务端满足以下全部条件后发送一次 DONE：

- 已接收客户端 FIN，且该方向先前的数据已完整交给目标，目标写半关闭已完成。
- 已收到目标读 EOF，将目标此前的全部响应 DATA 发出，并向客户端发送过服务端 FIN。
- 两个方向都没有尚未报告的错误。

DONE 必须排在该流服务端 FIN 之后；可与 FIN 放进同一个 TLS 写入或记录。不得仅因为本地看见一个 EOF 就发送 DONE，也不需要等待目标应用额外发送“已处理”确认。DONE 只证明代理完成转发，不保证远端业务已持久化数据。

客户端仅在已经发送自身 FIN、接收服务端 FIN 后接受 DONE；提前/重复 DONE 或客户端发送 DONE 是会话错误。客户端可在排空响应并收到服务端 FIN 后向本地应用传播 EOF，但将此逻辑流判为成功、回收其槽位必须等待 DONE。DONE 不需要 ACK。

服务端在完成并送出 DONE 后可回收流槽位。流 ID 仍不可复用。其他槽位可在旧流关闭期间新建流；不要把整个物理会话锁到旧流 DONE 才可使用。

### 7.4 RESET、错误与物理断开

任一方向出现非 EOF 读写错误、取消、超时或用户放弃流时，发送 RESET（若会话仍可写）并终结逻辑流。RESET 无 payload，不区别错误原因；收到 RESET 后终止两方向、释放缓冲并向等待者报告失败，不回复 RESET。

正常的目标连接拒绝通过 FAILED 报告；已建立流的错误通过 RESET 报告。两者只影响该 ID，健康物理会话继续复用。错误帧、流量额度违规和非法状态则关闭整个物理会话，不尝试在任意字节位置重新同步。

物理 TCP EOF/RST、TLS alert 或无法解密时，所有尚未成功 DONE 的流必须收到连接失败。物理 EOF 不能伪装成每条流的正常 FIN/DONE；否则截断下载会被误报成功。已完成的流不因随后会话回收而改判失败。

主动结束整条会话可通过正常 TLS/TCP 关闭；本协议没有会话关闭帧。对未完成流这仍是失败。重连建立新会话与新 ID；不得自动重放已交付的 DATA。

### 7.5 活动流上的接收检查表

| 收到的帧 | 合法前提 / 动作 |
| --- | --- |
| OPENED | 本端是客户端，OPEN 尚未确认；设置 opened |
| FAILED | 本端是客户端，OPEN 尚未确认；终结 |
| DATA | opened，尚未接收对端 FIN，接收额度大于 0；扣 1 额度 |
| FIN | opened，尚未接收对端 FIN；设置 remote_fin，排空后报告 EOF |
| RESET | 任一未终结状态；终结并取消等待 |
| CREDIT | 额度值符合第 8 节；即使本方向已 FIN，也可消费此前在途的合法返还 |
| DONE | 本端是客户端，local_fin 与 remote_fin 均为真，尚未 DONE；成功终结 |

收到 DATA 后仍需消费完整声明长度才能解析下一帧；取消仅改变该 payload 的去向，不改变它的边界。对于已终结 ID，先应用第 5.1 节的迟到帧规则。

## 8. 按帧计数的流量控制

每个新流的**每个方向**都有隐含的 256 个 DATA 帧额度；不发送初始 CREDIT，也不协商窗口大小。一个 1 字节 DATA 和一个 32768 字节 DATA 都消耗 1 个额度，不是按字节扣费。

发送方维护 `send_credit = 256`。发送一个 DATA 前必须满足 `send_credit > 0`，然后减 1。收到 CREDIT(k) 时必须满足 `1 <= k <= 256 - send_credit`，再加 k；若不满足，是会话错误。FIN、RESET、OPEN 等控制帧不消耗 DATA 额度，不得被数据额度阻塞。

接收方维护 `receive_credit = 256` 和 `consumed_unreturned = 0`。每个 DATA 头通过检查时扣 1；在该帧的全部 payload 已交付应用或安全消费、对应缓冲可以释放后，`consumed_unreturned += 1`。发送 CREDIT(k) 只能返还尚未返还的完整帧，且须同步把自己的 receive_credit 加 k、consumed_unreturned 减 k。不能因刚收到头或部分 payload 就返还整帧额度。

为避免小控制包，接收方通常在累计 16..64 个完整帧后返还，可对同一 ID 合并尚未发送的 CREDIT。计数是逻辑 DATA 帧数，不是 TLS 记录数、socket Read 次数或字节数。合法实现可以采用别的返还批次，但必须及时返还，不能等 257 个 DATA 才返还前 256 个，也不能因为反方向无业务数据就一直不发 CREDIT。

发送额度耗尽只阻塞该流的 DATA，不能阻止物理读循环处理其他流和控制帧。单流最多容纳 256 × 32768 = 8 MiB 未消费载荷；8 流最多 64 MiB 载荷，再加实现元数据与 TLS 缓冲。实现可按需分配，不能要求每条空流预分配 8 MiB。

## 9. 随机化、TLS 填充和发送调度

本节区分接收互通要求和本地发送策略。两端的范围参数可以不同；它们不随 AUTH/OPEN 传输，无需 seed 同步。流 ID、类型、长度、密码算法和计数规则不得随机化。

### 9.1 接收端必须支持

- 任意合法的 DATA 帧长度，控制帧与 DATA 的合法组合，以及跨 TLS 记录的帧。
- 标准 TLS 1.3 记录填充，由 TLS 层剥离；不能把记录尾部 padding 当成 Veil payload。
- 同一记录内多个流的帧；同一流的有序字节不能因调度被重排。
- 没有填充的合法帧。无填充是可互通的发送策略，但没有流量伪装保证。

Veil 没有 `WASTE` 或 padding MUX 帧。不得在帧之间塞随机字节、空 DATA 或未知帧来做填充。无需发送周期性假包或强制上下行比例。

### 9.2 可独立实现的范围策略

以下描述当前发送策略的参数及一种完整取样方式，属于可选发送策略，不是握手协商字段。策略版本 1 与 Veil AUTH 版本 3 无关。每项都是包含边界的整数范围 `[min,max]`，必须满足 `min <= max` 及硬界限：

| 参数 | 硬界限 | 默认范围 | 单位 / 作用 |
| --- | --- | --- | --- |
| `quantum_bytes` | 4096..32768 | 8192..32768 | 多个可写流每轮调度份额 |
| `startup_bytes` | 128..8192 | 512..2048 | 启动阶段一次应用批次目标上限，含帧头 |
| `startup_writes` | 0..12 | 2..6 | 启动应用写批次数 / TLS 填充记录窗口 |
| `padding_limit` | 0..512 | 64..256 | 单 TLS 应用记录最多追加填充字节 |
| `padding_budget` | 0..4096 | 512..1024 | 一次启动激活的总填充字节预算 |
| `credit_blocks` | 16..64 | 16..64 | 返还额度的累计完整 DATA 帧阈值 |
| `control_padding_limit` | 0..128 | 16..96 | 控制帧单独成批时的记录填充上限 |

每个物理连接、每个发送方向，对每项部署范围独立取整数中心 `c ~ UniformInteger(min,max)`，收窄成 `[max(min,c-floor(c/4)), min(max,c+floor(c/4))]`。此后每次需要值时，在该连接的收窄范围内重新均匀取整数。尺寸取样不参与任何密码运算；密钥、随机 nonce 和 TLS 随机数必须另用密码学安全随机源。

可采用以下调度规则重现当前策略，而无需重现线程或锁：

1. 一个批次以 OPEN 或 OPENED 开始且当前仅有一条活动流时，取 startup_writes、padding_limit、padding_budget，重新激活本方向启动窗口。每个受启动窗口影响的应用批次取一次 startup_bytes；用完窗口恢复普通批次。认证前缀可与首个 OPEN 一起写入；不能为满足目标大小截掉 AUTH 或帧。
2. 当前多于一条活动流时停止启动切分。多个流同时可写时轮转访问，每轮每个流取 quantum_bytes 份额；单独可写流每帧至多 32768 字节。每个轮次先给每条就绪流一次机会，不能让大流无限占据写端。
3. 帧可以在约 128 KiB 的应用批次中合并；这只是发送缓冲策略，接收方不能将 128 KiB 当作解析边界。完整控制帧必要时可超出启动批次目标。
4. 激活填充时安装 `(limit, records, budget)`。每个后续 TLS 应用记录先扣一个 records；设该记录的应用内容长度为 n，取 `cap = min(16384-n, limit, budget)`，若 cap>0，则追加 `UniformInteger(1,cap)` 个零字节并从 budget 扣除。内容类型字节仍按 TLS 编码放在填充之前；达到 budget 或 records 即停止。满记录同样消耗一次窗口，但不强行膨胀。
5. 只有控制帧的非启动批次可重新安装一次 `limit=budget=control_padding_limit`、`records=1` 的预算，避免 CREDIT/FIN 等总是形成固定长度的独立记录。其他非独占启动或多流就绪批次可以关闭旧填充窗口。
6. 不为了凑足批次等待未来业务数据。不增添定时空包；不对未认证回落或 TLS alert 应用这些 Veil 业务填充预算。

这里的启动应用批次数与 TLS 记录数是不同计数器；一次应用批次可能拆成多条 TLS 记录。控制批次重设的预算也不是整条连接的累计上限，不能把 padding_budget 误解为物理连接生命周期总额。

更新范围只影响之后建立的物理连接，旧连接保持快照，是一种易于维护的本地配置行为；它不会产生线上更新帧。库、语言和 CPU 优化都不应改变以上强制线协议规则。

## 10. 超时、资源与最小实现步骤

超时不在线上协商。可采用握手及 AUTH 10 秒、目标拨号 10 秒、流空闲 120 秒、客户端空闲会话回收 20 秒；服务端无人使用的会话可保留更久。双方可以配置不同值，第三方不应把这些数字当作协议定时信号。

流空闲应按双向实际数据进展计时，部分 DATA 的进展也计入；不能仅因上传方向安静而杀掉持续下载。应用仍应有本地超时，以处理没有 FIN/RST 的网络黑洞。连接池策略、多少条物理连接、是否把大流分离到第二条会话均属本地策略。

最小客户端实现顺序：

1. 读部署信息；建立 TCP，完成 T 或 R 的 TLS 1.3 与服务端认证。
2. 导出 B，生成 N 和 AUTH；紧接 OPEN(id=1, target) 发送，不等待 AUTH 回复。
3. 收到 OPENED 后进入双向传输。每个 DATA 至多 32768 字节，维护两方向独立额度。
4. 本地读 EOF 后发送 FIN；收到对端 FIN 后排空接收数据并半关闭本地写方向。
5. 等双方 FIN 后的 DONE 完成这条流；可以在同一物理连接上发送 OPEN(id=3)。
6. 增加并发流、RESET/FAILED、额度阻塞恢复、截断检查及错误传播。
7. 最后增加可选随机发送策略，先用协议向量和互通场景验证字节正确性。

最小服务端执行对应逆过程，且必须把服务端身份握手认证与 Veil 客户端 AUTH 分开处理。不能用“TLS 握手成功”跳过 K 的校验，也不能用 REALITY short_id 替代 K。

## 11. 固定测试向量

本节全部密钥、nonce、时间戳及 TLS 中间量都是公开的测试值，不得用于部署。静态向量只验证字节运算；真实连接的 exporter、随机数和时间戳必须现场产生。

下面三个子向量相互独立。AUTH 的 B 是人为指定的 20..3f；不是 exporter_sha256 子向量的输出。exporter_sha256 使用 SHA-256 套件及给定 EMS。REALITY 的时间戳仅供静态计算，不可原样用于活连接；CH0 是完整 Handshake 消息，签名算法列表为 0403、0804、0805，未声明 Ed25519。P-256 公钥来自测试私钥标量 1。

```json
{
  "auth": {
    "B": "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f",
    "K": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
    "N": "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf",
    "frame": "0100003103a0a1a2a3a4a5a6a7a8a9aaabacadaeaf9e1d50ff1133d04ce8400dacfaab3a595cd58deeebb918e97ea7f9b38710268b"
  },
  "exporter_sha256": {
    "B": "807310dc0ceb373e3a87b7025b2ffa0ff42e1ad295609ded856c830010cae5b3",
    "EMS": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
  },
  "reality": {
    "A": "68e5a4d6fbfc0f93477d737fbdd45bd5f81578fbd172327b6db8e963e2ba4a3c",
    "CH0": "010000b20303000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20000000000000000000000000000000000000000000000000000000000000000000021301010000670000000f000d00000a636f7665722e74657374000a00040002001d000d00080006040308040805002b0003020304003300260024001d00208520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a0010000b000908687474702f312e31",
    "SID": "0d16c0d168880122363144ba2eb8cd582919414a0810eb467682fe060dac3ea6",
    "Z": "4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742",
    "client_private": "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a",
    "client_public": "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a",
    "client_random": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
    "p256_spki_der": "3059301306072a8648ce3d020106082a8648ce3d030107034200046b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c2964fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5",
    "server_private": "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb",
    "server_public": "de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f",
    "short_id": "0102030405060708",
    "subject_key_identifier": "287430d7d05648203e574da3af59e3d8038019c4fa8f6f7873e8a1be4b6e532c",
    "unix_time_hex": "65010203"
  }
}
```

SubjectKeyIdentifier 扩展的 extnValue 内容为 `0420287430d7d05648203e574da3af59e3d8038019c4fa8f6f7873e8a1be4b6e532c`。ECDSA 签名本身不要求固定输出；必须验证其数学正确性。下面的 MUX 字节与 TLS 密钥无关：

| 含义 | 完整帧十六进制 |
| --- | --- |
| OPEN(1), 127.0.0.1:443 | `0100000001000007017f00000101bb` |
| OPENED(1) | `0200000001000000` |
| DATA(1), ASCII abc | `0400000001000003616263` |
| CREDIT(1), 返还 32 帧 | `07000000010000020020` |
| FIN(1) | `0500000001000000` |
| DONE(1) | `0800000001000000` |
| FAILED(3), connection refused | `030000000300000103` |
| RESET(5) | `0600000005000000` |


## 12. 互通验收场景

实现至少应验证以下行为；只看到 TCP connect 或 TLS 握手成功不足以说明 Veil 互通：

1. T 和 R 分别完成 TLS → AUTH → OPENED → 双向数据 → 双 FIN → DONE。
2. AUTH 与第一个 OPEN 合并发送，以及逐字节/任意 TLS 记录分片，结果一致。
3. 一条物理连接上顺序使用 ID 1、3、5；再验证最多 8 条独立并发流。
4. 单方向发送超过 256 个 DATA，实际依靠 CREDIT 继续；把一条流停在零额度不应卡住另一条流。
5. 目标拒绝返回 FAILED(3)，随后在同会话打开有效目标成功。
6. 在 OPEN 等待或 DATA 中途 RESET，只取消对应流；对在途残余 payload 完整丢弃。
7. 两种半关闭先后顺序，先前数据完整、反方向可以继续；不能用 RESET 冒充正常完成。
8. 截断 DATA、物理 EOF/RST、提前 DONE、偶数 ID、重复 OPEN、未知帧、超大帧、零/溢出 CREDIT 均不会被当作正常成功。
9. 错 K、错 exporter、改 nonce/版本的 AUTH 被拒；把同一 AUTH 重放到新 TLS 连接被拒。
10. R 的错 short_id、错 AAD、过期时间不会进入 Veil；客户端拒绝普通网站证书和错误 HMAC，即使它们受公开 CA 信任。
11. TLS padding 剥离后 payload 不变；更换发送方随机范围不需要接收方同步参数。

互通验证必须分别记录传输档位和实际覆盖场景。浏览器外观、抗主动探测、真实网络阻断率和性能需另外测试；本文没有为这些指标提供通过承诺。

## 13. 公开基础标准

下列文档定义本文使用的基础格式/算法，不需要任何 Veil 仓库文件：

- [RFC 8446：TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446.html)，尤其是记录层、CertificateVerify、密钥派生和 §7.5 exporter。
- [RFC 5869：HKDF](https://www.rfc-editor.org/rfc/rfc5869.html)。
- [RFC 7748：X25519](https://www.rfc-editor.org/rfc/rfc7748.html)。
- [RFC 5280：X.509 与 SubjectKeyIdentifier](https://www.rfc-editor.org/rfc/rfc5280.html)，尤其是 §4.2.1.2。
- [RFC 5480：X.509 中的椭圆曲线公钥编码](https://www.rfc-editor.org/rfc/rfc5480.html)。
- [RFC 2104：HMAC](https://www.rfc-editor.org/rfc/rfc2104.html)及 [FIPS 180-4：SHA-2](https://doi.org/10.6028/NIST.FIPS.180-4)。
- [NIST SP 800-38D：AES-GCM](https://doi.org/10.6028/NIST.SP.800-38D)。
- [RFC 4648：base64url](https://www.rfc-editor.org/rfc/rfc4648.html)。
- [RFC 8879：TLS 证书压缩](https://www.rfc-editor.org/rfc/rfc8879.html)，仅提供该可选扩展时需要。

REALITY 附加认证的全部互通细节已在第 3 节给出。本文没有把“调用某库的 REALITY 函数”作为协议定义，也没有要求复刻某一实现的内存布局或补丁。
