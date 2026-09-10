-- phase3-storage-tenant-key.sql：③期 T6 —— storage-service tenant_key 列化
-- 在 storage-service 的 PG 上执行一次（幂等：重复执行无害）。
--
-- 步骤（D-③4 顺序）：加列 → 按映射回填 → 建新索引（名字与 GORM 模型 tag 一致，
-- AutoMigrate 不会重复建）→ 对账（应回填行数达标，不等即中止）。
-- 无旧组合唯一索引需要删除：storage 的去重域是（不可变的）key_prefix 本身，
-- 对象键内嵌前缀即隔离；本脚本唯一的唯一性变更是 storage_apps 上新增
-- uniq_storage_apps_tenant_key（一个 tenant 一行配置行）。
-- 旧列（app_key on storage_files / storage_upload_sessions）保留到 ④ 期窗口
-- 关闭；窗口期代码按 tenant_key 写入新行、读路径已切换。
--
-- 语义：
--   storage_apps            tenant_key = app_key 字面量（③期映射即 app_key；
--                           T10 总装把存量行重映射到 ten_*）。
--                           存量行 key_prefix 不重算（不可变——对象已在旧前缀
--                           下，且前缀即去重域）；仅 trusted 首见的新租户懒建
--                           行派生 "{tenant_key}/"（代码路径，非本脚本）。
--   storage_files           冗余 app_key → 加 tenant_key，按 app_key 映射回填；
--                           app_key='' 的史前行保持 NULL（无从归属）。
--   storage_upload_sessions 同 files（会话为短生命周期行，回填幂等）。
--
-- 对账口径（控制器输入）：执行前记录 SELECT count(*) FROM storage_apps 等基准；
-- apps：count(tenant_key) 必须 = count(*)；files/sessions：app_key<>'' 的行必须
-- 全部回填（app_key='' 行保持 NULL）。不满足即 RAISE EXCEPTION 中止整个事务
-- （新索引未动，可排查后安全重试）。
--
-- 执行：psql "postgres://…/testkit" -v ON_ERROR_STOP=1 -f phase3-storage-tenant-key.sql
-- dry-run：将末尾 COMMIT 改为 ROLLBACK。
--
-- 警告：compose 栈若配置表前缀（STORAGE_SERVICE_DB_TABLE_PREFIX），裸表名解析不到
--       目标 —— 执行前先确认 search_path 下的 storage_* 就是目标表
--       （dev testkit 栈为无前缀裸表名）。

BEGIN;

-- ── 1. 加列（幂等）────────────────────────────────────────────
ALTER TABLE storage_apps             ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);
ALTER TABLE storage_files            ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);
ALTER TABLE storage_upload_sessions  ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);

-- ── 2. 回填（幂等：仅回填 NULL 行）────────────────────────────
-- apps：映射列 = app_key 字面量（③期目录键即 app_key）。
UPDATE storage_apps SET tenant_key = app_key WHERE tenant_key IS NULL;

-- files / upload_sessions：按存储的 app_key 映射回填；app_key=''（史前行，
-- 无从归属）保持 NULL。
UPDATE storage_files f SET tenant_key = a.tenant_key
  FROM storage_apps a
  WHERE f.app_key = a.app_key AND f.app_key <> '' AND f.tenant_key IS NULL;

UPDATE storage_upload_sessions s SET tenant_key = a.tenant_key
  FROM storage_apps a
  WHERE s.app_key = a.app_key AND s.app_key <> '' AND s.tenant_key IS NULL;

-- ── 3. 建新索引（名字与 GORM 模型 tag 一致）────────────────────
-- apps：一个 tenant 一行配置行（Trusted 懒建 + 存量映射共用该唯一性）。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_storage_apps_tenant_key ON storage_apps(tenant_key);

-- ── 4. 对账：不等即中止（新索引未动，可排查后重跑）──────────────
DO $$
DECLARE
  total bigint;
  filled bigint;
BEGIN
  EXECUTE 'SELECT count(*), count(tenant_key) FROM storage_apps' INTO total, filled;
  RAISE NOTICE 'phase3 storage tenant_key reconcile storage_apps: total=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 storage_apps backfill incomplete: % of % rows filled', filled, total;
  END IF;

  EXECUTE 'SELECT count(*) FILTER (WHERE app_key <> ''''), count(tenant_key) FILTER (WHERE app_key <> '''') FROM storage_files' INTO total, filled;
  RAISE NOTICE 'phase3 storage tenant_key reconcile storage_files: attributable=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 storage_files backfill incomplete: % of % attributable rows filled (rows whose app_key matches no storage_apps.app_key keep NULL)', filled, total;
  END IF;

  EXECUTE 'SELECT count(*) FILTER (WHERE app_key <> ''''), count(tenant_key) FILTER (WHERE app_key <> '''') FROM storage_upload_sessions' INTO total, filled;
  RAISE NOTICE 'phase3 storage tenant_key reconcile storage_upload_sessions: attributable=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 storage_upload_sessions backfill incomplete: % of % attributable rows filled (rows whose app_key matches no storage_apps.app_key keep NULL)', filled, total;
  END IF;
END $$;

COMMIT;
