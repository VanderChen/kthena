# 014 — Proposal analyst 报告（第二轮）：把 ServingGroup 做成独立对象交给 Karmada 放置，是不是更好的选择

- 角色：Proposal analyst（第二轮，只做源码刻画、方案比较和评估；不实现，不跑集群）。
- 日期：2026-11-28。
- 问题（用户原话）：「评估如果构造servinggroup cr是不是一个更好的选择」。
- 基线：Karmada `6d007132ec5112d2eed41b602d4aeeebaf96f4fc`（`karmada/`）；kthena upstream main `1a5145e9`（`kthena-014/`）；
  任务 010 的 ServingGroup CR 原型 `98fcbf01`（`kthena-010/`，只读）。下文路径前缀 `karmada:`、`kthena:`、`kthena-010:` 分别指这三处。
- 术语：**方案 M** 是 `DESIGN.md` 的现方案（ModelServing 模板被 Karmada 按 `spec.replicas` 拆分）。
  **方案 G** 是「每个 ServingGroup 一个对象，整体放进恰好一个集群」。G-a、G-b、G-c 见任务说明；本文另把 G-c 细分成两种（第 7 节）。
  「单元」指 G 里被 Karmada 放置的那个对象，「单元绑定」指它的 ResourceBinding。

## 0. 证据分级和方法

| 标签 | 含义 |
| --- | --- |
| 【源码】 | 在上述 commit 读到，带 `路径:行号` |
| 【实测】 | 本轮用临时 Go 测试直接调用 Karmada 的真实包得到（包级，不经过 gRPC、informer、真实集群）。原始输出在附录 A，探针的构造说明在附录 B（源码已随临时目录删除） |
| 【推断】 | 由源码事实推出，没有执行 |
| 【未验证】 | 需要 Kind 或上游确认 |

带「位置」一栏的表格，各行默认是【源码】，另有标注的除外。第 9 节的比较和建议是基于前面各节的判断，属于【推断】。

实测方法：测试文件放在源码树之外的临时目录，用 `go test -overlay` 映射进 `karmada/pkg/...`，`GOTMPDIR` 也指到该临时目录；跑完整个目录已删除。
`karmada/`、`kthena-014/`、`kthena-010/` 没有写入任何文件。Go 1.27.3，`GOFLAGS=-mod=vendor`。
没有创建 Kind 集群、容器或 GitHub 对象；`gh` 只读了 karmada#8104、#5115、#7895、#8030、PR #7830 和 kthena#2148。
上游 issue 的内容作为输入引用（2026-11-28 读取），不属于上面四类标签。

本文引用第一轮 Kind 结果时写「第一轮 Kind：Cx」，那是协调者的证据，不是本轮产出。

## 1. 结论（先看这一节）

### 1.1 一句话

**G 在原理上更对，但「构造 ServingGroup CR」这一步现在不值得做。** 更对的部分来自「一个组一个绑定」这个形状，不来自新 CRD：
同样的形状用「`replicas: 1` 的 ModelServing」（G-c）就能得到，kthena 一行代码都不用改，成员集群还保留完整的本地自愈。
而任务 010 的 ServingGroup CR 原样拿来做单元（G-b）走不通；改成能用的样子（G-a）等于重新设计这个 CR 并拆开 010 的两个控制器，
成本落在 kthena#2148 还没有结论的那条路上，换来的多集群收益 G-c 都有。

建议：方案 M 的第 1 阶段照做（但暂缓把解释器提交为 Karmada 内置定制，理由见 9.3）；把 G-c 作为第 2、3 阶段的候选替代，先在 Kind 上按第 9.4 节的场景对比，再决定重调度走哪条路；
ServingGroup CR 是否进 kthena 仍由 #2148 决定，但现在就该把「如果做，spec 必须自包含」这条多集群约束提给 #2148。

### 1.2 七个问题的答案

| # | 问题 | 答案 |
| --- | --- | --- |
| 1 | 一个多组件单元在 Karmada 里走哪条路 | 走得通，三个前提缺一不可：门控 `MultiplePodTemplatesScheduling`（alpha）、解释器实现 `componentResource`、策略带「集群 1..1」的 spread 约束。节点级估算按组件逐个做 First Fit，认每个组件自己的 nodeSelector、硬亲和、容忍；集群汇总估算不认。两个估算器取最小值。默认资源模型的 GPU 问题在组件路径上同样存在【实测 M2】。不能实现 `replicaRevision` |
| 2 | 多个单元连续调度 | 不开保护时，同一批创建的单元全部落到「估算可用组数最多」的那一个集群【实测：5 个单元全去 b，b 只能放 2 个】。`SchedulingOvercommitProtection`（alpha）能挡住【实测：b、b、c，其余 2 个报容量不足】，但它是调度器内存里的记账，TTL 固定 5 分钟，靠 health 变成 Healthy 释放 |
| 3 | 移动卡住的单元 | 现成机制里只有应用故障转移能把一个不拆分的绑定整体移走，触发条件只有 health。没有「有去处才移」的检查，没有去处时会循环。WorkloadRebalancer 只在原集群估算为 0 时才移，而且不能当成长期意图。descheduler 不适用 |
| 4 | 010 的 ServingGroup CR 能不能原样做单元（G-b） | 不能。成员侧的 ServingGroup 控制器要求同集群里有 UID 匹配的父 ModelServing 和它拥有的 ControllerRevision，Karmada 下发时会去掉 ownerReferences；Lua 解释器也读不到模板 |
| 5 | 010 的 ModelServing 级控制器能不能跑在 Karmada 控制面（G-a） | 现在不能。它读 Pod 缓存来判断组的状态是否可信（`Fresh`），控制面没有 Pod，结果是没有任何组能被确认 Ready，滚动永远不推进。另有 8 处要改（6.2） |
| 6 | G 对方案 M 的 8 条 Kind 发现 | 修掉 4 条（1、3、5、6），保留 3 条（2、7、8），1 条换了形态（4）。新增 12 个问题（第 8 节） |
| 7 | G 是不是更好 | 估算、单元语义、上游对齐更好；成熟度、对象数量、kthena 成本更差。分期和判据见第 9 节 |

### 1.3 这一轮新确认的事实（任务说明里没有的）

1. **已经在运行的单元不「粘」在原集群。** 策略是 `Divided` 加 spread 1..1 时，单元内任何一个 role 的副本数变化都会触发重新调度；
   重新调度要求原集群还能**再**放下一整组，否则单元被搬到别的集群，没有别的集群时调度失败。【实测 S3、S3b、S3c】
   上游 #7895（第五阶段，open）把它列为未完成项：「重调度时的估算应考虑已调度的组件；扩容只算增量」。
2. **策略写成 `Duplicated`（或不写 `replicaScheduling`）时不看容量。** 所有集群都放不下也照样选一个【实测 S2d】；已放置的单元在重新调度时总是留在原集群（被驱逐除外），
   WorkloadRebalancer 对它不起作用【实测 S4d】。上游自己的 e2e 用的是 `Divided/Aggregated` 加 spread 1..1（`karmada:test/e2e/suites/base/schedule_multi_template_test.go:167-176`）。
3. **只要有一个候选集群连不上 scheduler-estimator，整份节点级估算结果被丢掉**，所有集群退回汇总估算。【源码】
   方案 M 的「多节点的组所在集群不部署 scheduler-estimator」因此会让同一次调度里的其他集群也失去节点级估算。
4. **节点级的组件集估算不是处处都对。** 它是按组件声明顺序、节点列表顺序的 First Fit：会低估，不会在资源维度上高估。
   【实测 O1：真实能放 1 组，估算 0；把大 Pod 的组件排在前面后是 1；O3：大的排前面仍然是 0】
5. **不写 spread 约束，单元被复制到所有候选集群。**【实测 X2：三个集群各一份】不装解释器时单元落到按名字排第一的集群，不看容量。【实测 X1】
6. **应用故障转移在没有去处时会循环。** 单元先从原集群删掉，原集群被排除到驱逐任务结束（默认 10 分钟），之后可以回到原集群，再过一个容忍时间又被驱逐。【源码 + 实测 S5b、E3、E6】
7. **FederatedHPA 不能以 G 的父对象为目标**（父对象没有绑定）；CronFederatedHPA 可以。【源码】
8. **G-c 和方案 M 在同一个 Karmada 控制面上对 ModelServing 这个 kind 互斥**（解释器按 kind 生效，门控是全局的）。G-a 用的是另一个 kind，可以和 M 共存。【源码 + 第一轮 Kind：C3】
9. **G-a 在控制面不可用或成员失联时失去组级重建。** 成员只有组内控制器，`Failed` 的组要等控制面的父控制器来删除重建。M 和 G-c 没有这个问题。【源码 + 推断】
10. **多集群是第一个 Plan B（账本放 `ModelServing.status`）给不了、而「组是独立对象」能给的收益**：只有独立对象才能被单独放置。这是可以带回 #2148 的新论据，但对象不必是新 CRD。

### 1.4 对已有结论的更正

| 已有结论 | 更正 |
| --- | --- |
| `DESIGN.md` 4.3：「节点级的组件集估算在全部场景里都对」 | 在那 4 个场景里对。它是 First Fit，存在真实能放、估算为 0 的情形（上面第 4 点）。结论应改成「不会在资源维度上高估，可能低估；解释器应把单 Pod 请求大的组件排在前面」 |
| 第一轮本报告的 G3（降副本后副本被还回原集群）已被 Kind 改掉 | 对单元绑定，这件事是确定的而不是时序相关的：原集群估算还有 1 组时，重新调度一定留在原集群【实测 S4c】；估算为 0 且别处有空时一定搬走【实测 S4】 |

## 2. Q1：一个多组件单元在 Karmada 里的完整路径

### 2.1 进入组件路径的三个前提

| 前提 | 位置 | 缺了会怎样 |
| --- | --- | --- |
| 门控 `MultiplePodTemplatesScheduling` 打开。alpha，默认关。controller-manager（detector 填组件）、scheduler（估算和选集群）、webhook（校验绑定里的组件）都要开 | `karmada:pkg/features/features.go:192`；`pkg/detector/detector.go:1503`；`pkg/scheduler/core/common.go:42`、`util.go:113-118`；`pkg/webhook/resourcebinding/validating.go:92-93` | 绑定里即使有组件，调度器也按「没有副本的非工作负载」处理，落到名字排第一的集群【实测 X4】 |
| 单元的 kind 有 `componentResource`（`GetComponents`） | `pkg/detector/detector.go:1503-1512` | 同上，且绑定里没有组件【实测 X1】 |
| 策略有 `spreadConstraints: [{spreadByField: cluster, minGroups: 1, maxGroups: 1}]`。用集群亲和恰好选中一个集群不算 | `pkg/scheduler/core/estimation.go:43-65` | 单元被下发到**所有**候选集群，每个集群一份完整的组【实测 X2】。`minGroups` 不写会被 webhook 补成 1（`pkg/util/helper/policy.go:32-44`） |

