// Q4 分层架构：handler → service → repository → model。
// 运行：go run ./interview/ch09_engineering/q04_layering
package main

import (
	"errors"
	"fmt"
)

// model：数据结构（GORM 表映射）。
type CartItem struct {
	ID    int64
	UserID int64
	SKUID int64
}

// repository：数据访问。
type repo struct{ items []CartItem }

func (r *repo) ListByUser(uid int64) ([]CartItem, error) {
	return r.items, nil
}

// service：业务规则（校验/编排/事务边界）。
type service struct{ repo *repo }

func (s *service) ListCart(uid int64) ([]CartItem, error) {
	if uid <= 0 {
		return nil, errors.New("非法用户")
	}
	return s.repo.ListByUser(uid)
}

// handler：HTTP 出入参、状态码（Gin）。
type handler struct{ svc *service }

func (h *handler) GET(uid int64) {
	items, err := h.svc.ListCart(uid)
	if err != nil {
		fmt.Println("HTTP 400:", err)
		return
	}
	fmt.Println("HTTP 200:", items)
}

func main() {
	h := handler{svc: &service{repo: &repo{items: []CartItem{{ID: 1, UserID: 7, SKUID: 2}}}}}
	h.GET(7)
	h.GET(0)
	fmt.Println("各层只依赖相邻层：handler 不直接碰 DB，repo 不做业务判断")
}

// 项目位置：internal/cart/{handler,service,repository,model} 即此四层结构，
// 全项目模块同构；跨模块只能经 service 接口（模块 DAG 约束）。
