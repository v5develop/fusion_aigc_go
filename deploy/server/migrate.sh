# Go 服务的数据库迁移。记录写在 go_schema_history 表里，与 Java 的 flyway_schema_history 分开。
#
# 由 install.sh source，依赖它先定义的：q / db_exec / run_sql_file / has_table / has_column /
# backup_db / die / green / yellow，以及 ${PKG}（安装包目录）。
#
# 每个 Go_V<版本>__<描述>.sql 的规则：
#   · 已记录、校验和一致     → 跳过；
#   · 已记录、但文件被改过   → 中止。已发布的迁移不能改，要改就新增版本号；
#   · 未记录，但库里已有它建的结构（老库手工执行过）→ 登记为 baseline，不再执行；
#   · 其余                   → 执行，成功后登记。
# 版本号取自文件名：Go_V1.0.0__ai_copywriting.sql → 1.0.0。
# 注意：脚本执行成功、登记失败时，下次会再执行一次；登记失败会直接中止，需人工核对后再重跑。

# 迁移记录表。与 Java 的 flyway_schema_history 分开：两边各记各的版本。IF NOT EXISTS，每次安装都执行。
GO_SCHEMA_HISTORY_DDL='CREATE TABLE IF NOT EXISTS `go_schema_history` (
  `installed_rank` INT          NOT NULL AUTO_INCREMENT COMMENT '"'"'执行顺序'"'"',
  `version`        VARCHAR(50)  NOT NULL COMMENT '"'"'版本号，取自文件名 Go_V<版本>__*.sql'"'"',
  `description`    VARCHAR(200) NOT NULL COMMENT '"'"'描述，取自文件名'"'"',
  `script`         VARCHAR(200) NOT NULL COMMENT '"'"'脚本文件名'"'"',
  `checksum`       CHAR(64)     NOT NULL COMMENT '"'"'脚本内容的 sha256：已执行的脚本被改动时拒绝继续'"'"',
  `installed_by`   VARCHAR(100) NOT NULL COMMENT '"'"'谁执行的'"'"',
  `installed_on`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `execution_ms`   INT          NOT NULL DEFAULT 0 COMMENT '"'"'执行耗时（毫秒）；0 表示是 baseline 登记，没有执行'"'"',
  `success`        TINYINT(1)   NOT NULL DEFAULT 1,
  PRIMARY KEY (`installed_rank`),
  UNIQUE KEY `uk_go_schema_history_version` (`version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='"'"'Go 服务迁移记录（与 Java 的 flyway_schema_history 分开）'"'"''

file_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# 库里是否已经有这个迁移建的结构。只用来给老库登记 baseline；未列出的新脚本返回 1，即正常执行。
guard_done() {
  case $1 in
    Go_V1.0.0__*) has_table afv_copywriting ;;
    Go_V1.0.1__*) has_table afv_digital_human ;;
    Go_V1.0.2__*) has_table afv_image_text_note ;;
    Go_V1.0.3__*) return 1 ;; # 只插配置数据，脚本本身可重复执行（NOT EXISTS）
    Go_V1.0.4__*) has_table afv_image_creation ;;
    Go_V1.0.5__*) has_column afv_image_creation input_image_url ;;
    Go_V1.0.6__*) has_column afv_image_creation input_bg_url ;;
    Go_V1.0.7__*) has_table afv_shop_profile ;;
    Go_V1.0.8__*) has_column afv_image_text_note shop_profile_id ;;
    Go_V1.0.9__*) has_table afv_video_creation ;;
    Go_V1.0.10__*) has_column afv_video_creation source_video_url ;;
    Go_V1.0.11__*) has_column afv_image_creation copywriting_id ;;
    Go_V1.0.12__*) has_column afv_digital_human model ;;
    Go_V1.0.13__*) [[ $(q "SELECT COUNT(*) FROM afv_billing_offer WHERE entry_code='aiCopywriting' AND deleted=0") != 0 ]] ;;
    *) return 1 ;;
  esac
}

record_migration() {
  local version=$1 name=$2 sum=$3 ms=$4 desc
  desc=${name#*__}; desc=${desc%.sql}
  db_exec "INSERT INTO go_schema_history (version, description, script, checksum, installed_by, execution_ms, success) VALUES ('$version', '$desc', '$name', '$sum', 'install.sh', $ms, 1)" \
    || die "登记迁移 $name 失败（脚本可能已执行，请人工核对后重跑）"
}

migrate_go() {
  backup_db
  db_exec "$GO_SCHEMA_HISTORY_DDL" || die "创建 go_schema_history 失败"
  local f name version sum recorded t0 t1
  for f in "$PKG"/sql/Go_V*.sql; do
    name=$(basename "$f")
    version=$(printf '%s' "$name" | sed -E 's/^Go_V([0-9.]+)__.*/\1/')
    [[ $version != "$name" && -n $version ]] || die "迁移文件名不合规（应为 Go_V<版本>__<描述>.sql）：$name"
    sum=$(file_sha256 "$f")
    recorded=$(q "SELECT checksum FROM go_schema_history WHERE version='$version' AND success=1 LIMIT 1")
    if [[ -n $recorded ]]; then
      [[ $recorded == "$sum" ]] || die "$name 已执行过，但文件内容被改了（记录 ${recorded}，现在 ${sum}）。已发布的迁移不能改，请新增版本号"
      echo "  - $name 已记录，跳过"
      continue
    fi
    if guard_done "$name"; then
      record_migration "$version" "$name" "$sum" 0
      yellow "  ~ $name 库里已有对应结构，登记为 baseline（未重复执行）"
      continue
    fi
    t0=$(date +%s)
    run_sql_file "$f" || die "执行 $name 失败（未登记；修好后重跑 install.sh 会从这里继续）"
    t1=$(date +%s)
    record_migration "$version" "$name" "$sum" $(( (t1 - t0) * 1000 ))
    green "  ✓ $name"
  done
}
