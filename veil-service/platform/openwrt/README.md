# OpenWrt / LuCI

提供 `veil` 与 `luci-app-veil` 两个软件包。procd 管理 `veild`，LuCI 通过 rpcd 的固定方法操作公共控制服务；代理数据直接经过 Go 核心。

界面支持英文、简体中文，使用 LuCI 原生权限与翻译。可导入 JSON 连接配置、选择 SOCKS5 / HTTP / 同端口混合入口、修改监听地址、校验、保存、启动、停止和应用配置。保存保留当前连接；应用已保存配置时先确认再重启。默认主题和 Argon 的浅色、深色、手机布局均经过实际浏览器检查。

## 构建

当前产物为 OpenWrt 24.10 使用的 `.ipk`；实际安装验收环境为 24.10.8 x86_64、Argon 2.4.7。使用 Go 1.26.3、Python 3、GNU tar，以及对应 OpenWrt SDK 中的 `scripts/ipkg-build`。翻译工具来自官方 LuCI：

```sh
# 在 LuCI 源码中构建宿主机翻译工具
make -C /path/to/luci/modules/luci-base/src po2lmo CC=cc

# 在仓库根目录构建
python3 veil-service/scripts/openwrt_package.py \
  --sdk /path/to/openwrt-sdk \
  --po2lmo /path/to/luci/modules/luci-base/src/po2lmo \
  --architecture x86_64 --version 0.3.0-1
```

产物和 SHA-256 校验文件位于 `veil-service/.build/openwrt/`。`--architecture` 必须与设备 `opkg print-architecture` 一致；构建器支持 `x86_64`、`aarch64_*`、`arm_*`、`mips_*`、`mipsel_*`，ARM 使用 GOARM=5，MIPS 使用软浮点。ARM/MIPS 构建仍需对应设备验收。OpenWrt 的 apk 格式需另行打包。

## 安装与连接

将对应架构的软件包传到设备 `/tmp/`：

```sh
opkg update
opkg install /tmp/veil_0.3.0-1_x86_64.ipk /tmp/luci-app-veil_0.3.0-1_all.ipk
/etc/init.d/veil enable
/etc/init.d/veil start
```

打开 **服务 → Veil**，导入服务端管理员提供的客户端 JSON，选择代理协议和监听地址，保存后启动。示例见 [client.mixed.json](../../../veil-core/examples/client.mixed.json)，其中凭据、服务器地址和证书路径需替换为实际值。自定义 CA 文件使用路由器上的绝对路径。

`mixed` 在同一端口接受 SOCKS5 CONNECT、普通 HTTP 转发和 HTTPS CONNECT。入口不要求本地用户认证；只供路由器自身使用时监听 `127.0.0.1:1080`，供 LAN 使用时选择路由器的 LAN 地址。由外部组件管理分流、透明代理、DNS 和路由。

```sh
curl --proxy socks5h://192.168.1.1:1080 https://example.com/
curl --proxy http://192.168.1.1:1080 https://example.com/
```

普通 HTTP 每个本地 TCP 连接处理一个请求，正文按背压流式转发；HTTPS CONNECT 保持隧道长连接和半关闭。

## 配置与生命周期

配置唯一来源为控制服务的 `/etc/veil/config.json`。LuCI 使用 revision 防止覆盖其他管理员刚保存的修改；只读账户只能查看状态，读取含凭据的配置需要写权限。UCI 不复制代理业务配置。

```sh
veilctl -socket /var/run/veil/control.sock status
veilctl -socket /var/run/veil/control.sock -config /tmp/client.json validate
veilctl -socket /var/run/veil/control.sock -config /tmp/client.json save
veilctl -socket /var/run/veil/control.sock start
logread -e veild
```

LuCI 的停止按钮停止代理实例，保留管理服务；procd 下次启动 daemon 时会启动已保存配置。需要禁用开机启动时执行 `/etc/init.d/veil disable`。升级 `veil` 软件包会停止旧进程，保留配置，升级后执行 `/etc/init.d/veil start`。卸载先移除 `luci-app-veil` 再移除 `veil`；配置保留在 `/etc/veil/`，重装后可继续使用。sysupgrade 保留列表也包含该目录。

rpcd 适配器位于 `/usr/libexec/rpcd/veil`，只支持 `status/config/validate/save/start/stop/restart`，固定连接私有 Unix socket，不接受命令或 socket 路径参数。控制协议见[服务说明](../../README.md)。

## 界面验收

`scripts/luci_smoke.cjs` 使用 Playwright 操作真实 LuCI：导入、保存、启停、取消和确认应用、错误提示、桌面与手机截图。测试登录发生在录屏之前，演示不展开含凭据的 JSON。仅允许运行于显式指定的本地测试 VM：

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

安装 Playwright 及 Chromium，或用 `PLAYWRIGHT_MODULE`、`CHROMIUM_PATH` 指定已有工具。设置 `VEIL_HTTP_TARGET`、`VEIL_HTTPS_TARGET`、`VEIL_TEST_CA` 和 `VEIL_PROXY_ADDRESS` 可增加四条真实代理路径检查；测试站正文需包含 `Veil SOCKS5 + HTTP acceptance passed`。`VEIL_LUCI_USER` 与 `VEIL_LUCI_READONLY=1` 用于只读账户验收，同时检查直接调用配置 RPC 被拒绝。只读测试账户的 read 组需包含 `unauthenticated`、`luci-base`、`luci-app-veil`，使用 Argon 时再加 `luci-theme-argon`；不授予 write 组。
