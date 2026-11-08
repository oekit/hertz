## [v0.10.8] — 2026-11-08

- fix(hertzotel): 「engine 未装 server tracer」缺失诊断判据重构（#168）——旧判据 `c.GetTraceInfo() == nil` 在 hertz v0.10.6 判据失效：engine 构造时 enableTrace=true，ctxPool.New 给每个 RequestContext 预置 TraceInfo，只有 initTrace 发现完全没有任何 tracer.Tracer 时才置 false——只要 engine 上还挂着任意非 otel 的 tracer（stats/自研 tracer 实现同一接口），GetTraceInfo() 就非 nil，警告在「混合装配」（有别的 tracer、缺的恰是 otel server tracer）下恒不触发，而 otel carrier（由 otel serverTracer 的 Start 注入）确实缺失，ServerMiddleware 静默不产 span——正是注释宣称要防的「半截 span 哑火」形态。修复后判据为两条任一命中：① TraceInfo 缺失（engine 零 tracer，逐 engine 精确）；② 进程从未经 NewServerTracer/NewServerTracing 创建过 otel server tracer（包级「已配对安装」状态 serverTracerCreated，覆盖混合装配哑火）。NewServerMiddleware 内部为建中间件 cfg 调上游 NewServerTracer 的那份 Option 被丢弃、不置位（否则诊断自身哑火）；NewServerTracing godoc 补挂两个字段必须成对用满的 MustUse 口径。已知局限：进程粒度而非 engine 粒度——多 engine 进程里只要任意 engine 配过本包 tracer，另一台裸 engine 上的哑火探测不到（carrier 类型在上游 internal 包内，请求期无法逐 engine 验证），已在注释钉住。表驱动测试钉住 4 象限判据矩阵（零 tracer / 混合装配 / 已配对 / 别处配过）+ 置位只归两个导出构造器
- fix(governance): 限流桶快照合并改按配置代际单向推进，桶态跨策略热更保留（#169）——旧实现 LoadOrStore 后按策略值比较改写：配置热更（P1→P2）后，一个在更新前拿到 P1 快照、迟到进入比较段的请求会把桶策略改回 P1 并把 tokens 重置为 P1.Rate；随后新请求再翻回 P2——策略 flap 窗口内桶被反复清零，等价对限流路径的一次突发放行（高 RPS 配置调大时尤甚）。修复后：请求起点改用 store.LoadVersion() 一次拿到一致的（配置, 代际）对（Load()+Version() 两次调用可能拿到「新值配旧代次」的自相矛盾对，core#79），桶内带代际 gen，applySnapshot 只允许新于桶代际的快照改写策略（旧快照按桶内现策略执行，单向「代际大者为准」，不可能回退），且改写只换 policy 参数、tokens/last 保留——已消耗的配额不因配置切换被凭空补满，tokens 超出新 burst 由 allow 的上限钳制自然收敛。表驱动测试钉住代际合并矩阵（新快照生效 / 旧快照不回退 / 同代际幂等，三例均验证桶态保留）+ 行为级回归（切换瞬间不凭空放行、新速率自然补满后恢复）；原 DynamicPolicy 用例的「切换即满桶」断言随语义修正
- fix(adaptor): NativeStatic 剥挂载前缀校验路径段边界，半段前缀按 miss（#170）——旧实现 `strings.TrimPrefix(c.Path(), "/"+prefix)` 不校验前缀落在段边界上："/staticfoo/a.js" 会被 "/static" 剥成 "foo/a.js"，与 "/static/a.js" 解析到同一 fsys 文件，产生未声明的别名路径（缓存键/审计路径口径分裂）。挂载在 "/static/*filepath" 精确路由下不可达，但 SPA 场景常见做法是把 handler 挂 /*filepath 兜底路由（NativeStaticSPAHandler 文档即建议 prefix="" 根挂载；prefix 非空 + catch-all 组合完全可能）。修复后剥前缀余串必须以 "/" 开头或为空，否则按 miss 处理（FS 变体 404，SPA 变体不进回退链）；根挂载（prefix=""）与多段 prefix 行为不变。表驱动测试钉住 11 例（正常挂载 / 半段 miss / 多段 prefix / 根挂载），行为级回归用真实 fixture（testdata/foo/hello.html）钉住 catch-all 下别名路径 404、合法路径 200、SPA 变体不回退
- fix(health): /health 的 "timeout" 标签改按逐 checker 截断判定，探针断连不再吞真实故障（#171）——旧实现以共享 runCtx 的取消状态（`runCtx.Err() != nil`）推断超时：探针客户端提前断连（K8s 探针超时砍连接是常态）取消的是请求 ctx（Canceled），此刻恰好因真实故障（DB down 等）失败的 checker 全被改标为 "timeout"（StatusError 不变、503 不变，但 verbose 错误文案与告警分诊全指向超时）；反之超时后迟到 goroutine 投递的真实错误也被丢弃。修复后给每个 checker 单独派生 checkerCtx（context.WithCancel(runCtx)），以 checkerCtx.Err() == context.DeadlineExceeded 判「真被总超时截断」；父 ctx 取消只携带 Canceled，原始 err 文案保留（脱敏/verbose 口径不变）。表驱动测试钉住 4 象限（断连+真实故障脱敏/verbose、断连+通过、无取消基线）+ 真超时仍标 timeout 的另一半

关联问题单：hertz#168、hertz#169、hertz#170、hertz#171。

## [v0.10.7] — 2026-11-08

