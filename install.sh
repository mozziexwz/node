#!/usr/bin/env bash
# MSBOOST release bootstrap. Review this script before running it as root.
set -Eeuo pipefail
umask 077

readonly REPOSITORY=mozziexwz/node
readonly INITIAL_VERSION=v1.0.0

bootstrap_help() {
  printf '%s\n' \
    'MSBOOST 网站部署管理（Debian 12/13，需 root 权限）' \
    '  bash install.sh                       中文交互菜单' \
    '  bash install.sh install --domain panel.example.com --email 12345678@qq.com' \
    '  bash install.sh install --ip 203.0.113.10 --email 12345678@qq.com --allow-insecure-http' \
    '  bash install.sh upgrade [--version v1.0.0]' \
    '  bash install.sh upgrade --version vX.Y.Z --recover-incomplete  （仅恢复 v0.1.1 的失败首次安装）' \
    '  bash install.sh repair|status|logs|uninstall|purge' \
    '  bash install.sh admin-password        本机交互修改已有管理员密码（不停止服务）' \
    '  bash install.sh disaster-backup|disaster-config|disaster-disable' \
    '  bash install.sh disaster-restore --archive /root/msboost-backup/整站备份.tar.gz [--version vX.Y.Z]' \
    '默认拉取预构建镜像；只有显式 --build 才在服务器编译源码。' \
    'uninstall 卸载但保留数据；purge 为独立彻底清理，需要两次交互确认。'
}

bootstrap_menu() {
  printf '\n%s\n' 'MSBOOST 网站部署管理' '  1) 安装网站' '  2) 升级（先备份）' '  3) 修复（保留配置和密钥）' '  4) 查看状态' '  5) 查看日志' '  6) 卸载（保留全部数据）' '  7) 彻底清理（不可恢复）' '  8) 一键整站灾难备份' '  9) 设置整站备份目录 / 远程密码 / 每日计划' '  10) 一键灾难恢复（仅全新目标）' '  11) 停用整站自动备份计划' '  12) 修改已有管理员密码（仅本机 root）' '  0) 退出' >&2
  local choice
  read -r -p '请选择: ' choice </dev/tty
  case "$choice" in
    1) printf install ;; 2) printf upgrade ;; 3) printf repair ;; 4) printf status ;;
    5) printf logs ;; 6) printf uninstall ;; 7) printf purge ;; 0) printf exit ;;
    8) printf disaster-backup ;; 9) printf disaster-config ;; 10) printf disaster-restore ;; 11) printf disaster-disable ;;
    12) printf admin-password ;;
    *) printf '%s\n' '无效选择' >&2; return 1 ;;
  esac
}

