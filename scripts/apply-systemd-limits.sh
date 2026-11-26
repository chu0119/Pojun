#!/usr/bin/env bash
# 给已装好的 pojun systemd 服务补一份资源限额 drop-in。
#
# 新装用户由 install.sh 直接写入单元，无需本脚本；存量部署用本脚本补：
#   sudo ./scripts/apply-systemd-limits.sh                      # 按物理内存 65%/80%、CPU 80% 生成
#   sudo ./scripts/apply-systemd-limits.sh --dry-run            # 只打印，不落盘
#   sudo ./scripts/apply-systemd-limits.sh --mem-high 16G --mem-max 24G --cpu-quota 600% --tasks-max 4096
#   sudo ./scripts/apply-systemd-limits.sh --unit pojun-internal
#   sudo ./scripts/apply-systemd-limits.sh --disable            # 删除本脚本生成的 drop-in
#
# 脚本只写 drop-in 并提示后续命令，不会自动重启服务——重启时机由运维决定。
# 数值格式非法时直接退出，避免把 systemd 会静默忽略的坏值写进去。
set -euo pipefail

UNIT="pojun"
DRY_RUN=0
DISABLE=0
MEM_HIGH_OPT=""
MEM_MAX_OPT=""
CPU_QUOTA_OPT=""
TASKS_MAX_OPT=""

usage() {
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
}

# >>> RESOURCE_HELPERS
# normalize_mem_limit: 归一为 systemd 接受的大小（如 16G）；空=未设置；1=非法。
normalize_mem_limit(){
  local v num suffix
  v=$(printf '%s' "${1:-}" | tr -d '[:space:]' | tr '[:lower:]' '[:upper:]')
  [ -z "$v" ] && { printf ''; return 0; }
  case "$v" in *%*) return 1 ;; esac
  case "$v" in *B) v="${v%B}" ;; esac
  num="${v%%[KMGTP]*}"
  suffix="${v#"$num"}"
  case "$num" in ''|*[!0-9]*) return 1 ;; esac
  [ "$num" -gt 0 ] 2>/dev/null || return 1
  case "$suffix" in ''|K|M|G|T) printf '%s%s' "$num" "$suffix" ;; *) return 1 ;; esac
}

# normalize_cpu_quota: 带 % 原样；不带 % 时 <=256 视为核数，>256 视为百分比。
normalize_cpu_quota(){
  local v n
  v=$(printf '%s' "${1:-}" | tr -d '[:space:]' | tr '[:lower:]' '[:upper:]')
  [ -z "$v" ] && { printf ''; return 0; }
  case "$v" in
    *%)
      n="${v%\%}"
      case "$n" in ''|*[!0-9]*) return 1 ;; esac
      [ "$n" -gt 0 ] 2>/dev/null || return 1
      printf '%s%%' "$n"
      ;;
    *)
      case "$v" in ''|*[!0-9]*) return 1 ;; esac
      [ "$v" -gt 0 ] 2>/dev/null || return 1
      if [ "$v" -le 256 ]; then
        printf '%s%%' "$((v * 100))"
      else
        printf '%s%%' "$v"
      fi
      ;;
  esac
}

# normalize_tasks_max: 正整数；0/off/no/false/infinity/unlimited = 不写该指令。
normalize_tasks_max(){
  local v
  v=$(printf '%s' "${1:-}" | tr -d '[:space:]' | tr '[:lower:]' '[:upper:]')
  [ -z "$v" ] && { printf ''; return 0; }
  case "$v" in
    0|OFF|NO|FALSE|INFINITY|UNLIMITED) printf ''; return 0 ;;
  esac
  case "$v" in ''|*[!0-9]*) return 1 ;; esac
  printf '%s' "$v"
}

# build_resource_limits MEM_TOTAL_MB CPU_CORES
build_resource_limits(){
  local mem_total_mb="${1:-}" cpu_cores="${2:-}"
  local enabled
  enabled=$(printf '%s' "${POJUN_RESOURCE_LIMITS:-1}" | tr '[:lower:]' '[:upper:]')
  case "$enabled" in
    0|OFF|NO|FALSE) printf ''; return 0 ;;
  esac
  local mem_high mem_max cpu_quota tasks_max
  if ! mem_high=$(normalize_mem_limit "${POJUN_MEM_HIGH:-}"); then
    printf 'POJUN_MEM_HIGH/--mem-high 格式非法（示例 16G / 16384M）\n' >&2
    return 1
  fi
  if ! mem_max=$(normalize_mem_limit "${POJUN_MEM_MAX:-}"); then
    printf 'POJUN_MEM_MAX/--mem-max 格式非法（示例 24G / 24576M）\n' >&2
    return 1
  fi
  if ! cpu_quota=$(normalize_cpu_quota "${POJUN_CPU_QUOTA:-}"); then
    printf 'POJUN_CPU_QUOTA/--cpu-quota 格式非法（示例 6 或 600%%）\n' >&2
    return 1
  fi
  if ! tasks_max=$(normalize_tasks_max "${POJUN_TASKS_MAX:-4096}"); then
    printf 'POJUN_TASKS_MAX/--tasks-max 格式非法（正整数，0/off=不限制）\n' >&2
    return 1
  fi

  if [ -z "$mem_high" ] && [ -n "$mem_total_mb" ] && [ "$mem_total_mb" -ge 1024 ] 2>/dev/null; then
    mem_high="$((mem_total_mb * 65 / 100))M"
  fi
  if [ -z "$mem_max" ] && [ -n "$mem_total_mb" ] && [ "$mem_total_mb" -ge 1024 ] 2>/dev/null; then
    mem_max="$((mem_total_mb * 80 / 100))M"
  fi
  if [ -z "$cpu_quota" ] && [ -n "$cpu_cores" ] && [ "$cpu_cores" -ge 1 ] 2>/dev/null; then
    cpu_quota="$((cpu_cores * 80))%"
  fi

  printf 'MemoryAccounting=yes\nCPUAccounting=yes\nIOAccounting=yes\nIOWeight=100\n'
  [ -n "$tasks_max" ] && printf 'TasksMax=%s\n' "$tasks_max"
  [ -n "$mem_high" ] && printf 'MemoryHigh=%s\n' "$mem_high"
  [ -n "$mem_max" ] && printf 'MemoryMax=%s\n' "$mem_max"
  [ -n "$cpu_quota" ] && printf 'CPUQuota=%s\n' "$cpu_quota"
  return 0
}
# <<< RESOURCE_HELPERS