第三条是 G 最容易踩的配置错误：结果是 N 个单元变成 N × 集群数个组，没有任何报错。

### 2.2 detector

`GetComponents` 成功后直接返回，不再调用 `GetReplicas`：绑定里 `spec.components` 有值，`spec.replicas` 是 0，`spec.replicaRequirements` 为空。
【源码 `pkg/detector/detector.go:1498-1534`】只有模板的 spec 变化才会重算（`pkg/detector/detector.go:340`），status 写回不触发。

### 2.3 调度器怎么选出那一个集群

| 步骤 | 做什么 | 位置 |
| --- | --- | --- |
| 过滤 | 和普通绑定相同：API 是否安装、污点、集群亲和、spread 字段、是否在驱逐任务里 | `pkg/scheduler/framework/plugins/registry.go:33-40`；`clustereviction/cluster_eviction.go:51` |
| 打分 | 已经在 `spec.clusters` 里的集群 100 分，其余 0 分 | `clusterlocality/cluster_locality.go:50-61` |
| 估算 | 对每个注册的估算器调 `MaxAvailableComponentSets`，出错的估算器整个跳过，其余按集群取最小值 | `pkg/scheduler/core/util.go:57-110,157-170` |
| 排序 | 分数高的在前；分数相同，估算可用组数多的在前；再相同按名字 | `spreadconstraint/group_clusters.go:353-378`；`spreadconstraint/util.go:43-61` |
| 选择 | 需要的「副本数」固定为 1（一整组） | `pkg/scheduler/core/common.go:42-46` |
| 分配 | 不做副本分配；组件数大于 1 时把各组件的副本数记在 `spec.clusters[].components` 里 | `pkg/scheduler/core/common.go:68-82` |

选择这一步随策略的 `replicaSchedulingType` 不同而不同【源码 `spreadconstraint/select_clusters.go:28-39,71-80`；`select_clusters_by_cluster.go:28-95`】【实测】：

| 情形（一组 = 4 个 Pod × 1 GPU） | `Divided` + spread 1..1 | `Duplicated` 或不写 + spread 1..1 |
| --- | --- | --- |
| 首次放置，空闲 GPU：a=0、b=8、c=4 | b（S1） | b（S1d） |
| 首次放置，没有集群放得下一整组 | 失败，`InsufficientCapacityError`，绑定没有集群，之后约每 5 分钟重试一次（S2） | 照样选 a（S2d）。组在 a 里排队 |
| 已在 a 运行；模板里某个 role 从 3 个 worker 扩到 4 个；a 已满，b 有空 | **搬到 b**（S3） | 留在 a（S3d） |
| 同上，但哪里都没有空 | 失败；`spec.clusters` 不变，但新模板仍然下发到 a（S3b） | 留在 a |
| 同上，a 还能再放一整组 | 留在 a（S3c） | 留在 a |
| 卡在 a（Pod 都 Pending，计入 a 的 allocating），显式触发重调度，b 有空 | 搬到 b（S4） | 留在 a，等于没触发（S4d） |
| 同上，哪里都没有空 | 失败，留在 a（S4b） | 留在 a |
| 同上，但 a 的汇总看起来还能再放一组 | 留在 a（S4c） | 留在 a |
| a 被驱逐（有驱逐任务），b 有空 | b（S5） | b |
| a 被驱逐，哪里都没有空 | 失败，绑定没有集群（S5b） | 选 b（S5c） |
| a 被驱逐，策略里只有 a | `FitError`，调度器不再重试，直到绑定变化（S5d） | 同左 |

带编号的格子是【实测】。右列里没有编号的格子走的是和 S3d、S4d 相同的代码路径（不看容量，取排序第一的集群），是【推断】，没有单独测。

读这张表要注意三点：

- **新单元去的是「可用组数最多」的集群**，不是「刚好放得下」的集群。这是分散，不是聚拢；方案 M 里 `Aggregated` 的「尽量少的集群」没有对应物。
- 「已经放过的集群加回已分配副本数」这一步对单元绑定不起作用：单元绑定的 `spec.clusters[].replicas` 永远是 0
  （`spreadconstraint/group_clusters.go:368`；`pkg/apis/work/v1alpha2/binding_types_helper.go:124-132`）。
  所以重新调度时，单元自己占着的资源被算成「已用」，原集群要有**额外**一整组的空间才留得住。这就是 1.3 第 1 点的根因。
- `Divided` 严格但不粘，`Duplicated` 粘但不看容量。两个都不是 G 想要的语义，要等上游 #7895 的两个未完成项。

触发重新调度的条件【源码 `pkg/scheduler/scheduler.go:395-467`；`pkg/util/binding.go:38-98`】：策略的 placement 变了；`spec.clusters` 为空；
记录的各组件副本数和期望不一致（PR #7830，2026-10-19 合入）；`rescheduleTriggeredAt` 晚于上次调度时间；目标集群正在被删除；
以及 `Duplicated` 策略下绑定每次变化。只改镜像、资源请求不改组件副本数，不触发。只有一个组件的单元不记录组件结果，所以它的副本数变化也不触发【实测 S6】。

### 2.4 两个估算器各自看什么

| | 集群汇总（general） | 节点级（scheduler-estimator） |
| --- | --- | --- |
| 算法 | 每种资源：`(allocatable − allocated − allocating) ÷ 一组的请求之和`，取最小，再受 Pod 数限制 | 每个节点的剩余 = allocatable − 已绑定 Pod 的请求；按组件声明顺序、节点顺序做 First Fit，一组一组地放，直到放不下 |
| 位置 | `pkg/estimator/client/general.go:163-201,393-407` | `pkg/estimator/server/estimate.go:79-109`；`server/framework/plugins/noderesource/noderesource.go:135-194`；`pkg/estimator/scheduling_simulator_components.go:51-125` |
| 还没绑定节点的 Pod | 计入 `allocating` 并扣掉（`pkg/controllers/status/cluster_status_controller.go:588-599`），最长 10 秒刷新一次 | 看不到 |
| 节点碎片 | 看不到 | 看得到 |
| 每个组件的 `nodeClaim`（nodeSelector、硬节点亲和、容忍） | 忽略【实测 M4：没有任何节点满足的 selector，仍然估算 4 组】 | 生效【实测 N1 = 1、N2 = 0、N3 = 0】（`scheduling_simulator_components.go:83-112,149-156`；`pkg/estimator/client/accurate.go:156-177`） |
| 默认资源模型 | 门控 `CustomizedClusterResourceModeling`（默认开）且集群有模型时，再用模型节点做一次 First Fit 并取较小值；默认模型只有 cpu、memory，带 GPU 的组得 0【实测 M2 = 0，对照 M1 = 16，清空模型后 M3 = 4】（`general.go:241-256,262-294,296-360`） | 不受影响 |
| Volcano 的 gang、队列配额、网络拓扑、Pod 间亲和、拓扑分布、PVC、DRA | 都看不到【推断：两个估算器的输入只有资源请求和 `nodeClaim`】 | 都看不到【推断，同左】 |

合并规则：按集群取最小值（`pkg/scheduler/core/util.go:157-170`）。节点级估算对任何一个候选集群出错（包括没有连接），
整份结果作废，只剩汇总估算（`pkg/estimator/client/accurate.go:279-306`；`pkg/estimator/client/cache.go:96-104`；`pkg/scheduler/core/util.go:94-96`）。【源码】
第一轮 Kind 的 C2c（去掉一个集群的 estimator 后按 `[1, 2]` 拆分）和这条一致。

First Fit 的顺序敏感【实测】：

| 场景 | 真实能放 | 估算 |
| --- | --- | --- |
| O1：节点 [4, 2] GPU；组件按模板顺序：小 Pod（2 × 1 GPU）在前，大 Pod（1 × 4 GPU）在后 | 1 | **0** |
| O2：同上，大 Pod 的组件排在前面 | 1 | 1 |
| O3：节点 [6, 4] GPU；大 Pod（1 × 4 GPU）在前，小 Pod（2 × 3 GPU）在后 | 1 | **0** |

所以解释器应该把单 Pod 请求大的组件排在前面，这能去掉一部分低估，去不掉全部。低估的后果是「不放」或「放到别处」，是显式的失败方式。

### 2.5 绑定到 Work

`ensureWork` 对每个目标集群：绑定是工作负载并且这个 kind 有 `ReviseReplica` 钩子，就调用 `ReviseReplica(对象, 该集群的副本数)`；
单元绑定的副本数是 0。【源码 `pkg/controllers/binding/common.go:86-96`；`binding_types_helper.go:219-224`】这就是第一轮 Kind 的 C3。
所以：**单元的 kind 不能实现 `replicaRevision`**；不实现时模板原样下发。

调度失败不阻止下发：binding controller 不看 `Scheduled` 条件（`pkg/controllers/binding/binding_controller.go:110-150`）。
S3b 的情形里，新模板照样到达 a。上游 #7895 的另一个未完成项正是「重调度失败时不要把新配置下发下去」。

### 2.6 状态和健康

成员对象状态变化 → `ReflectStatus` 和 `InterpretHealth` 写进 `Work.status`（`pkg/controllers/status/work_status_controller.go:239-289`）
→ `ResourceBinding.status.aggregatedStatus`（`pkg/util/helper/workstatus.go`）→ 有 `AggregateStatus` 钩子时写回模板的 status（`pkg/controllers/status/common.go:148-213`）。
每一跳一次写；第一跳和第三跳各发一条事件（`work_status_controller.go:246`；`status/common.go:204`）。【源码】单元只有一个目标集群，聚合就是拷贝那一项；优雅驱逐期间会同时有新旧两个集群的两项，Lua 聚合脚本拿不到 `spec.clusters`，分不出哪一项是当前的【推断】，
所以单元的驱逐应使用 `purgeMode: Directly`。

没有 `InterpretHealth` 钩子时永远是 `Healthy`（`work_status_controller.go:270-275`）；Work 没应用成功时是 `Unknown`（`workstatus.go:186`）。

### 2.7 不装解释器，但策略带 spread 1..1

绑定既没有副本数也没有组件，是「非工作负载」：不做估算，所有集群可用量相同，按名字排序取第一个。【实测 X1：三个集群里 a 没有空，仍然选 a】
N 个单元全部落到同一个集群。没有健康解释，永远 `Healthy`；状态进聚合项但不写回模板。
之后装上解释器并更新模板：绑定多出组件，但已有的 `spec.clusters[]` 里没有记录组件结果，不算「变化」，`Divided` 下不重新调度；
这个 kind 没有 `replicaRevision`，成员对象不会被改写成 0。所以单元留在原处，第一轮 Kind 的 C10（清零）不会发生，但那次不看容量的放置结果也留下了。【推断，未验证】

