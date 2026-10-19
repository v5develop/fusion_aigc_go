-- 执行面提交快照表 afv_exec_submission 的 exec_task_id 在 Go_V1.0.19 建表时没写 COLLATE，继承了服务器默认的 utf8mb4_0900_ai_ci。
-- 任务中心同步里 x.exec_task_id = COALESCE(gen_task_id, ...)（业务表是 utf8mb4_unicode_ci）两边排序规则不同，
-- MySQL 直接报 1267 Illegal mix of collations，整条同步语句失败。统一为库内约定的 utf8mb4_unicode_ci。
-- 只改排序规则，不改结构；Go_V1.0.19 已应用，不改它。
ALTER TABLE `afv_exec_submission` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
