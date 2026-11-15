# ModelServing 滚动与恢复：终稿

本目录是任务「ModelServing 滚动：行为规范审视、状态设计与白盒检视」第二轮的产出，第三轮（2026-11-15）按维护者逐项的决定更新。第一轮的三份文档在上一级目录：[行为规范](../modelserving-rollout-behavior-spec.md)、[状态设计](../modelserving-rollout-state-design.md)、[检视意见](../modelserving-rollout-implementation-review.md)。

全部是文档，没有修改 controller 代码，没有在 Kind 上运行任何东西。

## 文档

| 文档 | 回答的问题 |
| --- | --- |
| [01 复评补充的复核结论](01-rereview-disposition.md) | 2026-11-14 补充的检视意见（提交 `2f157b7e`）哪些成立，哪些要更正，依据是什么 |
| [02 以 Pod 状态变化为入口的检视](02-pod-state-entry-review.md) | 从 Pod 的每一种状态变化出发，当前实现各做了什么，漏了什么 |
| [03 故障 Pod 的处理：社区对照和恢复语义](03-fault-pod-handling-comparison.md) | `None` 改成「未就绪不处理、`Failed` 要处理」之后，和 StatefulSet 等 controller 相比还有哪些行为差异；恢复怎样跨重启 |
| [04 状态机改进方案](04-state-machine.md) | 完整的目标设计：成员、单元、滚动阶段、恢复四层状态，一轮 reconcile，重启 |
| [05 重构方案](05-refactoring-plan.md) | 从当前实现走到目标设计的工作包、顺序、对外变化、验证和证据 |

只想知道结论，读 04 的第 1、2、6 节和 05 的第 1 节。要评审某一条检视意见，从 05 第 5 节的对照表查它落在哪个工作包。

## 基线

| 项 | 值 |
| --- | --- |
| controller | `volcano-sh/kthena` `main` @ `b4e536bb` |
| 契约 | `kthena-rollout-runner` `main` @ `3929bf5`，版本 2.7。复评引用的 2.8～2.12 不在工作区里，终稿没有用它们作依据 |

## 决定

| 决定 | 内容 | 日期 |
| --- | --- | --- |
| D1 | surge 被 N 覆盖时转正，并清除标记 | 2026-11-12 |
| D2 | 受保护的单元发生故障时，占用滚动预算 | 2026-11-12 |
| D3 | SG 模式下修改 `roles[].replicas` 立即应用 | 2026-11-12 |
| D9 | 故障计时不跨重启，新 leader 重新计满 grace | 2026-11-14 |
| D10 | `None`：未就绪的 Pod 不处理；`Failed` 和 `Succeeded` 的 Pod 立即删除并按原名字重建，只动这一个 Pod。`Succeeded` 在所有策略下都和 `Failed` 一样算终态。当时一并定的「带失败退避」由 D14 撤回 | 2026-11-14 |
| D11 | 已经开始的恢复要能跨重启做完，机制从简 | 2026-11-14 |
| D12 | R37 收窄：整单元消失后按序号和 partition 重建 | 2026-11-15 |
| D13 | `Failed`、`Succeeded` 的 Pod 在所有策略、所有 grace（包括 `-1`）下等同于被删除的 Pod：`None` 只重建这一个，范围策略立即重建范围。grace 和 `-1` 只约束重启故障和原地更新超期 | 2026-11-15 |
| D14 | 不做退避。按不做来设计，标为待讨论 | 2026-11-15 |
| D15 | 范围恢复保持两步删除 | 2026-11-15 |

[04](04-state-machine.md) 第 14 节列出的规范和契约条款在 2026-11-15 按四组批准，SG 规范 A.4 第 3 条除外（批准之后才发现，随 O3 讨论）；条款和用例本身还没有改，随实现在同一个版本里改。

待讨论的两项：O3，重建要不要加退避或次数上限（现在按不做设计）；O9，外部删除之后、第一个恢复标记写上之前的窗口。仍然待定的：行为规范的 D4～D8，以及 O4～O7。都在 [04](04-state-machine.md) 第 15 节。

## 三处最重要的变化

