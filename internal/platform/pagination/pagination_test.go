package pagination

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// queryContext 构造带查询串的 gin.Context（走真实 query 解析）。
func queryContext(t *testing.T, rawQuery string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	r := httptest.NewRequest(http.MethodGet, "/?"+rawQuery, nil)
	c.Request = r
	return c
}

func TestFromQueryDefaults(t *testing.T) {
	p := FromQuery(queryContext(t, ""))
	assert.Equal(t, 1, p.Page)
	assert.Equal(t, 20, p.PageSize)
}

func TestFromQueryValid(t *testing.T) {
	p := FromQuery(queryContext(t, "page=3&page_size=15"))
	assert.Equal(t, 3, p.Page)
	assert.Equal(t, 15, p.PageSize)

	// 边界值：下界 1、上界 50 均合法保留。
	p = FromQuery(queryContext(t, "page=1&page_size=1"))
	assert.Equal(t, 1, p.Page)
	assert.Equal(t, 1, p.PageSize)
	p = FromQuery(queryContext(t, "page=2&page_size=50"))
	assert.Equal(t, 2, p.Page)
	assert.Equal(t, 50, p.PageSize)
}

func TestFromQueryInvalidFallsBack(t *testing.T) {
	// 非数字 / 负数 / 零：page 回退 1，page_size 回退 20。
	for _, q := range []string{
		"page=abc&page_size=xyz",
		"page=-1&page_size=-5",
		"page=0&page_size=0",
		"page=1.5&page_size=2.5",
		"page=%20&page_size=%20",
	} {
		p := FromQuery(queryContext(t, q))
		assert.Equal(t, 1, p.Page, "query %q: page 应回退 1", q)
		assert.Equal(t, 20, p.PageSize, "query %q: page_size 应回退 20", q)
	}
}

func TestFromQueryClampsMax(t *testing.T) {
	// page_size 超上限钳到 50（与各模块 service 层钳制一致）；page 无上限（深翻页）。
	p := FromQuery(queryContext(t, "page=999&page_size=999"))
	assert.Equal(t, 999, p.Page)
	assert.Equal(t, 50, p.PageSize)

	p = FromQuery(queryContext(t, "page_size=51"))
	assert.Equal(t, 50, p.PageSize)
}
