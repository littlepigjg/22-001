# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务创建接口出现一组相互关联的字段异常：用户提交 http/https 以外的协议（如 ftp、mailto、tel、file、data）或相对路径、无 host 的 URL 时，本应被拦截却能通过创建校验，保存后访问跳转要么 404 要么打开空白页；提交合法 https 链接时，带 query 参数和 #fragment 锚点的部分会被莫名剥离，导致业务追踪参数全部丢失；设置较远的过期时间（如 30 天、半年）会被强制缩短成「从现在起 24 小时内」；设置较大的 max_visits（如 30 亿、50 亿）会被截断成一个小数字，有时甚至变成负数并被拒绝。另外，只传 TTL（相对有效期）让系统自动计算过期时间时，会直接报错 "expire_at must be before created_at"，明明传的是未来的时间跨度。整体来看是「输入校验、字段规范化、过期判定」三个环节同时出了问题，互相组合导致短链创建与读取的数据都不对。

## 2. 环境信息（Environment）
- 操作系统：Linux（amd64，常见发行版，内核 5.x 以上即可）
- Go 版本：go1.26.5 linux/amd64（其他 1.22+ 版本也可）
- 项目模块/依赖：module shurl，纯标准库，无第三方依赖（go.mod 仅声明 go 1.22）
- 运行参数：`go test . -count=1 -run '^TestRedGreen$'`（或 -count=3 连跑），无需额外环境变量
- 硬件信息：CPU 任意，内存 > 256MB 即可；缺陷为纯逻辑问题，不依赖机器规格

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录：`cd <repo-root>`；确认 `go.mod` 存在，模块名 `shurl`。
2. 执行 `go build ./...` 确保编译通过；执行 `go vet ./...` 确保无静态警告。
3. 查看缺陷验证用例文件：根目录下存在 `red_green_test.go`（package shurl_test），其中 `TestRedGreen` 汇总了 12 个子用例覆盖现象。
4. 在项目根目录执行：`go test . -count=1 -run '^TestRedGreen$' -v`。
5. 观察子用例输出中是否同时出现以下类型失败：
   - validator.URL 对 mailto/tel/data/file 不报错；
   - validator.URL 对相对路径或无 host 的 http:///https://?q=1 不报错；
   - CreateReq.Validate 对 ftp://、mailto、http:// 等放行；
   - MaxVisits 由 30 亿被截断成负数；
   - ExpireAt（30 天后）被压缩成「24 小时后」；
   - RawURL 的 query/fragment 被剥离；
   - ShortURL.Validate 对过期早于创建反而通过、过期晚于创建反而报错；
   - URLService.Create 对「RawURL 带 query」、「MaxVisits 大整数」、「ExpireAt 30 天后」、「TTL=60 天」等合法请求报错或返回错误值。
6. 观察测试最终输出末尾：应当打印一行「RED（红灯，缺陷未修复）」，进程退出码非 0。

## 4. 实际结果（Actual Behavior / Observed Output）
- 具体错误信息（典型片段）：
  - `validator.URL("mailto:bob@example.com") 应当报错但通过了`
  - `validator.URL("/relative/path") 应当报错但通过了`
  - `CreateReq.Validate(raw="ftp://files.example.com/pub.zip") 应报错但通过`
  - `MaxVisits 被截断了: 期望=3000000000 实际=-1294967296`
  - `ExpireAt 被错误修改: 期望≈1年后 实际=24小时后`
  - `RawURL 不应被修改: 期望="https://example.com/path?x=1&y=2#section" 实际="https://example.com/path"`
  - `ShortURL.Validate 对过期早于创建的情形应报错但通过`
  - `ShortURL.Validate 对 ExpireAt>CreatedAt 应通过但报错: shorturl: expire_at must be before created_at`
  - `Create 返回 RawURL 丢失 query/fragment: 期望="https://example.com/orders?id=42&ref=home#confirm" 实际="https://example.com/orders"`
  - `MaxVisits 被 int32 截断: 期望=5000000000 实际=705032704`
  - `Create 失败: shorturl: expire_at must be before created_at`
  - `Create 失败: shorturl: max_visits must be non-negative`
- RED/GREEN 判定结果：RED（红灯，缺陷未修复）
- 其他异常现象：
  - 仅使用相对有效期 TTL 创建短链时，合法时长（60 天）也会被直接报 expire_at must be before created_at 而创建失败；
  - 某些合法的大 MaxVisits（如 40 亿）在入库前被转成负数，从而被 max_visits 非负校验拦截；
  - 即使创建侥幸成功（如 MaxVisits 截断后仍为非负），通过 Get 读取时 MaxVisits 会被再次截断，读取值不等于写入值；
  - 带 query/fragment 的跳转目标 URL 入库前就被剥离，302 Location 缺参，业务追踪/UTM/落地页锚点全部失效；