- fix(crudh): 排序白名单对齐 DB 真实列名，出 SQL 恒用规范化列名（#165）——修复前白名单收「Go 字段名小写 + json 名 + gorm column tag」，但无 column tag 字段的真列名是 gorm NamingStrategy 蛇形（DisplayName → display_name）：白名单收的 "displayname" 与真列名脱节，前端按 json 名 `sort=displayName` **通过白名单校验**后，用户原文 "displayname" 直接拼进 ORDER BY ⇒ 列不存在 ⇒ DB 报错 5xx——v0.9.0 消灭的 5xx 形态换道复活（v0.9.0 只堵住了反向缺口：真列名被白名单拒掉）。修复后：白名单改为「别名 → 规范化列名」映射——规范化列名与 gorm 建表列名对齐（gorm `column:` tag 优先，无 tag 字段套与 NamingStrategy 逐字对齐的蛇形转换，缩写词表 ID/HTTP/UTF8 等全量对齐并以 gorm v1.31.2 真实输出录制 golden 用例）；json 名保留为**放行别名**但出 SQL 一律改用规范化列名，用户原文不再进 ORDER BY；Go 字段名小写不再是别名（与真列名脱节的正是它，v0.10.5 及以前按它排序本就 5xx，此为 400 化的行为收敛而非回归）。显式 WithSortableColumns 语义不变（调用方书写形式原样进 ORDER BY）；WithSortValidation(false) 的全放开口径不变。表驱动回归钉住：别名映射端到端（json 名/蛇形名/大小写变体/多段混排 → Query.Orders 断言出规范化列名）+ 缩写词字段 + golden 表
- fix(bootstrap): 信号双路关闭的良性竞争按 engine 状态判定，不再按错误文本匹配（#166）——信号关机时 bootstrap 与 h.Run() 内置 waitSignal 双路并发 Shutdown，后到者因 engine 已非 running 得到 hertz 私有的 errStatusNotRunning（未导出 sentinel、文案无兼容承诺）；修复前按文本 "engine is not running" 识别该良性错误，hertz 上游改措辞 ⇒ 每次信号关机都产生假 error、关闭链报失败。修复后 shutdownErrTolerable 按 h.IsRunning() 状态判定：err==nil 恒容忍；engine 已非 running 时任何错误都视为双路竞争产物按成功处理（不依赖文案，措辞变化免疫）；engine 仍在 running 时的错误（排空超时等真失败）照常上抛。用例钉住两态判定（从未启动的 engine 错误容忍 / 真启动运行的 engine 错误上抛），不再有钉文案的用例
- fix(nns): InstanceWatcher 回调异步派发，单个慢/挂死回调不再拖住全部服务的实例轮询（#167）——修复前回调在 Start 的 ticker 主循环内同步串行执行（单回调最多同步等 5s，3 回调各挂 5s = 该轮 15s），期间 checkAll 顺序遍历的所有服务的 Discover/变更检测被压后、ticker tick 被丢弃且无任何信号区分「轮询被回调拖慢」，单个挂死消费者把实例变更感知延迟拖到分钟级。修复后：每个回调独立 goroutine（async.GoSafe，panic 收编对齐仓内并发口径，一个回调 panic 不影响同服务其它回调与后续轮次派发）；派发决策与 inflight 计数仍在主循环内同步完成，#133 的「同服务去重 + 跳过轮不动 cache + 排空后重 diff 补发」语义保持；并发执行下各回调拿各自的事件深拷贝（串行时代共享 evt 仅靠「回调别改」的自觉，并发后显式隔离，回调间互不污染）。移除按回调同步等待的 5s 超时计时器（等待主体不存在了，慢回调的影响被 inflight 跳过机制收敛）。新增回归：挂死回调不阻塞其它服务 Discover、panic 隔离与派发恢复、回调间事件隔离、inflight 跳过与补发

关联问题单：hertz#165、hertz#166、hertz#167。

## [v0.10.6] — 2026-11-08

- fix(transportx): requestHeader/replyHeader 的 Values 键匹配改大小写不敏感（EqualFold），与 Get 同一口径（#163）——hertz 存取键均规范化为 MIME canonical 形态（NormalizeHeaderKey，如 Set("X-Trace-ID",…) 落盘为 "X-Trace-Id"），Get 大小写不敏感命中而 Values 此前原文比较：调用方传 "x-trace-id" 这类变体时 Get 命中、Values 返回 nil，同一适配器两套键口径（core/transport 契约注释明说「各框架适配层负责大小写规范化差异」，本适配器此前没接住）。同修 replyHeader.Values；Keys()/Get 的 godoc 补挂 canonical 形态契约（Keys 返回规范化键，消费方应大小写不敏感回读）。表驱动测试钉住 4 种大小写变体 × Get/Values 一致性 + replyHeader 同口径 + Keys canonical 形态

关联问题单：hertz#163。

## [v0.10.5] — 2026-11-08

- fix(bootstrap): 探活路径 onReady panic 改按启动失败处置（#152）——修复前两处 onReady 调用裸奔在 async.GoSafe 里：panic 沿 probeAndReady 展开，外层「探活失败 ⇒ Error + RequestShutdown」处置整段被跳过，GoSafe recover 后仅记日志 ⇒ server 以「探活已过、回调丢失、永不注册」的半启动形态继续 serve（进程假活，典型形态：延迟 Nacos 注册永不发生）；修复后 panic 在 probeAndReady 内转为 error（callOnReady），走既有探活失败链关停 server，与 probeDisabled 同步路径的「响亮启动失败」同一 panic 政策

