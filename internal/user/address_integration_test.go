// 地址簿集成测试（主 seam）：真实 MySQL + httptest 起完整路由，
// 覆盖 新增/编辑/删除/设默认 CRUD 闭环、默认地址唯一（含 DB 指针校验）、
// 跨用户越权拒绝（owner 校验）、鉴权与字段校验。
package user_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func addressBody(receiver, phone string, isDefault bool) string {
	return fmt.Sprintf(`{"receiver":%q,"phone":%q,"province":"广东省","city":"深圳市","district":"南山区","detail":"科技园路 1 号","is_default":%v}`,
		receiver, phone, isDefault)
}

// createAddress 新增地址并返回地址 id。
func createAddress(t *testing.T, env *testEnv, token, receiver string, isDefault bool) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/addresses", addressBody(receiver, "13800138000", isDefault), token)
	require.Equal(t, http.StatusCreated, w.Code, "新增地址失败: %s", w.Body.String())
	id, ok := body["id"].(float64)
	require.True(t, ok)
	return int64(id)
}

// defaultAddressID 直查 DB 取默认地址指针。
func defaultAddressID(t *testing.T, env *testEnv, userID int64) *int64 {
	t.Helper()
	var id *int64
	require.NoError(t, env.gdb.Raw("SELECT default_address_id FROM users WHERE id = ?", userID).Scan(&id).Error)
	return id
}

// ---- CRUD 闭环 ----

