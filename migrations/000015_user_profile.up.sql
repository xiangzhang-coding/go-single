-- T02 个人资料：昵称/头像端到端缺席，补齐列。均可空——存量用户无昵称时
-- 前端回退展示 username；头像经 POST /api/files 上传（MinIO 私有桶）后由
-- PATCH /api/users/me 写入 URL 字符串（与消息/动态引用图片同一模式）。
ALTER TABLE users
    ADD COLUMN nickname VARCHAR(64) NULL COMMENT '昵称（可空，展示用；服务层校验 ≤32 字符）' AFTER username,
    ADD COLUMN avatar_url VARCHAR(255) NULL COMMENT '头像 URL（可空，POST /api/files 上传返回的引用地址）' AFTER nickname;
