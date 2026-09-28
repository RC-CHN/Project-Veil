# Linux 安装与管理

发行包包含静态 Go batch 版 `veild`、`veilctl`、`veil-system`，以及 systemd 模板、用户声明和构建清单。提供 amd64、arm64、armv7 三种架构。每个实例使用独立的配置目录、控制 socket 和日志。

## 构建与安装

在源码仓库执行：

```sh
python3 veil-service/scripts/package.py --version 0.3.0 --arch amd64
```

产物位于 `veil-service/.build/releases/`。`manifest.json` 记录版本、提交、工作区是否有未提交修改、架构、Go 版本及文件摘要。解压前校验下载的包：

```sh
sha256sum -c veil-0.3.0-linux-amd64.tar.gz.sha256
tar -xzf veil-0.3.0-linux-amd64.tar.gz
cd veil-0.3.0-linux-amd64
sudo sh install.sh install
veilctl -version
```

安装位置为 `/usr/local/bin`、`/usr/local/lib/systemd/system`、`/usr/local/lib/sysusers.d` 和 `/usr/local/share/veil`。安装时创建专用 `veil` 用户并刷新 systemd 单元；实例由管理员显式启用。再次安装会替换程序文件，已运行实例在执行 `daemon-restart` 后使用新程序。

## 创建实例

实例名使用 1–48 个英文字母、数字、下划线或短横线。以下创建名为 `home` 的实例：

```sh
sudo veil-system home enable
sudo veil-system home validate /path/to/client.json
sudo veil-system home import /path/to/client.json
sudo veil-system home start
sudo veil-system home status
```

`enable` 启动守护进程并设置开机启动；首次启动后等待导入配置。`import` 校验并保存配置，现有连接继续运行；已有实例应用新配置使用 `restart`。配置也可以从标准输入导入：`sudo veil-system home import - < client.json`。

证书与私钥应放在 `veil` 用户可读的系统目录，并在配置中使用绝对路径。服务的 systemd 沙箱限制访问用户主目录。每个实例的持久配置位于 `/var/lib/veil-home/config.json`，控制入口为 `/run/veil-home/control.sock`。

## 日常操作

| 命令 | 操作 |
| --- | --- |
| `veil-system home status` | JSON 格式的代理状态、计数、最近连接错误与配置 revision |
| `veil-system home start` | 启动代理；已运行时保持当前配置 |
| `veil-system home stop` | 停止代理，保留守护进程的配置入口 |
| `veil-system home restart` | 用已保存配置重启代理，会断开当前连接 |
| `veil-system home daemon-status` | 查看 systemd 守护进程状态 |
| `veil-system home daemon-restart` | 重启整个守护进程，用于程序升级 |
| `veil-system home logs` | 查看最近 100 条日志 |
| `veil-system home logs --follow` | 持续查看日志 |
| `veil-system home disable` | 停止守护进程并取消开机启动 |

管理操作通常使用 `sudo`。`stop` 只停止当前代理运行；仍启用的实例在机器或守护进程重启后会自动启动保存的配置。`status` 中 `restart_required: true` 表示有待应用配置。若控制 socket 不可达，先看 `daemon-status` 和 `logs`。

高级脚本可直接使用 `veilctl -socket /run/veil-home/control.sock ...`，配合 `-if-revision` 避免并发修改覆盖。`veilctl profilegen` 可生成范围式流量参数。

## 卸载与暂存

先逐一停止并禁用实例，再卸载程序。持久配置和服务用户保留，方便重新安装。

```sh
sudo veil-system home disable
sudo sh /usr/local/share/veil/uninstall.sh uninstall
```

制作镜像或检查安装文件时使用已存在的暂存目录：

```sh
mkdir -p /tmp/veil-stage
sh install.sh install --destdir /tmp/veil-stage
sh install.sh uninstall --destdir /tmp/veil-stage
```

暂存模式只操作该目录，不调用 systemd 或修改主机用户。运行机制参考 systemd 官方的 [目录与权限管理](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)、[实例启停](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html) 和 [sysusers](https://www.freedesktop.org/software/systemd/man/latest/systemd-sysusers.html)。

## 验证发行包

`python3 veil-service/scripts/package_check.py ARCHIVE.tar.gz` 检查本机架构包的摘要、版本、暂存安装、升级、卸载与命令参数。CI 使用解压后的程序继续执行隔离网络中的生命周期检查。

`scripts/systemd_check.py` 在一次性 systemd Docker 容器内运行完整安装、多实例、权限隔离、升级、日志和卸载重装检查。将发行包解压到容器 `/release` 后，以 `VEIL_SYSTEMD_TEST=1 python3 systemd_check.py /release` 执行；脚本会拒绝普通主机环境。已在 Ubuntu 22.04 / systemd 249 验证。
