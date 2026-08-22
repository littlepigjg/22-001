# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务中 Referer 相关的处理在特定边界输入下会直接越界 panic。对路径长度为 2 的 URL（如 /x、/a）做 Referer 规范化、批量传入 3 条 URL 做批量规范化，或是请求头命中恰好 2 个不同 Referer 值触发重定向写访问日志时，服务都会崩溃并报 index out of range。此外统计聚合在扫描含有长度恰好为 2 的 Referer 脏数据时同样会越界崩溃。整条 Referer 处理链路表现为「输入稍微短一点、数量恰好凑到边界就崩溃」。

## 2. 环境信息（Environment）
- 操作系统：Linux 6.8.0-90-generic（Ubuntu SMP PREEMPT_DYNAMIC x86_64 GNU/Linux）
- Go 版本：go version go1.26.5 linux/amd64
- 项目模块/依赖：module shurl（go.mod 无第三方依赖，纯标准库）
- 运行参数：go test . -count=1/3 -run '^TestRedGreen$'（非并发类缺陷，无需 -race），可加 -v 查看明细
- 硬件信息（与性能相关）：16 核 x86_64 CPU

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...` 确保编译通过（此时应无报错，编译本身不会触发越界，越界只在运行时对边界输入执行）。
2. 执行 `go vet ./...` 确保无静态检查告警（同样不会报越界，因为是运行时下标问题）。
3. 直接运行项目自带的验证用例：`go test . -count=1 -run '^TestRedGreen$' -v`，查看输出与退出码。
4. 为确认非偶发，再执行 `go test . -count=3 -run '^TestRedGreen$'`，重复三次观察是否稳定复现。
5. （可选）若做手工验证，在自己的小脚本里构造如下最小调用：
   - `netutil.NormalizeReferer("https://example.com/x")` → 运行即可复现。
   - `netutil.NormalizeRefererBulk([]string{"https://a.com/abc","http://b.com/xyz","https://c.com/mno"})` → 3 条全成功时复现。

## 4. 实际结果（Actual Behavior / Observed Output）
按复现步骤执行后，实际观察到的结果如下：
- 具体 panic 信息（关键报错片段）：
  - 对路径 len==2 的单条 URL：`panic: runtime error: index out of range [2] with length 0`
  - 对 3 条全部成功的批量列表：`panic: runtime error: index out of range [3] with length 3`
  - 单测汇总信息中还可见 ExtractRefererHost 同样命中 len==2 路径 panic。
- RED/GREEN 判定结果：缺陷未修复时，测试输出显式打印「RED（红灯，缺陷未修复）」，总计 8 个触发点，FAIL。
- 其他异常现象：HTTP 重定向请求命中 2 个 Referer 头组合时服务请求直接异常；后台统计扫描坏 Referer 数据时聚合中断，接口 500。
- go test -race 是否报告 DATA RACE：不涉及，缺陷为纯切片越界，与并发无关，无 DATA RACE 报告。

## 5. 期望结果（Expected Behavior）
修复后按相同步骤运行应得到如下正确行为：
- 无 panic、无越界错误（任何 index out of range 类的 panic 都不应出现）。
- RED/GREEN 判定结果：`go test . -count=3 -run '^TestRedGreen$'` 三次连续运行全部显式打印「GREEN（绿灯，缺陷已修复）」，测试状态 PASS，退出码 0。
- 具体的正确业务行为：
  - NormalizeReferer 对空串、相对 URL、路径为 "/"、"/x"（len==2）、"/xx"、"/path"、含编码 RawPath 等各种输入要么返回规范化字符串，要么返回明确错误，绝不越界崩溃。
  - NormalizeRefererBulk 对 1、2、3、5 条 URL 列表均返回合法切片与长度，不再对 len(result) 下标访问。
  - 重定向写入访问日志时，无论请求头含 0~3 个 Referer 相关头，都正常落日志不崩溃。
  - 统计聚合扫描长度 0/1/2/更长的 Referer 数据时，trim/slice 等操作全部安全完成，来源分布统计正常返回。
- go build ./... 与 go vet ./... 全部通过无告警，go test ./... 全项目其余单测同样全部通过。

## 6. 触发频率（Frequency）
必现（100%）；只要提供的输入命中对应边界（URL.Path 长度恰好 2、批量恰好 3 条全成功、恰好 2 个不同 Referer 头、Referer 脏字符串长度恰好 2、输入 >=5 条批量清洗），每次调用都会 panic，无偶发性；重复 go test -count=3 / -count=20 全部稳定复现 RED，与 CPU 核数或并发量无关。

## 7. 影响范围（Impact / Scope）
- 线上可用性：HTTP 重定向接口对携带特定 Referer 头的外部请求会直接崩溃，中间件虽能 recover 但返回 500，用户短链无法正常跳转。
- 管理与分析面：统计接口聚合访问日志时，若历史日志中存在边界 Referer 字符串，会直接中断统计，后台报表不可用。
- 数据面：Referer 信息不完整或写入链路崩溃会导致访问日志写失败、丢失 Referer 字段，后续来源分布统计失真。
- 运维面：错误监控会持续报 index out of range panic，需额外人工排查；压测中只要命中边界样本就会出现大量失败请求。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法：
- 对外网关层对 Referer 头做白名单过滤，只允许放行路径长度 >=3 的 Referer，或干脆丢弃极短路径的 Referer 避免命中越界点。
- 禁止对 NormalizeRefererBulk 一次传恰好 3 条 URL；调用方在批量前做列表截断或分片（拆成 2 条或 4 条），暂时避开批量恰好 3 条的触发分支。
- 统计侧在扫日志前先过滤掉 Referer 字段长度恰好为 2 的记录，避免 trim 逻辑越界。

以上只是临时 workaround，仍可能遗漏场景，建议彻底修复 Referer 处理链路的边界校验。
