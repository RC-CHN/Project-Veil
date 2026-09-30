# OpenWrt / LuCI

提供 `veil` 与 `luci-app-veil` 两个软件包。procd 管理 `veild`，LuCI 通过 rpcd 的固定方法操作公共控制服务；代理数据直接经过 Go 核心。

界面支持英文、简体中文，使用 LuCI 原生权限与翻译。首页按连接展示名称、对端地址、中转路径、本地代理入口、启用状态和 Google HTTPS 测试结果。每条连接可独立添加、编辑、导入、导出、复制、启停或删除。正常配置使用表单，JSON 编辑位于高级设置。

同一连接可以开启多个 SOCKS5、HTTP 或混合入口，共享一个 Veil 连接池。增删入口保留其他入口的连接；修改服务器或中转参数只重新建立受影响的连接。中转节点作为公共配置被引用，正在使用的中转不可删除。

“测试 Google”通过该连接请求 `https://www.google.com/generate_204`，校验证书并要求 HTTP 204，超时 8 秒。结果包含必要的隧道建立、目标连接和 TLS 握手时间，不等于 ping RTT。测试不会依赖浏览器或操作系统默认代理。

## 构建

构建器支持 OpenWrt 24.10 的 `.ipk` 和 25.12 的 `.apk`。多连接界面在 ImmortalWrt 25.12 x86_64、Argon 2.4.3 上实际安装验收。使用 Go 1.26.3、Python 3、GNU tar，以及对应 OpenWrt SDK 中的 `scripts/ipkg-build`。翻译工具来自官方 LuCI：

```sh
# 在 LuCI 源码中构建宿主机翻译工具
make -C /path/to/luci/modules/luci-base/src po2lmo CC=cc

# 在仓库根目录构建
python3 veil-service/scripts/openwrt_package.py \
  --sdk /path/to/openwrt-sdk \
  --po2lmo /path/to/luci/modules/luci-base/src/po2lmo \
  --architecture x86_64 --version 0.3.4-1
```

产物和 SHA-256 校验文件位于 `veil-service/.build/openwrt/`。`--architecture` 必须与设备 `opkg print-architecture` 一致；构建器支持 `x86_64`、`aarch64_*`、`arm_*`、`mips_*`、`mipsel_*`，ARM 使用 GOARM=5，MIPS 使用软浮点。ARM/MIPS 构建仍需对应设备验收。APK 包使用 apk-tools 3 的 `mkpkg`，通过 fakeroot 生成 root 属主：

```sh
fakeroot python3 veil-service/scripts/openwrt_package.py \
  --format apk --apk-tool /path/to/apk \
  --po2lmo /path/to/po2lmo \
  --architecture x86_64 --version 0.3.4-r1
```

若 apk 动态链接本地 libapk，先在 `LD_LIBRARY_PATH` 中加入其目录。构建器继承 fakeroot 所需的环境。

## 安装与连接

将对应架构的软件包传到设备 `/tmp/`：

```sh
opkg update
opkg install /tmp/veil_0.3.4-1_x86_64.ipk /tmp/luci-app-veil_0.3.4-1_all.ipk
/etc/init.d/veil enable
/etc/init.d/veil start
```

APK 设备对已校验的本地包执行 `apk add --allow-untrusted /tmp/veil-*.apk /tmp/luci-app-veil-*.apk`，随后启用并启动服务。

打开 **服务 → Veil**，添加连接填写服务器信息，或导入客户端 JSON 自动填入表单；选择直连或已保存的中转，添加本地代理入口，再“保存并应用”。导入配置默认不启用，确认监听地址后勾选启用。示例见 [client.mixed.json](../../../veil-core/examples/client.mixed.json)，其中凭据、服务器地址和证书路径需替换为实际值。自定义 CA 文件使用路由器上的绝对路径。

`mixed` 在同一端口接受 SOCKS5 CONNECT、普通 HTTP 转发和 HTTPS CONNECT。入口不要求本地用户认证；只供路由器自身使用时监听 `127.0.0.1:1080`，供 LAN 使用时选择路由器的 LAN 地址。由外部组件管理分流、透明代理、DNS 和路由。

