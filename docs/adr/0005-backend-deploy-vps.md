# 后端云部署选型：VPS + Docker Compose 为主，PaaS 仅演练

后端依赖有状态的 MySQL/Redis/RabbitMQ/MinIO（deploy/docker-compose.yml 现成编排，含 Nginx SSL 终止），
选型以"低成本、全可控、贴近生产、顺带学运维"为先：**VPS（轻量云服务器，如 Hetzner/腾讯云轻量 2C4G 档）+ Docker Compose + Nginx 反代**为主选——
compose 即部署单，本地与云端同构、迁移复现成本低；systemd 守护、ufw 防火墙、Nginx + Let's Encrypt、备份与监控（Prometheus/Loki 已在 compose）
恰是就业向学习目标本身。Fly.io / Railway / Render 等 PaaS 对运行期有状态服务与多容器网络（RabbitMQ/MinIO 端口暴露、卷挂载）限制或收费偏贵，且抽象掉运维概念，仅作选型演练备选；Kubernetes 对单一部署单元的模块化单体属过度设计。选型对比与云端部署步骤见 docs/DEPLOYMENT.md。