## 3. Q2：多个单元连续调度

### 3.1 窗口有多大

调度器只有一个 worker（`pkg/scheduler/scheduler.go:327`），一个单元的调度是毫秒到百毫秒级。上一个单元的 Pod 要经过
「绑定更新 → Work → 下发 → kthena 建 Pod →（汇总估算）集群状态采集，周期 10 秒 /（节点级估算）Pod 绑定到节点」才对估算器可见。
gang 调度下放不下的组，Pod 一直不绑定节点，节点级估算永远看不到它，只有汇总估算的 `allocating` 能看到。【源码 + 推断】

所以同一批创建的单元看到的是同一份容量。【实测】集群 b 能放 2 组、c 能放 1 组、a 放不下，连续调度 5 个单元：

| `SchedulingOvercommitProtection` | 5 个单元的去向 |
| --- | --- |
| 关（默认） | b、b、b、b、b |
| 开 | b、b、c、容量不足、容量不足 |

扩容、滚动升级时的 surge、父控制器一次建出 N 个单元，都是这种批量。对 G 来说这个门控不是可选项。方案 M 一次拆分一个绑定，同一个 ModelServing 内部没有这个问题。

### 3.2 `SchedulingOvercommitProtection` 具体做什么

| 项 | 行为 | 位置 |
| --- | --- | --- |
| 成熟度 | alpha（v1.18），默认关。karmada-scheduler 和每个 karmada-scheduler-estimator 都要开 | `pkg/features/features.go:195`；`pkg/estimator/server/estimate.go:51-53,98-100` |
| 记什么 | 调度结果成功写回并且有变化之后，对结果里的每个集群记一条：这个绑定的全部组件（副本数和每副本需求）。组件型绑定同样适用 | `pkg/scheduler/scheduler.go:686-714,728-775`（`:759-766` 是组件分支） |
| 存在哪 | 调度器进程内存。重启或换主后清空 | `pkg/scheduler/cache/cache.go:153-163,231-247` |
| 怎么用 | 下一次估算时带给两个估算器：汇总估算从可用量里减掉；节点级估算先把它们用 First Fit 放到节点上，再估算 | `pkg/scheduler/core/util.go:83-85`；`pkg/estimator/client/general.go:177-183,205-213`；`noderesource.go:166-185` |
| 什么时候释放 | 该集群在聚合状态里从非 Healthy 变成 Healthy；集群被移出调度结果；绑定被删除；TTL 到期 | `pkg/scheduler/event_handler.go:196-201,272,496-535`；`scheduler.go:748-757` |
| TTL | 固定 5 分钟，这个 commit 没有命令行参数（设计文档提到的 `--assumption-ttl` 没有实现）；每分钟清一次 | `pkg/scheduler/cache/cache.go:41`；`pkg/scheduler/scheduler.go:245,330-342` |

【实测】汇总估算，16 个空闲 GPU，一组 4 GPU：无记账 4 组；记 1 组后 3 组；记 4 组后 0 组（P1–P3）。

### 3.3 还挡不住什么

1. **重复计算。** Pod 已经绑定、出现在 `allocated` 里，但单元还没 Healthy、TTL 也没到：这一组被扣两次。【实测 P4：真实还能放 3 组，估算 2 组；释放后 P5 = 3】
   health 定义成「全部就绪」而模型加载超过 5 分钟时，每个新单元都会让所在集群在最长 5 分钟内少算一组。偏保守，不会多放。
2. **释放得太早。** health 定义成「没有不可调度的 Pod」时，成员对象刚创建、Pod 还没建出来的那一刻就是 Healthy，记账随即释放，窗口重新打开。
   健康脚本要同时要求「已观察到最新 spec、组已经建出 Pod」。即便如此，Pod 建出到被调度器处理之间还有几秒的空隙。【推断】
3. **TTL 之后的 gang 等待组。** 5 分钟后记账消失，节点级估算又把这个集群看成空的；只剩汇总估算的 `allocating` 按总量挡着，看不到碎片。【推断】
4. **调度器重启。** 记账全丢。
5. **不经过调度器的写入。** descheduler 或任何外部控制器直接改 `spec.clusters`，不会产生记账。
6. **模拟放置和真实放置不同。** 记账的组被 First Fit 放在模拟里的前几个节点上，真实的 Pod 由 Volcano 放到别处，碎片估算有偏差。

### 3.4 FederatedResourceQuota

`FederatedQuotaEnforcement`（alpha）在绑定的校验 webhook 里按组件计算用量（`pkg/webhook/resourcebinding/validating.go:120-170,330-340`）。
G 下每个单元各自过一次配额：超额时只有后面的单元被拒，前面的照常运行；M 下是整个绑定的增量被拒。和本题的结论无关，只是行为差别。【源码】

## 4. Q3：移动卡住的单元

### 4.1 现成机制一览

本表各行是【源码】，展开和【实测】编号在 4.2–4.4。

| 机制 | 能不能整体移走一个不拆分的绑定 | 触发 | 对卡住的单元的问题 |
| --- | --- | --- | --- |
| 应用故障转移（`failover.application`） | 能 | 只有 health：该集群连续 Unhealthy 超过 `tolerationSeconds` | 不检查有没有去处；循环；health 被迫承担「是否卡住」的语义 |
| WorkloadRebalancer / `rescheduleTriggeredAt` | `Divided` 下：原集群估算为 0 且别处有空时能；`Duplicated` 下不能 | 人或控制器创建对象 | 原集群估算不为 0 时不动；失败后的重试是个没有期限的意图（4.4） |
| karmada-descheduler | 不能 | — | 只认 Deployment，只会降副本数；单元绑定没有副本数可降（`pkg/descheduler/core/filter.go:31-33,52-62`） |
| 集群故障转移、污点驱逐 | 能，但按集群，不按单元 | 集群不可用或 `NoExecute` 污点 | 不是这个用途 |
| 直接改 `ResourceBinding.spec.clusters`（定向迁移） | 能。新集群那一项没有记录组件结果，调度器不认为有变化，不会改回去【源码 `pkg/util/binding.go:77-98`】 | 外部控制器 | 绕过调度器的过滤；不产生容量记账 |

### 4.2 应用故障转移逐步

1. **生效条件**：策略有 `failover.application`；聚合状态非空；这个 kind 有 `InterpretHealth`；策略的 `propagateDeps` 为 true。
   没有门控，控制器默认启用。`purgeMode: Gracefully` 需要 `GracefulEviction`（beta，默认开）。
   【源码 `pkg/controllers/applicationfailover/rb_application_failover_controller.go:219-243`；`cmd/controller-manager/app/controllermanager.go:237,255`；`applicationfailover/common.go:138-171`】
2. **计时**：控制器第一次看到某集群 Unhealthy 时记下时间，存在进程内存里；变回 Healthy 或 Unknown 就清掉。controller-manager 重启后重新计时。
   `tolerationSeconds` 默认 300。【源码 `rb_application_failover_controller.go:89-119`；`common.go:34-123`；`pkg/apis/policy/v1alpha1/propagation_types.go:398-406`】
3. **驱逐**：把该集群从 `spec.clusters` 里拿掉，加一条驱逐任务。【源码 `rb_application_failover_controller.go:121-165`；`binding_types_helper.go:156-193`】
4. **旧集群上的对象**：`Directly` 立即删除，并且等删完才向新集群下发；`Gracefully` 保留到任务结束；`Never` 保留到人工确认。
   【源码 `pkg/util/helper/binding.go:191-209`；`pkg/controllers/binding/binding_controller.go:115-125`】
5. **重新调度**：`spec.clusters` 为空触发调度；`ClusterEviction` 过滤掉有驱逐任务的集群。
6. **驱逐任务什么时候结束（旧集群什么时候重新可选）**【源码 `pkg/controllers/gracefuleviction/evictiontask.go:67-115`；`rb_graceful_eviction_controller.go:80-105`】【实测 E1–E6】：

   | 情形 | 任务 |
   | --- | --- |
   | 调度器已处理最新的绑定，新集群 Healthy | 立即结束（E1） |
   | 新集群还不 Healthy | 保留（E2） |
   | 没有集群接收（调度失败，调度器没有确认这一代绑定） | 保留到超时（E3、E5）；超时是任务的 `gracePeriodSeconds`，没有就是 `--graceful-eviction-timeout`，默认 10 分钟（E6） |
   | `Never` | 一直保留 |

7. **没有别的集群放得下时**（`Divided`）：调度失败，绑定没有集群，调度器约每 5 分钟重试一次
   （`pkg/scheduler/scheduler.go:939-951`；`internal/queue/scheduling_queue.go:45,188`）。`Directly` 下原集群里的组已经被删了。
   约 10 分钟后任务超时结束，原集群重新可选；它的估算如果又是 1（卡住的 Pod 已删，汇总估算回到高估），单元回到原集群，重新开始计时，再被驱逐。
   **循环的周期约是「容忍时间 + 10 分钟」，每一圈删一次、建一次，没有退避。**【源码 + 实测 S5b、E3、E6；循环本身是推断，未验证】
   策略里只有这一个集群时是 `FitError`，调度器把它当成「已完成」，不重试，直到任务结束使绑定变化（`pkg/scheduler/helper.go:124-130`）。【实测 S5d】
8. **新集群也放不下时**：新集群 Unhealthy，超过容忍时间后被驱逐；此时原集群的任务多半已经结束，单元可能回到原集群。应用故障转移对「刚失败过的集群」的记忆只到驱逐任务结束。【推断】
9. `Duplicated` 策略下第 7 步不会失败：调度器照样选一个放不下的集群【实测 S5c】，单元每过一个容忍时间换一个集群。

和方案 M 的第 7 条发现相比：循环同样存在，周期更长（M 是 5 分钟），而 M 的修法（规则 6，有去处才降）在应用故障转移里没有可以挂的地方。

### 4.3 把 `InterpretHealth` 定义成「没有不可调度的 Pod」

读 health 的地方只有这些【源码，全仓 `pkg/`、`cmd/` 检索】：

| 消费者 | 位置 | 定义改变后的后果 |
| --- | --- | --- |
| 应用故障转移 | `applicationfailover/common.go:109-123` | 得到想要的效果：正在拉镜像、加载模型的单元是 Healthy，不会被移走。副作用：崩溃循环的单元也永远不会被移走 |
| 优雅驱逐的结束条件 | `gracefuleviction/evictiontask.go:92-115` | 「新集群 Healthy」变成「新集群的 Pod 调度上了」。对卡住的单元没有影响；对**正在服务**的单元（集群故障转移、`NoExecute` 污点，`Gracefully`），旧副本会在新副本刚调度上、还没加载完模型时就被删，中间有一段没有容量 |
| 容量记账的释放 | `pkg/scheduler/event_handler.go:496-535` | 更接近设计意图（Pod 绑定后就不需要记账了），但有 3.3 第 2 点的过早释放 |
| `Work.status`、绑定聚合项里的 `health` 字段 | `work_status_controller.go:270-289` | 用户、看板、告警看到的 Healthy 不再表示「在服务」。要改看聚合后的 `availableReplicas` |

