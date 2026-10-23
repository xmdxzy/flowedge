# duck 关闭时不要留 asplit 旁路

**规则**：`duck=false` 时，filter_complex 里不应出现为侧链准备的 `asplit` 分支。

**为什么**：多余的 asplit 会多一路未使用的流，ffmpeg 在某些版本下会因此改变混音结果或报"未连接的输出"。功能没开就不该有它的图元。

**动作**：按 duck 开关生成**不同的** filter_complex 字符串，而不是生成一份超集再靠参数禁用。
