// 地址簿 service 层单元测试（中间 seam）：fake 地址仓储，
// 覆盖字段校验、首条自动默认、设默认唯一性、编辑/删除、对象级授权（owner 校验）。
package service

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

// fakeAddresses 地址簿仓储 seam 的测试替身；默认唯一性模拟
// users.default_address_id 指针语义（map[userID]addressID 一列指向一条）。
type fakeAddresses struct {
	byID     map[int64]*model.Address
	defaults map[int64]int64
	order    int64
}

func newFakeAddresses() *fakeAddresses {
	return &fakeAddresses{byID: map[int64]*model.Address{}, defaults: map[int64]int64{}}
}

func (f *fakeAddresses) Create(_ context.Context, a *model.Address) error {
	f.order++
	a.ID = f.order
	f.byID[a.ID] = a
	return nil
}

func (f *fakeAddresses) Update(_ context.Context, a *model.Address) error {
	if v, ok := f.byID[a.ID]; ok {
		v.Receiver, v.Phone = a.Receiver, a.Phone
		v.Province, v.City, v.District, v.Detail = a.Province, a.City, a.District, a.Detail
	}
	return nil
}

func (f *fakeAddresses) Delete(_ context.Context, id int64) error {
	delete(f.byID, id)
	for userID, def := range f.defaults {
		if def == id {
			delete(f.defaults, userID)
		}
	}
	return nil
}

func (f *fakeAddresses) GetByID(_ context.Context, id int64) (*model.Address, error) {
	return f.byID[id], nil
}

// GetDefaultAddress 模拟 GORM 实现：按 defaults 指针读取，无默认返回 (nil, nil)。
func (f *fakeAddresses) GetDefaultAddress(_ context.Context, userID int64) (*model.Address, error) {
	def, ok := f.defaults[userID]
	if !ok {
		return nil, nil
	}
	a, ok := f.byID[def]
	if !ok {
		return nil, nil
	}
	if a.UserID != userID {
		return nil, nil
	}
	return a, nil
}

// ListByUser 模拟 GORM 实现：派生 is_default 标记，默认地址排最前。
func (f *fakeAddresses) ListByUser(_ context.Context, userID int64) ([]model.Address, error) {
	def := f.defaults[userID]
	list := make([]model.Address, 0)
	for _, a := range f.byID {
		if a.UserID == userID {
			c := *a
			c.IsDefault = a.ID == def
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDefault != list[j].IsDefault {
			return list[i].IsDefault
		}
		return list[i].ID > list[j].ID
	})
	return list, nil
}

func (f *fakeAddresses) CountByUser(_ context.Context, userID int64) (int64, error) {
	var n int64
	for _, a := range f.byID {
		if a.UserID == userID {
			n++
		}
	}
	return n, nil
}

func (f *fakeAddresses) SetDefault(_ context.Context, userID, addressID int64) error {
	f.defaults[userID] = addressID
	return nil
}

// EnsureDefaultExists 模拟 GORM 实现：无默认且仍有余下地址时，把最新一条提为默认。
func (f *fakeAddresses) EnsureDefaultExists(_ context.Context, userID int64) error {
	if _, ok := f.defaults[userID]; ok {
		return nil
	}
	var latest int64
	for _, a := range f.byID {
		if a.UserID == userID && a.ID > latest {
			latest = a.ID
		}
	}
	if latest > 0 {
		f.defaults[userID] = latest
	}
	return nil
}

// 合法地址参数。
func validAddressParams() AddressParams {
	return AddressParams{
		Receiver: "张三", Phone: "13800138000",
		Province: "广东省", City: "深圳市", District: "南山区", Detail: "科技园路 1 号",
	}
}

type addressFixture struct {
	svc Service
	add *fakeAddresses
}

func newAddressFixture() *addressFixture {
	add := newFakeAddresses()
	svc := newTestService(newFakeUsers(), add, &fakeIssuer{})
	return &addressFixture{svc: svc, add: add}
}

