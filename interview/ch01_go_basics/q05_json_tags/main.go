// Q5 结构体标签与 JSON 序列化（omitempty / json.RawMessage）。
// 运行：go run ./interview/ch01_go_basics/q05_json_tags
package main

import (
	"encoding/json"
	"fmt"
)

// specs 用 json.RawMessage 保存原始 JSON（SKU 规格快照，不解析结构只透传）。
type SKU struct {
	ID     int64           `json:"id"`
	Title  string          `json:"title"`
	Specs  json.RawMessage `json:"specs"`
	Hidden string          `json:"-"`
	Note   string          `json:"note,omitempty"` // 空值不输出
}

func main() {
	sku := SKU{ID: 1, Title: "红色 M 码", Specs: json.RawMessage(`{"color":"red"}`), Hidden: "secret"}
	b, _ := json.Marshal(sku)
	fmt.Println("序列化（Hidden 被忽略、omitempty 生效）:", string(b))

	var back SKU
	_ = json.Unmarshal(b, &back)
	fmt.Println("RawMessage 透传:", string(back.Specs))

	var out []byte
	if json.Valid(back.Specs) {
		out = append(out, back.Specs...)
	}
	fmt.Println("json.Valid 校验合法:", string(out))
}

// 项目位置：internal/product/model/product.go 的 SKU.Specs 与
// internal/order/model/order.go 的 OrderItem 快照均用 json.RawMessage；
// internal/product/service/product_service.go 的 validateSKU 用 json.Valid 校验规格。
