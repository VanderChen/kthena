# 待决策点

- 作者：@VanderChen，2026-11-28
- 状态：[01-design.md](01-design.md) 是提案草稿，没有实现。下面是继续往前走之前需要定的事。
- 「建议」是写方案的人的建议，不是已经做出的决定。每项的依据都能在 01、02、04 里找到。

## 1. 先要回答的三个事实问题

这三件事是使用方才知道的事实。它们比继续做分析更能决定方向，所以排在最前面。

| # | 问题 | 为什么重要 | 如果答案是 A | 如果答案是 B |
| --- | --- | --- | --- | --- |
| F1 | 主要用法是哪种：A 按各集群剩余容量动态放置；B 每个集群一份（Duplicated）或固定比例 | 重调度、估算准不准、按组分发，全都只和 A 有关 | 需要决定 D3、D4；E3 值得做 | 第 1 阶段就够用，D3、D4 可以搁置 |
| F2 | 典型部署里每个集群有几个 ServingGroup：A 一两个大组；B 很多组 | 方案 M 下每个集群按自己的份额滚动。每个集群只有一两个组时，这等于所有集群同时停 | 全局滚动编排是硬需求，按组分发（D4）的价值变大 | 方案 M 的按份额滚动够用 |
| F3 | 是否依赖 kthena autoscaler，尤其是按 role 伸缩：A 依赖；B 不依赖 | 按组分发下组数只能在控制面改，而且改 role 副本数会把正在运行的组搬到别的集群（[02](02-servinggroup-cr.md) 3.3） | 按组分发之前要先重新设计自动扩缩容 | 不构成阻碍 |

## 2. 方向

