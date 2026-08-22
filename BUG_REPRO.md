# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
运行时特性开关（FeatureStore）在新增新配置项时，变更操作类型被错误地记录为 REPLACE（覆盖），实际应为 ADD（新增）；同时审计消息会带上一条类似 "safemap: key not found: xxx" 的错误，并且 OldValue 字段显示为空串，导致调用方无法区分「首次写入一个新 key」和「把旧值从 "" 覆盖成新值」两种完全不同的业务语义。受影响的 API 包括单独写入（SetFeature）、批量写入（BatchApply）以及基于审计历史的查询（LastChangeFor、History）。另外底层 safemap 包的 Swap、SwapMany、MustSwap 三个方法对不存在的 key 首次调用时，新旧值与错误返回值之间互相冲突，同样无法区分「旧值不存在」与「旧值为空但已存在」。

## 2. 环境信息（Environment）
- 操作系统：Linux（Ubuntu 内核 6.8.0-90-generic）
- Go 版本：go1.26.5 linux/amd64
- 项目模块/依赖：module shurl（零第三方依赖，纯标准库）
- 运行参数：go test . -count=1 -run '^TestRedGreen$'（无需 -race，非并发缺陷）
- 硬件信息（如与并发/性能相关可补充）：x86_64，多 CPU 核（无特殊要求）

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...`，确保无编译错误；
2. 执行 `go vet ./...`，确保无静态分析警告；
3. 在项目根目录下执行验证命令：`go test . -count=1 -run '^TestRedGreen$' -v`；
4. 观察测试输出中的逐个子检查：safemap.Swap/SwapMany/MustSwap 语义、FeatureStore.SetFeature 新增场景、FeatureStore.BatchApply 新增/混合场景、History/LastChangeFor 审计一致性；
5. 可选：自行编写最小复现片段，构造 `safemap.New()` 调 Swap 对新 key 写入、构造 `admin.NewFeatureStore()` 调 SetFeature("x","v") 并打印返回的 FeatureChange.Op / OldValue / message 字段。

## 4. 实际结果（Actual Behavior / Observed Output）
按上述步骤执行 go test 后，会看到至少 7 项断言失败，并最终输出：

```
--- FAIL: TestRedGreen (0.00s)
    safemap.Swap semantics: new key Swap should not return error, got err=safemap: key not found: k_new
    safemap.Swap semantics: new key Swap should return old=nil, got old="" (type string)
    safemap.SwapMany semantics: SwapMany[k_fresh] unexpected err=safemap: key not found: k_fresh
    safemap.SwapMany semantics: SwapMany[k_fresh].Old="" (type string), want nil
    safemap.MustSwap semantics: MustSwap(fresh key) err=safemap: key not found: fresh, want nil
    safemap.MustSwap semantics: MustSwap(fresh key) replaced=true, want false
    FeatureStore.SetFeature (ADD case): SetFeature(ADD) Op="REPLACE", want "ADD" (message="safemap: key not found: feature_alpha")
    FeatureStore.BatchApply (ADD case): BatchApply(ADD key=flag_a) Op="REPLACE", want "ADD"
    FeatureStore.BatchApply (REPLACE case): BatchApply new: op="REPLACE" want ADD
    FeatureStore history audit: LastChangeFor(y) Op="REPLACE" want ADD
========================================
RED（红灯，缺陷未修复）—— failed 7 checks
========================================
FAIL
exit status 1
```

其他异常现象总结：
- 底层 Swap 对不存在的 key 返回 old=""（非 nil）并且 err 非 nil，语义冲突：old 非 nil 看起来像「有旧值」，err 又说「key 不存在」。
- FeatureStore 对全新 key 首次写入一律记为 REPLACE，OldValue=""，message 带一串 "safemap: key not found: xxx"，让审计系统把新增项误认为是覆盖。
- BatchApply 混合了存在的 key 和不存在的 key 时，不存在的条目也会被判成 REPLACE。
- LastChangeFor 针对首次写入的 key 返回的变更类型是 REPLACE 而不是 ADD，导致历史审计不可信。
- go test -race 不涉及 DATA RACE（本缺陷非并发类型）。

## 5. 期望结果（Expected Behavior）
修复后按相同步骤运行应得到以下正确行为：

1. 无 panic、无额外错误 message；本缺陷非并发，-race 无数据竞争；
2. go test . -count=1 -run '^TestRedGreen$' 全部断言通过并输出「GREEN（绿灯，缺陷已修复）」，退出码 0；
3. safemap.Swap 对新 key 写入：old=nil，err=nil（成功新增）；对已存在 key 写入：old 返回真实旧值，err=nil（成功覆盖）。
4. safemap.SwapMany 对新 key：Old=nil、Err=nil、Exists=false、New 写入成功；对已存在 key：Old 返回真实旧值、Err=nil、Exists=true。
5. safemap.MustSwap 对新 key：oldValue=""、replaced=false、opErr=nil；对已存在 key：oldValue=真实旧值、replaced=true、opErr=nil。
6. FeatureStore.SetFeature("new_key", "v") 返回 Op=ADD，OldValue=""，message 为空；随后再次修改时返回 Op=REPLACE 并携带真实旧值。
7. FeatureStore.BatchApply 混合批量中：全新 key 条目标记 ADD，已有 key 条目标记 REPLACE，无多余错误 message。
8. FeatureStore.LastChangeFor("first_key") 对首次写入的 key 返回 Op=ADD；后续覆盖后再查返回最新的 REPLACE 记录。
9. go build ./... 与 go vet ./... 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。只要调用的 key 之前不存在，上述错误语义每次都会发生；-count=3 重复执行验证命令，结果一致，不存在偶发情况。

## 7. 影响范围（Impact / Scope）
- 特性开关（Feature Flag）热替换过程中，配置审计日志被全面脏化：所有新增项都被误记为覆盖旧值，后续的灰度发布、变更回滚、合规审计都无法判断某个开关到底是新加的还是被改过值的，线上分流出错后难以追溯。
- 调用方如果以 Op==ADD 作为触发下游通知（例如第一次启用某特性触发初始化回调）的条件，会导致初始化逻辑永不执行，而 REPLACE 分支被错误地提前触发，引起业务状态错配。
- 底层 safemap.Swap / SwapMany / MustSwap 的返回契约不清晰，任何其他业务层代码如果复用了 old!=nil 判定、或者 err 判定都会出现同样的语义错判，造成更大范围的业务逻辑错误。
- 管理员/运营在管理后台看到「特性开关被覆盖」的警告或统计数据失真，影响配置变更判断与问题排障效率。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法：
- 在业务层使用 safemap 时，不要用 Swap 或 MustSwap 判定 ADD/REPLACE；改先用 Get 判断 key 是否存在，再 Set，以规避 Swap 返回语义的模糊性。
- 在 FeatureStore 上层使用前，可先对传入的 key 做 Get 检查，再根据 Get 的结果强行修正 Op 字段，但该 workaround 无法修复底层 Swap 的契约问题，且会增加每一次写入的额外锁开销。

正式修复仍建议从 safemap 层统一 Swap、SwapMany、MustSwap 的返回契约，再在 FeatureStore 层对齐 ADD/REPLACE 的判定逻辑，两层同时修改并加回归测试确认。