另外三个要处理的边界【推断】：

- **原本在服务的单元**，role 扩容或 Pod 重建后新 Pod 调度不上，也会变成 Unhealthy，超过容忍时间后整个单元（包括还在接流量的 Pod）被驱逐。
  kthena 的 `unschedulableReplicas` 不区分「从来没起来过」和「起来过、现在缺一个 Pod」（`kthena:pkg/model-serving-controller/controller/unschedulable_replicas.go:28-59`）。
  区分这两者需要的正是 kthena#2148 里说的「established」记录。
- 同一个 kind 只有一份健康脚本。G-c 的单元也是 ModelServing，这份定义会同时作用在控制面上所有 ModelServing 模板上；Lua 能读对象的标签，可以按标签分支。
- 节点自动扩容期间组也是「不可调度」，容忍时间要大于扩容时间。和方案 M 的规则 3 相同。

### 4.4 WorkloadRebalancer 和 `rescheduleTriggeredAt`

WorkloadRebalancer 只做一件事：把绑定的 `rescheduleTriggeredAt` 设成自己的创建时间（`pkg/controllers/workloadrebalancer/workloadrebalancer_controller.go:189-240`）。
调度器照常过滤、打分、选择；「全新分配」只影响副本分配算法（`pkg/scheduler/core/assignment.go:118`），对单元绑定没有作用，原集群照样有 100 分。

【实测】`Divided` 下：原集群估算为 0、别处有空，搬走（S4）；哪里都没有空，失败并留在原集群（S4b）；原集群的汇总还显示有一组的空间，留在原集群（S4c）。
前两条正好是「有去处才移，没去处留在原地排队」。但有两个缺陷：

- S4c：卡住的原因估算器看不到时（队列配额、拓扑、PVC、没写进 `nodeClaim` 的约束），不会移走。
- S4b 失败后 `rescheduleTriggeredAt` 仍然晚于上次调度时间，调度器会一直重试。如果期间原集群腾出了资源、单元已经在原集群跑起来，
  下一次重试看到的是「原集群没有额外一整组的空间、别的集群有」，会把**已经在运行的单元**搬走。【推断，由 S3 和 `pkg/util/binding.go:147-156` 推出，未验证】
  所以它不能当成长期挂着的迁移意图，除非有控制器在单元恢复后撤销它。

### 4.5 如果要「单元的 Pod 不可调度超过 T 就移走」，最少要改什么

三种做法的比较是【推断】，依据是 4.2–4.4。

| 做法 | 改 Karmada 吗 | 得到什么 | 缺什么 |
| --- | --- | --- | --- |
| (a) 应用故障转移 + 4.3 的健康定义 + `tolerationSeconds: T` + `purgeMode: Directly` + `propagateDeps: true` | 不改 | 触发条件和整体移动 | 有去处才移；不回到刚失败的集群；计时不落盘；health 语义被占用 |
| (b) 上游改动：驱逐后的那次调度以「没有集群」结束时，把被驱逐的集群放回去，而不是让绑定空着。做成 `ApplicationFailoverBehavior` 里的可选项 | 改调度器核心和策略 API，要走提案 | 补上「有去处才移」 | 和上游现有讨论（#6233 独立的初始化容忍时间、#7662 只重调度不能运行的部分）有交叠，需要先对齐【未验证】 |
| (c) kthena 侧的控制器，对 karmada-apiserver 工作：从聚合状态读 `unschedulableSince`；用集群汇总公式或 estimator 预检别的集群；预检通过后给绑定加一条 `Directly` 的驱逐任务（和 (a) 写的是同一个字段），或者定向改 `spec.clusters` | 不改 | 全部语义，包括退避和失败集群记忆；预检可以用节点级的组件集估算，比方案 M 规则 6 的汇总公式准 | 第三方控制器写 `ResourceBinding.spec`，和方案 M 的后备「独立控制器」是同一类做法 |

对比方案 M：M 的改动落在可选组件 karmada-descheduler 里，可以只替换一个镜像；G 对应的上游改动落在调度器和 controller-manager 的核心路径上，更难带分支，也更难合入。
所以 G 下实际可行的是 (a) 起步、(c) 补齐。

## 5. Q4：010 的 ServingGroup CR 能不能原样做单元（G-b）

不能。逐项：

| 问题 | 事实 | 位置 |
| --- | --- | --- |
| `GetComponents` 能算出什么 | spec 里只有 `expectedPods` 和每个 role 的 `replicas`，没有 Pod 模板。能给出组件名和副本数，给不出资源请求和 `nodeClaim`。汇总估算退化成只按 Pod 数限制，节点级估算每个 Pod 按零请求放 | `kthena-010:pkg/apis/workload/v1alpha1/servinggroup_resource_types.go:25-28,65-68,141-169` |
| Lua 能不能去读 ControllerRevision | 不能。Lua 只看得到传入的那一个对象，`kube` 库只有 4 个纯函数。要读别的对象只能用 webhook 解释器（一个要部署、要高可用的 HTTP 服务；pull 模式下成员侧的操作还要求 agent 能访问它） | `karmada:pkg/resourceinterpreter/customized/declarative/luavm/kube.go:59-62` |
| 把 ControllerRevision 当依赖下发 | `GetDependencies` 可以列出任意 kind（按 `apiVersion/kind` 经 RESTMapper 解析），名字是 `<ModelServing 名>-<revision>`，可以由标签和 spec 里的三个 revision 字段拼出来。但下发时 ownerReferences 被去掉 | `karmada:pkg/dependenciesdistributor/dependencies_distributor.go:411`；`pkg/resourceinterpreter/default/native/prune/prune.go:77` |
| 成员侧认不认下发来的 ControllerRevision | 不认。解析 revision 时要求 ControllerRevision 的 controller owner 是 UID 相同的父 ModelServing，否则当成「无法解析」 | `kthena-010:pkg/model-serving-controller/workload/history.go:162-177` |
| 成员侧认不认下发来的 ServingGroup | 不认。组内控制器先找父对象：没有指向 ModelServing 的 controller ownerReference，记一条 `OwnerNotFound` 事件，「the group is left alone」，一个 Pod 都不建。有 ownerReference 也要求同集群缓存里有同名、同 UID、没在删除的 ModelServing | `kthena-010:pkg/model-serving-controller/sgcontroller/reconcile.go:107-113,292-308` |
| 同时把父 ModelServing 也下发过去行不行 | 成员里的 UID 和控制面的不同，ownerReference 也带不过去；而且成员的 ModelServing 级控制器会把它当成自己的工作负载，再建一套组 | `karmada:pkg/resourceinterpreter/default/native/prune/prune.go:47-81`；`kthena-010:pkg/model-serving-controller/manager/manager.go:75-175` |
| finalizers | 下发时去掉；成员上已有的保留 | `prune.go:79`；`karmada:pkg/util/objectwatcher/objectwatcher.go:126-136` |
| status | 下发时去掉，成员自己写。整份反射的话，`roleInstances[]` 账本和 `observedPods` 摘要每次 Pod 变化都会经过 Karmada 三跳 | `prune.go:81` |
| `modelserving.volcano.sh/created-revision` 注解 | 注解能带过去，但语义变了：单元换集群后，成员上的对象是新建的、Pod 全是新的，注解却还是最初创建时的 revision。带 role partition 的滚动里，组内控制器会按 baseline revision 建实例，于是新集群还得有 baseline 的模板 | `kthena-010:pkg/apis/workload/v1alpha1/servinggroup_resource_types.go:48-56`【推断】 |
| `sourceGeneration` | 是控制面 ModelServing 的 generation，原样带过去，成员侧没有可以比较的对象 | 同上 `:59-63` |
| 010 的 webhook | 没有针对 ServingGroup 资源的准入 webhook，不会拦下发。Pod 驱逐 webhook 要求 Pod 的 ServingGroup 被 UID 相同的 ModelServing 控制，成员里不成立，`evictionStrategy` 的保护失效 | `kthena-010:pkg/model-serving-controller/webhook/eviction_handler.go:779-810`；`kthena-010:charts/kthena/charts/workload/templates/kthena-controller-manager/component/validating-webhook.yaml` |
| 删除 | Karmada 删成员对象用的是后台级联。控制面上的对象删完不代表成员的 Pod 已经没了 | `karmada:pkg/util/objectwatcher/objectwatcher.go:248` |

结论：G-b 的每一个信任锚（父对象 UID、ControllerRevision 的 owner、Pod 的 owner 链）都假设父子在同一个 apiserver 里。
要让它能被下发，spec 必须自包含（Pod 模板、插件、调度器名都在 spec 里），组内控制器不能再找父对象。那就是 G-a，是重新设计，不是复用。

## 6. Q5：010 的 ModelServing 级控制器能不能跑在 Karmada 控制面（G-a）

### 6.1 010 的两个控制器各读写什么

【源码 `kthena-010:pkg/model-serving-controller/`】

| | ModelServing 级（`mscontroller`） | 组内（`sgcontroller`） |
| --- | --- | --- |
| 读 | ModelServing、ServingGroup（缓存，写之前直读）、ControllerRevision、**Pod**（缓存 `observe.go:149,260`、`revision.go:105`；直读 `adoption.go:101`、`rollback.go:172,225`）、Service（lister，交给插件 `reconcile.go:169`） | ServingGroup、**父 ModelServing**（`reconcile.go:303,325`）、ControllerRevision（`round.go:141`）、Pod、Service、PodGroup |
| 写 | ServingGroup 的创建、更新、删除；ModelServing 的 status 和注解；ControllerRevision；事件。回退路径里还改 Pod 和 Service 的 owner、删 PodGroup（`rollback.go:196,214,250`） | Pod、PodGroup、Service（插件）、ServingGroup 的 status |
| 进程内共享 | 同一个 Manager、同一组 informer 和缓存（`manager/manager.go:139`）；一把 `rounds` 读写锁（`manager.go:154`，`mscontroller/rollback.go:100`，`sgcontroller/reconcile.go:83-84`） | 同左 |

包注释说 ModelServing 级控制器「never touches a Pod」（`mscontroller/controller.go:23-25`），指的是不写 Pod；它读 Pod。

### 6.2 具体的阻碍

「阻碍」一栏是【源码】；「后果」和「要改成什么」是【推断】，没有把 010 的控制器真的指向 karmada-apiserver 跑过。