func (fx *addressFixture) create(t *testing.T, userID int64, mutate func(*AddressParams)) *model.Address {
	t.Helper()
	p := validAddressParams()
	if mutate != nil {
		mutate(&p)
	}
	a, err := fx.svc.CreateAddress(context.Background(), userID, p)
	require.NoError(t, err)
	return a
}

// ---- 新增与校验 ----

func TestCreateAddressValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AddressParams)
	}{
		{"空收货人", func(p *AddressParams) { p.Receiver = "  " }},
		{"收货人超长", func(p *AddressParams) { p.Receiver = string(make([]rune, 33)) }},
		{"手机号缺位", func(p *AddressParams) { p.Phone = "1380013800" }},
		{"手机号非 1 开头", func(p *AddressParams) { p.Phone = "23800138000" }},
		{"手机号非法号段", func(p *AddressParams) { p.Phone = "12800138000" }},
		{"空省", func(p *AddressParams) { p.Province = "" }},
		{"空市", func(p *AddressParams) { p.City = "" }},
		{"空区", func(p *AddressParams) { p.District = "" }},
		{"空详细地址", func(p *AddressParams) { p.Detail = "" }},
		{"详细地址超长", func(p *AddressParams) { p.Detail = string(make([]rune, 256)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newAddressFixture()
			p := validAddressParams()
			tc.mutate(&p)
			_, err := fx.svc.CreateAddress(context.Background(), 1, p)
			require.ErrorIs(t, err, ErrInvalidAddress)
		})
	}
}

// 首条地址自动设为默认；第二条不默认；显式 is_default=true 设默认。
func TestCreateAddressDefaultRules(t *testing.T) {
	fx := newAddressFixture()

	first, err := fx.svc.CreateAddress(context.Background(), 1, validAddressParams())
	require.NoError(t, err)
	assert.True(t, first.IsDefault, "首条地址应自动设为默认")
	assert.Equal(t, first.ID, fx.add.defaults[1])

	second, err := fx.svc.CreateAddress(context.Background(), 1, validAddressParams())
	require.NoError(t, err)
	assert.False(t, second.IsDefault, "第二条不应默认")
	assert.Equal(t, first.ID, fx.add.defaults[1], "默认仍是首条")

	third, err := fx.svc.CreateAddress(context.Background(), 1, AddressParams{
		Receiver: "李四", Phone: "13900139000", Province: "浙江省", City: "杭州市",
		District: "西湖区", Detail: "文一西路 100 号", IsDefault: true,
	})
	require.NoError(t, err)
	assert.True(t, third.IsDefault)
	assert.Equal(t, third.ID, fx.add.defaults[1], "显式设默认后旧默认失效")

	// 不同用户互不影响。
	other, err := fx.svc.CreateAddress(context.Background(), 2, validAddressParams())
	require.NoError(t, err)
	assert.True(t, other.IsDefault, "另一用户首条也应默认")
}

// ---- 列表 ----

