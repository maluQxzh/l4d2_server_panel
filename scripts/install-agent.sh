#!/usr/bin/env sh
set -eu
umask 077

CENTRAL_URL="${CENTRAL_URL:-}"
ENROLLMENT_TOKEN="${ENROLLMENT_TOKEN:-}"
NODE_NAME="${NODE_NAME:-$(hostname)}"
AGENT_VERSION="${AGENT_VERSION:-latest}"
AGENT_DOWNLOAD_URL="${AGENT_DOWNLOAD_URL:-}"
AGENT_BINARY="${AGENT_BINARY:-}"
AGENT_SHA256="${AGENT_SHA256:-}"
ALLOW_UNVERIFIED_DOWNLOAD="${ALLOW_UNVERIFIED_DOWNLOAD:-false}"
MINIMUM_FREE_SPACE_GB="${MINIMUM_FREE_SPACE_GB:-20}"

service_name=l4d2panel-agent
service_user=l4d2panel
config_dir=/etc/l4d2panel
config_path=$config_dir/agent.json
data_root=/var/lib/l4d2panel
instance_root=/srv/l4d2panel/instances
install_root=/opt/l4d2panel/bin
target_binary=$install_root/l4d2panel-agent

usage() {
  echo "用法: sudo CENTRAL_URL=https://panel.example.com ENROLLMENT_TOKEN=... $0" >&2
  echo "回环或私网 IP 可显式使用 http://；公网地址必须使用 https://。" >&2
  echo "升级现有节点时 ENROLLMENT_TOKEN 可省略；可用 AGENT_BINARY 指向本地 amd64 二进制。" >&2
}

die() { echo "错误: $*" >&2; exit 1; }
run_as_agent() {
  runuser -u "$service_user" -- env HOME="$data_root" "$@"
}

[ "$(id -u)" -eq 0 ] || die '安装系统服务需要 root 权限。'
[ -n "$CENTRAL_URL" ] || { usage; exit 2; }
case "$CENTRAL_URL" in https://*|http://*) ;; *) die 'CENTRAL_URL 必须使用 https://，或对回环/私网 IP 使用 http://。' ;; esac
case "$CENTRAL_URL" in *[[:space:]]*|*'"'*|*'\'*|*'@'*|*'?'*|*'#'*|*..*) die 'CENTRAL_URL 包含不允许的字符或路径' ;; esac
case "$CENTRAL_URL" in
  http://*) echo '警告: 局域网 HTTP 不加密注册令牌、节点凭据、任务和结果；仅应在可信隔离网络中使用。' >&2 ;;
esac
case "$(uname -m)" in x86_64|amd64) ;; *) die '首版仅支持 amd64。' ;; esac
case "$NODE_NAME" in ''|*[!A-Za-z0-9._-]*) die 'NODE_NAME 只能包含字母、数字、点、下划线和连字符。' ;; esac
case "$ENROLLMENT_TOKEN" in *[!A-Za-z0-9._-]*) die '注册令牌格式无效。' ;; esac
case "$AGENT_VERSION" in *[!A-Za-z0-9._-]*) die 'AGENT_VERSION 格式无效。' ;; esac
case "$MINIMUM_FREE_SPACE_GB" in ''|*[!0-9]*) die 'MINIMUM_FREE_SPACE_GB 必须是整数。' ;; esac
[ "$MINIMUM_FREE_SPACE_GB" -ge 10 ] || die 'MINIMUM_FREE_SPACE_GB 不得低于 10。'
command -v systemctl >/dev/null 2>&1 || die '需要 systemd/systemctl。'
command -v runuser >/dev/null 2>&1 || die '需要 util-linux 的 runuser。'

if [ -r /etc/os-release ]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID:-}:${VERSION_ID:-}" in
    ubuntu:22.04|ubuntu:24.04|debian:12) ;;
    *) echo "警告: ${PRETTY_NAME:-当前系统} 属于尽力兼容范围；正式支持 Ubuntu 22.04/24.04、Debian 12。" >&2 ;;
  esac
fi

