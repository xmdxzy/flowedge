package handler

import (
	"encoding/json"
	"strconv"
)

// 两个只在卡片接口里用到的小工具，避免为一个函数引入整个 fmt 依赖链。

func jsonUnmarshal(raw []byte, v interface{}) error { return json.Unmarshal(raw, v) }

func uintToString(n uint) string { return strconv.FormatUint(uint64(n), 10) }