bootstrap_main() {
  local action=install version= work='' tag_json archive expected actual entry
  local -a forward=()
  if [[ $# -eq 0 ]]; then
    [[ -r /dev/tty ]] || { bootstrap_help; return 2; }
    action=$(bootstrap_menu)
  elif [[ $1 == --help || $1 == -h || $1 == help ]]; then bootstrap_help; return 0
  elif [[ $1 != --* ]]; then action=$1; shift
  fi
  [[ $action != exit ]] || return 0
  case "$action" in install|upgrade|repair|status|logs|uninstall|purge|disaster-backup|disaster-config|disaster-disable|disaster-restore|admin-password) ;; *) bootstrap_help; return 2 ;; esac
  # Reject all arguments before parsing or echoing them: a password accidentally
  # supplied as an option must never be reflected into deployment diagnostics.
  if [[ $action == admin-password && $# != 0 ]]; then printf '%s\n' 'admin-password 不接受参数；邮箱和密码只能从本机终端输入。' >&2; return 2; fi
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version) [[ $# -ge 2 ]] || { printf '%s\n' '缺少 --version 参数' >&2; return 2; }; version=$2; shift 2 ;;
      --source-dir) printf '%s\n' '--source-dir 仅供已校验的发布入口使用' >&2; return 2 ;;
      *) forward+=("$1"); shift ;;
    esac
  done
  [[ $(id -u) == 0 && $(uname -s) == Linux ]] || { printf '%s\n' '请在目标 Debian 12/13 服务器以 root 运行；不会部署到你的本地浏览器。' >&2; return 1; }
  if [[ $action != install && $action != upgrade && $action != disaster-restore ]]; then
    [[ -f /opt/msboost/.managed-by-msboost && ! -L /opt/msboost && -f /opt/msboost/deploy/manage.sh ]] || { printf '%s\n' '未发现此安装器管理的 /opt/msboost' >&2; return 1; }
    exec bash /opt/msboost/deploy/manage.sh "$action" "${forward[@]}"
  fi
  for entry in curl tar sha256sum sed mktemp; do command -v "$entry" >/dev/null || { printf '缺少必要依赖：%s\n' "$entry" >&2; return 1; }; done
  if [[ -z $version ]]; then
    if [[ $action == install || $action == disaster-restore ]]; then version=$INITIAL_VERSION
    else
      tag_json=$(curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 60 --retry 2 "https://api.github.com/repos/$REPOSITORY/releases/latest") || return
      # GitHub may return a single compact JSON line; the key is not necessarily
      # at the start of a line. The extracted value still passes strict tag validation.
      version=$(printf '%s\n' "$tag_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
    fi
  fi
  [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || { printf '%s\n' '版本必须是已发布的 vX.Y.Z（不使用 latest 镜像）' >&2; return 1; }
  archive="msboost-deploy-$version.tar.gz"
  work=$(mktemp -d /tmp/msboost-release.XXXXXXXX)
  BOOTSTRAP_WORK=$work
  # Only the exact directory allocated by mktemp is eligible for cleanup.
  trap 'if [[ -n ${BOOTSTRAP_WORK:-} && $BOOTSTRAP_WORK == /tmp/msboost-release.* && ! -L $BOOTSTRAP_WORK ]]; then rm -rf -- "$BOOTSTRAP_WORK"; fi' EXIT
  printf '\n[1/3] 下载正式发布 %s（优先预构建镜像）\n' "$version"
  for entry in "$archive" SHA256SUMS; do
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 600 --max-filesize 67108864 --retry 2 \
      "https://github.com/$REPOSITORY/releases/download/$version/$entry" -o "$work/$entry" || return
  done
  expected=$(awk -v asset="$archive" '$2 == asset || $2 == "*" asset {print $1}' "$work/SHA256SUMS")
  printf '\n[2/3] 校验部署包 SHA-256 与安全解包路径\n'
  [[ $expected =~ ^[a-fA-F0-9]{64}$ ]] || { printf '%s\n' 'Release 校验清单缺失、重复或无效' >&2; return 1; }
  actual=$(sha256sum "$work/$archive"); actual=${actual%% *}
  [[ ${actual,,} == ${expected,,} ]] || { printf '%s\n' '部署包 SHA256 不一致；停止执行' >&2; return 1; }
  # Checksums share GitHub's HTTPS/release trust boundary; they are not signatures.
  tar -tzf "$work/$archive" > "$work/members" || return
  while IFS= read -r entry; do
    [[ $entry != /* && ! $entry =~ (^|/)\.\.(/|$) ]] || { printf '%s\n' '部署包包含危险路径' >&2; return 1; }
  done < "$work/members"
  if tar -tvzf "$work/$archive" | awk 'substr($0,1,1)!="-" && substr($0,1,1)!="d" {bad=1} END {exit !bad}'; then
    printf '%s\n' '部署包不允许符号链接、硬链接或设备文件' >&2; return 1
  fi
  mkdir "$work/bundle" || return
  tar -xzf "$work/$archive" -C "$work/bundle" --no-same-owner --no-same-permissions || return
  [[ -f $work/bundle/deploy/manage.sh && -f $work/bundle/install.sh ]] || { printf '%s\n' '部署包结构无效' >&2; return 1; }
  printf '\n[3/3] 执行部署管理（详细进度如下）\n'
  bash "$work/bundle/deploy/manage.sh" "$action" --source-dir "$work/bundle" --version "$version" "${forward[@]}"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then bootstrap_main "$@"; fi
