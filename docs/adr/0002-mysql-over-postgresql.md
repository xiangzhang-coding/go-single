# 使用 MySQL 8 而非 PostgreSQL

就业向定位下国内互联网公司以 MySQL 为绝对主流，秒杀/库存的锁与事务语义正是面试考点，因此选 MySQL 8。PostgreSQL 能力上更强但中文资料与团队栈对齐度低，列入 backlog。存储层经仓储接口抽象（ADR-0003），换库成本被限制在 driver 与 SQL 方言内。
