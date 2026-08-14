// Package pagination 统一分页参数解析：消除各 handler 重复的
// page/page_size 查询参数解析与钳制（T05，7 处团块收敛于此）。
package pagination

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// 全仓统一边界（与各模块 service 层钳制一致：默认 20、上限 50）。
const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 50
)

// PageParams 已钳制到合法区间的分页参数。
type PageParams struct {
	Page     int
	PageSize int
}

// FromQuery 从查询参数解析分页：page 默认 1（非法或 <1 回退），
// page_size 默认 20（非法或 <1 回退，>MaxPageSize 钳制）；page 不设上限
// （深翻页合法）。service 层钳制保留为非 HTTP 调用方的幂等防线。
func FromQuery(c *gin.Context) PageParams {
	page, err := strconv.Atoi(c.DefaultQuery("page", strconv.Itoa(DefaultPage)))
	if err != nil || page < DefaultPage {
		page = DefaultPage
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", strconv.Itoa(DefaultPageSize)))
	if err != nil || pageSize < 1 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return PageParams{Page: page, PageSize: pageSize}
}
