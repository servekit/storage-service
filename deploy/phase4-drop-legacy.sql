-- phase4-drop-legacy.sql：④期 T8 —— 窗口关闭（数据侧）：storage-service
-- 在 storage-service 的 PG 上执行一次（幂等：重复执行无害）。
--
-- 前置（控制器输入，全部满足才执行本脚本）：
--   1. 代码对账：T7/T8 后代码已无 files/sessions app_key 与 app 表
--      app_secret 的读写（grep 各列名，仅历史注释提及）；
--   2. pg_dump 留档：相关表已备份到 deploy/backups/；
--   3. 行数记录：删前每表 SELECT count(*) 基准已记录。
--
-- 删除清单（spec §9.1.3：凭据列废弃不迁；④ 窗口关闭删旧指针与旧凭据）：
--   storage_files            app_key 列（tenant_key 已承载归属/审计维度）
--   storage_upload_sessions  app_key 列（KeyPrefix+TenantKey 已承载会话快照）
--   storage_apps             app_secret 列（表保留 —— 租户配置行/key_prefix 载体）
--
-- 对账口径：files/sessions 的"可归属行"（app_key 非空的行）必须全部携带
-- tenant_key（③ 回填完备）；不等即 RAISE EXCEPTION 中止整个事务。
-- 空 app_key 行（pre-app 时代）本就无归属，不在对账范围。
--
-- 备份（执行前手工跑一次，产物不进 git）：
--   pg_dump "postgres://…/testkit" -t storage_files -t storage_upload_sessions \
--     -t storage_apps -Fc -f deploy/backups/phase4-storage-pre-drop.dump
--
-- 执行：psql "postgres://…/testkit" -v ON_ERROR_STOP=1 -f phase4-drop-legacy.sql
-- dry-run：将末尾 COMMIT 改为 ROLLBACK。
--
-- 警告：compose 栈若配置表前缀，裸表名解析不到目标 —— 执行前先确认
--       search_path 下的 storage_* 就是目标表（dev testkit 栈为无前缀
--       裸表名）。make migrate 的镜像步骤（postMigrateDropLegacy）做同一
--       件事，先跑哪个都可以。

BEGIN;

-- ── 1. 对账：可归属行（app_key 非空）必须已携带 tenant_key────────
DO $$
DECLARE
  tbl text;
  total bigint;
  filled bigint;
BEGIN
  FOREACH tbl IN ARRAY ARRAY['storage_files', 'storage_upload_sessions'] LOOP
    EXECUTE format('SELECT count(*) FILTER (WHERE app_key <> %L), count(tenant_key) FILTER (WHERE app_key <> %L) FROM %I', '', '', tbl) INTO total, filled;
    RAISE NOTICE 'phase4 drop-legacy reconcile %: attributable=% tenant_key_filled=%', tbl, total, filled;
    IF filled <> total THEN
      RAISE EXCEPTION 'phase4 drop-legacy: % has % of % attributable rows without tenant_key; refusing to drop app_key', tbl, total - filled, total;
    END IF;
  END LOOP;
END $$;

-- ── 2. 删列（幂等）─────────────────────────────────────────────
ALTER TABLE storage_files            DROP COLUMN IF EXISTS app_key;
ALTER TABLE storage_upload_sessions  DROP COLUMN IF EXISTS app_key;
-- storage_apps 保留（租户配置行/key_prefix/桶绑定载体）；仅删凭据列
-- （spec §9.1.3）。
ALTER TABLE storage_apps             DROP COLUMN IF EXISTS app_secret;

COMMIT;