// 新增（首条自动默认）→ 列表 → 编辑 → 设为默认 → 删除，全程状态正确。
func TestAddressLifecycleClosedLoop(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("addr_%d", time.Now().UnixNano())
	reg := registerUser(t, env, username, "secret123")
	userID := int64(reg["id"].(float64))
	_, loginBody := login(t, env, username, "secret123")
	token := tokenOf(t, loginBody)

	// 首条地址自动为默认。
	a := createAddress(t, env, token, "张三", false)
	require.Equal(t, a, *defaultAddressID(t, env, userID), "首条地址应自动成为默认")

	// 第二条不默认。
	b := createAddress(t, env, token, "李四", false)

	w, list := doJSON(t, env, http.MethodGet, "/api/addresses", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items := list["items"].([]any)
	require.Len(t, items, 2)
	first := items[0].(map[string]any)
	require.Equal(t, float64(a), first["id"])
	require.Equal(t, true, first["is_default"], "默认地址排最前且标记为默认")

	// 编辑地址（不改默认）：收货人更新，is_default 仍为 true。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/addresses/%d", b),
		`{"receiver":"李四改","phone":"13900139000","province":"浙江省","city":"杭州市","district":"西湖区","detail":"文一西路 100 号"}`, token)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 设为默认：旧默认失效（端点切换）。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/addresses/%d/default", b), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, b, *defaultAddressID(t, env, userID), "设置新默认后 DB 指针应指向新地址")

	w, list = doJSON(t, env, http.MethodGet, "/api/addresses", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items = list["items"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, float64(b), items[0].(map[string]any)["id"])
	require.Equal(t, true, items[0].(map[string]any)["is_default"])
	require.Equal(t, "李四改", items[0].(map[string]any)["receiver"])
	require.Equal(t, false, items[1].(map[string]any)["is_default"], "旧默认应失效")

	// 删除非默认地址 → 默认不变。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", a), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, b, *defaultAddressID(t, env, userID))

	// 删除默认地址 → FK ON DELETE SET NULL 自动解除指向，列表为空。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", b), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Nil(t, defaultAddressID(t, env, userID), "删除默认地址后指针应置空")
	w, list = doJSON(t, env, http.MethodGet, "/api/addresses", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, list["items"])
}

// ---- 默认地址唯一 ----

// 删除默认地址后自动提拔最新余下地址为默认。
func TestAddressDeleteDefaultPromotes(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("prom_%d", time.Now().UnixNano())
	reg := registerUser(t, env, username, "secret123")
	userID := int64(reg["id"].(float64))
	_, loginBody := login(t, env, username, "secret123")
	token := tokenOf(t, loginBody)

	a := createAddress(t, env, token, "张三", false)
	b := createAddress(t, env, token, "李四", false)

	// 删除默认地址 a → b 自动成为默认（指针自愈）。
	w, _ := doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", a), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, b, *defaultAddressID(t, env, userID), "删除默认地址后应自动提拔最新余下地址")

	// 删除非默认地址（此时 b 是默认，先造一条非默认）→ 默认不变。
	c := createAddress(t, env, token, "王五", false)
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", c), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, b, *defaultAddressID(t, env, userID))

	// 删除最后一条 → 默认指向置空。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", b), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Nil(t, defaultAddressID(t, env, userID))
}

// 新增时显式 is_default=true 同样切换默认；DB 层指针唯一，旧默认失效。
func TestAddressDefaultUnique(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("def_%d", time.Now().UnixNano())
	reg := registerUser(t, env, username, "secret123")
	userID := int64(reg["id"].(float64))
	_, loginBody := login(t, env, username, "secret123")
	token := tokenOf(t, loginBody)

	a := createAddress(t, env, token, "张三", false)
	b := createAddress(t, env, token, "李四", true)

	require.Equal(t, b, *defaultAddressID(t, env, userID), "显式设默认后指针应指向新地址")

	// 列表仅一条默认（旧默认失效）。
	w, list := doJSON(t, env, http.MethodGet, "/api/addresses", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items := list["items"].([]any)
	require.Len(t, items, 2)
	for _, it := range items {
		m := it.(map[string]any)
		if int64(m["id"].(float64)) == b {
			require.Equal(t, true, m["is_default"])
		} else {
			require.Equal(t, false, m["is_default"])
		}
	}

	// 默认切回 a。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/addresses/%d/default", a), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, a, *defaultAddressID(t, env, userID))

	// DB 侧唯一性由构造保证：再直查确认只有一条默认标记。
	var cnt int64
	require.NoError(t, env.gdb.Raw(`SELECT COUNT(*) FROM user_addresses a JOIN users u ON u.id = a.user_id
		WHERE a.user_id = ? AND u.default_address_id = a.id`, userID).Scan(&cnt).Error)
	require.Equal(t, int64(1), cnt)
}

// ---- 对象级授权 ----

// 跨用户访问他人地址被拒：列表不可见，编辑/删除/设默认 → 403。
func TestAddressCrossUserDenied(t *testing.T) {
	env := requireEnv(t)
	uid := time.Now().UnixNano()
	userA := fmt.Sprintf("ava_addr_%d", uid)
	userB := fmt.Sprintf("bob_addr_%d", uid)
	registerUser(t, env, userA, "secret123")
	registerUser(t, env, userB, "secret123")
	_, loginA := login(t, env, userA, "secret123")
	tokenA := tokenOf(t, loginA)
	_, loginB := login(t, env, userB, "secret123")
	tokenB := tokenOf(t, loginB)

	addrA := createAddress(t, env, tokenA, "张三", false)

	// B 的列表看不到 A 的地址。
	w, list := doJSON(t, env, http.MethodGet, "/api/addresses", "", tokenB)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, list["items"])

	// B 编辑 / 删除 / 设默认 A 的地址 → 403。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/addresses/%d", addrA),
		`{"receiver":"hack","phone":"13800138000","province":"广东省","city":"深圳市","district":"南山区","detail":"x"}`, tokenB)
	require.Equal(t, http.StatusForbidden, w.Code)
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", addrA), "", tokenB)
	require.Equal(t, http.StatusForbidden, w.Code)
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/addresses/%d/default", addrA), "", tokenB)
	require.Equal(t, http.StatusForbidden, w.Code)

	// A 的地址未被改动。
	w, list = doJSON(t, env, http.MethodGet, "/api/addresses", "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, list["items"], 1)

	// 不存在的地址 → 404。
	w, _ = doJSON(t, env, http.MethodPut, "/api/addresses/999999/default", "", tokenA)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// ---- 鉴权与校验 ----

func TestAddressAuthAndValidation(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("val_%d", time.Now().UnixNano())
	reg := registerUser(t, env, username, "secret123")
	_, loginBody := login(t, env, username, "secret123")
	token := tokenOf(t, loginBody)

	// 未带 token → 401。
	w, _ := doJSON(t, env, http.MethodGet, "/api/addresses", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 非法手机号 → 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/addresses",
		`{"receiver":"张三","phone":"12345","province":"广东省","city":"深圳市","district":"南山区","detail":"x"}`, token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 缺少必填字段（binding）→ 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/addresses", `{"receiver":"张三","phone":"13800138000"}`, token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 非法 id 参数 → 400。
	w, _ = doJSON(t, env, http.MethodPut, "/api/addresses/abc/default", "", token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 校验不通过不落库（按用户计数，测试库跨包共享）。
	var cnt int64
	require.NoError(t, env.gdb.Table("user_addresses").Where("user_id = ?", reg["id"]).Count(&cnt).Error)
	require.Zero(t, cnt, "校验失败不应落库")
}
