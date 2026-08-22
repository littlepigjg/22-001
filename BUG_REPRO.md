# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链接服务的批量创建接口（BatchCreate）在使用偶数次重试配置（例如默认的 2 次）时，如果底层存储层在连续多次尝试中均返回失败（例如高延迟网络、磁盘瞬时故障、批量 API 连续两次返回写入错误），接口会把「全部失败」误报为「全部成功」：返回值中报告的成功条数等于请求条数、失败列表为空、接口 error 也为 nil，但实际随后通过存储层查询这些短码时全部不存在，存储计数也为 0。相同失败场景下如果把重试次数改为奇数（例如 3 次），接口又能正确地报告失败并返回错误。这种误判会造成调用方以为数据已经落盘，但实际上批量短链接记录一条都没有真正写入。

## 2. 环境信息（Environment）
- 操作系统：Linux（Ubuntu 内核 6.8.0-90-generic x86_64）
- Go 版本：go1.26.5 linux/amd64
- 项目模块/依赖：module shurl（零第三方依赖，仅使用标准库）
- 运行参数：go test . -count=3 -run '^TestRedGreen$'，无需 -race；批量接口默认重试次数为 2（偶数）
- 硬件信息（如与并发/性能相关可补充）：复现与硬件无关，确定性触发，无需多核或高并发

## 3. 复现步骤（Steps to Reproduce）
按编号逐条列出能够稳定（或在合理重复次数下）触发缺陷的操作步骤：
1. 进入项目根目录，执行 `go build ./...`，确保编译通过；再执行 `go vet ./...`，确保没有静态检查告警。
2. 确认项目根目录下存在 `red_green_test.go` 测试文件（其中包含偶数次重试与奇数次重试两个对照组：MaxAttempts=2 模拟批量接口默认偶数次重试，MaxAttempts=3 作为奇数次重试的对照）。
3. 执行 `go test . -count=3 -v -run '^TestRedGreen$'`，连续跑 3 轮验证稳定性。
4. 观察标准输出中对偶数次重试的日志：`[even MaxAttempts=2] reported created=N actual_count=M codes_in_store=K`，以及奇数次重试：`[odd MaxAttempts=3] ...`。
5. 关注测试末尾打印的「RED（红灯，缺陷未修复）」或「GREEN（绿灯，缺陷已修复）」判定结果，以及 `go test` 的整体退出码。
6. （可选手动二次校验）在测试代码中对每个批量创建请求的自定义短码调用 `store.Exists(code)`，查看实际有多少条真正被写入。

## 4. 实际结果（Actual Behavior / Observed Output）
按照复现步骤执行后，实际观察到的结果：
- even MaxAttempts=2 场景下输出：
  `reported created=3 actual_count=0 reported_failed=0 codes_in_store=0`
  即：报告成功创建 3 条、失败 0 条、接口 error 为 nil，但存储实际计数为 0、Exists 命中 0 条。
- odd  MaxAttempts=3 场景下输出：
  `reported created=3 actual_count=3 reported_failed=0 codes_in_store=3`
  即：奇数次重试的对照组正常，报告条数与实际落盘数一致。
- RED/GREEN 判定结果（缺陷未修复时应为 RED）：
  每一轮都输出 `RED（红灯，缺陷未修复）`，测试以 FAIL 结束。
- 其他异常现象：返回给调用方的 Created 切片里每条 ShortURL 的 Code 字段都合法，字段校验也都通过，但随后读存储返回 ErrCodeNotFound，给上层造成"写入成功却查不到"的不一致。
- go test -race 是否报告 DATA RACE：本缺陷与并发无关，-race 下无 DATA RACE 输出，属于确定性的错误传播/契约问题。
- 退出码：1（FAIL）

## 5. 期望结果（Expected Behavior）
修复后按相同步骤运行应该出现的正确行为：
- 无 panic、无数据竞争（go test -race 无警告）。
- RED/GREEN 判定结果应为 GREEN（绿灯，缺陷已修复），3 轮全部 PASS，退出码 0。
- 具体的正确业务行为描述：
  - even MaxAttempts=2 场景：如果底层所有重试均失败，接口返回的 error 必须非 nil 且能够体现最后一次失败的原因；result.Created 不得虚报，必须等于实际写入的条目数，每个 code 都能通过 `store.Exists(code)` 验证存在，并且 `len(Created) + len(Failed) == len(reqs)`。
  - odd MaxAttempts=3 场景（对照组）：仍然保持报告条数与实际写入数一致，不得因修复引入回归。
  - 边界：MaxAttempts=1、2、3、4 四种配置在「全部失败」时都不能返回 nil error，也不能出现「成功条数 > 实际写入条数」。
- go build ./... 与 go vet ./... 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。只要：
- 批量创建接口使用的 MaxAttempts 为偶数（例如默认值 2、4、6 等），并且
- 每条记录在 MaxAttempts 次连续尝试中均以失败结束（例如通过模拟 IO 错误注入、或真实高延迟/坏盘场景），
就能稳定触发错误丢失与虚报成功。`-count=3` 重复跑 3 轮可以稳定验证。

## 7. 影响范围（Impact / Scope）
- 数据一致性：批量写入的短链接实际从未落盘，但上层会当作成功，导致业务方在随后创建/访问这些短链接时遇到 404，数据永久丢失。
- 接口返回错误结果：批量 API 的 result.Created / result.Failed 与 error 三者相互矛盾，破坏调用方判断逻辑，后续的对账、统计、回滚全部失真。
- 错误传播链断裂：真实的 IO / 存储错误被吞掉，运维侧无法通过错误日志发现故障，监控会把连续失败场景误判为 100% 成功。
- 线上可用性下降：批量上传/迁移短链的场景下会出现大量"看起来创建成功但实际打不开"的链接，终端用户投诉率升高；如果用于计费或审计数据，还会产生账目错误。
- 与并发无关：不是竞态问题，不依赖 -race 检测；在单线程顺序调用下也能稳定触发。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法（Workaround）：
1. 调用批量接口前，把 MaxAttempts 强制改成奇数（例如 3 或 5），可绕开偶数次全失败触发的误判路径。但这只是规避方式，一旦底层遇到奇数次全部失败与偶数次交替，或后续在其它路径上新增带重试的批量写入，问题依旧会出现。
2. 调用方在拿到 BatchCreateResult.Created 之后，不要信任接口报告的成功条数，对每一条 code 再主动做一次 `Get / Exists` 校验，校验失败则按失败处理。代价是多一轮存储读取，接口吞吐下降。
相关日志样例（测试中实际输出）：
```
[even MaxAttempts=2] reported created=3 actual_count=0 reported_failed=0 codes_in_store=0
[odd  MaxAttempts=3] reported created=3 actual_count=3 reported_failed=0 codes_in_store=3
RED（红灯，缺陷未修复）
```