if command -v apt-get >/dev/null 2>&1; then
  dpkg --add-architecture i386
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates curl tar lib32gcc-s1 lib32stdc++6
else
  for dependency in curl tar; do command -v "$dependency" >/dev/null 2>&1 || die "缺少 $dependency。"; done
  ldconfig -p 2>/dev/null | grep -Eq 'libgcc_s.*(x86-32|i386|lib32)|libstdc\+\+.*(x86-32|i386|lib32)' || \
    echo '警告: 未确认 32 位 libgcc/libstdc++；请按发行版安装对应 i686 运行库。' >&2
fi

required_kb=$((MINIMUM_FREE_SPACE_GB * 1024 * 1024))
check_free_space() {
  storage_path=$1
  available_kb=$(df -Pk "$storage_path" 2>/dev/null | awk 'NR==2 {print $4}')
  case "${available_kb:-}" in ''|*[!0-9]*) die "无法读取 $storage_path 所在文件系统的可用空间。" ;; esac
  [ "$available_kb" -ge "$required_kb" ] || die "$storage_path 所在磁盘可用空间不足，需要至少 ${MINIMUM_FREE_SPACE_GB} GB。"
}
# The final directories do not exist on a fresh node. Check their existing
# parent mount points so installation does not mistake a failed df lookup for
# zero free space. Backups/data and game instances may live on different
# filesystems, therefore both locations must satisfy the configured minimum.
check_free_space /var/lib
if [ -d /srv ]; then
  check_free_space /srv
else
  check_free_space /
fi

if ! id "$service_user" >/dev/null 2>&1; then
  useradd --system --home-dir "$data_root" --create-home --shell /usr/sbin/nologin "$service_user"
fi
install -d -o root -g root -m 0755 "$install_root"
install -d -o root -g "$service_user" -m 0770 "$config_dir"
install -d -o "$service_user" -g "$service_user" -m 0750 "$data_root" "$instance_root"

downloaded_binary=$(mktemp /tmp/l4d2panel-agent.XXXXXX)
checksum_file=
cleanup_download() {
  rm -f -- "$downloaded_binary"
  [ -z "$checksum_file" ] || rm -f -- "$checksum_file"
}
trap cleanup_download EXIT INT TERM
if [ -n "$AGENT_BINARY" ]; then
  [ -f "$AGENT_BINARY" ] || die "找不到 AGENT_BINARY: $AGENT_BINARY"
  cp "$AGENT_BINARY" "$downloaded_binary"
else
  if [ -z "$AGENT_DOWNLOAD_URL" ]; then
    if [ "$AGENT_VERSION" = latest ]; then
      AGENT_DOWNLOAD_URL=https://github.com/l4d2-panel/l4d2panel/releases/latest/download/l4d2panel-agent-linux-amd64
    else
      AGENT_DOWNLOAD_URL="https://github.com/l4d2-panel/l4d2panel/releases/download/${AGENT_VERSION}/l4d2panel-agent-linux-amd64"
    fi
  fi
  case "$AGENT_DOWNLOAD_URL" in https://*) ;; *) die 'AGENT_DOWNLOAD_URL 必须使用 https://' ;; esac
  curl --fail --location --proto '=https' --tlsv1.2 "$AGENT_DOWNLOAD_URL" -o "$downloaded_binary"
  if [ -z "$AGENT_SHA256" ]; then
    checksum_file=$(mktemp /tmp/l4d2panel-agent.XXXXXX.sha256)
    if curl --fail --location --proto '=https' --tlsv1.2 "$AGENT_DOWNLOAD_URL.sha256" -o "$checksum_file"; then
      AGENT_SHA256=$(awk 'match(tolower($0), /[a-f0-9]{64}/) { print substr(tolower($0), RSTART, RLENGTH); exit }' "$checksum_file")
    elif [ "$ALLOW_UNVERIFIED_DOWNLOAD" != true ]; then
      die '无法下载 Agent SHA-256 校验文件；如确认来源可信，可显式设置 ALLOW_UNVERIFIED_DOWNLOAD=true。'
    fi
  fi
