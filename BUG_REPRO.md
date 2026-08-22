# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务的创建短码接口在「调用方带超时/取消」和「后端存在慢读/重试」两类场景叠加时，出现了一组相互关联的异常：客户端带 deadline 的请求经常收到一个 HTTP 200 的伪成功响应（提示已排队但系统中实际未入库），服务端在客户端已 cancel/超时后仍完整执行短码生成重试、落盘等所有步骤，并在日志里把取消原因误报为「generate short code failed」；同时，项目内统一的重试组件在取消场景下把 context 取消/超时错误换成了一句通用文本，导致上层用 errors.Is 判断 context.DeadlineExceeded 永远得不到预期结果；存储层打开了慢读校准开关后，即使外层 WithTimeout 设得很短，多次读操作也不会被提前打断，每次都会完整等待一遍延迟，放大了取消信号失效的影响，最终表现在压测中创建接口 P99 明显变长、客户端误判成功而实际未写库、错误信息对排障没有价值。

## 2. 环境信息（Environment）
- 操作系统：Linux（任意发行版，本项目可在容器或裸机运行，复现环境实测 kernel 版本不限）
- Go 版本：go version go1.26.5 linux/amd64（项目 go.mod 声明 Go 1.22，1.22+ 任意版本均可复现）
- 项目模块/依赖：module shurl；零第三方依赖，纯标准库实现
- 运行参数：go test . -count=5 -run '^TestRedGreen$' -timeout 30s；其中子断言会使用 2ms~15ms 的人工慢读、字符空间占用率 95%、deadline 7ms~12ms 等特定参数
- 硬件信息（可选）：任意多核 CPU 均可；与并发/竞态无关，单线程即稳定复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...` 与 `go vet ./...`，确认编译与静态检查均通过（保证代码可运行）。
2. 执行 `go test . -count=1 -run '^TestRedGreen$' -v -timeout 30s`，这是聚合测试入口，内部会分四个子断言分别验证 Service 层取消返回、Handler 层 HTTP 响应、retry 包错误链、store 层慢读响应 ctx 四条行为是否正确。
3. 为了提高稳定性，推荐 `go test . -count=5 -run '^TestRedGreen$' -timeout 30s` 连续跑 5 次；缺陷不是偶发竞态，应当每次都复现。
4. 观察测试 stdout 的末尾判定行：缺陷存在时会出现「RED（红灯，缺陷未修复）」字样，并伴随 go test 的 FAIL + 非零退出码。
5. （可选）单独跑四个顶层测试进一步定位现象：
   - `go test . -count=1 -run '^TestServiceContextCancelledCreate$' -v`
   - `go test . -count=1 -run '^TestHandlerTimeoutCancellation$' -v`
   - `go test . -count=1 -run '^TestRetryPropagatesContextCancel$' -v`
   - `go test . -count=1 -run '^TestStoreExistsRespectsContextCancel$' -v`
   各自都会打印自己的 RED 描述，用于理解哪条链路出问题。
6. （可选，手动脚本级复现）启动 HTTP 服务，用 `curl --max-time 0.012`（12ms 超时）循环发 `POST /api/urls`，观察响应码与 body：正常应为 201 带短码或 4xx/5xx，异常情况会出现 HTTP 200 且响应体 code 为空并含 queued=true。

## 4. 实际结果（Actual Behavior / Observed Output）
按上面的步骤执行，能够稳定观察到以下现象（均来自真实一次测试 stdout）：
- TestServiceContextCancelledCreate：`svc elapsed=10.80ms err=model: generate short code failed code=""`，错误被改成了「生成短码失败」，本该返回的取消类错误（model.ErrCanceled / context.Canceled / context.DeadlineExceeded）一个都匹配不到；
- TestStoreExistsRespectsContextCancel：`store exists observed=30.34ms`，阈值 ≤ 22ms，说明两次慢读每次都完整等待了 15ms，完全没响应外界的短 deadline；
- TestRetryPropagatesContextCancel：`retry err=retry: operation could not be completed hasCancel=false sentinel=true`，context.DeadlineExceeded 被替换成一句不相关的通用 sentinel，`errors.Is(err, context.DeadlineExceeded)` 返回 false；
- TestHandlerTimeoutCancellation：在带取消的客户端请求下，若取消发生得晚，则会返回 HTTP 201 但耗时异常久（~11ms，几乎完整跑完所有重试），客户端以为是正常创建，实际过程中所有取消信号都被丢弃；
- RED/GREEN 判定：聚合测试 `TestRedGreen` 输出「RED（红灯，缺陷未修复）」，测试框架 FAIL，退出码为 1；
- 说明：缺陷不涉及数据竞争，`go test -race` 也不会报 DATA RACE（但建议修复回归仍带 -race 验证）。

## 5. 期望结果（Expected Behavior）
修复后按相同复现步骤执行，应当出现如下全部结果：
- 无 panic、无数据竞争：`go test -race -count=5` 均 PASS 且无 DATA RACE 告警；
- RED/GREEN 判定：聚合 `TestRedGreen` 每次都打印「GREEN（绿灯，缺陷已修复）：全部子断言通过（TestRedGreen 聚合）」，go test 退出码为 0；
- Service 层：带短 deadline 创建短码时，若 deadline 在重试过程中到达，必须返回能被 `errors.Is(err, model.ErrCanceled|context.Canceled|context.DeadlineExceeded)` 识别的取消类错误，且总耗时明显短于完成全部重试的最坏耗时（<15ms），绝不能被改写为「generate short code failed」；
- Handler 层：HTTP 请求取消/超时后，要么在超时前真实创建成功并返回 201 带合法短码，要么向客户端明确暴露取消/超时语义（499 / 5xx / 客户端 Client.Timeout），绝对不允许再出现 HTTP 200 + `{code:"", queued:true}` 这种静默丢数据的伪成功；
- Store 层：打开慢读校准后，连续多次 Exists 调用总耗时必须小于「每次完整延迟之和」，证明每次延迟都响应了调用方 ctx 的取消；
- Retry 层：retry.Do 的返回值在任何取消/超时场景都能被 errors.Is/As 识别出 context.Canceled 或 context.DeadlineExceeded，不会被替换成通用 sentinel；
- 基本非回归：不带取消的普通 Create / Get / Delete / Patch 流程保持原有业务语义，短码合法、自定义冲突仍为 409、查不到仍为 404。

## 6. 触发频率（Frequency）
必现（100%）。四条异常链路是纯逻辑与跨文件契约问题，不依赖调度器/时序，-count=5 每次都稳定复现；与 goroutine 并发无关，即便单线程调用也能稳定触发。

## 7. 影响范围（Impact / Scope）
1. 业务正确性：客户端收到 HTTP 200 queued=true 会被误导为「已排队待创建」，实际没有任何后台任务落地，属于静默数据丢失，用户认为创建成功最终却查不到短码；
2. 可观测性与排障：取消类错误被统一改写为「生成短码失败」或「operation could not be completed」，研发拿到日志后无法从错误文本、errors.Is 快速判断是真的冲突还是取消，排障成本急剧上升；
3. 资源消耗：客户端 cancel 后，后端仍完整执行所有重试 + 慢读（每次都硬睡），在短 deadline 压测下会把请求全部堆积成慢请求，拉 P99、占 CPU/锁；
4. 接口一致性：超时/取消/正常成功三者的返回语义被混淆，服务治理层（网关、熔断、重试框架）无法基于真实取消信号做决策，导致错误重试策略、误用降级等连锁问题。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方式（治标不治本，不建议长期使用）：
- 业务侧调用 POST /api/urls 时把客户端超时设得更长（例如 1s 以上），并关闭所有存储层人工慢读开关；能大幅降低触发概率，但并未从根上修复 context 信号丢失与错误改形，一旦下游出现真实慢读（磁盘、同步等）仍会复现；
- 在网关层禁止把「HTTP 200 且 code 为空」作为成功语义对待，要求前端/网关严格校验响应体中 code 非空才认为创建成功，能避免伪成功带来的静默丢数据，但无法修复后端取消不生效、错误日志不对的问题；
- 对 retry 包的调用方在拿到错误时暂时改用错误字符串包含判定（不推荐），能勉强判断取消场景，但 errors.Is/As 仍会失效，后续易在重构时再次引入。

最终仍然建议回到代码中，从「ctx 不被剥离」「取消错误不被改形」「延迟路径响应 ctx」「retry 错误链保留 context 语义」这几个角度一起修复，才能彻底解决这一组问题。
