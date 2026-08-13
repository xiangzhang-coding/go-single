package main

import (
	"errors"
	"testing"
)

// 表驱动测试：与主程序共享被测函数 placeOrder。
func TestPlaceOrder(t *testing.T) {
	cases := []struct {
		name    string
		engine  Engine
		wantErr bool
		wantIs  error
	}{
		{"成功", fakeEngine{nil}, false, nil},
		{"库存不足", fakeEngine{ErrSoldOut}, true, ErrSoldOut},
		{"DB 故障", fakeEngine{errors.New("conn refused")}, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := placeOrder(c.engine)
			if (err != nil) != c.wantErr {
				t.Fatalf("wantErr=%v got=%v", c.wantErr, err)
			}
			if c.wantIs != nil && !errors.Is(err, c.wantIs) {
				t.Fatalf("want errors.Is(%v) got=%v", c.wantIs, err)
			}
		})
	}
}