1. **Pod 的每一种状态都有归类。**当前实现的事件处理把 `Succeeded`、长期 Pending、没有重启的未就绪都放进「其他」，终止中的直接返回；reconcile 只问「在不在」「Ready 不 Ready」。目标设计用六个成员状态加「缺失」「外来」覆盖全部情形，事件处理和 reconcile 用同一份归类（[04](04-state-machine.md) 第 2 节）。
2. **恢复只由 worker 发出，已经开始的恢复跨重启做完。**当前实现里因故障而发的删除全部出自 informer 回调和 grace 的 goroutine。目标设计把它们搬进 worker，用 Pod 上的一个注解记录「这个范围已被承诺重建」和承诺时范围内的 Pod 数：先标记，后删除；范围内的 Pod 少于这个数就是已经开始，必须做完；一个没少就可以撤销（[04](04-state-machine.md) 6.3）。标记是 Pod 的属性，不改变 Pod 的健康归类。删除沿用现在的两步：故障的 Pod 到期各自被删，其余的 Pod 之后再删。第二步的时机有一处变化：现在是被删的 Pod 里最先消失的那一个消失时，改成范围内正在终止的 Pod 都消失之后。grace 的起点只放在内存里。
3. **终态的 Pod 等同于被删除的 Pod。**`Failed`、`Succeeded` 的 Pod 不会被 kubelet 重新启动，留着它只是永久少一个成员。`None` 下它被单独重建；`RoleRecreate`、`ServingGroupRecreate` 下立即按范围重建，不等 grace，`-1` 也不例外。未就绪和反复重启的 Pod 的处理不变。没有退避，重建出来的 Pod 立刻又进入终态时会连续重建，这是接受的风险，待讨论（[04](04-state-machine.md) 6.1、6.4，[03](03-fault-pod-handling-comparison.md)）。

## 与第一轮文档的关系

三份原文档保留为第一轮评审和复评的记录。下面这些部分由本目录取代。

| 原文档 | 被取代的部分 | 取代它的 |
| --- | --- | --- |
| 行为规范 | R32～R34；4.10 末尾；D9；引用契约 2.8～2.12 的句子 | [04](04-state-machine.md) 第 6、10、14 节 |
| 状态设计 | 第 4、5、6 节 | [04](04-state-machine.md)、[05](05-refactoring-plan.md) |
| 检视意见 | 「复评补充」；第 1 节的统计；第 5 节 | [01](01-rereview-disposition.md)、[02](02-pod-state-entry-review.md)、[05](05-refactoring-plan.md) |

行为规范的 R1～R31、R35～R38，状态设计的第 3 节（当前状态机），检视意见的 49 条意见明细，仍然有效。其中 R37 有一处收窄，已经批准（D12），见 [04](04-state-machine.md) 第 14 节。原文档里需要更正的陈述列在 [01](01-rereview-disposition.md) 第 4 节。

## 证据

| 类型 | 内容 |
| --- | --- |
| 走读 | `b4e536bb` 的代码，契约 2.7，各上游项目的源码 |
| 单元级探针 | 第一轮 79 个测试函数，第二轮 51 个，第三轮 9 个。fake clientset，驱动真实的 controller 函数 |
| 模型 | 滚动的参考模型（第一轮）；恢复规则的推演模型（第二轮建立，第三轮按 D13、D14 更新），以及 D13 的 34 个脚本化用例 |
| 调研 | Kubernetes、OpenKruise、LeaderWorkerSet、RoleBasedGroup、Volcano、JobSet、KubeRay 的源码、文档、KEP、issue 和 PR |
| Kind | 没有 |

证据不在本仓库里，在 workspace 的任务目录 `issues/features/005-modelserving-rollout-spec-state-review-IP/` 下：`subagents/` 是各项分析的完整报告，`probes/` 是探针的源码和输出，`model/` 是两个模型和它们的输出。

没有验证的内容各文档末尾都有一节。最主要的四条：目标设计没有实现；所有关于当前实现的结论都没有 Kind 证据；恢复的规则只在一个不含滚动的模型里推演过，它和滚动、缩容的交互没有模型；恢复标记在回退和再升级时的行为是按规则推出来的。