| # | 阻碍 | 后果 | 要改成什么 |
| --- | --- | --- | --- |
| B1 | `Fresh`：比较组 status 里的 `observedPods` 摘要和**自己 Pod 缓存**算出的摘要（`mscontroller/observe.go:165`）。控制面没有 Pod，成员的组只要有 Pod 就永远不等 | 「确认 Ready」要求 `Observed && Fresh && Ready`（`grouplevel/classify.go:25-31`），所以没有任何组能被确认；每轮都以 `ErrNotSettled` 结束（`mscontroller/reconcile.go:150-155`）。滚动、缩容预算全部卡住 | 去掉这道校验，改用经 Karmada 传回的「已观察到的代数」。它原本防的是两个控制器在同一个 Pod 缓存上看到不同时刻；拆开后父控制器不再看 Pod，这个风险换成了 6.3 的陈旧度问题 |
| B2 | 其余来自 Pod 缓存的事实：`Memberless`、`PodsReady`、`HasPods`、`DeletionCost`、无主 Pod 的恢复（`observe.go:167-190,260-303`） | 缩容顺序不再看 Pod 是否就绪和 deletion cost；surge 组的回收判断（只看 `HasPods`，`grouplevel/plan.go:631`）出错；「对象丢了、Pod 还在」的恢复做不了 | 这些事实要由成员写进组的 status 再传回来 |
| B3 | `Observed`：`status.observedGeneration >= metadata.generation`（`observe.go:164`）。控制面模板的 generation 和成员对象的 generation 是两个计数器 | 比较没有意义 | 用 Karmada 第三方定制的惯例：聚合脚本只在「成员已观察到自己的最新代，且成员对象上的模板代数注解等于模板当前代数」时才推进。方案 M 的解释器已经这样做 |
| B4 | 成员侧没有父对象和 ControllerRevision（第 5 节） | 组内控制器什么都不做 | spec 自包含。带 role partition 的滚动需要 baseline 和 target 两份模板，spec 要么带两份，要么改变这种滚动在多集群下的语义 |
| B5 | 两个控制器共用进程、缓存和锁；接管旧版本工作负载、回退这两条路径直接改 Pod 的 owner | 不能分开部署 | 拆成两个二进制或两种运行模式；多集群模式下去掉接管和回退 |
| B6 | 父控制器每轮调用插件的 `OnModelServingSync`，传进去的是 kube client 和 Service lister | 插件会在 karmada-apiserver 上建 Service | 控制面模式下不调用，或只在成员调用 |
| B7 | 组名保留序号，替换一个组会复用名字；010 靠前台删除保证「对象没了 Pod 也没了」 | 控制面的对象删除后，成员上的旧 Pod 可能还在终止（后台级联）；Work 带 finalizer，成员失联时旧 Work 删不掉，新单元如果调度到同一个集群会撞上同名的旧 Work【推断，未验证】 | 父控制器等旧 Work 消失再建同名单元，或者改用不复用的名字（010 的上一版设计是随机后缀，后来为了保留序号改掉了：`kthena/docs/proposal/modelserving-servinggroup-cr/README.md` 开头） |
| B8 | 新增的中间状态：单元没被调度（没有集群放得下）；已调度但 Work 应用失败；成员失联、状态停在旧值 | 010 的分类里没有这些状态。失联集群上最后一次上报是 Ready 的组会一直被当成 Ready | 父控制器要读绑定和 Work 的条件，并给状态加时效 |
| B9 | 驱逐保护、成员侧自动扩缩容都以「成员里有 ModelServing」为前提 | 成员里没有 ModelServing | 驱逐保护只能做到单元内；组数的自动扩缩容要搬到控制面（第 8 节 N9） |

### 6.3 父控制器依赖组的哪些状态，能容忍多旧

「用在哪」是【源码】，「陈旧的后果」是【推断】。

| 字段 | 用在哪 | 陈旧的后果 |
| --- | --- | --- |
| `Ready` 条件 | 预算：`Floor = N − maxUnavailable`，只在已确认 Ready 的组数高于下限时才删下一个（`grouplevel/types.go:216-240`） | 一个组在最近几秒里变成不 Ready 而控制面还不知道：同时不可用的组数短暂超过 `maxUnavailable`。成员失联时没有上限 |
| `Failed`、`Drained` 条件 | 删除失败的组（`grouplevel/plan.go:272-280`） | 只会晚，不会错 |
| `RecreateRequired` 条件、`status.revision` | 判断组是否在目标 revision、能不能原地更新 | 晚 |
| `status.pods` | 是否「没有成员」，没有成员的组可以直接改 revision（`plan.go:339-378`） | 误判成没有成员时会给一个其实已经有 Pod 的组改 revision，随后组内控制器要处理 revision 和 Pod 不一致。原设计用 Pod 缓存交叉确认，控制面做不到 |
| `status.observedGeneration` | 上表 B3 | — |

010 在写之前会把 ModelServing 和它的组从 apiserver 直读一遍再重新计划（`mscontroller/reconcile.go:110-129,182-229`）。
在控制面上，直读读到的是 Karmada 写回的那份拷贝，本身就落后一条状态管道（成员 → Work → 绑定 → 模板，三次写，各自受限速影响；
controller-manager 默认 `--kube-api-qps=40`、各类并发 5，`cmd/controller-manager/app/options/options.go:199-214`）。
要读到成员的现状只能经 Karmada 的集群代理直读成员，这又把父控制器和成员的可达性绑在一起。【推断】

### 6.4 在这条边界上拆开，对 #2148 列的 Plan A 缺点的影响

本小节全部是【推断】。

| Plan A 的缺点（#2148 原文） | 影响 | 理由 |
| --- | --- | --- |
| 只解决组这一层，Role 实例仍然要账本 | 不变 | 账本还在组的 status 里，由成员写。单元换集群就是新的一代，账本不用带走 |
| 运行中 Pod 的 owner 迁移 | 对新建的多集群部署：没有。对已有的单集群用户：不变；如果单集群继续用现在的控制器，kthena 要长期维护两套控制器 | — |
| 两个控制器之间的滞后；「组对象先于 Pod 存在」 | **变差** | 滞后从进程内的毫秒级变成跨集群的秒级到无上限；中间状态多出三种（B8）；`Fresh` 校验和共享锁都没法保留 |
| 跨组预算不是原子的 | 机制不变，窗口变大。但相对方案 M 是净收益：M 根本没有全局预算 | — |
| API 写入量 2.35 倍 | **变差** | 见下 |
| 同名重建依赖垃圾回收时序 | 变差 | B7 |

写入量【推断，输入是 #2148 里的单集群 e2e 实测，没有多集群实测】：那一轮里 164 个 ServingGroup 被更新 1,573 次，其中 status 写 1,427 次（360 个 Pod，约每个 Pod 4 次）；
51 个 ModelServing 被更新 668 次。整份 status 反射时，成员上的每次 status 变化在控制面上至少是 3 次写（Work、绑定、模板），外加事件：

| | 控制面上的 status 相关写入 |
| --- | --- |
| 方案 M（成员 ModelServing 的 status 经三跳传回）| 约 668 × 3 ≈ 2,000。前提是 upstream main 的 ModelServing 写入量和 010 的同量级，这一点没有基线数据 |
| G-a（组的 status 经三跳传回，ModelServing 的 status 在控制面直接算）| 约 1,427 × 3 + 668 ≈ 4,950 |

另外每个单元的创建、调度、删除各自是几次控制面写入（模板、绑定、调度结果、条件、Work），方案 M 里组的增减不产生任何 Karmada 对象。
只反射父控制器需要的几个字段、不反射账本和摘要，可以把第一项压下去很多，但那样 B1、B2 需要的事实就要另想办法。

反过来，多集群给 Plan A 加了一条 #2148 里没有的好处：只有独立对象才能被单独放置（1.3 第 10 点）。

## 7. G-c：不新增 CRD，单元是 `replicas: 1` 的 ModelServing

任务说明里的 G-c 实际上有两种做法，差别在解释器（下表是【源码 + 推断】，没有在 Kind 上跑过）：

| | G-c1：组件解释器 | G-c0：方案 M 的解释器原样 |
| --- | --- | --- |
| 解释器 | `componentResource`（每个 role 的 entry、worker 各一个组件，带 `nodeClaim`）；没有 `replicaRevision` | `replicaResource`（整组求和）+ `replicaRevision` |
| 策略 | spread 1..1 + `Divided/Aggregated` | `Divided/Aggregated`，副本数就是 1 |
| 门控 | `MultiplePodTemplatesScheduling`，批量创建时还要 `SchedulingOvercommitProtection` | 不需要 |
| 估算 | 节点级组件集 | 和方案 M 相同（整组求和；多节点的组不能配 scheduler-estimator） |
| 能否和方案 M 的普通用法共存于同一个控制面 | 不能。同一个 kind 只能有一份解释器；开了门控又有 `componentResource` 时不再调用 `GetReplicas`，这时再留着 `replicaRevision` 就是第一轮 Kind 的 C3 | 能 |
| 应用故障转移能否移动单个卡住的单元 | 能 | 能。驱逐后 `spec.clusters` 为空，各集群副本之和 0 不等于 1，触发调度（`karmada:pkg/util/binding.go:63-66`）【推断，未验证】 |

两种做法共有的性质【源码 + 推断】：

- **成员集群跑的是没有改动的 kthena。** 组的重建、role 级滚动、原地更新都在本地完成；控制面不可用时不受影响。这一点 G-a 做不到（1.3 第 9 点）。
- `unschedulableReplicas` 只会是 0 或 1，`unschedulableSince` 就是这一组自己的时间，不再是「最老一组的时间」。方案 M 的规则 7 不需要了。
- **数量、滚动、缩容选哪一个，都要有东西在控制面上管**：N 个对象谁来建、升级时一次动几个、缩容删哪个。可以是用户的工具（Helm、Argo CD 的分批），
  也可以是一个很薄的父控制器。父控制器只读每个单元聚合回来的 ModelServing status（`availableReplicas`、`updatedReplicas`、`unschedulableReplicas`、代数），
  比 G-a 的父控制器要读的东西少得多，但 6.3 说的陈旧度问题一样存在。
- 成员按 `replicas: 1` 校验单元的 `rolloutStrategy`（`kthena:pkg/model-serving-controller/webhook/validator.go:228-259`）。
  父对象上的组级设置（整数 `maxUnavailable: 2`、`partition: 8`）不能照抄进单元，由父控制器自己执行。
- 名字会变：单元叫 `<名字>-<序号>`，里面唯一的组叫 `<名字>-<序号>-0`，Pod 名多一段 `-0`（`kthena:pkg/model-serving-controller/utils/utils.go:77-79`）。
  `modelserving.volcano.sh/name` 标签每个单元不同；`ModelServer.workloadSelector.matchLabels` 要改用模板里的公共标签
  （`kthena:pkg/apis/networking/v1alpha1/modelserver_types.go:85-95`）。