fi
if [ -n "$AGENT_SHA256" ]; then
  case "$AGENT_SHA256" in *[!A-Fa-f0-9]*) die 'AGENT_SHA256 格式无效。' ;; esac
  [ "${#AGENT_SHA256}" -eq 64 ] || die 'AGENT_SHA256 必须为 64 位十六进制。'
  actual_sha256=$(sha256sum "$downloaded_binary" | awk '{print $1}')
  [ "$(printf '%s' "$actual_sha256" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$AGENT_SHA256" | tr '[:upper:]' '[:lower:]')" ] || die 'Agent SHA-256 校验失败。'
elif [ -z "$AGENT_BINARY" ] && [ "$ALLOW_UNVERIFIED_DOWNLOAD" != true ]; then
  die '下载的 Agent 未通过 SHA-256 校验。'
fi
chmod 0755 "$downloaded_binary"

service_exists=false
if systemctl cat "$service_name.service" >/dev/null 2>&1; then
  service_exists=true
  systemctl stop "$service_name.service" || true
fi
install -o root -g root -m 0755 "$downloaded_binary" "$target_binary"
rm -f -- "$downloaded_binary"
[ -z "$checksum_file" ] || rm -f -- "$checksum_file"
trap - EXIT INT TERM

keep_existing_enrollment=false
if [ -f "$config_path" ] && \
   grep -Eq '^[[:space:]]*"?node_id"?[[:space:]]*:[[:space:]]*"[^"[:space:]]+"' "$config_path" && \
   grep -Eq '^[[:space:]]*"?credential"?[[:space:]]*:[[:space:]]*"[^"[:space:]]+"' "$config_path"; then
  keep_existing_enrollment=true
fi
if [ "$keep_existing_enrollment" = true ]; then
  configured_central=$(sed -n 's/^[[:space:]]*"\{0,1\}central_url"\{0,1\}[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$config_path" | head -n 1)
  [ "${configured_central%/}" = "${CENTRAL_URL%/}" ] || die '现有节点凭据属于另一个 CENTRAL_URL；请先卸载并移走旧配置后重新注册。'
fi
if [ "$keep_existing_enrollment" = false ]; then
  [ -n "$ENROLLMENT_TOKEN" ] || die '首次安装必须设置 ENROLLMENT_TOKEN。'
  cat >"$config_path" <<EOF
{
  "central_url": "$CENTRAL_URL",
  "enrollment_token": "$ENROLLMENT_TOKEN",
  "node_name": "$NODE_NAME",
  "instance_roots": ["$instance_root"],
  "steamcmd_path": "",
  "data_dir": "$data_root",
  "heartbeat_interval": "10s",
  "metrics_interval": "15s",
  "steamcmd_concurrency": 1,
  "insecure_skip_verify": false
}
EOF
  chown "$service_user:$service_user" "$config_path"
  chmod 0600 "$config_path"
  run_as_agent "$target_binary" validate --config "$config_path"
  run_as_agent "$target_binary" enroll --config "$config_path"
else
  run_as_agent "$target_binary" validate --config "$config_path"
fi

dropin_dir=/etc/systemd/system/$service_name.service.d
install -d -o root -g root -m 0755 "$dropin_dir"
cat >"$dropin_dir/hardening.conf" <<EOF
[Service]
User=$service_user
Group=$service_user
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectHostname=true
ProtectClock=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
ReadWritePaths=$config_dir $data_root $instance_root
Restart=always
RestartSec=10s
TimeoutStopSec=45s
KillMode=control-group
EOF
chmod 0644 "$dropin_dir/hardening.conf"

if [ "$service_exists" = false ]; then
  "$target_binary" install --config "$config_path"
else
  systemctl daemon-reload
  systemctl enable "$service_name.service" >/dev/null
  systemctl restart "$service_name.service"
fi
systemctl is-active --quiet "$service_name.service" || die 'Agent systemd 服务未能启动。'

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
  echo '提示: UFW 已启用。节点只需出站连接 CENTRAL_URL 对应端口；创建实例后请仅开放实际使用的游戏/A2S/RCON 端口。' >&2
elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
  echo '提示: firewalld 已启用。节点只需出站连接 CENTRAL_URL 对应端口；创建实例后请仅开放实际使用的游戏/A2S/RCON 端口。' >&2
fi
echo "节点 Agent 已以低权限账号 $service_user 安装并启动。"
