# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在打开了审计日志与访问记录多维分文件写入后，稍微有一点流量，进程就会快速积累打开句柄，最终触碰到系统 `ulimit -n` 的上限，出现 "too many open files" 错误；在错误发生后，大量审计/访问写文件会失败，HTTP 接口也会随之出现随机 500 或明显的响应抖动。问题的严重程度会随着请求量/时间线性加重，重启进程只能短暂缓解。

## 2. 环境信息（Environment）
- 操作系统：Linux（建议 any recent distro）
- Go 版本：Go 1.24+ 或同等版本（项目自带 go.mod）
- 项目模块：shurl（内部 Go 短链服务），无第三方特殊依赖
- 运行参数：不要求 -race；单测以 `go test . -count=1 -run '^TestRedGreen$'` 运行
- 硬件信息：任何支持 go 1.24 的平台均可；CPU 核数不限
- 关键系统配置：建议测试/压测时执行 `ulimit -n 256`，便于在较少请求量下触发 "too many open files"

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...` 与 `go vet ./...` 确保编译通过、无静态错误。
2. 执行 `go test . -count=1 -run '^TestRedGreen$' -v`。
3. 观察标准输出，重点关注 `PeakOpenHandles during probe`、`threshold` 以及最后的 RED/GREEN 判定。
4. （可选）若想复现生产级表现：
   - 在一个 shell 中执行 `ulimit -n 256`，然后用 ab/hey 或自己写的循环脚本，对短链服务的首页、/health、/api/health、若干短码、/api/urls 创建接口等路径持续打 5000~20000 请求。
   - 观察应用日志，出现 `open *: too many open files` 的报错，并伴随 HTTP 5xx 比例上升。
5. （可选）在压测过程中，查看进程的 fd 列表：`ls /proc/<pid>/fd | wc -l` 或 `lsof -p <pid>`，可以看到 audit 相关目录下堆积了大量打开的 .log 文件句柄。

## 4. 实际结果（Actual Behavior / Observed Output）
- 单测输出关键片段：
  ```
  PeakOpenHandles during probe  = 329
  threshold (peak >= RED)       = 128
  RED（红灯，缺陷未修复）: 审计/访问记录多文件 for 循环内 defer Close 未在循环迭代释放，导致生命周期内文件句柄峰值累计过高（peak=329，阈值=128），高请求量下将出现 too many open files 并使后续 HTTP 写文件/落盘失败。
  --- FAIL: TestRedGreen (0.16s)
  FAIL
  FAIL    shurl   0.168s
  ```
- 压测环境下常见错误信息：
  - `open .../audit/batch/YYYY-MM-DD/<dimension>/xx.log: too many open files`
  - `open .../audit/topics/.../xx.log: too many open files`
  - HTTP 侧表现为一定比例的 500 / 记录丢写，随着请求持续，错误比例逐步攀升
- RED/GREEN 判定：RED（红灯，缺陷未修复）
- 是否涉及 race：否；缺陷为 defer 生命周期内句柄累积，非数据竞争。

## 5. 期望结果（Expected Behavior）
- 单测 `go test . -count=1 -run '^TestRedGreen$'` PASS，并输出：
  ```
  GREEN（绿灯，缺陷已修复）
  ```
  且 `PeakOpenHandles during probe` 远低于阈值 128（个位数）。
- 压测：在 `ulimit -n 256` 下持续 5 万次请求，不出现任何 `too many open files` 报错；HTTP 接口错误率维持在 0 或业务预期的极小值；`/proc/<pid>/fd` 中 audit 相关句柄不会无上限累积。
- `go build ./...` 与 `go vet ./...` 全部通过。
- 对外公开 API / 结构体导出字段 / 构造函数签名保持不变；故障演练与诊断相关钩子方法保持可用、行为一致。

## 6. 触发频率（Frequency）
必现（100%）。只要经过足够多请求触发访问批量刷盘 + 审计多主题写入，PeakOpenHandles 会稳定超过阈值，单测直接 RED；在 ulimit 较小的机器上，压测很短时间就能观察到 too many open files。

## 7. 影响范围（Impact / Scope）
- 资源泄漏（文件句柄），逐步耗尽进程可打开的 fd 数量
- 审计日志与访问记录丢失/写入失败，影响安全审计、访问分析与后续报表
- HTTP 接口随机 500，用户请求失败，整体可用性下降；极端情况下整个服务无法接受新连接或写新文件
- 重启可暂时缓解，但流量恢复后问题再次复现，导致运维成本上升与 SLA 下降

## 8. 附加说明（Additional Notes / Workaround）
- 临时规避方式：调大 `ulimit -n` 或将审计分维度数量调低，可以延缓错误出现，但并非根本修复。
- 如需要对比修复前后效果，可在修复后的代码中重复执行 `go test . -count=1 -run '^TestRedGreen$'`，应稳定为 GREEN，并在压测过程中 fd 数量保持稳态，不再累积。
