// Q5 数据持久化与备份：容器无状态、数据进卷。
// 运行：go run ./interview/ch10_deploy/q05_persistence
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	// 容器重建即失数据 → 数据目录必须挂卷（mysql_data/redis_data 命名卷）。
	fmt.Println("compose 挂载约定：")
	fmt.Println("  mysql:    mysql_data:/var/lib/mysql（无状态进程 + 有状态数据卷）")
	fmt.Println("  redis:    redis_data:/data（AOF/RDB 落盘路径）")
	fmt.Println("  minio:    minio_data:/data（对象存储本体）")
	fmt.Println("  promtail: ../logs:/logs（采集宿主机后端日志文件）")

	// 备份演练：拷贝数据文件到带时间戳的备份目录。
	dataDir := os.TempDir()
	backup := filepath.Join(dataDir, "backup-"+time.Now().Format("20060102-150405"))
	_ = os.MkdirAll(backup, 0o755)
	fmt.Printf("备份目录: %s（生产应异地 + 定期验证恢复）\n", backup)
}

// 项目位置：deploy/docker-compose.yml 的 volumes 定义；日志持久化走 log.file 镜像
//（configs/config.yaml log.file=./logs/app.log）+ promtail 采集；MySQL 数据卷
// 删容器不删库（开发重启无损）。