| # | 要定的事 | 选项 | 建议 | 依据 |
| --- | --- | --- | --- | --- |
| D1 | 是否进入第 1 阶段：解释器加使用指南 | 做；不做；改了再做 | 做。它不改任何控制器代码，只用到 Karmada 的稳定能力，也是后面所有方向的前提 | [01](01-design.md) 第 4、5、10 节 |
| D2 | 解释器是否提交为 Karmada 的内置定制 | 现在提交；暂缓 | 暂缓。同一个 kind 只能有一种解释方式，内置之后「ModelServing 按副本拆分」就是 Karmada 的默认行为，以后改成按组分发是破坏性变更。等 D4 定了再提交 | [01](01-design.md) 4.5 |
| D3 | 调度不上的 ServingGroup 怎么换集群 | (a) 改 karmada-descheduler：从聚合状态读不可调度数，有去处才降；(b) 同样的算法做成独立控制器；(c) 改走按组分发，用 Karmada 现有的应用故障转移 | 取决于 F1。答案是 A 时选 (a)：Kind 上不需要改调度器，改动只在一个可选组件里。(b) 留作上游不接受时的后备。(c) 在 D4 的前提满足之前不选 | [01](01-design.md) 第 8 节；[04](04-evidence.md) 6.3、6.4 |
| D4 | 是否改走按组分发（单元是 `replicas: 1` 的 ModelServing） | 现在切换；列为备选；不考虑 | 列为备选。它的估算明显更准，但今天的 Karmada 会把正在运行的单元搬到别的集群，这一条解决之前不能用于生产。和方案 M 在同一个控制面上是二选一 | [02](02-servinggroup-cr.md) 第 3、6、7 节 |
| D5 | 是否为多集群构造 ServingGroup CR | 做；不做 | 不做。按组分发的好处来自「一个组一个绑定」，不来自新 CRD；现有的 CR 原型不能直接当单元用。CR 本身做不做，留给 [kthena#2148](https://github.com/volcano-sh/kthena/issues/2148) | [02](02-servinggroup-cr.md) 第 1、4 节 |
| D6 | Karmada 侧的改动在哪里开发和发布 | 建 Karmada 的 fork 并在上面开发；只在上游提 issue | 进入第 2 阶段时再建 fork。上游合入之前，可以从 fork 构建 karmada-descheduler 镜像给早期使用者，调度器和 controller-manager 保持原版 | [01](01-design.md) 8.4 |

## 3. kthena 现有行为要不要改

下面三处和多集群用法冲突。改它们都会改变现有语义，所以单列出来。建议第 1 阶段都不改，用文档约束；
后两项等第 2 阶段在 Kind 上看到实际后果再定，因为它们会改变 PR #2079 刚定下来的语义。

| # | 现有行为 | 冲突 | 如果要改 |
| --- | --- | --- | --- |
| K1 | 校验按成员对象上的 `spec.replicas` 判断整数 `maxUnavailable`、`partition`、`evictionStrategy.minAvailable` | 模板在单集群合法，拆分后被分到小份额的集群拒绝（Kind 已复现） | 副本数小于这些整数值时改为钳制而不是拒绝 |
| K2 | 滚动升级的 surge 组计入 `unschedulableReplicas` | GPU 已满的集群里，一次滚动可能把这个集群的份额换到别的集群 | surge 组不计入 |
| K3 | 原本在服务的组，新 Pod 调度不上时整组计入 `unschedulableReplicas` | 还在接流量的组可能被整组迁走 | 只统计从未就绪过的组 |

## 4. 对外动作

到目前为止没有向任何上游仓库提交 issue、PR 或评论。下面每一项都只有要点和证据，还没有成文的草稿；起草和发出各自需要单独决定。

| # | 动作 | 对象 | 现有材料 | 建议 |
| --- | --- | --- | --- | --- |
| E1 | 报告 bug：默认资源模型下，请求 GPU 的副本估算为 0，动态拆分调度失败 | karmada-io/karmada | Kind：`artifacts/logs/kind-C2a-gpu-estimate-zero.log`；包级探针 | 报告。它和 ModelServing 无关，普通的 GPU Deployment 也受影响，修掉对这个用法是直接的帮助 |
| E2 | 报告 bug：绑定从非工作负载变成工作负载时，成员的副本数在调度器分配之前被写成 0；`componentResource` 加 `replicaRevision` 是同一个根因的另一种表现 | karmada-io/karmada | Kind：`kind-C10-*.log`、`kind-C10b-*.log`、`kind-C3-*.log` | 报告 |
| E3 | 报告并参与：多组件工作负载重新调度时不把自己占用的资源加回去，运行中的对象被搬走 | karmada-io/karmada，[#7895](https://github.com/karmada-io/karmada/issues/7895) | Kind：`kind-G6-*.log`、`kind-G7-*.log` | F1 的答案是 A 时报告 |
| E4 | 回复 [karmada#8104](https://github.com/karmada-io/karmada/issues/8104) | karmada-io/karmada | [01](01-design.md) 第 12 节的六条要点 | 回复。maintainer 的问题都有了带数据的答案 |
| E5 | 提出 descheduler 的状态来源，和 [PR #8109](https://github.com/karmada-io/karmada/pull/8109)、[#4046](https://github.com/karmada-io/karmada/issues/4046) 协调 | karmada-io/karmada | [01](01-design.md) 8.2、8.4 | 和 D3 一起定 |
| E6 | 给 [kthena#2148](https://github.com/volcano-sh/kthena/issues/2148) 提供多集群这一条输入 | volcano-sh/kthena | [02](02-servinggroup-cr.md) 第 9 节的四条 | 提供。它是讨论里还没有出现过的论据，同时带三条对 CR 的约束 |

## 5. 这些决定之间的依赖

- F1 决定 D3、D4、E3 要不要现在处理。答案是 B 时，做完 D1 就可以停。
- D2 依赖 D4：定了不走按组分发，解释器才适合提交为内置定制。
- D4 的前提是 E3 指向的问题有解（上游修，或者我们修），以及 F3 不构成阻碍。
- D5 不阻塞任何一项：按组分发不需要 CR。它只在 #2148 选了 CR 路线之后才需要重新评估。
- K2、K3 依赖 D3：先有重调度，才看得到它们的实际后果。