func TestListAddressesDefaultFirst(t *testing.T) {
	fx := newAddressFixture()
	a := fx.create(t, 1, nil)
	b := fx.create(t, 1, nil)

	list, err := fx.svc.ListAddresses(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.True(t, list[0].IsDefault, "默认地址排最前")
	assert.Equal(t, a.ID, list[0].ID)

	// 切换默认后排序随之变化。
	require.NoError(t, fx.svc.SetDefaultAddress(context.Background(), 1, b.ID))
	list, err = fx.svc.ListAddresses(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, b.ID, list[0].ID)
	assert.True(t, list[0].IsDefault)
	assert.False(t, list[1].IsDefault, "旧默认失效")

	// 他人地址不可见。
	other, err := fx.svc.ListAddresses(context.Background(), 2)
	require.NoError(t, err)
	assert.Empty(t, other)
}

// ---- 编辑 ----

func TestUpdateAddress(t *testing.T) {
	fx := newAddressFixture()
	a := fx.create(t, 1, nil)

	p := validAddressParams()
	p.Receiver = "王五"
	p.Phone = "13700137000"
	require.NoError(t, fx.svc.UpdateAddress(context.Background(), 1, a.ID, p))

	list, err := fx.svc.ListAddresses(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, "王五", list[0].Receiver)
	assert.Equal(t, "13700137000", list[0].Phone)
	assert.True(t, list[0].IsDefault, "编辑不应影响默认指向")

	// 不存在的地址 → 404 语义；他人地址 → 403 语义。
	require.ErrorIs(t, fx.svc.UpdateAddress(context.Background(), 1, 999, p), ErrAddressNotFound)
	require.ErrorIs(t, fx.svc.UpdateAddress(context.Background(), 2, a.ID, p), ErrAddressForbidden)
}

// ---- 删除 ----

func TestDeleteAddress(t *testing.T) {
	fx := newAddressFixture()
	a := fx.create(t, 1, nil)
	b := fx.create(t, 1, nil)
	c := fx.create(t, 1, nil)

	// 删除非默认地址：默认不变。
	require.NoError(t, fx.svc.DeleteAddress(context.Background(), 1, b.ID))
	assert.Equal(t, a.ID, fx.add.defaults[1])

	// 删除默认地址：余下最新一条自动提为默认。
	require.NoError(t, fx.svc.DeleteAddress(context.Background(), 1, a.ID))
	assert.Equal(t, c.ID, fx.add.defaults[1], "删除默认地址后应自动提拔最新余下地址")

	list, err := fx.svc.ListAddresses(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].IsDefault)

	// 删除最后一条：无余下地址，默认指向解除。
	require.NoError(t, fx.svc.DeleteAddress(context.Background(), 1, c.ID))
	_, ok := fx.add.defaults[1]
	assert.False(t, ok, "删除最后一条地址后默认指向应解除")

	// 不存在的地址 → 404；他人地址 → 403。
	require.ErrorIs(t, fx.svc.DeleteAddress(context.Background(), 1, 999), ErrAddressNotFound)
	other := fx.create(t, 2, nil)
	require.ErrorIs(t, fx.svc.DeleteAddress(context.Background(), 1, other.ID), ErrAddressForbidden)
	require.NoError(t, fx.svc.DeleteAddress(context.Background(), 2, other.ID), "owner 删除自己的地址应成功")
}

// ---- 设为默认 ----

func TestSetDefaultAddress(t *testing.T) {
	fx := newAddressFixture()
	a := fx.create(t, 1, nil)
	b := fx.create(t, 1, nil)

	require.NoError(t, fx.svc.SetDefaultAddress(context.Background(), 1, b.ID))
	assert.Equal(t, b.ID, fx.add.defaults[1], "设置新默认后旧默认失效")

	// 幂等：重复设为同一地址无副作用。
	require.NoError(t, fx.svc.SetDefaultAddress(context.Background(), 1, b.ID))
	assert.Equal(t, b.ID, fx.add.defaults[1])

	// 不存在的地址 → 404；他人地址 → 403。
	require.ErrorIs(t, fx.svc.SetDefaultAddress(context.Background(), 1, 999), ErrAddressNotFound)
	require.ErrorIs(t, fx.svc.SetDefaultAddress(context.Background(), 2, a.ID), ErrAddressForbidden)
}

// ---- 默认地址读取（秒杀异步落单用）----

func TestGetDefaultAddress(t *testing.T) {
	fx := newAddressFixture()
	a := fx.create(t, 1, nil)

	got, err := fx.svc.GetDefaultAddress(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, got, "有默认地址应返回")
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, int64(1), got.UserID)

	// 无默认地址（从未建地址）→ (nil, nil)。
	none, err := fx.svc.GetDefaultAddress(context.Background(), 99)
	require.NoError(t, err)
	assert.Nil(t, none)
}