关联问题单：hertz#152。

## [v0.10.4] — 2026-11-08

- build: 依赖地板对齐——本版把 oekit 依赖地板抬到各依赖当前最新已发布版本（matrix-tools/scripts/align_floors.py 维护轮）

## [v0.10.3] — 2026-11-08

- test(bootstrap): #146 复审收口——负数禁用探活的「不发 dial」半边补回归钉（计数 listener 断言对可建连目标零建连）；修复本体已于 v0.10.1 随 #129/#132 落地，#146 系按旧实现状态的重报，注释引用同步补挂
- docs(bootstrap): 负数禁用语义注释补挂 #146——「零探活协程、零 dial、不吞用户超时」三不变式显式成文

关联问题单：hertz#146。

## [v0.10.2] — 2026-11-07

- build: 全线依赖地板对齐（16 项，v0.1.27 维护轮）

## [v0.10.1] — 2026-11-07

- fix(bootstrap): WithReadyProbeTimeout 负数=禁用探活+GoBackground 前同步回调 onReady——文档承诺兑现，探活失败关健康 server 的路径整个不存在（#129 #132）
- docs(AGENTS): 就绪探活覆盖面口径修正——Options.Addr 单地址，非「全部 HostPort」（#130）
- fix(bootstrap): Addr 为空时退回探 Listener.Addr / 等 engine running——自定义 listener 的未监听即注册窗口关闭（#131）
- fix(nns): 跳过派发轮不消费变更基线——实例变更不再被 cache 吞掉，排空后自动补发（#133）
- docs(health): 「与 bootstrap/healthz 的规范与差异」——hertz/health（信封+三态）为 HTTP 规范形态，差异表钉住（oekit#140 hertz 侧）
- build: 依赖地板对齐（bootstrap v0.6.15 / core v0.13.1 / pkg v0.37.1 等全线）

关联问题单：hertz#129、hertz#130、hertz#131、hertz#132、hertz#133。

## [v0.10.0] — 2026-11-07