- go test -race 是否报告 DATA RACE：本缺陷为纯逻辑/字段处理问题，不涉及数据竞争，-race 下通常无 race 报告（但仍会 RED）。

## 5. 期望结果（Expected Behavior）
- 无 panic、无数据竞争（go test -race 无 DATA RACE 警告）。
- RED/GREEN 判定结果应为 GREEN（绿灯，缺陷已修复），`go test . -count=1 -run '^TestRedGreen$'` 退出码为 0。
- 具体的正确业务行为：
  - validator.URL / 领域级 ValidateRawURL 只放行 http/https 绝对 URL 且必须带 host；mailto、tel、data、file、ftp、ftps、相对路径、无 host 的 http:// / https://?q=1 一律返回 error；
  - CreateReq.Validate 不对 RawURL 做 query/fragment 剥离、不在未告知的情况下把 MaxVisits 压缩到 int32 范围、不把任意未来 ExpireAt 强制截成 now+24h；字段值在成功返回后保持与输入一致；
  - ShortURL.Validate 的过期方向正确：当且仅当 ExpireAt 非零且早于 CreatedAt 时返回错误；不在 Validate 内部去覆盖 s.RawURL；
  - URLService.Create 对 RawURL 只做一次 TrimSpace（如有），不剥离 query/fragment、不做 2048 字节静默截断；对 MaxVisits 按 int64 原封不动入库；对 ExpireAt：用户显式传远未来时间就按远未来保存，TTL 模式下按 createdAt+TTL 正确计算；默认永不过期（零值）时不强行设置成创建时间；
  - URLService.Get 返回的条目与存库值一致：RawURL 不会再被「规范化」改短，MaxVisits 不会被 int32 截断；
  - go build ./... 与 go vet ./... 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。所有子用例都是纯逻辑判断，只要代码未修复就稳定触发，和并发、机器、随机种子无关；go test -count=3 连续 3 次稳定 RED。

## 7. 影响范围（Impact / Scope）
- 创建阶段：
  - 非 Web URL（mailto/tel/ftp/file/data）被错误入库，用户访问时不会被正确重定向，造成大量 404 / 白页 / 无法跳转，影响最终用户可用性；
  - 业务方依赖的 query 参数（utm_*、订单 id、分享 token、#deep-link anchor）被默默剥掉，数据分析、转化归因、广告投放全部失真，属于业务数据语义层面的脏数据污染；
  - 大 MaxVisits 被截断导致需要「千万/亿级访问量」的分享链接提前达到上限被封禁为 410，投诉、工单显著增加；若被截断为负数则直接在接口阶段返回「max_visits 必须非负」，正常业务创建失败；
  - ExpireAt 被限制到 24 小时或因过期判断反向导致创建直接失败，活动链接、长生命周期短链全部无法使用，活动发布直接阻塞；
- 读取阶段：Get 返回的 RawURL 仍被再次剥离 query/fragment，导致管理后台展示的跳转目标和用户实际提交不一致，运营/客服排查困难；
- 线上影响：属于短链核心链路（创建、跳转、查询、管理）全量污染，任何一个字段类型（RawURL、MaxVisits、ExpireAt、TTL）的使用都会出问题；不修复会导致数据不可逆脏写（已被截断、被剥参的数据无法还原），越早修复损失越小。

## 8. 附加说明（Additional Notes / Workaround）
- 临时规避（workaround）：接口调用侧在调用创建接口前自行校验 URL 必须为 http(s) 绝对带 host URL；不要传入带有 query/fragment 的 RawURL（如果有参可以先缩短到不带参的落地页）；不要传入超过 2^31-1 的 MaxVisits；不要传入超过 24 小时之后的绝对过期时间，改为每 24 小时后续期；尽量避免使用 TTL，改为显式计算 ExpireAt 并把差值限制在 24 小时内。以上仅为临时缓解，无法恢复已被剥参/截断的历史数据，也无法保证跳转正确，只能减少新增坏数据。
- 相关日志样例（正常 INFO 日志仍会打印，错误在接口层返回或在测试中断言）：
  - 创建成功但 RawURL 已错误：`{"level":"INFO","msg":"short url created","raw":"https://example.com/orders", ...}`，其中 `raw` 已丢失 `?id=42&ref=home#confirm` 部分。