while [ $# -gt 0 ]; do
  case "$1" in
    --unit) UNIT="${2:?--unit 需要参数}"; shift 2 ;;
    --mem-high) MEM_HIGH_OPT="${2:?--mem-high 需要参数}"; shift 2 ;;
    --mem-max) MEM_MAX_OPT="${2:?--mem-max 需要参数}"; shift 2 ;;
    --cpu-quota) CPU_QUOTA_OPT="${2:?--cpu-quota 需要参数}"; shift 2 ;;
    --tasks-max) TASKS_MAX_OPT="${2:?--tasks-max 需要参数}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --disable) DISABLE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$1（--help 查看用法）" >&2; exit 2 ;;
  esac
done

DROPIN_DIR="/etc/systemd/system/${UNIT}.service.d"
DROPIN="${DROPIN_DIR}/limits.conf"

if [ "$DISABLE" = "1" ]; then
  if [ "$DRY_RUN" = "1" ]; then
    echo "# 将删除：${DROPIN}（已有文件会先备份为 .bak.<时间戳>）"
    exit 0
  fi
  if [ "$(id -u)" != "0" ]; then
    echo "需要 root：sudo $0 --disable --unit ${UNIT}" >&2
    exit 1
  fi
  if [ -f "$DROPIN" ]; then
    BACKUP="${DROPIN}.bak.$(date +%Y%m%d%H%M%S)"
    cp -a "$DROPIN" "$BACKUP"
    rm -f "$DROPIN"
    echo "已删除 drop-in：${DROPIN}（备份 ${BACKUP}）"
  else
    echo "drop-in 不存在，无需删除：${DROPIN}"
  fi
  echo
  echo "下一步（不会自动执行）："
  echo "  systemctl daemon-reload"
  echo "  systemctl restart ${UNIT}.service"
  exit 0
fi

MEM_TOTAL_MB=$(awk '/^MemTotal:/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || true)
CPU_CORES=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || true)

POJUN_MEM_HIGH="${MEM_HIGH_OPT:-${POJUN_MEM_HIGH:-}}"
POJUN_MEM_MAX="${MEM_MAX_OPT:-${POJUN_MEM_MAX:-}}"
POJUN_CPU_QUOTA="${CPU_QUOTA_OPT:-${POJUN_CPU_QUOTA:-}}"
POJUN_TASKS_MAX="${TASKS_MAX_OPT:-${POJUN_TASKS_MAX:-4096}}"

if ! LIMITS=$(build_resource_limits "$MEM_TOTAL_MB" "$CPU_CORES"); then
  echo "资源限额参数非法，未写入任何文件。用 --help 查看示例。" >&2
  exit 1
fi

if [ -z "$LIMITS" ]; then
  echo "POJUN_RESOURCE_LIMITS=0：本次不写资源限制 drop-in。"
  echo "如需移除已有 drop-in：sudo $0 --disable --unit ${UNIT}"
  exit 0
fi

CONTENT="# 由 scripts/apply-systemd-limits.sh 生成。
# 程序自身按容量自动调优（设置页开关）；这里是系统级兜底，MemoryMax 到顶时
# OOM 只杀本服务，而不是把整机拖死。
[Service]
${LIMITS}"
CONTENT="${CONTENT%$'\n'}"

if [ "$DRY_RUN" = "1" ]; then
  echo "# 目标：${DROPIN}"
  printf '%s\n' "$CONTENT"
  exit 0
fi

if [ "$(id -u)" != "0" ]; then
  echo "需要 root：sudo $0 --unit ${UNIT}" >&2
  exit 1
fi

mkdir -p "$DROPIN_DIR"
if [ -f "$DROPIN" ]; then
  BACKUP="${DROPIN}.bak.$(date +%Y%m%d%H%M%S)"
  cp -a "$DROPIN" "$BACKUP"
  echo "已备份原 drop-in：${BACKUP}"
fi
printf '%s\n' "$CONTENT" > "$DROPIN"
chmod 644 "$DROPIN"

echo "已写入：${DROPIN}"
echo
echo "下一步（不会自动执行）："
echo "  systemctl daemon-reload"
echo "  systemctl restart ${UNIT}.service"
echo
echo "生效后核对："
echo "  systemctl show ${UNIT}.service -p MemoryHigh -p MemoryMax -p CPUQuotaPerSecUSec"
echo
echo "提示：程序启动日志里的 [resource] 行会打印它探测到的容量与计划，可与这里互相印证。"