- 每个单元在成员里各有一套 ControllerRevision 历史和一个 ModelServing status。每个单元的 status 写入量没有实测【未验证】。

G-c1 是「G-a 去掉新 CRD」：Karmada 一侧的行为和 G-a 完全相同（第 2–4 节全部适用），kthena 一侧零改动。
G-c0 是「把方案 M 切成 1 副本的片」：没有估算上的收益，但拿到了单元级的移动、滚动和缩容，不依赖任何 alpha 门控。

## 8. Q6：G 对方案 M 八条发现的影响，以及 G 带来的新问题

### 8.1 八条发现

| # | 方案 M 的发现 | G 下 | 说明 |
| --- | --- | --- | --- |
| 1 | 整组求和使节点级估算为 0；多节点的组所在集群不能配 scheduler-estimator；汇总估算看不到碎片、nodeSelector、拓扑、队列配额 | **修掉大部分**（G-c0 除外） | 碎片和 nodeSelector 看得到了，前提是所有候选集群都有 estimator（2.4）。拓扑、队列配额、gang 仍然看不到；First Fit 会低估。另外修掉了「放不全就一个不放」：放得下的单元照常放，只有放不下的那几个等着【源码 + 实测 N1–N3、O1–O3、b2b】 |
| 2 | 默认资源模型下 GPU 请求估算为 0 | **保留** | 组件路径同样是 0【实测 M2】。规避办法相同 |
| 3 | 已有模板后再装解释器，组被清空或重启 | **修掉**（待验证） | 单元的 kind 没有 `replicaRevision`，不会被改写成 0（2.7）。但不装解释器时的放置不看容量，仍然要先装【推断，未验证】 |
| 4 | `componentResource` + `replicaRevision` + 门控把成员副本数写成 0 | **换了形态** | G-a：单元是另一个 kind，不存在这个组合。G-c1：ModelServing 的解释器必须去掉 `replicaRevision`，于是这个控制面不能再用方案 M；有人把它加回去就会重现【源码 + 第一轮 Kind：C3】 |
| 5 | 成员按自己分到的份额执行 `rolloutStrategy`；合法模板被拒；各集群同时滚动 | **修掉**，但逻辑要有人实现 | G-a：父控制器按全局副本数执行预算、partition、序号；surge 组是一个新单元，由 Karmada 放到有空的集群。G-c：由父控制器或用户工具执行。role 级滚动本来就是组内的事，不受影响【推断】 |
| 6 | 跨集群缩容不看就绪情况 | **修掉** | 删哪个单元由父控制器（或用户）决定，可以先删调度不上的【推断】 |
| 7 | 降下来的副本没有去处时，5 分钟不属于任何集群，然后回到原集群 | **保留，略差** | 应用故障转移没有预检，循环周期约为容忍时间加 10 分钟（4.2）。用 4.5 的 (c) 可以修，而且预检能用节点级估算【源码 + 实测 S5b、E3、E6；循环是推断】 |
| 8 | 成员侧自动扩缩容不能和 Divided 一起用 | **保留，换了边界** | 组的数量只能在控制面改，成员建不出单元。单元内 role 副本数的成员侧扩缩容可以用 `Retain` 保留；但经模板改 role 副本数会触发 2.3 的重新调度【源码 + 实测 S3；`Retain` 部分是推断】 |

### 8.2 G 带来的新问题

| # | 问题 | 依据 |
| --- | --- | --- |
| N1 | 依赖两个 alpha 门控：`MultiplePodTemplatesScheduling`（scheduler、controller-manager、webhook）和 `SchedulingOvercommitProtection`（scheduler、每个 estimator）。前者的扩缩容重调度语义上游还在做（#7895） | 【源码】2.1、3.2 |
| N2 | 已运行的单元不粘：role 副本数变化、策略 placement 变化、显式重调度，都可能把运行中的单元搬走（`Divided`），或者让策略失去容量约束（`Duplicated`） | 【实测 S3、S2d】 |
| N3 | 批量创建时的扎堆过量分配；保护是内存态，TTL 固定 5 分钟，和 health 的定义互相牵制 | 【实测 b2b、P4】3.3 |
| N4 | 不写 spread 约束时单元被复制到所有集群；不装解释器时不看容量 | 【实测 X1、X2】 |
| N5 | 对象数量：每个单元一个模板、一个绑定、一个 Work，外加各自的状态管道和事件。方案 M 是每个 ModelServing 一个绑定、每个集群一个 Work | 【源码】 |
| N6 | 状态写入量：6.4 的估算，G-a 约是方案 M 的 2.5 倍 | 【推断】 |
| N7 | 调度器吞吐：单 worker，每个单元一轮对所有候选集群的 gRPC 估算，外加两次绑定写入，受 `--kube-api-qps=40` 限制。几百个单元的首次放置在十秒到几十秒量级 | 【源码 + 推断】`karmada:cmd/scheduler/app/options/options.go:150-151` |
| N8 | 失去一次拆分里的整体装箱：没有 `Aggregated` 的「尽量少的集群」，没有静态权重、动态权重。要按比例分布只能由父控制器给单元钉集群，等于在 kthena 里重做一遍拆分 | 【源码】2.3 |
| N9 | 组数的自动扩缩容：FederatedHPA 要先找到目标对象的绑定（`karmada:pkg/controllers/federatedhpa/federatedhpa_controller.go:241-256`），父对象不下发，没有绑定。CronFederatedHPA 直接写 scale 子资源，可以用（`pkg/controllers/cronfederatedhpa/cronfederatedhpa_job.go:171-210`）。kthena autoscaler 靠本集群的 Pod 抓指标，搬不到控制面 | 【源码】 |
| N10 | 应用故障转移的误触发和 health 语义被占用（4.3） | 【源码 + 推断】 |
| N11 | G-a：控制面不可用或成员失联时，需要整组重建的组起不来。G-c、方案 M 不受影响 | 【推断】6.2 |
| N12 | G-a：`evictionStrategy` 的跨组保护在成员里无法计算。G-c：只能保护到单元内 | 【源码】第 5 节 |

## 9. Q7：比较、建议、分期

### 9.1 比较表

本表是前面各节的汇总，每一格的依据和标签见对应小节，这里不重复标注。

| 维度 | M（现方案） | G-a（自包含的 ServingGroup CR + 控制面父控制器） | G-b（010 的 CR 原样） | G-c（1 副本 ModelServing） |
| --- | --- | --- | --- | --- |
| 估算准确度 | 汇总估算，看不到碎片和 nodeSelector；多节点的组不能配 estimator；放不全就一个不放 | 节点级组件集，认 `nodeClaim`；会低估不会高估（资源维度）；能部分放置 | 没有资源信息，等于不估算 | c1 同 G-a；c0 同 M |
| 重调度 | 改 descheduler：从状态读不可调度数，有去处才降。改动在可选组件里 | 应用故障转移可以零改动起步，但会循环；要补齐得写 kthena 侧控制器 | — | 同 G-a |
| 滚动语义 | 各集群按自己的份额各自滚动；没有全局预算、全局 partition、全局序号 | 全局预算、partition、序号、跨集群 surge；预算基于滞后的状态 | — | 父控制器或用户工具实现后同 G-a；没有的话靠人工分批 |
| 缩容选择 | 跨集群不看就绪 | 父控制器选，可以先删调度不上的 | — | 同 G-a |
| 自动扩缩容 | 两种模式二选一；控制面模式可以用 FederatedHPA | 组数只能在控制面改，FederatedHPA 不可用；单元内 role 的成员侧扩缩容可以保留 | — | 同 G-a |
| 状态和可观测性 | 控制面一行汇总；每集群明细在绑定里；被拒的集群从汇总里消失 | 每个单元一行，健康、所在集群、不可调度时间都是单元自己的；父对象的汇总由 kthena 自己算 | — | 同 G-a，但没有父对象时没有汇总 |
| 失效模式 | 5.4 的校验拒绝；重调度的状态新鲜度；没有去处的循环（规则 6 可修） | 单元不粘；扎堆；循环；控制面依赖（组级重建）；父子状态滞后 | 成员上什么都不建 | 单元不粘、扎堆、循环（c1）；成员本地自愈完整 |
| 对 Karmada 的依赖和成熟度 | 第 1 阶段只用稳定能力；第 2 阶段要改 descheduler | 两个 alpha 门控；#7895 两项未完成；应用故障转移（稳定） | 同 G-a，外加 webhook 解释器 | c1 同 G-a；c0 不依赖任何 alpha |
| kthena 架构成本 | 文档和一份 YAML | 很高：新 CRD（重新设计）、拆开 010 的两个控制器、替换 Pod 来源的事实、控制面部署、单集群和多集群两套路径 | 看似为零，实际不可用 | 控制器零改动；可选的薄父控制器 |
| 上游对齐 | Karmada：#8104 里 maintainer 质疑「整组当一个副本」；状态来源的 descheduler 不确定能否合入。kthena：不涉及 #2148 | Karmada：正是多组件调度的设计形状（#5115），回应了 #8104 的两条质疑。kthena：依赖 #2148 选 Plan A，而且要求 CR 自包含 | — | Karmada：同 G-a。kthena：不涉及 #2148 |
| 迁移 | 接管已有 ModelServing 有现成路径（`conflictResolution: Overwrite`） | 已有工作负载无法原地转成单元；单集群用户另有 Pod owner 迁移 | — | 已有的多副本 ModelServing 要拆成 N 个对象，Pod 名和标签会变，等于重建 |

上游对齐这一行里的事实依据：#8104 里 maintainer 2026-11-24 的评论指出两点：整组求和会被按单节点检查；descheduler 没有 workload 自定义的重调度单位。
G 对这两点的回答是「不需要新概念」：单元就是一个多组件工作负载，重调度单位就是绑定。#5115 的用户故事是「调度到恰好一个成员集群」；
#7895 是它的第五阶段，open，2026-11-26 更新。#8030（另一位用户，2026-11-06）要的也是「多角色推理整体放进一个集群」上的行为。
上游没有人说过「kthena 应该新增 ServingGroup CR」，也没有人承诺「把多个完整组件集拆到不同集群」。（上游 issue 原文，2026-11-28 只读查询）

### 9.2 G 是不是更好

**形状更好，载体不必是新 CRD，时机还不到。**

- 更好的地方：估算（修掉方案 M 最硬的那条限制，并且去掉了要向上游争取的第 3 阶段）；部分放置；单元级的滚动、缩容、移动和时间戳；和 Karmada 的多组件路线一致。
- 不更好的地方：站在两个 alpha 门控上，其中一个的重调度语义没做完，直接后果是运行中的单元会被搬走（N2）；
  「卡住就移走」在 G 下并没有变简单，循环还在，补齐它仍然要写控制器；对象和写入量按单元数线性增长。
