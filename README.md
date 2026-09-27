# Veil v0.1

Go TCP 代理，提供 SOCKS5 CONNECT 客户端、TLS 1.3/REALITY 服务端、业务鉴权、连接复用与双向半关闭。支持有界连接池、背压、取消和超时。当前为实验原型，不提供 TUN、UDP 或移动端 UI；流量特征与回落行为尚未通过抗识别验收。

v0.1 将首次 AUTH 与 OPEN 同批发送，目标连接成功后返回 OPEN_OK；满 DATA 帧连同帧头为 128 KiB。客户端和服务端需要同时升级，旧版鉴权会被拒绝。

## 构建与运行

使用 Go 1.26.3 和 Python 3。原生构建不依赖 cgo 或 OpenSSL。请用 Makefile 或 `scripts/build.py` 构建：所有后端均需要版本校验的 REALITY 回落接口补丁，直接 `go build` 不包含该接口，会编译失败。

```sh
make build
.build/veil -keygen
.build/veil -config server.json
.build/veil -config client.json
```

从 `examples/` 复制配置，替换全部占位符。两端使用相同业务 `secret` 和 `short_id`；REALITY 私钥只放服务端，公钥放客户端。配置文件包含凭据，应限制读取权限。

REALITY 的 `cover_address` 指向兼容所用浏览器模板的 TLS 1.3 参考站点，两端 `server_name` 一致。普通 TLS 模式使用 `certificate` 和 `private_key_file`，客户端通过 `ca_file` 增加自有 CA；证书和主机名验证始终启用。

SOCKS5 入口仅支持无认证 CONNECT，默认监听回环地址。按需修改监听地址、连接数和超时配置；超时单位为秒。

## 检查

```sh
make test
make race
make vet
```

TLS/REALITY 集成测试使用本机 `openssl s_server`，只连接自有回环端点。

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

生成代码、依赖、临时凭据、日志和二进制集中在 `.build/`，由 `make clean` 删除。

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

`benchmark.py --modes E --rounds 10000` 测热连接延迟；`--modes C --connections 1 --rounds 1` 测冷连接。输出目录不可覆盖已有原始结果；测量数据不纳入版本控制。

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

本项目使用 GPL-3.0-or-later；第三方代码来源见 [THIRD_PARTY.md](THIRD_PARTY.md) 和 [LICENSE](LICENSE)。