### Added（adaptor）
- **feat(adaptor)**：`NativeStaticSPAHandler`——SPA（history 路由）静态托管。
  前端构建产物的路由由 JS 掌管，服务器不认识 `/orders/42` 这类路径：直接静态托管
  会把用户刷新页面变成 404，于是**每个 embed 前端都要自己重写一遍「miss 就发
  index.html」+ 一张 MIME 表 + 一套缓存策略**（`NativeStaticFSHandler` 的
  「不写死 Cache-Control」原则在 SPA 场景恰好不适用）。本函数把这套收进 adaptor
  （[#112](https://github.com/oekit/hertz/issues/112)）
  - **回退判据四条**（全部不满足才回退）：① 仅 `GET`/`HEAD`；② 不在
    `WithSPASkipPrefix` 列出的前缀下；③ 请求路径末段**不带扩展名**——带扩展名
    视为「要一个具体文件」，miss 就 **404**（旧 hash 资源被回退成 HTML 会让浏览器
    报 MIME 错，甚至把 HTML 当 JS 缓存）；④ 路径合法（穿越/非法即 404）
  - **缓存双档**：`WithSPAImmutablePrefix` 列出的前缀（默认 `assets/`）回
    `public, max-age=31536000, immutable`；入口文件与其余路径回 `no-cache`。
    安全缺省——入口 HTML 被长缓存就是「发版后白屏」的经典事故形态
  - **构造期校验**：入口文件（默认 `index.html`，`WithSPAIndex` 可改）在
    `NewServer` 阶段就 `fs.Stat` 校验，缺失即 panic，错误里带 fsys 根的前 5 个
    条目并点出 `fs.Sub` 层级写错这一头号成因（否则表现为「路由走了、回退成功、
    回一个 404 页面」）
  - 逃生口：`WithSPAServeDottedPaths`（带点的前端路由，如 `/release/v1.2`）；
    `WithSPASkipPrefix` 可累加多次（`api/`、`debug/` 这类前缀不该被回退成 HTML）
  - 复用既有 `WithNativeCacheControl` / `WithNativeHeader`；`NativeStaticFSHandler`
    行为逐字不变（本版把 MIME 分发、路径解析、HEAD 无体等共用件抽成
    `newNativeStaticConfig` / `resolveNativeStaticPath` / `serveNativeFile`，
    避免 SPA 变体复制一份 MIME 表——复制正是 issue 抱怨的根源）
  - 命名取舍：issue 建议 `WithSPAAPIPrefix`，本版取 `WithSPASkipPrefix`——
    判据是「这些前缀不回退」这一机制语义，`API` 只是最常见的使用场景
## [v0.9.6] — 2026-11-06

- build: 矩阵依赖地板对齐（第二轮发版后置·收敛圈）

## [v0.9.5] — 2026-11-06

- build: 矩阵依赖地板对齐（第二轮发版后置）

## [v0.9.4] — 2026-11-06

- build: 矩阵依赖地板对齐（维护轮 #233）

## [v0.9.3] — 2026-11-06

- build: 矩阵依赖地板对齐（维护轮 #233）

## [v0.9.2] — 2026-11-05

- build: genproto 撞包加固（[#109](https://github.com/oekit/hertz/issues/109)）——旧单体 `google.golang.org/genproto`（2019-08，经 tjfoc/gmsm → grpc v1.31.0 的 go.mod 图边入选 module graph）仍含 `googleapis/rpc/status`，与分裂模块 `genproto/googleapis/rpc` 撞包：go1.27 工具链的包装载对图内多提供者的判定不同，`nns` 包在 go1.27 下 setup 阶段报 ambiguous import（go1.26 CI 绿，纯属工具链兼容面问题，go1.27 消费方 import `oekit/hertz/nns` 即撞）。主 go.mod 显式 require 新单体 genproto（2026-10，googleapis/rpc 已拆走）⇒ MVS 选中新单体、旧单体不再提供该包，歧义永久消失；同期 `genproto/googleapis/rpc` 跟随升到 2026-10-19。显式 require 需要 import 边才能在 `go mod tidy` 下存活（tidy 会剪掉无人 import 的 require）——以 `//go:build tools` 标签的 tools.go 提供「只被 tidy 看见、不被正常编译」的钉版 import 边（指向新单体的纯别名占位包 protobuf/field_mask，零新依赖）。验证：go1.27.3 与 GOTOOLCHAIN=go1.26.9 双工具链 `go test ./...` 全绿（18 包，含 nns）；`go mod tidy` 幂等（二次 tidy 零 diff）

关联问题单：hertz#109。

# Changelog — oekit/hertz

所有 notable 变更记录如下。格式参考 [Keep a Changelog](https://keepachangelog.com/)。

## [v0.9.1] — 2026-11-04

- build: 矩阵内依赖地板 `gormx` v0.7.1 → v0.8.0（源仓 gormx v0.8.0 把纯 Go sqlite 入口移出根包、拆 opt-in 子包 `gormx/sqlitedrv`，源仓 oekit/gormx#72；本仓未引用该入口，除依赖版本外无 API 变更）

## [v0.9.0] — 2026-11-03
- **crudh 分页契约统一为「有界 + 归一」**（BREAKING）：缺省 List 从「无参 = 全表」改为「page=1、page_size=20、最多 500 条」，且响应的 `page/page_size/pages` 与实际 LIMIT **恒等**

  **为什么 breaking**：此前 `GET {prefix}` 不带 query（或分页参数非正）时
  `crud.Query.page == nil` ⇒ SQL **无 LIMIT/OFFSET** ⇒ 一次匿名列表请求 = 全表扫描 + 全量序列化；
  且 `page_size` 未归一（SQL 层封顶 500、响应层回显原值）⇒ **元数据说谎**
  （请求 `page_size=100000` 实返 500 条，响应却写 `page_size=100000`、`pages≈1` ⇒ 前端算出的页数与实收数据矛盾）。
  同仓 `validatorh` 早已走 `pagination.Normalize()`（封顶 500）⇒ 两个包口径相反。

  **迁移位**：`WithPageNormalization(false)` 逐字回退 v0.8.14 的分页口径；
  `WithListParams(false)` 语义**不变**（忽略参数 / 恒全量 / 回显 0）；
  新增 `WithDefaultPageSize(n)`、`WithPageSizeLimit(n)`。

- **crudh 排序列白名单按实体字段生成**（BREAKING）：未设 `WithSortableColumns` 时从「任意标识符列可排序」改为「只允许实体类型 T 的字段（字段名 / json tag / gorm `column:` tag，小写口径）」，白名单外一律 **400 `invalid sort parameter`**

  **为什么 breaking**：此前缺省只做标识符正则 ⇒ 任意列名进 `ORDER BY`，而列名由 `db.Order(order)`
  **原文拼进 SQL** ⇒ 列不存在时 DB 报 `no such column` ⇒ 走通用错误出口 ⇒ 客户端拿到 **5xx 而不是 400**
  （前端拼错列名/驼峰大小写 → 500，运维难定位）。
  另修一处语义暗坑：「未设置 = 全放开」与「显式空调用 = 全拒绝」两种相反语义不再混同。

  **迁移位**：`WithSortValidation(false)` 恢复「任意标识符列可排序」；
  列名与字段名/json tag/gorm tag 不一致时（如无 json tag 的 `CreatedAt` → 列 `created_at`）
  需显式 `WithSortableColumns("created_at")` 补齐。

- **crudh 路径参数名可配置/可推导**（新增，默认行为不变）：新增 `WithIDParam(name)`，
  也可按 `Route.Path` 首个 `:param` 自动推导（推导不出回退 `"id"`）

  **为什么补**：此前路径参数名**硬编码 `"id"`**，而 `WithRouteTemplate` 允许任意路径
  ⇒ 注册 `GET /projects/:code` 时端点注册成功、实际永远不可用（string 主键拿到 `""` ⇒ 404；
  int 主键 `ParseInt("")` 失败 ⇒ 400 `invalid id`）⇒ 真·扩展点缺失。

- **fix(transportx)**：reply header 的 `Add` 误用 `Set`（**覆盖**而非追加）⇒ 与同文件 request header 及 core/transport 的 `Header.Add` 契约不符（补测试覆盖）

- **fix(hertzotel)**：`NewServerMiddleware` 丢弃 `NewServerTracer` 返回的 server option ⇒ span 不生成；
  现在**显式诊断**（每进程一次）并订正包注释（「只 `h.Use`」不产 span，成对入口是 `NewServerTracing`）

- **fix(governance)**：策略键用 `string(c.Path())`（**原始路径**）⇒ 带路径参数的路由（如 `/order/123`）永远命中不了 path 级策略、只落到全局键 ⇒ 改用**路由模板**（静态路由不变；按具体实例路径配的策略需改到模板键）

- **feat(hertztracer)**：导出裸 `Option struct{}` 且 `NewServerMiddleware(_ ...Option)` **忽略全部参数**（违反 AGENTS.md「禁止裸 Option」，且是无扩展点空壳）⇒ 改为真 Option（`WithTracer` / `WithOperationName`）并新增 `NewServerTracing`；
  engine 未装 tracer 时中间件自建 span（此前上游 `ServerCtx` 会**中断链路** ⇒ 请求无响应）

- **feat(nns)**：`NewRegistry` 未校验 nil client（同包 `health.go` 对 nil discoverer 明确 panic，口径不一致）⇒ 构造期 panic；
  新增 `RegisterContext` / `DeregisterContext` 透传 ctx（此前 `Register`/`Deregister` 用 `context.Background()`）
  ⚠️ `NewRegistry` 返回类型由 `hregistry.Registry` 改为 `*Registry`（仍实现该接口；仅函数值赋值写法会破）

- **feat(registry)**：`ResolverOption` 是**死扩展点**（无任何构造函数，`NewResolver` 的 opts 循环只作用于无可配置字段的 resolver）⇒ 新增 `WithFetchErr`

- **fix(metrics)**：`MetricsHandlerFor` 每次调用新建 `promhttp.HandlerFor`（无缓存）⇒ 按 Gatherer 缓存；
  删除空实现却自称 `http.Flusher` 的 `Flush()`（已查证 promhttp 全程不用它）

- **refactor(versionh)**：改用 `mw.Response` 统一信封（对外 JSON 逐字不变：键序 / `message:"ok"` / trace_id 来源不变）

- **docs(bootstrap)**：订正注释——hertz v0.10.6 只有**单个**监听地址（`WithHostPorts(hp string)` → `Options.Addr`），
  原注释称「全部 HostPort 可建连」与实现不符

- **docs(sse)**：登记上游空 eventType 的行为（不写 `event:` 行 ⇒ 按 SSE 规范落到默认 `message`）；
  `Close()` 的 `s.w == nil` **不是死分支**（`Stream` 是导出类型，`&Stream{}` 零值可被外部直接构造，
  既有测试依赖 `Close()` 对零值安全）⇒ 保留该守卫并补注释

  **依据**：全部为实测复现（每个修复都有独立反例夹具 + 变异自证：把实现改回旧写法，
  对应测试**精确转红**，改回来即通过）。gofmt/build/vet/test/-race/golangci-lint（2.14.0，与 CI 同版本）全绿。

## [v0.8.14] — 2026-11-03

- crudh：补 Op 语义三处残余缺口——List 参数绑定、id 解析失败出口、Delete found 策略与级联钩子（#101）；缺省行为逐字不变

关联问题单：hertz#101。

## [v0.8.13] — 2026-11-03

- feat(crudh): 响应信封/错误码/路由布局三类可注入 Option（WithResponseWriter/WithErrMapper/WithRouteTemplate）——缺省行为逐字不变（#94）

关联问题单：hertz#94。

## [v0.8.12] — 2026-11-02

- 抬嵌套模块地板：github.com/oekit/registry/nacosimpl v0.1.0 → v0.1.1、github.com/oekit/logx/hlog v0.1.4 → v0.1.6（oekit/oekit#104）

## [v0.8.11] — 2026-11-02

- build: 矩阵内依赖地板 `concurrency` v0.23.2 → v0.23.3、`gormx` v0.6.5 → v0.6.6、`kitx` v0.3.3 → v0.3.4、`middleware` v0.13.6 → v0.13.7（维护轮全量对齐；本版不含 API 变更）

## [v0.8.10] — 2026-11-02

- build: 矩阵内依赖地板 `bootstrap` v0.6.6 → v0.6.7、`middleware` v0.13.5 → v0.13.6、`pkg` v0.31.0 → v0.31.1（维护轮全量对齐；本版不含 API 变更）

## [v0.8.9] — 2026-11-01

- build: 矩阵内依赖地板 `gormx` v0.6.4 → v0.6.5、`middleware` v0.13.4 → v0.13.5、`nns` v0.5.4 → v0.5.5（维护轮全量对齐；本版不含 API 变更）

## [v0.8.8] — 2026-10-30

- build: 矩阵内依赖地板 `tracing` v0.3.1 → v0.3.2（维护轮全量对齐；本版不含 API 变更）

## [v0.8.7] — 2026-10-29

- build: 依赖地板对齐——concurrency 抬到 v0.23.2

## [v0.8.6] — 2026-10-28

- build(deps): 依赖地板对齐 gormx v0.6.0 → v0.6.2，crudh 消费方拿不到 gormx v0.6.2 的 IsUniqueViolation sqlite 分支

## [v0.8.5] — 2026-10-25

- feat(clientip): 跟随 pkg v0.26.0——**默认不采信 X-Real-IP**（BREAKING）：X-Real-IP 是单跳头，HTTP 头无法区分「代理写的」与「客户端自带的」，网关默认不写该头但原样透传客户端自带的 ⇒ 客户端可任意改写自己的客户端 IP（限流/锁定计数桶被绕过、审计与访问日志来源 IP 可改写，oekit/pkg#106）；GetClientIP / GetClientIPWithDefaultProxies 不再采信该头，确需采信改用新增的 GetClientIPWithXRI / GetClientIPWithDefaultProxiesAndXRI（前置条件：可信代理必须覆写该头）；新增 GetClientIPWithSet 走预解析可信代理集合，请求期不再逐条 ParseCIDR 整份清单（oekit/pkg#107 热路径形态）；基座地板 pkg v0.25.2 → v0.26.0

## [v0.8.4] — 2026-10-25

- feat(clientip): Hertz 侧客户端 IP 解析适配——Hertz 自带的 c.ClientIP() 默认信任任意来源的 X-Forwarded-For，直连方可随意伪造，于是限流/审计/访问控制三处的 IP 口径只能各写一遍；新增 clientip 包，把采信判定交给 pkg/convert.ResolveClientIP（与 net/http 侧同一份实现），Hertz 侧只负责取对端地址与两个代理头；GetClientIP(c, trustedProxies) 按可信代理列表解析、GetClientIPWithDefaultProxies(c) 用 pkg/convert 的默认可信网段；基座依赖地板跟到 pkg v0.25.2（#95）

## [v0.8.3] — 2026-10-24

- fix(bootstrap): RunServer 就绪探活被正常关停打断时误报 ERROR「ready probe failed」——关停与探活失败两种语义未区分；探活侧改为返回可判别哨兵 errProbeAborted，调用侧 errors.Is 命中后只记一条 INFO、不再请求关机（进程本就在关停，再请求是多余动作）；并让关停优先于超时判定——探活超时仍按启动失败处理，既有语义不变（#48）

## [v0.8.2] — 2026-10-24

- fix(bootstrap): RunServer 就绪探活与 h.Run() 并发——此前串行先探活必然 connection refused、探活必超时，h.Run() 根本不被调用、server 从不监听；探活改为并发执行，且只在 engine running 后发起、探活通过后复核 engine 仍在运行（端口被他人占用时不得回调 onReady，避免把不属于自己的实例注册进注册中心）（#43）

## [v0.8.1] — 2026-10-22

- build(deps): logx/hlog 地板 v0.1.1 → v0.1.4（父模块地板与 logx v0.7.1 对齐；零代码改动）

## [v0.8.0] — 2026-10-22

- fix(health): `/health` 响应形态提供导出面——消费方不必手写信封结构体（[#26](https://github.com/oekit/hertz/issues/26)）
  - 新增 `health.HealthResponse`（信封 `{code,data,message,trace_id}`，`data` 强类型）+ `health.HealthData`（`{checks,status}`），消费方可直接 `json.Unmarshal` 到导出类型
  - 新增 `health.Status` 类型与 `StatusOK` / `StatusDegraded` / `StatusError` 常量——比较状态不再硬编码字面量
  - **BREAKING**: `CheckResult.Status` 由 `string` 改为 `health.Status`（底层仍是 `string`）。与字面量比较的调用点不受影响；把 `CheckResult.Status` 赋给 `string` 变量的调用点需显式 `string(...)` 转换
  - 响应体字节不变：`data` 由 `map[string]any` 改为强类型结构，键序与取值与既有响应完全一致
- build: 测试改用嵌套模块 registry/nacos——registry v0.5.0 起不再提供该子包
  - 依赖地板：registry v0.4.1 → **v0.5.0**（新增 require `github.com/oekit/registry/nacos v0.1.0`，中接 `registry/nacosimpl v0.1.0`）；logx v0.7.0 → v0.7.1
  - registry v0.5.0 把 Nacos 客户端门面拆成独立发版的嵌套模块，import 路径不变但「由哪个 module 提供」变了——不补 require 则 `go mod tidy` 认不出提供者
- 补记：health 软依赖降级与错误脱敏（`Degraded(err)` / `WithVerboseErrors()`）随 v0.5.1 节首发，本版未再改动（#6）

## [v0.7.1] — 2026-10-19

- build: 矩阵内依赖地板对齐（align_floors 不动点）：bootstrap v0.6.1 → v0.6.2、concurrency v0.5.4 → v0.17.0、core v0.7.0 → v0.9.2、kitx v0.1.1 → v0.2.0、logx v0.6.0 → v0.7.0、middleware v0.7.2 → v0.12.0、nns v0.4.1 → v0.4.2、pkg v0.11.0 → v0.15.1

## [v0.7.0] — 2026-10-19

**BREAKING**: crudh 主键类型泛型化（跟随 gormx v0.6.0 `[T, ID comparable]`，gormx#7 问题 1）

- `Handler[T, C, U]` → `Handler[T, ID comparable, C, U]`；`HandlerOption`/`WithSortableColumns`/
  `NewHandler` 签名跟随（`crud.Service[T, ID, C, U]`）
- 新增 `WithIDParser(fn func(string) (ID, error))`：路径参数 → ID 解析注入
- 缺省解析（不注入时）：int 系主键 `strconv.ParseInt`（宽度随 ID 类型，int64 与 v0.6 之前
  行为一致）、string 主键直通；其余 comparable 类型必须显式注入，缺省解析失败按 400 拒绝
  （fail-closed）
- 存量 int64 调用点机械迁移：`NewHandler[Item, C, U]` → `NewHandler[Item, int64, C, U]`；
  文本主键项目传恒等解析 `WithIDParser[Doc, string, C, U](func(s string) (string, error) { return s, nil })`
- gormx 地板 v0.5.5 → **v0.6.0**
- 新增文本主键端到端测试（hex 串路径参数经 WithIDParser 直通 + Get/Delete/404 链）与
  `defaultParseID` 单测（int64/string 双形态）

## [v0.6.4] — 2026-10-18

- docs(log): NewLogger 注释改按判据描述 hlog 递归风险（IsHlogBacked 实测值 + 常见触发路径：先 hlog.SetLogger 改全局背端再取 Default/WithService），panic message 同步带上实测判据（[#10](https://github.com/oekit/hertz/issues/10)）

## [v0.6.3] — 2026-10-18

- fix(deps): middleware v0.7.1 → v0.7.2 地板对齐（零代码改动）

## [v0.6.2] — 2026-10-18

- fix(deps): gormx v0.5.4 → v0.5.5、metrics v0.1.6 → v0.1.7、tracing v0.2.1 → v0.2.2 地板对齐（零代码改动）

## [v0.6.1] — 2026-10-18

- fix(deps): 全部 oekit 依赖地板对齐最新（bootstrap v0.5.3 → v0.6.1、logx v0.5.0 → v0.6.0、middleware v0.7.0 → v0.7.1、nns v0.4.0 → v0.4.1、registry v0.4.0 → v0.4.1，零代码改动）

## [v0.6.0] — 2026-10-17

### Added（go-kratos 对齐）
- `transportx` 新子包：Hertz RequestContext → core/transport 适配
  （`NewServerTransport`/`NewServerContext`；Kind=http、Endpoint=Host、Operation=PATH，
  header 读写经协议无关 `transport.Header`）
- `chainmw` 新子包：core/chain 中间件栈接入 Hertz——`Wrap`（自动注入 server Transporter，
  错误出口默认接 `mw.Fail` 统一响应格式，可 `WithOnError` 接管）、`FromHertz` 混编桥
- `RunServer` 变参扩展：`WithOnReady`（TCP 探活通过后回调，典型用途 `app.Ready()`
  触发 bootstrap/nacos v0.3.0 延迟注册）、`WithReadyProbeTimeout`（默认 5s；探活失败
  按启动失败触发关机）。hertz OnRun 钩子先于 listenAndServe 执行，无原生就绪信号，
  探活是对真实就绪的近似；未设置 OnReady 时行为与 v0.5.x 一致

### Added（adaptor）
- **feat(adaptor)**：`MockConn` + `PerformRequest` 测试 helper——补上
  `rc.GetWriter()` 所需的 network.Conn，4 个导出 handler 可用 ut 风格单测
  （原先 `ut.PerformRequest` 直接 nil deref）。响应体在 `MockConn.Out()`，
  不在 `ctx.Response.Body()`（HijackWriter 直写 conn，godoc 已注明）
  （[#9](https://github.com/oekit/hertz/issues/9)）
- **feat(adaptor)**：`NativeStaticFSHandler`——真正零 adaptor 转换的原生静态
  handler（fs.ReadFile + c.Write）。Content-Type 走 mime 表、HEAD、404+nosniff、
  路径穿越拒绝；**不写死 Cache-Control**（`WithNativeCacheControl` 可选）。
  `StaticFSHandler` godoc 点明内部仍走 adaptor
  （[#8](https://github.com/oekit/hertz/issues/8)）

### Changed
- 依赖升级：registry v0.4.0 / nns v0.4.0 / core v0.7.0 / middleware v0.7.0
  （registry/nns 包提升至模块根，导入路径变更）

## [v0.5.10] — 2026-10-17

- bootstrap 地板 v0.5.2 → v0.5.3；nns 地板 v0.3.4 → v0.3.5

## [v0.5.9] — 2026-10-17

- kitx 地板 v0.1.0 → v0.1.1

## [v0.5.8] — 2026-10-17

- pkg 地板 v0.11.0；concurrency v0.5.4、gormx v0.5.4、middleware v0.6.2 地板跟随

## [v0.5.7] — 2026-10-17

- fix(deps): 地板跟随——middleware v0.5.1 → v0.6.1、gormx v0.5.2 → v0.5.3、concurrency v0.5.2 → v0.5.3（pkg/kitx 已在 v0.10.0/v0.1.0）

## [v0.5.6] — 2026-10-17

- refactor(validatorh): 底层校验迁移至 [kitx](https://github.com/oekit/kitx)/validator（pkg v0.10.0 拆分承接）——import 机械替换，Bind*/Fail 400 错误映射行为不变；deps: pkg v0.9.3 → v0.10.0、新增 kitx v0.1.0

## [v0.5.5] — 2026-10-17

- fix(lifecycle): bootstrap_test 迁移 logx/shutdown → core/lifecycle（SA1019）；fix(deps): bootstrap 地板 v0.5.1 → v0.5.2

## [v0.5.4] — 2026-10-17

- **fix(lint)**：`health_test.go` 循环内 `resp` 改复用外层声明（govet shadow）——
  v0.5.2 tag 曾因 CI 存量红被重指向 shadow 修复提交，导致 tag 树与
  sum.golang.org 首次记录的校验和不一致（默认 GOSUMDB 用户拉取报
  checksum mismatch，[#7](https://github.com/oekit/hertz/issues/7)）。
  **处置**：v0.5.2 tag 已恢复至与 sumdb 记录一致的原提交（默认 GOSUMDB 可正常拉取）；
  shadow 修复自 v0.5.4 起可用（v0.5.3 曾按本 issue 请求发布，后随事故版本批次撤删，
  勿引用），受影响用户请使用 v0.5.4 或更高版本

## [v0.5.2] — 2026-10-17

### Fixed

- bootstrap 结构重置对齐（v0.5.0 起包提升至模块根）：`bootstrap_test.go` 的 import 由旧路径 `github.com/oekit/bootstrap/bootstrap` 改为模块根
- 依赖地板升重置新基线：bootstrap v0.4.1 → v0.5.0、gormx v0.3.0 → v0.5.1（旧版本 tag 已随结构重置清空，原地板会导致 `go mod tidy` 与直连拉取失败；测试外代码不涉旧路径，build/vet 不受影响）

## [v0.5.1] — 2026-10-17

仓库结构重置后的首个版本（历史与旧版本号 v0.1.0–v0.5.0 已清空重做，从 v0.5.1 起算）。

### 目录结构

```
github.com/oekit/hertz                  # package hertz — RunServer / hlog 桥接 / Hook（模块根）
├── adaptor/                            # package adaptor — net/http 互转
├── health/                             # package health — K8s 探针 + Degraded 降级
│   └── redischeck/                     # package redischeck — go-redis PING 检查器
├── metrics/                            # package metrics — Prometheus /metrics
├── render/                             # package render — 业务 Render 实现
├── sse/                                # package sse — Server-Sent Events
├── crudh/                              # package crudh — 泛型 CRUD Handler
├── validatorh/                         # package validatorh — 参数绑定校验
├── versionh/                           # package versionh — 版本端点
├── governance/                         # package govhertz — 动态限流/超时中间件
├── nns/                                # package nns — NNS → Hertz 注册发现适配
├── registry/                           # package registry — ekit registry → Hertz 适配
├── hertzotel/                          # package hertzotel — OpenTelemetry 链路
└── hertztracer/                        # package hertztracer — OpenTracing 链路
```

- **核心包 `hertz` 提升到模块根**：import 从 `github.com/oekit/hertz/hertz`
  改为 `github.com/oekit/hertz`（包名仍为 `hertz`），消除双层目录
- **子包收成一级**：`hertz/health` → `health`、`hertz/sse` → `sse`、
  `hertz/metrics` → `metrics`、`hertz/render` → `render`、`hertz/adaptor` → `adaptor`
- **适配器扁平化**：
  `governance/hertz` → `governance`（包名 govhertz 不变，避免与 core/governance 冲突）、
  `registry/hertz` → `registry`（包名 hertz → registry）、
  `nns/hertz` → `nns`（包名 hertz → nns）
- **可观测路径收拢**：`observability/tracing/instrumentation/hertzotel` → `hertzotel`、
  `observability/tracing/instrumentation/hertztracer` → `hertztracer`
- **middleware 依赖对齐 v0.5.0**：`mw` import 路径
  `github.com/oekit/middleware/middleware` → `github.com/oekit/middleware`

### 能力面（自 ekit 拆分保留）

- `RunServer` — 接 bootstrap 生命周期，失败自动 RequestShutdown，容忍双路 Shutdown 竞态
- `NewLogger` / `hlog` 桥接 — Hertz 框架日志归一到 logx，caller 归属已修正
- `AddShutdownHook` — 关停前刷数据钩子
- `health.RegisterHealthRoutes` — /ping、/health、/buildVersion；`Degraded(err)` 软依赖降级；（#5）
  `WithVerboseErrors()` 可配置脱敏；空路径跳过注册
- `sse` — 官方 protocol/sse 并发安全封装 + LastEventID 续传
- `metrics.InitAndRegister` — Prometheus /metrics（gzip/OpenMetrics 协商透传）
- `crudh` — 泛型 CRUD Handler，直接消费 gormx `crud.Service`
- `validatorh` — Hertz 参数绑定+校验，ValidationError 自动 400
- `versionh` — 版本信息端点
- `govhertz.NewLimiterMiddleware` / `NewTimeoutMiddleware` — 治理动态限流/超时
- `registry.NewRegistry` / `NewResolver` / `InstancesToHertz` — ekit registry 适配
- `nns.NewRegistry` / `NewResolver` / `NewInstanceWatcher` — NNS 适配（含降级链）
- `hertzotel` / `hertztracer` — server/client 全量 tracing 集成
- fix(health): Redis 检查器迁至子包 `health/redischeck`（#4）——根包只留 stdlib + hertz 核心，非 Redis 消费方不再被迫引入 go-redis/v9

### 迁移（从旧路径）

| 旧 | 新 |
|---|---|
| `github.com/oekit/hertz/hertz` | `github.com/oekit/hertz` |
| `github.com/oekit/hertz/hertz/{health,metrics,sse,render,adaptor}` | `github.com/oekit/hertz/{health,metrics,sse,render,adaptor}` |
| `github.com/oekit/hertz/hertz/health/redischeck` | `github.com/oekit/hertz/health/redischeck` |
| `github.com/oekit/hertz/governance/hertz` | `github.com/oekit/hertz/governance` |
| `github.com/oekit/hertz/registry/hertz` | `github.com/oekit/hertz/registry` |
| `github.com/oekit/hertz/nns/hertz` | `github.com/oekit/hertz/nns` |
| `github.com/oekit/hertz/observability/tracing/instrumentation/hertzotel` | `github.com/oekit/hertz/hertzotel` |
| `github.com/oekit/hertz/observability/tracing/instrumentation/hertztracer` | `github.com/oekit/hertz/hertztracer` |
| `github.com/oekit/middleware/middleware`（本仓 crudh 依赖） | `github.com/oekit/middleware` |

包名变化：`registry/hertz` 与 `nns/hertz` 的包名由 `hertz` 分别改为 `registry`、`nns`。