- G-a 相对 G-c 多出来的只有 #2148 讨论的那些单集群收益（持久状态、组的 UID），代价是 6.2 的九处改动、控制面依赖（N11）和两套控制器。
  kthena#2148 对 CR 路线还没有结论；多集群这个新用例不足以单独决定它，因为 G-c 用现有的 ModelServing 就能得到同样的 Karmada 侧行为。

满足下面条件时 G（先 G-c）应该成为主路径：

1. 动态放置（按容量）是主要用法，而不是 Duplicated 或静态权重；
2. 成员集群有节点碎片或按 role 的 nodeSelector，汇总估算经常把组放到放不下的集群；
3. 能接受在 Karmada 上开 alpha 门控，或者上游的 #7895 两项已经合入；
4. 每个 ModelServing 的组数在几十的量级；
5. 需要全局的滚动预算或按就绪情况缩容。

只需要 Duplicated 和静态权重时，方案 M 更简单，也足够。

如果要的只是「卡住的组能换集群」这一个能力，改动最小的做法既不是 G-a 也不是改 descheduler，而是 G-c0 加应用故障转移：
不改 Karmada，不改 kthena，不开 alpha 门控，还能和方案 M 的普通用法共存。代价是没有估算上的收益，并且带着 4.2 的循环。【推断，未验证，见 K17】

### 9.3 建议的分期

| 阶段 | 做什么 | 说明 |
| --- | --- | --- |
| 1（基本不变） | 方案 M 的解释器和使用指南 | Duplicated、静态权重、状态汇总都需要它。把 1.4 的更正、1.3 第 3 点写进指南。**建议在 1b 有结论之前，暂缓把这份解释器提交为 Karmada 的内置定制**：同一个 kind 只能有一种解释方式，内置之后「ModelServing 按副本拆分」就成了 Karmada 发行版的默认行为，以后要换成组件解释（G-c1）是破坏性变更（用户自装的定制可以覆盖内置的，但默认值已经定了） |
| 1b（新增，零代码） | 在 Kind 上按 9.4 刻画 G-c1 和 G-c0，和方案 M 的 C2c、C4、C5 对照 | 协调者正在做。结果决定第 2 阶段的方向 |
| 2（二选一，看 1b 的结果） | (i) 现方案：改 karmada-descheduler；(ii) 单元模式：G-c + 应用故障转移起步，再加 4.5 (c) 的控制器 | (i) 的风险是上游不接受状态来源；(ii) 的风险是 N1、N2、循环。两条路都要写一个「有去处才移」的逻辑，区别是写在 Karmada 的可选组件里还是 kthena 的控制面组件里 |
| 3（替换原第 3 阶段） | 如果 1b 表明单元模式成立：做一个薄的父控制器（数量、滚动预算、缩容选择、卡住单元的移动），单元仍然是 ModelServing。原来的「向上游提按组件集拆分副本」降级为备选 | 原第 3 阶段要改调度器语义，和 PR #8084 的方向相反（第一轮报告 3.6）；单元模式不需要它 |
| 条件触发 | #2148 如果选了 Plan A：ServingGroup CR 从一开始按「可被下发」设计（spec 自包含；不靠父对象 UID 做信任锚；父控制器不读 Pod），届时单元可以从 ModelServing 换成 ServingGroup | 现在就可以把这条约束作为多集群的输入提给 #2148（只起草，发布需要授权） |

同一个控制面上，方案 M 和 G-c1 不能同时用于 ModelServing（1.3 第 8 点）。第 2 阶段选 (ii) 时要么全部切到单元模式，要么用 G-c0。

### 9.4 建议在 Kind 上跑的 G 场景

环境同第一轮，另外：karmada-scheduler、karmada-controller-manager、karmada-webhook 都开 `MultiplePodTemplatesScheduling`；每个成员都部署 scheduler-estimator；清空资源模型。
单元用 `replicas: 1` 的 ModelServing（G-c1），解释器只有 `componentResource`、状态和健康，没有 `replicaRevision`。

| # | 场景 | 要确认的 |
| --- | --- | --- |
| K1 | 3 个单元（每组 4 × 1 GPU），m1 是 2 节点 × 2 GPU | m1 的节点级估算是 1（方案 M 下是 0）；成员的 `spec.replicas` 保持 1；`spec.clusters[].components` 有记录 |
| K2 | 每组 3 GPU + 1 GPU | m1 估算 0，单元不会被放到 m1（方案 M 的 C4 里被放进去并卡住） |
| K3 | 组件顺序：小 Pod 在前和大 Pod 在前各一次 | 复现 O1、O2 |
| K4 | role 带 nodeSelector，写进 `nodeClaim` | 节点级估算认；去掉一个成员的 estimator 后退回汇总估算（1.3 第 3 点） |
| K5 | 默认资源模型不清空 | 估算 0，调度失败 |
| K6 | 一次创建 5 个单元，`SchedulingOvercommitProtection` 关和开各一次 | 复现 3.1 的表；开的时候观察记账何时释放、有没有重复计算 |
| K7 | 运行中的单元，模板里 role 副本数加 1，所在集群没有额外一整组的空间 | `Divided`：单元被搬走，或 `Scheduled=False` 但成员照样扩容；`Duplicated`：不动 |
| K8 | 卡住的单元 + 应用故障转移（健康 = 没有不可调度的 Pod，`tolerationSeconds: 60`，`Directly`，`propagateDeps: true`），别处有空 | 被移走；正在启动（已调度、未就绪）的单元不被移走 |
| K9 | 同 K8，别处都满 | 单元离开原集群；原集群被排除多久；是否回到原集群；循环周期 |
| K10 | 对卡住的单元建 WorkloadRebalancer | 复现 S4、S4b、S4c；S4b 之后让原集群腾出资源，看运行起来的单元会不会被搬走（4.4） |
| K11 | 不写 spread 约束；不装解释器 | 复现 X2、X1 |
| K12 | 缩容：删掉调度不上的那个单元 | Running 的单元 Pod UID 不变 |
| K13 | 逐个单元更新镜像 | 不触发重新调度；每个单元在原集群内重建 |
| K14 | 在 karmada-apiserver 上 watch Work、绑定、模板 | 每个单元一生的写入次数，和方案 M 的同规模对照（6.4 的估算目前没有实测） |
| K15 | 成员装 Volcano，gang 放不下的单元等待超过 5 分钟 | 记账过期后节点级估算是否把这个集群当成空的；汇总估算的 `allocating` 是否挡得住 |
| K16 | 在已有方案 M 绑定的控制面上把解释器换成组件版 | 已有的 ModelServing 会怎样（1.3 第 8 点） |
| K17 | G-c0：1 副本、方案 M 的解释器、应用故障转移 | 卡住的单元能否被移走（第 7 节的推断） |

### 9.5 什么证据会改变这个建议

| 证据 | 建议往哪边动 |
| --- | --- |
| 上游在 #8104 里明确不接受 descheduler 的状态来源，或者明确建议「一个单元一个对象，用多组件调度」 | 第 2 阶段选 (ii) |
| #7895 的两个未完成项合入，`MultiplePodTemplatesScheduling` 升 beta | G-c1 可以进生产建议 |
| K1、K2 显示节点级组件集估算在真实的碎片集群上明显减少卡住的组；K7、K9 的问题有可接受的规避 | 第 2 阶段选 (ii) |
| K14 显示写入量或调度延迟在目标规模下不可接受；或者每个 ModelServing 有上百个组 | 留在方案 M |
| 用户的主要用法是 Duplicated 或静态权重 | 留在方案 M，不需要 G |
| 不能在生产 Karmada 上开 alpha 门控 | 方案 M，或 G-c0 |
| #2148 选 Plan A 并接受「spec 自包含」 | 单元的载体可以从 ModelServing 换成 ServingGroup；G-a 重新评估，重点是 N11 |
| K9 显示循环不出现（例如原集群估算在驱逐任务结束时稳定为 0） | 应用故障转移可以单独用，不需要 4.5 (c) |

## 10. 没有验证的

| # | 内容 | 怎么确认 |
| --- | --- | --- |
| 1 | 第 2–4 节的全部【实测】都是包级的：没有经过真实的 detector、webhook、gRPC、状态管道 | K1–K11 |
| 2 | 应用故障转移的循环周期和「回到原集群」的条件 | K9 |
| 3 | 失败的显式重调度会不会把后来跑起来的单元搬走 | K10 |
| 4 | 不装解释器时放置的单元，装上之后是否保持不动、不被清空 | K11 之后装解释器 |
| 5 | 优雅驱逐期间两个聚合项对状态聚合脚本的影响 | K8 用 `Gracefully` 再跑一次 |
| 6 | G 下控制面的写入量和调度延迟 | K14 |
| 7 | Volcano gang 下记账过期后的行为 | K15 |
| 8 | 同名单元在旧 Work 未删完时重建 | 让成员失联后删除并重建单元 |
| 9 | G-c0 下应用故障转移对 1 副本绑定的行为 | K17 |
| 10 | upstream main 的 ModelServing 在成员上的 status 写入次数（6.4 的方案 M 一侧用的是 010 构建的数字） | K14 |
| 11 | 上游对 4.5 (b) 这类改动的态度；#7895 两项的时间 | 上游讨论，本轮没有发任何内容 |
| 12 | GPU 通过 DRA 申请而不是扩展资源时，两个估算器都看不到。本轮没有读相关代码 | 另行刻画 |

## 附录 A：包级实测原始输出

命令（临时目录 `$S` 已删除；`$K` 是 `karmada/`）：

```text
cd $K && GOTMPDIR=$S/gotmp GOFLAGS=-mod=vendor go test -overlay $S/overlay-all.json \
  ./pkg/scheduler/core/ ./pkg/estimator/ ./pkg/controllers/status/ ./pkg/controllers/gracefuleviction/ \
  -run 'TestK014SGProbe' -count=1 -v
```

`pkg/scheduler/core`（调度器，汇总估算；一组 = 1 entry + 3 worker，各 1 GPU；`free` 是空闲 GPU 数）：

