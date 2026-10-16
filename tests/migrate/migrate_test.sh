#!/usr/bin/env bash
# 迁移记录逻辑的离线测试：用内存里的假库替换 mysql 调用，验证四种情形：
#   1) 全新库：全部执行并登记；2) 老库（手工执行过）：baseline 登记，不重复执行；
#   3) 重复安装：全部跳过；4) 已登记的脚本被改动：中止。
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0
ok()   { echo "  ✓ $*"; }
bad()  { echo "  ✗ $*"; fail=1; }

# ---- 夹具：两个迁移脚本，一个老库里已有结构的，一个新的
mkpkg() {
  PKG=$T/pkg; rm -rf "$PKG"; mkdir -p "$PKG/sql"
  printf 'CREATE TABLE IF NOT EXISTS afv_copywriting (x INT);\n' > "$PKG/sql/Go_V1.0.0__ai_copywriting.sql"
  printf 'CREATE TABLE IF NOT EXISTS demo_new (x INT);\n'        > "$PKG/sql/Go_V1.0.1__demo_new.sql"
}

# ---- 假库：记录在 $DB/records/<version>，已建的表在 $DB/tables/<name>，执行过的脚本记在 $DB/executed
reset_db() { DB=$T/db; rm -rf "$DB"; mkdir -p "$DB/records" "$DB/tables"; : > "$DB/executed"; }
q() {
  case $1 in
    "SELECT checksum FROM go_schema_history WHERE version='"*)
      local v=${1#*version=\'}; v=${v%%\'*}
      [[ -f $DB/records/$v ]] && cat "$DB/records/$v"
      return 0 ;;
    *afv_billing_offer*) echo 0 ;;
    *) echo "unexpected query: $1" >&2; exit 9 ;;
  esac
}
has_table()  { [[ -f $DB/tables/$1 ]]; }
has_column() { return 1; }
backup_db()  { :; }
die()        { echo "DIE: $*" >&2; exit 1; }
green()      { :; }
yellow()     { :; }
run_sql_file() {
  local n; n=$(basename "$1"); echo "$n" >> "$DB/executed"
  if [[ $n == Go_V1.0.0__* ]]; then : > "$DB/tables/afv_copywriting"; fi
  if [[ $n == Go_V1.0.1__* ]]; then : > "$DB/tables/demo_new"; fi
  return 0
}
db_exec() {
  if [[ $1 == CREATE\ TABLE\ IF\ NOT\ EXISTS\ \`go_schema_history\`* ]]; then return 0; fi
  if [[ $1 =~ VALUES\ \(\'([0-9.]+)\',\ \'[^\']*\',\ \'[^\']*\',\ \'([0-9a-f]+)\' ]]; then
    echo "${BASH_REMATCH[2]}" > "$DB/records/${BASH_REMATCH[1]}"
  else
    echo "bad insert: $1" >&2; exit 9
  fi
}
# 跑一次迁移，子 shell 里执行，返回退出码
run_migrate() { ( . "$ROOT/deploy/server/migrate.sh"; migrate_go ) >"$T/out" 2>&1; }

echo "1) 全新库：两个脚本都执行并登记"
mkpkg; reset_db
run_migrate; rc=$?
[[ $rc == 0 ]] && ok "退出码 0" || bad "退出码 $rc: $(cat "$T/out")"
grep -q "^Go_V1.0.0__ai_copywriting.sql$" "$DB/executed" && grep -q "^Go_V1.0.1__demo_new.sql$" "$DB/executed" && ok "两个脚本都执行了" || bad "执行清单: $(cat "$DB/executed")"
[[ -f $DB/records/1.0.0 && -f $DB/records/1.0.1 ]] && ok "两个版本都已登记（1.0.0 / 1.0.1）" || bad "登记缺失"
[[ -f $DB/records/1.0.0 ]] && [[ $(cat "$DB/records/1.0.0") == $(shasum -a 256 "$PKG/sql/Go_V1.0.0__ai_copywriting.sql" | cut -d' ' -f1) ]] && ok "登记的校验和等于文件 sha256" || bad "校验和不对"

echo "2) 老库：手工执行过 1.0.0 的结构，只登记 baseline，不再执行"
mkpkg; reset_db; : > "$DB/tables/afv_copywriting"
run_migrate; rc=$?
[[ $rc == 0 ]] && ok "退出码 0" || bad "退出码 $rc"
grep -q "Go_V1.0.0__ai_copywriting.sql" "$DB/executed" && bad "老库的 1.0.0 被重复执行了" || ok "1.0.0 未重复执行"
grep -q "Go_V1.0.1__demo_new.sql" "$DB/executed" && ok "新的 1.0.1 正常执行" || bad "1.0.1 没执行"
[[ -f $DB/records/1.0.0 ]] && ok "1.0.0 已登记为 baseline" || bad "baseline 未登记"

echo "3) 重复安装：全部跳过（只看 Go_V 迁移脚本）"
: > "$DB/executed"
run_migrate; rc=$?
[[ $rc == 0 ]] && ok "退出码 0" || bad "退出码 $rc"
[[ -z $(grep "^Go_V" "$DB/executed") ]] && ok "没有任何 Go_V 脚本再次执行" || bad "又执行了: $(cat "$DB/executed")"

echo "4) 已登记的脚本被改动：中止"
printf -- '-- 偷偷改了\n' >> "$PKG/sql/Go_V1.0.1__demo_new.sql"
run_migrate; rc=$?
[[ ${rc} != 0 ]] && grep -q "文件内容被改了" "$T/out" && ok "中止并说明原因" || bad "没有中止（rc=${rc}）: $(cat "$T/out")"

if (( fail )); then echo "FAILED"; exit 1; fi
echo "ALL PASSED"
