# ModelServing + Karmada 多集群：方案、分析和待决策点

- 作者：@VanderChen，2026-11-28
- 状态：**提案草稿，待评审。** 没有改 Karmada 和 kthena 的任何实现代码；没有向任何上游仓库提交 issue、PR 或评论。
- 基线：Karmada `6d007132`（master，`v1.20.0-beta.0-44`）；kthena `1a5145e9`（main，含 PR #2079）。

要回答的问题有两个：

1. 用 Karmada 把一个 ModelServing 分发到多个集群，今天能做到什么，缺什么，怎么补。
2. 为此把 ServingGroup 做成独立的 CR，是不是更好的选择。

## 文档

| 文档 | 内容 |
| --- | --- |
| [01-design.md](01-design.md) | 使用方案：部署拓扑、解释器、三种分发模式、状态、自动扩缩容、重调度、分期 |
| [02-servinggroup-cr.md](02-servinggroup-cr.md) | 和 ServingGroup CR 的关系：按组分发的形状、Kind 上的结果、现有 CR 原型能不能当分发单元 |
| [03-decisions.md](03-decisions.md) | 待决策点：三个事实问题、方向、kthena 现有行为、对外动作 |
| [04-evidence.md](04-evidence.md) | 证据：源码现状、包级实测、解释器离线实测、两轮 Kind 刻画、方案取舍、验证计划、还没有验证的 |
| [artifacts/](artifacts/README.md) | 解释器草稿、Kind 脚本和 YAML、包级探针源码、全部日志 |
| [appendix/](appendix/README.md) | 两轮共六份独立分析报告（方案分析、社区调研、行为和回归），以及它们的哪些结论被采纳、哪些被 Kind 改掉 |

只想看结论：读本页和 [03-decisions.md](03-decisions.md)。

## 结论

### 使用方案（方案 M：Karmada 按 `spec.replicas` 拆分 ModelServing）

一个副本就是一个 ServingGroup。控制面上写一个 ModelServing 和一个 PropagationPolicy，成员集群运行原版 kthena。

| 能力 | 今天的 Karmada | 要补的 | 阶段 |
| --- | --- | --- | --- |
| 每个集群一份完整副本 | 能下发，但控制面没有状态，健康永远是 Healthy | 一份解释器配置（`ResourceInterpreterCustomization`） | 1 |
| 按固定比例拆副本 | 不装解释器时 `Divided` 被静默当成每个集群完整副本 | 同上 | 1 |
| 按各集群剩余容量拆副本 | 默认配置下，任何请求 GPU 的副本估算为 0，调度失败 | 解释器，加两条 Karmada 配置 | 1 |
| 调度不上的 ServingGroup 换集群 | karmada-descheduler 只处理 Deployment | 改 karmada-descheduler：从聚合状态读不可调度数，有去处才降 | 2 |
| 准确估算「这个集群能放几整组」 | 估算器能算对，但调度器只在整份放进一个集群时才用它 | 上游改调度器：按组件集拆分副本 | 3 |

第 1 阶段只有 YAML 和文档，不改任何控制器。它有几条必须写进指南的硬约束，都在 Kind 上复现过：

- 解释器必须在创建模板之前装好。之后再装，下一次模板更新会删掉所有集群正在运行的组。
- 按容量拆分时，「一组的资源求和」被当成一个副本的需求。节点级估算会把「能跨节点放下一组」的集群估成 0，所以这种模式有适用条件。
- 成员集群按自己分到的副本数校验 `rolloutStrategy`，单集群合法的写法拆分后可能被拒绝。
- 自动扩缩容只能二选一：在控制面改副本数，或者在成员侧伸缩并让 Karmada 不覆盖。

### 和 ServingGroup CR 的关系

**「每组一个分发单元」这个形状在 Karmada 里更对；为它构造 ServingGroup CR，现在不是更好的选择。**

- 好处是真实的：方案 M 估错的三种情况全部估对，放不下的组留在控制面排队，卡住的组可以单独换集群。
- 好处来自「一个组一个绑定」，不来自新的 CRD。Kind 上用的单元是 `replicas: 1` 的 ModelServing，kthena 没有改。
- 现有的 ServingGroup CR 原型不能直接当单元：spec 不带 Pod 模板，跨集群下发后没有父对象，组内控制器不处理它。
- 这个形状在今天的 Karmada 上有一个必须先解决的问题：**正在运行的单元会被搬到别的集群**
  （往策略里加一个集群，或者改一下 role 的副本数就会触发）。
