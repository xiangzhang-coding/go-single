// Q1 Docker Compose 编排：解析真实 compose 文件。
// 运行：go run ./interview/ch10_deploy/q01_docker_compose
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
		Healthcheck map[string]any    `yaml:"healthcheck"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

func main() {
	// 从当前目录向上找 deploy/docker-compose.yml。
	dir, _ := os.Getwd()
	for d := dir; ; d = filepath.Dir(d) {
		p := filepath.Join(d, "deploy", "docker-compose.yml")
		if b, err := os.ReadFile(p); err == nil {
			var cf composeFile
			if err := yaml.Unmarshal(b, &cf); err != nil {
				fmt.Println("解析失败:", err)
				return
			}
			names := make([]string, 0, len(cf.Services))
			for n := range cf.Services {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				s := cf.Services[n]
				hc := "无健康检查"
				if s.Healthcheck != nil {
					hc = "有健康检查"
				}
				fmt.Printf("%-10s %-28s 端口=%v 健康检查: %s\n", n, s.Image, s.Ports, hc)
			}
			return
		} else if d == "/" {
			break
		}
	}
	fmt.Println("未找到 compose 文件")
}

// 项目位置：deploy/docker-compose.yml（mysql/redis/rabbitmq/minio/nginx/prometheus/
// grafana/loki/promtail 全部带 healthcheck）；根目录 docker-compose.yml 只是 include 桥。