```text
K014SG sched | S1 first placement, Divided, free a=0 b=8 c=4 | before=[] -> suggested=[b(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S1d first placement, Duplicated(default), free a=0 b=8 c=4 | before=[] -> suggested=[b(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S2 first placement, Divided, free a=0 b=0 c=2 | before=[] -> suggested=[] errKind=InsufficientCapacityError err=failed to select clusters: no enough resource when selecting 1 clusters
K014SG sched | S2d first placement, Duplicated(default), free a=0 b=0 c=2 | before=[] -> suggested=[a(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S3 running in a, role scaled 3->4 workers, Divided, free a=0 b=8 c=0 | before=[a] -> suggested=[b(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S3d same, Duplicated(default) | before=[a] -> suggested=[a(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S3b running in a, role scaled, Divided, free a=0 b=0 c=2 | before=[a] -> suggested=[] errKind=InsufficientCapacityError err=failed to select clusters: no enough resource when selecting 1 clusters
K014SG sched | S3c running in a, role scaled, Divided, free a=8 b=64 c=0 | before=[a] -> suggested=[a(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S4 stuck in a (pods pending), rescheduleTriggeredAt, Divided, free a=0 b=8 c=0 | before=[a] -> suggested=[b(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S4b stuck in a, rescheduleTriggeredAt, Divided, free a=0 b=0 c=0 | before=[a] -> suggested=[] errKind=InsufficientCapacityError err=failed to select clusters: no enough resource when selecting 1 clusters
K014SG sched | S4c stuck in a but a's summary still shows room, rescheduleTriggeredAt, Divided, free a=4 b=8 c=0 | before=[a] -> suggested=[a(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S4d same as S4, Duplicated(default) | before=[a] -> suggested=[a(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S5 a evicted, Divided, free b=8 c=0 | before=[] -> suggested=[b(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S5b a evicted, Divided, free b=0 c=0 | before=[] -> suggested=[] errKind=InsufficientCapacityError err=failed to select clusters: no enough resource when selecting 1 clusters
K014SG sched | S5c a evicted, Duplicated(default), free b=0 c=0 | before=[] -> suggested=[b(replicas=0,components=2)] errKind=<nil> err=<nil>
K014SG sched | S5d a evicted and a is the only cluster of the policy | before=[] -> suggested=[] errKind=FitError err=0/3 clusters are available: 1 cluster(s) is in the process of eviction, 2 cluster(s) did not match the placement cluster affinity constraint.
K014SG sched | S6 single-component unit, first placement, Divided, free a=0 b=8 c=4 | before=[] -> suggested=[b(replicas=0,components=0)] errKind=<nil> err=<nil>
K014SG b2b | SchedulingOvercommitProtection=false | real room: b=2 sets, c=1 set | [deepseek-0=>b deepseek-1=>b deepseek-2=>b deepseek-3=>b deepseek-4=>b]
K014SG b2b | SchedulingOvercommitProtection=true | real room: b=2 sets, c=1 set | [deepseek-0=>b deepseek-1=>b deepseek-2=>c deepseek-3=>FAILED(failed to select clusters: no enough resource when selecting 1 clusters) deepseek-4=>FAILED(failed to select clusters: no enough resource when selecting 1 clusters)]
K014SG sched | X1 no interpreter (no replicas, no components), spread cluster 1..1, Divided, free a=0 b=8 c=4 | before=[] -> suggested=[a(replicas=0,components=0)] errKind=<nil> err=<nil>
K014SG sched | X2 components, policy WITHOUT spread constraint, Divided, free a=0 b=8 c=4 | before=[] -> suggested=[a(replicas=0,components=0) b(replicas=0,components=0) c(replicas=0,components=0)] errKind=<nil> err=<nil>
K014SG sched | X3 components, spread constraint maxGroups=1 minGroups unset, Divided, free a=0 b=8 c=4 | before=[] -> suggested=[a(replicas=0,components=0)] errKind=<nil> err=<nil>
K014SG sched | X4 gate MultiplePodTemplatesScheduling off, components present, spread cluster 1..1, Divided, free a=0 b=8 c=4 | before=[] -> suggested=[a(replicas=0,components=0)] errKind=<nil> err=<nil>
```

X3 只在包级能构造：策略的 webhook 会把 `minGroups` 补成 1（`karmada:pkg/util/helper/policy.go:39-42`）。b2b 里「开」的那一行，记账是测试按
`Scheduler.updateAssumptionsCache` 的做法手工写进同一个缓存的；真实路径在调度结果写回成功之后才记（`karmada:pkg/scheduler/scheduler.go:698-714`）。

`pkg/estimator`（节点级估算用的组件集模拟）：

```text
K014SG sim | O1 nodes[4,2], components in template order small(2x1GPU) then big(1x4GPU), real=1 | sets=0 err=<nil>
K014SG sim | O2 same nodes, components ordered big first, real=1 | sets=1 err=<nil>
K014SG sim | O3 nodes[6,4], big(1x4GPU) then small(2x3GPU), real=1 (4 on n2, 3+3 on n1) | sets=0 err=<nil>
K014SG sim | N1 nodes n1(pool=a,8GPU) n2(pool=b,1GPU); entry needs pool=b (1x1GPU), workers anywhere (3x1GPU); real=1 | sets=1 err=<nil>
K014SG sim | N2 same nodes; entry needs pool=b with 2 GPU; real=0 | sets=0 err=<nil>
K014SG sim | N3 same nodes; entry needs pool=c (no such node); real=0 | sets=0 err=<nil>
K014SG sim | N4 a node without Node object (as built from resource models); entry needs pool=c | sets=2 err=<nil>
```

`pkg/controllers/status`（汇总估算的组件路径；集群是 4 节点 × 32 cpu、128Gi、4 GPU，没有负载）：

```text
K014SG general | cluster: 4 nodes x (32 cpu, 128Gi, 4 GPU), nothing running; gate CustomizedClusterResourceModeling=true; default models=9 grades; allocatableModelings=[{0 0} {1 0} {2 0} {3 0} {4 0} {5 4} {6 0} {7 0} {8 0}]
K014SG general | M1 default resource models, unit without GPU (control), real=4+ | sets=16 err=<nil>
K014SG general | M2 default resource models, unit 4 x 1 GPU, real=4 | sets=0 err=<nil>
K014SG general | M3 resourceModels cleared, unit 4 x 1 GPU, real=4 | sets=4 err=<nil>
K014SG general | M4 resourceModels cleared, entry has a nodeSelector no node satisfies, real=0 | sets=4 err=<nil>
K014SG general | P1 gate on, no assumption, 16 GPU free | sets=4 err=<nil>
K014SG general | P2 gate on, 1 unit assumed (scheduled, Pods not visible yet) | sets=3 err=<nil>
K014SG general | P3 gate on, 4 units assumed | sets=0 err=<nil>
K014SG general | P4 gate on, the assumed unit's Pods are bound (4 GPU allocated) and the assumption is still held; real remaining=3 | sets=2 err=<nil>
K014SG general | P5 same cluster state, assumption released; real remaining=3 | sets=3 err=<nil>
```

`pkg/controllers/gracefuleviction`（驱逐任务何时结束；超时取默认的 10 分钟）：

```text
K014SG evict | E1 task is 2 min old, unit rescheduled to b, b Healthy | kept=0 removed=[a]
K014SG evict | E2 task is 2 min old, unit rescheduled to b, b Unhealthy (e.g. still not ready) | kept=1 removed=[]
K014SG evict | E3 task is 2 min old, no cluster could take the unit (scheduling failed: no result, generation not observed) | kept=1 removed=[]
K014SG evict | E4 same as E3 but the scheduler has observed the generation (empty result) | kept=0 removed=[a]
K014SG evict | E5 Gracefully with gracePeriodSeconds=600, task is 2 min old, no result | kept=1 removed=[]
K014SG evict | E6 task is 11 min old, no result | kept=0 removed=[a]
```

E4 只在包级能构造：调度失败时调度器不更新 `schedulerObservedGeneration`（`karmada:pkg/scheduler/scheduler.go:1012-1024`），真实情形是 E3。

五个包全部 `PASS`。

## 附录 B：探针的构造

测试文件已随临时目录删除。下面的描述足以重写它们；协调者如果要把源码落盘到 `evidence/`，按这里重建即可。

| 文件（overlay 到的包） | 构造 |
| --- | --- |
| `pkg/scheduler/core`，`TestK014SGProbeScheduler` | 用 `cache.NewIndexer` 装 3 个 `Cluster`（a、b、c），`clusterlister.NewClusterLister` 包成 lister，`schedulercache.NewCache(lister, 空 indexer, 0)`，`NewGenericScheduler(cache, frameworkplugins.NewInTreeRegistry())`，直接调 `Schedule`。集群的 `status.apiEnablements` 带 `workload.serving.volcano.sh/v1alpha1` 的 `ServingGroup`；`status.resourceSummary` 的 allocatable 是 cpu 1000、memory 4000Gi、pods 1000、`nvidia.com/gpu` N，allocated 和 allocating 只填 GPU，没有资源模型。绑定的 `spec.components` 是 `infer-entry`（1 副本）和 `infer-worker`（3 或 4 副本），每副本 cpu 1、memory 1Gi、1 GPU。placement 是 `clusterNames: [a,b,c]` 加 spread `cluster 1..1`，`Divided/Aggregated` 或不写。已放置用 `spec.clusters = [{name: a, components: buildTargetComponents(...)}]` 表示；显式重调度用 `rescheduleTriggeredAt = now`、`status.lastScheduledTime = now − 1h`；驱逐用一条 `gracefulEvictionTasks`。门控用 `features.FeatureGate.Set` 打开 |
| 同包，`TestK014SGProbeBackToBack` | 同一个调度器和缓存，连续调度 5 个不同名字的绑定，集群快照不变。保护开时每次成功后调用 `AssigningResourceBindings().Assume(key, 目标集群, 组件)` |
| 同包，`TestK014SGProbeMisconfiguration` | X1：绑定没有组件也没有副本数；X2：placement 去掉 spread；X3：spread 只写 `maxGroups: 1`；X4：关掉门控 |
| `pkg/estimator`，`TestK014SGProbeSimulator` | `schedulerframework.NewNodeInfo()` + `SetNode` + `Allocatable = util.NewResource(...)`（cpu 64、memory 512Gi、pods 110、GPU N，可带标签），`NewSchedulingSimulator(nodes).SimulateScheduling(components, MaxInt32)`。组件用 `pb.Component`，`nodeClaim` 只填 `NodeSelector`。N4 的节点不调 `SetNode` |
| `pkg/controllers/status`，`TestK014SGProbeGeneralComponentSets` | 沿用第一轮 G1 探针的做法：`mutation.SetDefaultClusterResourceModels` 得到默认模型，`getAllocatableModelings` 算出 `allocatableModelings`；调 `estimatorclient.NewGeneralEstimator().MaxAvailableComponentSets`。P 系列打开 `SchedulingOvercommitProtection`，用请求里的 `AssumedWorkloads` 传记账；P4、P5 给集群加 `allocated: {gpu: 4, pods: 4}` |
| `pkg/controllers/gracefuleviction`，`TestK014SGProbeEvictionTask` | 直接调包内函数 `assessEvictionTasks`，任务的 `creationTimestamp` 设为 2 分钟前或 11 分钟前，`assessmentOption` 的 `scheduleResult`、`observedStatus`、`hasScheduled` 按场景填 |