- 两种方式在同一个 Karmada 控制面上是二选一：同一个 kind 只能有一种解释方式。

所以：第 1 阶段照做；解释器暂不提交为 Karmada 的内置定制；按组分发列为「按容量动态放置」这一用法的备选方向；
CR 本身做不做留给 [kthena#2148](https://github.com/volcano-sh/kthena/issues/2148)，多集群给它一条新论据和三条约束。

## 待决策点

完整内容和依赖关系见 [03-decisions.md](03-decisions.md)。

| 类别 | 要定的事 |
| --- | --- |
| 事实 | 主要用法是按容量动态放置，还是每个集群一份或固定比例；每个集群有几个 ServingGroup；是否依赖按 role 的自动扩缩容 |
| 方向 | 是否进入第 1 阶段；解释器是否提交为 Karmada 内置定制；重调度放在哪里；是否改走按组分发；是否为多集群构造 CR；Karmada 侧改动在哪里开发 |
| kthena 行为 | 是否放宽按副本数的 `rolloutStrategy` 校验；surge 组和部分在服务的组是否计入 `unschedulableReplicas` |
| 对外 | 向 Karmada 报告刻画中发现的问题；回复 karmada#8104；给 kthena#2148 提供输入。目前只有要点和证据，没有发出任何内容 |

第一个事实问题最重要：答案是「每个集群一份或固定比例」时，做完第 1 阶段就够了。

## 证据的范围和局限

- 结论分四类证据：读源码、包级实测、解释器离线实测、Kind。正文里每条结论都标了是哪一类。
- Kind 环境：一个 Karmada 控制面加三个成员集群（两个 push、一个 pull），成员运行从 `1a5145e9` 构建的 kthena controller-manager。
- **成员集群没有装 Volcano**，用的是默认调度器；GPU 是打在节点上的扩展资源，Pod 是 pause 镜像。gang 调度下的行为都还没有验证。
- 没有做规模和性能测试。
- 第 2、3 阶段只有设计，没有原型。[04-evidence.md](04-evidence.md) 第 8 节列出了全部还没有验证的项。

## 记号

| 记号 | 含义 |
| --- | --- |
| `karmada:path` | [karmada-io/karmada](https://github.com/karmada-io/karmada) @ `6d007132ec5112d2eed41b602d4aeeebaf96f4fc` |
| `kthena:path` | [volcano-sh/kthena](https://github.com/volcano-sh/kthena) @ `1a5145e90e0e1d70c75def0ef9500055e2904c5f` |
| `sgcr:path` | ServingGroup CR 原型：本 fork 的分支 `feat/010-modelserving-servinggroup-cr-impl` @ `98fcbf01` |
| 方案 M | Karmada 按 `spec.replicas` 拆分一个 ModelServing（[01-design.md](01-design.md)） |
| 方案 G | 每个 ServingGroup 是控制面上的一个独立对象，整体放进一个集群（[02-servinggroup-cr.md](02-servinggroup-cr.md)） |

## 相关的上游讨论

| 编号 | 内容 |
| --- | --- |
| [karmada#8104](https://github.com/karmada-io/karmada/issues/8104) | karmada-descheduler 支持自定义工作负载类型。这个用法的讨论来源 |
| [karmada#8109](https://github.com/karmada-io/karmada/pull/8109) | PR（open）：让 descheduler 处理自定义工作负载类型 |
| [karmada#4046](https://github.com/karmada-io/karmada/issues/4046) | 按工作负载状态做 deschedule 的请求 |
| [karmada#7895](https://github.com/karmada-io/karmada/issues/7895)、[karmada#5115](https://github.com/karmada-io/karmada/issues/5115) | 多组件工作负载调度的跟踪 issue |
| [kthena#2148](https://github.com/volcano-sh/kthena/issues/2148) | ServingGroup 的持久身份：CR，还是 status 里的账本 |
| [kthena#2079](https://github.com/volcano-sh/kthena/pull/2079) | PR（已合入，在基线里）：`status.unschedulableReplicas` 和 `unschedulableSince` |
