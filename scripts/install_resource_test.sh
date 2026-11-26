#!/usr/bin/env bash
# install.sh / apply-systemd-limits.sh 资源护栏参数的纯 shell 回归。
#
# 背景：CI 只做 `bash -n` 语法检查，资源参数的归一（16g→16G、6→600%、
# off→不写 TasksMax、非法值必须失败）与默认 65%/80%/80% 计算都靠人工回归。
# 本脚本从两个文件里抽取 RESOURCE_HELPERS 标记之间的函数，做输入矩阵断言。
#
# 只依赖 bash + coreutils；不联网、不写系统路径、不改宿主限制。
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
fail=0
ok_case(){ printf 'ok   %s\n' "$1"; }
fail_case(){ printf 'FAIL %s\n  期望: %s\n  实际: %s\n' "$1" "$2" "$3" >&2; fail=1; }
check(){ # desc expected actual
  if [ "$2" = "$3" ]; then ok_case "$1"; else fail_case "$1" "$2" "$3"; fi
}

# clear_resource_env 清掉可能从调用者环境继承的值，保证用例间互不影响。
clear_resource_env(){
  unset POJUN_RESOURCE_LIMITS POJUN_MEM_HIGH POJUN_MEM_MAX POJUN_CPU_QUOTA POJUN_TASKS_MAX
}

# run_build MEM CPU [K=V ...]
run_build(){
  local mem="$1" cpu="$2"; shift 2
  (
    clear_resource_env
    for kv in "$@"; do export "$kv"; done
    build_resource_limits "$mem" "$cpu"
  )
}

# run_fail MEM CPU [K=V ...] → 返回 build 的退出码，stderr 由调用方收集。
run_fail(){
  local mem="$1" cpu="$2"; shift 2
  (
    clear_resource_env
    for kv in "$@"; do export "$kv"; done
    build_resource_limits "$mem" "$cpu" >/dev/null
  )
}

run_matrix(){
  local src="$1" tmp="$2"
  echo "== $src =="
  sed -n '/^# >>> RESOURCE_HELPERS/,/^# <<< RESOURCE_HELPERS/p' "$src" >"$tmp"
  if [ ! -s "$tmp" ]; then
    fail_case "抽取 RESOURCE_HELPERS" "非空" "空"
    return
  fi
  # shellcheck disable=SC1090
  . "$tmp"

  local expected
  expected='MemoryAccounting=yes
CPUAccounting=yes
IOAccounting=yes
IOWeight=100
TasksMax=4096
MemoryHigh=5324M
MemoryMax=6553M
CPUQuota=320%'
  check "默认 8G/4 核：65%/80%/80%" "$expected" "$(run_build 8192 4)"

  expected='MemoryAccounting=yes
CPUAccounting=yes
IOAccounting=yes
IOWeight=100
TasksMax=4096
MemoryHigh=2662M
MemoryMax=3276M
CPUQuota=160%'
  check "4G/2 核：默认比例" "$expected" "$(run_build 4096 2)"

  expected='MemoryAccounting=yes
CPUAccounting=yes
IOAccounting=yes
IOWeight=100
TasksMax=4096
MemoryHigh=16G
MemoryMax=24G
CPUQuota=320%'
  check "大小写与 B 后缀归一（16g/24GB）" "$expected" \
    "$(run_build 8192 4 POJUN_MEM_HIGH=16g POJUN_MEM_MAX=24GB)"

  expected='MemoryAccounting=yes
CPUAccounting=yes
IOAccounting=yes
IOWeight=100
TasksMax=4096
MemoryHigh=5324M
MemoryMax=6553M
CPUQuota=600%'
  check "CPU 核数写法 6 → 600%" "$expected" "$(run_build 8192 4 POJUN_CPU_QUOTA=6)"

  check "CPU 百分比写法原样" "CPUQuota=600%" "$(
    clear_resource_env; export POJUN_CPU_QUOTA=600%; build_resource_limits 8192 4 | tail -n1
  )"
  check "裸 600 视为百分比" "CPUQuota=600%" "$(
    clear_resource_env; export POJUN_CPU_QUOTA=600; build_resource_limits 8192 4 | tail -n1
  )"

  expected='MemoryAccounting=yes
CPUAccounting=yes
IOAccounting=yes
IOWeight=100
MemoryHigh=5324M
MemoryMax=6553M
CPUQuota=320%'
  check "TasksMax=off 不写该指令" "$expected" "$(run_build 8192 4 POJUN_TASKS_MAX=off)"

  check "整体关闭输出为空" "" "$(run_build 8192 4 POJUN_RESOURCE_LIMITS=0)"
  check "整体关闭（off 小写）输出为空" "" "$(run_build 8192 4 POJUN_RESOURCE_LIMITS=off)"

  expected='MemoryAccounting=yes
CPUAccounting=yes
IOAccounting=yes
IOWeight=100
TasksMax=4096'
  check "内存/CPU 探测不到时只写 accounting" "$expected" "$(run_build "" "")"

  if run_fail 8192 4 POJUN_MEM_HIGH=12X 2>/tmp/pojun-res-test-err.$$; then
    fail_case "非法内存值必须失败" "rc!=0" "rc=0"
  else
    if grep -q 'POJUN_MEM_HIGH' /tmp/pojun-res-test-err.$$; then
      ok_case "非法内存值失败且提示变量名"
    else
      fail_case "非法内存值提示" "含 POJUN_MEM_HIGH" "$(cat /tmp/pojun-res-test-err.$$)"
    fi
  fi
  rm -f /tmp/pojun-res-test-err.$$

  if run_fail 8192 4 POJUN_CPU_QUOTA=abc >/dev/null 2>&1; then
    fail_case "非法 CPU 值必须失败" "rc!=0" "rc=0"
  else
    ok_case "非法 CPU 值必须失败"
  fi
  if run_fail 8192 4 POJUN_TASKS_MAX=12.5 >/dev/null 2>&1; then
    fail_case "非法 TasksMax 必须失败" "rc!=0" "rc=0"
  else
    ok_case "非法 TasksMax 必须失败"
  fi

  check "normalize_mem_limit: 16384M" "16384M" "$(normalize_mem_limit 16384M)"
  check "normalize_mem_limit: 空" "" "$(normalize_mem_limit '')"
  if normalize_mem_limit 1.5G >/dev/null 2>&1; then
    fail_case "normalize_mem_limit 拒绝小数" "rc!=0" "rc=0"
  else
    ok_case "normalize_mem_limit 拒绝小数"
  fi
  check "normalize_cpu_quota: 25%" "25%" "$(normalize_cpu_quota 25%)"
  if normalize_cpu_quota 0 >/dev/null 2>&1; then
    fail_case "normalize_cpu_quota 拒绝 0" "rc!=0" "rc=0"
  else
    ok_case "normalize_cpu_quota 拒绝 0"
  fi
  check "normalize_tasks_max: Infinity" "" "$(normalize_tasks_max Infinity)"
  check "normalize_tasks_max: 1024" "1024" "$(normalize_tasks_max 1024)"
}

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

run_matrix "$ROOT/install.sh" "$TMP"
run_matrix "$ROOT/scripts/apply-systemd-limits.sh" "$TMP"

if [ "$fail" -ne 0 ]; then
  echo "资源护栏参数回归失败" >&2
  exit 1
fi
echo "资源护栏参数回归全部通过"
