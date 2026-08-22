# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在"批量生成短码 / 批量创建短链接"路径下存在结果异常：一次性调用批量生成短码接口或一次性提交多条原始 URL 创建短链接时，返回的短码列表中多个条目字符串内容完全相同，表现为"前 N-1 条短码被最后一条覆盖"。保存后通过短码回查原始 URL 出现错配，持久化的数据出现重复短码 / 数据不一致。

## 2. 环境信息（Environment）
- 操作系统：Linux（内核版本按实际环境）
- Go 版本：go1.26.5 linux/amd64
- 项目模块：module shurl（Go 1.22）
- 运行参数：
  - 编译/静态检查：`go build ./...`、`go vet ./...`
  - 缺陷验证命令：`go test . -count=5 -run '^TestRedGreen$' -v`
- 硬件信息（参考）：CPU 核数不限，本缺陷为非并发类确定性 slice/共享底层数组问题，单核 / 多核均可复现。

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...`，确认编译通过；再执行 `go vet ./...` 无静态告警。
2. 在根目录执行缺陷验证命令：
   ```
   go test . -count=5 -run '^TestRedGreen$' -v
   ```
   该命令会重复运行 5 次验证用例：
   - 子用例 1：直接构造 shortcode.Generator，调用 GenerateMany(10) 并比较返回的 10 个字符串是否相互重复 / 是否全部等于最后一条。
   - 子用例 2：构造 URLService 提交 10 条不同的 RawURL 调用 CreateMany，对返回的 ShortURL 集合逐个比较 Code 是否重复，并做短码 -> 原始 URL 的回查一致性验证。
3. 观察测试输出末尾是否打印 RED / GREEN 判定，以及 go test 退出码。
4. （可选）在本地 / 集成环境里启动服务，HTTP 调 `POST /api/urls/batch` 提交 items 数组（10 条以上不同 raw_url），检查响应 results[*].code 是否出现相同值。

## 4. 实际结果（Actual Behavior / Observed Output）
- go test 输出（缺陷未修复时稳定复现）：
  ```
  === RUN   TestRedGreen/GenerateMany所有结果互不相同
      red_green_test.go:206: RED case: GenerateMany(10) 第 1 个结果 "ihpUgcA" 重复，所有条目内容完全一致或互相重复
      red_green_test.go:206: RED case: GenerateMany(10) 第 2 个结果 "ihpUgcA" 重复，所有条目内容完全一致或互相重复
      ...（第 3~9 条同样重复，全部等于最后生成的 1 条）
  RED（红灯，缺陷未修复）
  --- FAIL: TestRedGreen (0.01s)
      --- FAIL: TestRedGreen/GenerateMany所有结果互不相同 (0.00s)
  FAIL
  exit status 1
  FAIL    shurl   0.020s
  ```
- RED/GREEN 判定：RED（红灯，缺陷未修复）。
- 其他异常现象：
  - 批量创建返回的多条短码完全一致，落到持久化层后出现重复 code，回查 RawURL 与创建时的索引不一致。
  - 对同一个 Generator，先调用 `GenerateMany(10)` 保存结果 A，再调用一次 `GenerateMany(10)`，观察到 A 里的字符串内容会被第二次调用改写。
- go test -race 是否报告 DATA RACE：本缺陷属于 slice 共享底层数组的确定性内存共享问题，非并发 race；带 -race 运行也不会出现 DATA RACE 警告（仍会稳定 RED）。

## 5. 期望结果（Expected Behavior）
- 无 panic、无异常退出。
- `go build ./...`、`go vet ./...` 全部通过。
- `Generator.GenerateMany(n)` 返回的 n 个字符串相互独立、各自拥有独立底层存储；相互内容不相同（允许极低概率的随机冲突，但绝不能"全部等于最后一次写入的值"）；后续再调用 GenerateMany 绝对不会修改前一次返回切片中的字符串内容。
- `URLService.CreateMany` / `POST /api/urls/batch` 批量创建 N 条不同 RawURL 时，返回的每条 ShortURL.Code 都互不相同，按 Code 回查能稳定拿到创建时对应的原始 URL，无错配无覆盖。
- 执行 `go test . -count=10 -run '^TestRedGreen$'`，全部 10 轮 PASS，测试末尾打印：
  ```
  GREEN（绿灯，缺陷已修复）
  ```
  go test 退出码为 0。

## 6. 触发频率（Frequency）
必现（100%）。由于是共享底层 byte 数组 + 每次覆盖写的 slice 语义问题，无论执行环境、CPU 核数、是否开启 -race，只要执行路径涉及 GenerateMany / CreateMany 即会命中，`-count=5` 5 轮全部相同结果，无偶发。

## 7. 影响范围（Impact / Scope）
- 数据一致性：批量创建返回的短码列表中多条记录共享同一个 code，导致存储层出现重复键，回查时不同原始 URL 被映射到同一个键覆盖、错配。
- 业务正确性：批量创建 10 条短链，实际只有 1 条（最后那条）的短码内容被所有条目使用，跳转目标、访问统计、后台列表全都会出现串号。
- 下游依赖：如果批量创建结果被直接写入数据库 / 消息队列并被其他服务消费，会造成外键冲突、多条记录关联同一短码、账单/报表错误等。
- 无服务崩溃 / 无 goroutine 泄漏，但属于确定性数据污染，危害性与持久化程度正相关。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法：调用方可以把批量 API 改走单条循环调用 `Create`，不使用 GenerateMany / CreateMany；或在拿到 GenerateMany 结果后，对每个字符串做 `strings.Clone(c)` / `string([]byte(c))` 立即复制一份再使用，避免返回值共享同一段底层字节被后续写入覆盖。该规避方法只是缓解，不应作为最终修复。
