# 和 ServingGroup CR 的关系：构造 CR 是不是更好的选择

- 作者：@VanderChen，2026-11-28
- 状态：评估结论，待评审。没有改任何实现代码。
- 对象：[01-design.md](01-design.md) 里的方案（下称方案 M）和「每个 ServingGroup 一个独立对象」的方案（下称方案 G）。
- 基线：Karmada `6d007132`；kthena upstream main `1a5145e9`。
- ServingGroup CR 原型：本 fork 的分支 `feat/010-modelserving-servinggroup-cr-impl` @ `98fcbf01`，下文记作 `sgcr:`；
  它的设计文档在分支 `feat/008-modelserving-servinggroup-cr-design` 的 `docs/proposal/modelserving-servinggroup-cr/`。
  CR 路线本身的取舍在 [volcano-sh/kthena#2148](https://github.com/volcano-sh/kthena/issues/2148) 讨论，本文只看它和多集群的关系。
- 证据标签同 [01-design.md](01-design.md)：【Kind】日志在 [`artifacts/logs/`](artifacts/logs/) 的 `kind-G*.log`；【源码】；【包级实测】；【推断】；【待验证】。
  Kind 环境和第一轮相同：成员没有装 Volcano，用的是默认调度器；GPU 是打在节点上的扩展资源。

## 1. 结论

**「每组一个分发单元」这个形状在 Karmada 里更对；为它构造 ServingGroup CR，现在不是更好的选择。**

三句话：

1. 形状带来的好处是真实的，在 Kind 上看到了：方案 M 估错的三种情况全部估对，放不下的组留在控制面排队而不是卡在成员集群里。
2. 这些好处来自「一个组一个 ResourceBinding」，不来自新的 CRD。Kind 上用的单元是 `replicas: 1` 的 ModelServing，kthena 一行没改。
3. 这个形状在今天的 Karmada 上有自己的问题，其中一条很重：**正在运行的单元会被搬到别的集群**（改一下策略的集群名单，或者调一下 role 的副本数）。

所以建议是：

| 事项 | 建议 |
| --- | --- |
| 方案 M 的第 1 阶段（解释器加指南） | 照做。Duplicated 和静态权重只需要它，而且它只用到 Karmada 的稳定能力 |
| 把方案 M 的解释器提交为 Karmada 内置定制 | **暂缓**。同一个 kind 只能有一种解释方式；内置之后再想换成按组分发就是破坏性变更 |
| 为多集群构造 ServingGroup CR | 不做。现有原型的 CR 不能直接当单元用，改到能用等于重新设计；而 `replicas: 1` 的 ModelServing 已经能当单元 |
| 按组分发（单元是 `replicas: 1` 的 ModelServing） | 作为「按容量动态放置」这一用法的候选主路径，前提是「运行中的单元会被搬走」这个问题有解（上游 #7895 的未完成项，或者我们来修） |
| kthena#2148（CR 还是状态账本） | 把两条新输入带回去：多集群是第一个只有独立对象才能给的收益；如果选 CR，它的 spec 必须自包含。目前只有草稿（第 9 节），没有发出 |

这个结论依赖三件还不知道的事，见第 7 节和 [03-decisions.md](03-decisions.md)。

## 2. 方案 G 是什么，有哪几种做法

共同点：每个 ServingGroup 在 Karmada 控制面上是一个独立对象，整体放进恰好一个集群，用 Karmada 的多组件估算
（解释器实现 `componentResource`，特性门控 `MultiplePodTemplatesScheduling`）。

| 做法 | 单元是什么 | 谁管数量、滚动、缩容 | kthena 要改什么 |
| --- | --- | --- | --- |
| G-a | 自包含的 ServingGroup CR（spec 里带 Pod 模板） | 跑在 karmada-apiserver 上的 ModelServing 级控制器 | 重新设计 CR；把原型的两个控制器拆到控制面和成员两边 |
| G-b | 原型的 ServingGroup CR 原样 | 同上 | 看似不用改，实际不可用（第 4 节） |
| G-c | `replicas: 1` 的 ModelServing | 用户工具，或一个很薄的父控制器 | 控制器不用改 |

G-c 又分两种：用组件解释器（G-c1，本轮 Kind 用的就是它），或者原样用方案 M 的解释器（G-c0，不需要 alpha 门控，没有估算上的好处）。

## 3. Kind 上看到的

单元解释器在 [`artifacts/unit/unit-customization.yaml`](artifacts/unit/unit-customization.yaml)，单元生成脚本和策略在 [`artifacts/unit/`](artifacts/unit/)。环境记录见 [04-evidence.md](04-evidence.md) 第 5 节。

集群容量：m1 是 2 个节点 × 2 GPU，m2、m3 各是 1 个节点 × 8 GPU。

### 3.1 比方案 M 好的

| # | 场景 | 方案 M（1–5 行是第一轮 Kind 的结果，6、7 行是设计） | 方案 G（本轮 Kind） | 日志 |
| --- | --- | --- | --- | --- |
| 1 | 每组 4 个 Pod × 1 GPU，放进 m1（真实能放 1 组），三个成员都有 scheduler-estimator | 估算 0，调度失败；只能去掉 estimator | 估算 1，放进 m1 并 Ready | `kind-G1-*.log` |
| 2 | 每组 3 GPU + 1 GPU，m1 没有 3 GPU 的节点 | 估算 1，被放进 m1 后卡住 | 估算 0，不放进 m1 | `kind-G2-G3-*.log` |
| 3 | 每个 Pod 带 nodeSelector，m1 的节点不满足 | 估算 1，被放进 m1 后卡住 | 估算 0，不放进 m1 | 同上 |
| 4 | 6 个单元，总容量 5 | 合计不够时一个也不放 | 5 个恰好按 1 + 2 + 2 放满并 Ready，第 6 个不放 | `kind-G1-*.log` |
| 5 | 放不下的组在哪里等 | 在成员集群里 Pending；没有 gang 调度时已调度的 Pod 占着 GPU | 在控制面等，成员里不建任何 Pod | `kind-G2-G3-*.log` |
| 6 | 卡住的组换集群（估算器看不到的约束：Pod 反亲和） | 要改 karmada-descheduler | 现有的应用故障转移，不健康 30 秒后整体迁走并 Ready，不改任何代码 | `kind-G5-*.log` |
| 7 | 已调度、还没 Ready 的组（代表加载模型中） | 不会被迁（`unschedulableReplicas` 为 0） | 把健康定义成「没有调度不上的 Pod」后，2 分钟内没有被迁 | 同上 |

第 2、3 行是方案 G 最实在的收益：节点级估算按每个 Pod 模板分别看，并且认每个模板自己的 nodeSelector、容忍和硬亲和。
方案 M 的「整组求和」在原理上做不到这一点。

节点级的组件集估算不是处处都准：它是 First Fit，不会在资源维度上高估，但可能低估（真实能放 1 组而估算为 0），
把单 Pod 请求大的组件排在前面可以减轻。【包级实测，见 [appendix/round2-proposal-analysis.md](appendix/round2-proposal-analysis.md)】拓扑、队列配额、Pod 反亲和它仍然看不到。

### 3.2 和方案 M 一样的

| # | 问题 | 观察到的 | 日志 |
| --- | --- | --- | --- |
| 1 | 默认资源模型下 GPU 请求估算为 0 | 单元调度失败：`no enough resource when selecting 1 clusters`。清空资源模型后恢复 | `kind-G0-*.log` |
| 2 | 排队的对象重试慢 | 容量腾出来 4 分 09 秒后才被调度（调度器 5 分钟的重试定时器，不是按容量变化触发） | `kind-G2-G3-*.log` |

### 3.3 方案 G 自己的问题

| # | 问题 | 观察到的 | 日志 |
| --- | --- | --- | --- |
| 1 | **运行中的单元不留在原集群** | 四个单元把 m2、m3 放满并 Ready。把 m1 加进策略的集群名单后：一个 Running 的单元被从 m2 搬到 m1；另外三个的绑定变成 `Scheduled=False`（Pod 没动）。另一次：单元在 m2 上把 worker 从 2 个缩到 1 个，m2 剩 1 个 GPU、不够再放一整组，单元被搬到 m3 | `kind-G6-*.log`、`kind-G7-*.log` |
| 2 | 策略写法错了不报错 | 策略不写 `replicaScheduling`（默认 Duplicated）时，调度器算出估算值（全是 0）然后忽略它，取排在最前的集群 | `kind-G0-*.log` |
| 3 | 批量创建会放超 | 一次创建 5 个单元，65 毫秒内按同一份容量快照估算，3 个进了只能放 2 个的 m2；没有 gang 调度，其中两个各占 2 个 GPU 互相卡死；m1 空着 | `kind-G4-*.log` |
| 4 | 上一条要靠另一个 alpha 门控解决 | 打开 `SchedulingOvercommitProtection` 后同样的 5 个恰好放满。但另一次批量创建里有 2 个单元没被调度，而当时还有一个集群放得下 | `kind-G4-*.log`、`kind-G6-*.log` |
| 5 | 没有去处时，应用故障转移让单元来回搬 | 两个候选集群都放不下：4 分半钟里被驱逐 6 次，在两个集群之间轮换，中间有时两边都不在；每次都删掉已经 Running 的那个 Pod | `kind-G5-*.log` |
| 6 | 应用故障转移有额外要求 | 策略必须 `propagateDeps: true`，否则被 Karmada 的 webhook 拒绝 | `kind-G5-*.log` |

第 1 条的原因：Karmada 重新调度一个多组件绑定时，不把它自己已经占用的资源加回去，于是要求原集群「还能再放一整组」。
触发重新调度的不只是用户改副本数：策略的 placement 变化、显式重调度、单元内任何组件的副本数变化都会触发。【源码 + 包级实测 + Kind】
上游的 #7895（多组件调度第五阶段，open）把「重调度时的估算应考虑已调度的组件」列为未完成项。
策略写成 Duplicated 可以让单元留在原集群，但那样首次放置就不看容量了（上表第 2 条）。

对 LLM 推理来说，这意味着运维往集群名单里加一个新集群，就可能让一批正在服务的组重新加载模型。在它解决之前，方案 G 不能用于生产。

## 4. 已有的 ServingGroup CR 原型能不能直接当单元

不能。【源码，附录里第二轮的三份分析报告交叉确认】

| 事实 | 位置 | 后果 |
| --- | --- | --- |
| ServingGroup 的 spec 里没有 Pod 模板，模板从本集群的 ControllerRevision 读 | `sgcr:pkg/apis/workload/v1alpha1/servinggroup_resource_types.go:25-28` | Karmada 的 Lua 解释器只看得到对象本身，算不出资源需求；成员集群里也没有那些 ControllerRevision |
| Karmada 下发时去掉 `ownerReferences` | `karmada:pkg/resourceinterpreter/default/native/prune/prune.go:77` | 成员上的 ServingGroup 没有父对象 |
| 组内控制器遇到没有父 ModelServing 的组直接返回，不建 Pod | `sgcr:pkg/model-serving-controller/sgcontroller/reconcile.go:107-113,292-308` | 下发下去的组什么都不会发生 |
| ModelServing 级控制器用自己的 Pod 缓存复核组的状态是否可信 | `sgcr:pkg/model-serving-controller/mscontroller/observe.go:149,165` | 控制面没有 Pod，没有任何组能被确认 Ready，滚动永远不推进 |

要让它能用（G-a），需要：spec 自包含；父控制器不再读 Pod，改读经 Karmada 传回的状态；两个控制器拆开部署；
去掉接管和回退路径；处理「单元没被调度」「Work 应用失败」「成员失联」这几种原型里不存在的状态。完整的 9 处见 [appendix/round2-proposal-analysis.md](appendix/round2-proposal-analysis.md) 6.2 节。

拆开之后，#2148 里列的 CR 路线的代价会怎样【推断】：

| #2148 列的代价 | 拆到控制面和成员两边之后 |
| --- | --- |
| 只解决组这一层，Role 实例仍然要账本 | 不变 |
| 两个控制器之间的滞后；「组对象先于 Pod 存在」 | 变差：滞后从进程内变成跨集群，经过三次异步写（成员 → Work → 绑定 → 模板） |
| 跨组预算不是原子的 | 窗口变大。但相对方案 M 是净收益，方案 M 根本没有全局预算 |
| API 写入量 2.35 倍 | 变差：组的每次状态变化在控制面上至少再写 3 次 |
| 运行中 Pod 的 owner 迁移 | 新建的多集群部署没有；已有的单集群用户不变，kthena 要长期维护两套路径 |

还多出一条结构性的代价：需要整组重建的组要等控制面的父控制器来删除重建。控制面不可用或成员失联时，这一级恢复停摆。
方案 M 和 G-c 没有这个问题，成员集群跑的是完整的 kthena。

## 5. 逐条对照方案 M 在 Kind 上暴露的问题

| 方案 M 的问题 | 方案 G 下 | 依据 |
| --- | --- | --- |
| 整组求和使节点级估算为 0；汇总估算看不到碎片和 nodeSelector | 修掉大部分 | Kind（3.1 的 1–3） |
| 合计不够时一个也不放 | 修掉 | Kind（3.1 的 4） |
| 成员按自己分到的份额执行 `rolloutStrategy`；合法模板被拒；各集群同时滚动 | 修掉，但全局预算要有人实现（父控制器或用户工具） | 推断 |
| 跨集群缩容不看就绪情况 | 修掉，删哪个单元由上层决定 | 推断 |
| 已有模板后再装解释器，组被清空 | 不再出现（单元没有 `replicaRevision`）；但不装解释器时放置不看容量 | 推断，待验证 |
| `componentResource` 陷阱 | 换了形态：同一个控制面上，ModelServing 不能同时按方案 M 和按组件解释 | 源码 + 第一轮 Kind |
| 默认资源模型下 GPU 估算为 0 | 保留 | Kind（3.2 的 1） |
| 没有去处时的循环 | 保留，更频繁 | Kind（3.3 的 5） |
| 成员侧自动扩缩容不能和 Divided 一起用 | 保留，边界变了：组数只能在控制面改；FederatedHPA 不能以父对象为目标 | 源码 |

## 6. 比较

| 维度 | 方案 M | G-a（新 CR + 控制面父控制器） | G-c（`replicas: 1` 的 ModelServing） |
| --- | --- | --- | --- |
| 估算 | 集群汇总，看不到碎片和 nodeSelector | 节点级，按模板分别看 | 同 G-a（用组件解释器时） |
| 放不下时 | 整个 ModelServing 调度失败，或者进成员后卡住 | 放得下的先放，其余在控制面排队 | 同 G-a |
| 卡住的组换集群 | 改 karmada-descheduler | 应用故障转移可以起步，没有去处时会来回搬 | 同 G-a |
| 滚动 | 各集群按自己的份额各自滚动 | 全局预算、全局 partition、全局序号；基于滞后的状态 | 要有父控制器或用户工具 |
| 运行中的组是否稳定 | 稳定（Divided 的增量调度保留已有分配） | **不稳定**，见 3.3 的 1 | 同 G-a |
| 成员本地自愈 | 完整 | 组一级依赖控制面 | 完整 |
| 自动扩缩容 | 两种模式二选一；控制面模式可用 FederatedHPA | 组数只能在控制面改；FederatedHPA 不可用 | 同 G-a |
| 对 Karmada 的要求 | 第 1 阶段只用稳定能力 | 两个 alpha 门控 | 同 G-a；用方案 M 的解释器时不需要 |
| Karmada 对象数量 | 每个 ModelServing 一个绑定，每个集群一个 Work | 每组一个模板、一个绑定、一个 Work | 同 G-a |
| kthena 的成本 | 文档和一份 YAML | 很高（第 4 节） | 控制器零改动；可选的薄父控制器 |
| 已有负载迁移 | 有现成路径 | 没有原地转换的路径 | 要拆成 N 个对象，Pod 名和 `modelserving.volcano.sh/name` 标签会变 |
| 和上游的关系 | Karmada maintainer 在 #8104 里质疑「整组当一个副本」 | 是 Karmada 多组件调度的设计形状；kthena 一侧依赖 #2148 选 CR | Karmada 一侧同 G-a；不涉及 #2148 |

上游这一行的依据（社区调研，第二轮）：Karmada 的多 Pod 模板提案把「一个 CR 的所有组件放在同一个集群」作为约束，
跨集群拆分要在调度之前做；上游自己的 e2e 用的就是「spread 1 个集群 + Divided/Aggregated」。
但没有 maintainer 说过 kthena 应该新增 ServingGroup CR；kthena 的 maintainer 也没有把 CR 和多集群联系起来。

可比项目里，对象层级最接近的是 RoleBasedGroup 的 `RoleBasedGroupSet → RoleBasedGroup`，但它是集群内的设计。
MultiKueue 对 LeaderWorkerSet 是每个副本一个 Workload，但所有副本跟着第一个去同一个集群，不是按组分别放置。
没有找到「父对象留在控制面、每组分别放到不同集群」的现成先例。

## 7. 什么会改变这个结论

下面三件事还不知道，它们比继续做分析更能决定方向：

| # | 问题 | 如果答案是这样，结论往哪边动 |
| --- | --- | --- |
| 1 | 主要用法是按容量动态放置，还是 Duplicated 或固定比例 | 后者：方案 M 就够，不需要按组分发 |
| 2 | 典型部署里每个集群有几个组 | 每个集群只有 1 到 2 个大组：方案 M 的「按份额滚动」没有意义，全局编排是硬需求，按组分发的价值变大 |
| 3 | 是否依赖 kthena autoscaler，尤其是按 role 伸缩 | 依赖：按组分发下它要重新设计，而且改 role 副本数会触发 3.3 的第 1 条 |

另外的触发条件：

| 证据 | 结论往哪边动 |
| --- | --- |
| 上游 #7895 的未完成项合入（重调度时考虑已调度的组件），或者我们修掉它 | 按组分发可以进入生产建议 |
| 上游明确不接受 descheduler 从状态取数（方案 M 的第 2 阶段），或者明确建议「一个单元一个对象」 | 第 2 阶段改走按组分发 |
| kthena#2148 选了 CR 路线，并且接受 spec 自包含 | 单元可以从 ModelServing 换成 ServingGroup，重新评估 G-a |
| 生产 Karmada 不允许开 alpha 门控 | 留在方案 M |
| 每个 ModelServing 有上百个组 | 留在方案 M（对象数量和写入量） |

## 8. 对方案 M 的修正

这次评估顺带发现 [01-design.md](01-design.md) 初稿里两处不准确，已经改掉：

| 位置 | 原来的说法 | 改成 |
| --- | --- | --- |
| 4.3 | 节点级的组件集估算在全部场景里都对 | 在测过的场景里都对；它是 First Fit，可能低估 |
| 5.3 前提 2 | 一组放不进单个节点的 ModelServing，其目标集群不部署 scheduler-estimator | 候选集群里只要有一个连不上 scheduler-estimator，这次调度的所有集群都退回汇总估算。第一轮 C2c 的日志里就是这样，当时没有注意到 |

## 9. 给 kthena#2148 的输入

供起草回复时使用，不代表已经发出。

1. **新论据**：#2148 比较的两条路线里，账本放在 `ModelServing.status` 的那条给不了多集群下的单独放置；
   只有独立对象才能被单独下发、单独估算、单独换集群。这是讨论里还没有出现过的、CR 路线独有的收益。
2. **但它不要求新 CRD**：`replicas: 1` 的 ModelServing 已经是这样一个对象，在 Kind 上验证过。所以它不足以单独决定 #2148。
3. **如果选 CR，多集群对它的三条约束**：spec 自包含（带 Pod 模板，不靠 ControllerRevision）；
   不把父对象的 UID 当作信任锚（跨集群下发会去掉 ownerReferences）；ModelServing 级控制器不读 Pod。
   现有原型三条都不满足。
4. **拆开之后的代价**：组一级的恢复依赖控制面；决策基于经过三次异步写的状态；写入量再放大。
