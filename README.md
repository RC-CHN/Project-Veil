# Veil v0

Go TCP 代理，提供 SOCKS5 CONNECT 客户端、TLS 1.3/REALITY 服务端、业务鉴权、连接复用与双向半关闭。支持有界连接池、背压、取消和超时。当前为实验原型，不提供 TUN、UDP 或移动端 UI；流量特征与回落行为尚未通过抗识别验收。

## 构建与运行

使用 Go 1.26.3。原生构建不依赖 cgo 或 OpenSSL。

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

## 许可

本项目使用 GPL-3.0-or-later；第三方代码来源见 [THIRD_PARTY.md](THIRD_PARTY.md) 和 [LICENSE](LICENSE)。
