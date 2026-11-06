-- 053_inference_route_pool_strategy_comments.sql
--
-- T7（配置行为收口）：把 inference_model_routes 的列注释改到与实现一致。
--
-- 只改 COMMENT —— 不改列类型、不改 CHECK、不动任何历史行（历史行的语义
-- 一律保持；本文件只让 schema 说出真相）。
--
--   - weight：只做「同 priority 组内的候选排序」（降序），**不是**概率
--     分流。本期不做比例分流：每个候选都留在候选列表里，只是顺序不同。
--     024 里的旧注释「同优先级按 weight 加权」会被读成比例分流，与实现
--     不符 —— 留着它就是在文档层面撒谎。
--   - priority：候选分组序号（数字小者优先 = failover 顺序）。
--   - account_pool_strategy：支持集只有 round_robin。least_loaded 与通用
--     session_sticky 是**历史值**：旧快照仍可读（否则整个目录变砖），
--     但写面明确拒绝、路由被标记不可调度 —— 绝不静默当成 round_robin。
--     DB 的 CHECK 保持原样（历史行必须仍能存在）。
--
-- Responses 的 previous_response_id 专用绑定走会话绑定器
-- （routing.SessionBinder），与本列无关 —— 拒绝通用 session_sticky 不影
-- 响那条链路。
COMMENT ON COLUMN inference_model_routes.priority IS
  '候选分组序号：数字小者优先（failover 顺序）';
COMMENT ON COLUMN inference_model_routes.weight IS
  '候选排序权重：同 priority 组内按 weight 降序排序（不是概率分流 —— 每个候选都留在列表里，只是顺序不同）';
COMMENT ON COLUMN inference_model_routes.account_pool_strategy IS
  '账号池策略：支持集只有 round_robin。least_loaded / session_sticky 是历史值（可读但不可调度，写面明确拒绝）';