```sh
curl --proxy socks5h://192.168.1.1:1080 https://example.com/
curl --proxy http://192.168.1.1:1080 https://example.com/
```

普通 HTTP 每个本地 TCP 连接处理一个请求，正文按背压流式转发；HTTPS CONNECT 保持隧道长连接和半关闭。

## 配置与生命周期

多连接配置唯一来源是 `/etc/veil/connections.json`（0600）。procd 使用 `veild -connections -autostart`；第一次启动时自动导入旧的单客户端 `config.json`，之后以多连接配置为准。包含固定转发目标的旧辅助服务需由部署者转换为中转配置。

LuCI 使用每条配置的 revision 防止覆盖其他管理员的修改；只读账户只能查看脱敏状态，读取凭据、测试或修改配置需要写权限。UCI 不复制代理配置。

```sh
veilctl -socket /var/run/veil/control.sock connections
veilctl -socket /var/run/veil/control.sock -id NODE_ID connection_test
veilctl -socket /var/run/veil/control.sock -id NODE_ID -if-revision HASH connection_stop
logread -e veild
```

“仅保存”保留当前运行状态并显示待应用提示；“保存并应用”使该连接生效。启用状态写入配置，停用后重启 daemon 仍保持停用。`/etc/init.d/veil disable` 关闭整个管理服务的开机启动。升级包保留 `/etc/veil/`，升级后执行 `/etc/init.d/veil start`；sysupgrade 保留列表包含该目录。

rpcd 固定连接 `/var/run/veil/control.sock`，不接受命令、目标测试 URL 或 socket 路径参数。多连接方法见[服务说明](../../README.md)。

## 界面验收

`scripts/luci_smoke.cjs` 使用 Playwright 操作真实 LuCI：导入、保存并应用、重命名、代理访问、权限边界、桌面与手机截图。测试登录发生在录屏之前，演示不展开含凭据的 JSON。仅允许运行于显式指定的本地测试 VM：

```sh
export VEIL_LUCI_TEST=1
export VEIL_LUCI_URL=http://127.0.0.1:18084/cgi-bin/luci/admin/services/veil
export VEIL_LUCI_PASSWORD='disposable-test-password'
export VEIL_LUCI_CONFIG=/path/to/test-client-mixed.json
export VEIL_LUCI_LANGUAGE=zh  # LuCI 服务端语言也需设置为 zh_cn
export VEIL_LUCI_COLOR=dark
export VEIL_LUCI_VIDEO=1
export VEIL_LUCI_OUTPUT=/path/to/ignored-artifacts
node veil-service/scripts/luci_smoke.cjs
```

安装 Playwright 及 Chromium，或用 `PLAYWRIGHT_MODULE`、`CHROMIUM_PATH` 指定已有工具。设置 `VEIL_HTTP_TARGET` 和 `VEIL_PROXY_ADDRESS` 可增加 SOCKS/HTTP 两条真实代理路径检查；测试站正文需包含 `Veil SOCKS5 + HTTP acceptance passed`。`VEIL_LUCI_USER` 与 `VEIL_LUCI_READONLY=1` 用于只读账户验收，同时检查直接调用配置 RPC 被拒绝。只读测试账户的 read 组需包含 `unauthenticated`、`luci-base`、`luci-app-veil`，使用 Argon 时再加 `luci-theme-argon`；不授予 write 组。

### 完整连接导入与导出

连接卡片的“导出”生成自包含 JSON：`version: 1`、`profile` 和可选 `relay`。中转地址、认证与 TLS 设置随连接一起携带；`ca_file` 引用的证书转换为 `tls.ca_pem`，不依赖原设备上的文件路径。导入自动识别路径并创建新的连接和中转 ID，保存时一次写入；本地代理监听地址和端口在确认表单中调整。导出的文件含连接凭据，请作为连接密钥保管。

中转卡片只管理共享配置；Google HTTPS 测试位于完整连接卡片上，测量经过所选中转及最终出口的请求。界面根据配置中的依赖关系显示路径，不根据节点名称、IP 或地区推断角色。

## 中转连接

完整的服务器安装、同端口多出口、连接包和双层 Veil 配置见[中转部署与连接](../../RELAY.md)。
