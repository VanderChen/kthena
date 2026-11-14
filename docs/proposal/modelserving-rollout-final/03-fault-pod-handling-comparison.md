# 故障 Pod 的处理：社区对照和恢复语义

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 状态 | 调研结论和决策记录，待评审 |
| 回答的问题 | `recoveryPolicy: None` 改成「未就绪的 Pod 不处理，`Failed` 的 Pod 要处理」之后，和 StatefulSet 等 controller 相比还有哪些行为差异；恢复怎样跨重启 |
| 调研对象 | Kubernetes StatefulSet、ReplicaSet 与 Deployment、DaemonSet、Job；OpenKruise CloneSet 与 Advanced StatefulSet；LeaderWorkerSet；RoleBasedGroup；Volcano Job；JobSet；KubeRay |
| 一手来源 | 全部是源码、官方文档、KEP、issue 和 PR。版本、文件、行号和完整链接在任务目录的 `subagents/r2-community-failed-pod-research.md`，下文只写仓库内路径 |
| 当前实现的基线 | `volcano-sh/kthena` `main` @ `b4e536bb` |
| 配套 | [状态机改进方案](04-state-machine.md)第 6 节是本文结论的落点 |

读取的版本：kubernetes `ad39e9b`、kruise `c6d1066`、lws `c58f5b8`、rbg `ea5e82a`、jobset `17bfd39`、kuberay `e04c80d`、volcano 本地检出 `b231f3e`、kubernetes/website `109ff5a`。

## 1. 结论

1. **删除 `Failed` 的 Pod 并按原名字重建，最直接的先例是 StatefulSet。**它在 reconcile worker 里先判断终态再做别的事，旧 Pod 从 etcd 消失之后的下一轮按同名同序号创建，不等任何 grace。Advanced StatefulSet、LeaderWorkerSet 的 `None`、RoleBasedGroup 的 `None` 都是同样的做法。
2. **`Succeeded` 应当和 `Failed` 一样处理。**Kubernetes 1.27 起，Pod 被删除、被驱逐或节点关机时，kubelet 会先把它置为终态；容器都以 0 退出时是 `Succeeded`。StatefulSet 曾经只处理 `Failed`，结果在 1.27、1.28 上出现回归，1.29 起两种都处理。对长期运行的服务，`Succeeded` 不是「正常完成」。
3. **在「不恢复」一类的策略下，没有任何先例删除未就绪但不是终态的 Pod。**StatefulSet、LeaderWorkerSet 的 `None`、RoleBasedGroup 的 `None`、ReplicaSet、CloneSet 都不动它。D10 的「未就绪不处理」和它们一致。
4. **立即重建终态 Pod 需要退避。**Kubernetes 核心里只有 DaemonSet 给这件事加了退避（1 秒到 15 分钟），代码注释写明原因是 kubelet 反复拒绝 Pod 造成的热循环。StatefulSet 没有，ReplicaSet 的同类问题从 2019 年起一直没有解决。
5. **组级重建在各个项目里都把意图放在 API 对象上，不依赖进程内存。**其中 Volcano Job 的做法和本设计的恢复标记是同一种：删除之前先给每个 Pod 写注解，写不上就不删。
6. **计时器放在内存里有先例。**DaemonSet 的退避和 Volcano Job 的延迟动作都只在内存里，重启后重新计。没有项目为 grace 或退避的起点单独做持久化标记。
7. 改成 D10 之后，本设计和 StatefulSet 仍有十处差别，原因逐条列在[第 4 节](#4-与-statefulset-的行为差异)。

## 2. Pod 是怎样进入终态的

模型服务的 Pod 通常是 `restartPolicy: Always`。容器崩溃时 kubelet 会一直重启它，Pod 保持 Running 或 Pending，不会变成 `Failed`。进入终态靠的是下面这些路径，都来自节点和 kubelet 一侧。

| 路径 | 结果 | 来源（kubernetes `ad39e9b`） |
| --- | --- | --- |
| 节点压力驱逐 | `Failed`，reason 是 `Evicted` | `pkg/kubelet/eviction/eviction_manager.go:654-655` |
| kubelet 准入拒绝：`UnexpectedAdmissionError`、`OutOfcpu`、`OutOfmemory`、`OutOfpods`、`NodeAffinity` 等 | `Failed` | `pkg/kubelet/kubelet.go:251-273`、`:2676-2684` |
| kubelet 为关键 Pod 抢占 | `Failed`，reason 是 `PreemptContainer` | `pkg/kubelet/preemption/preemption.go:115-126` |
| 节点优雅关机 | `Failed`，reason 是 `Terminated` | `pkg/kubelet/nodeshutdown/nodeshutdown_manager.go:162-165` |
| 超过 `activeDeadlineSeconds` | `Failed`，reason 是 `DeadlineExceeded` | `pkg/kubelet/active_deadline.go:68-76` |
| Pod 被删除（kubectl、驱逐 API、污点管理器、调度器抢占） | 1.27 起先置为终态再删除：容器都以 0 退出是 `Succeeded`，否则是 `Failed` | `pkg/kubelet/kubelet_pods.go:1876-1890` |
| 节点失联后的孤儿 Pod | PodGC 标成 `Failed` 后删除 | `pkg/controller/podgc/gc_controller.go:152-229` |

三点推论。

- `None` 下处理终态 Pod，覆盖的是节点一侧的事件。容器反复崩溃仍然属于「未就绪，不处理」。
- 准入拒绝是热循环的来源：调度器选了节点，kubelet 因为资源、亲和性或设备不满足而拒绝，重建之后大概率再被拒绝。
- 被删除的 Pod 会同时带 `deletionTimestamp` 和终态。成员状态的判定顺序把终止中排在终态之前（[状态机](04-state-machine.md) 2.1），所以它不会被当成终态 Pod 再处理一遍。

## 3. 对照

### 3.1 单个 Pod 的各种情形

「删」指 controller 主动删除这个 Pod；「不计」指只是不再把它算作有效副本，不删除。

| Pod 的情形 | StatefulSet | ReplicaSet、Deployment | DaemonSet | Advanced StatefulSet | CloneSet | LeaderWorkerSet `None` | RoleBasedGroup `None` |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `Failed` | 删，同名重建，立即 | 不计，补一个新名字的 Pod | 删，先过退避 | 删，同序号，占 `scaleMaxUnavailable` | 不计 | 同 StatefulSet | 删，同名重建 |
| `Succeeded` | 同 `Failed`（1.29 起） | 不计 | 删，没有退避 | 同 `Failed` | 不计 | 同 StatefulSet | 不处理，继续占着名字 |
| 未就绪，没有重启 | 不动。OrderedReady 下阻塞后续操作 | 不动 | 不动 | 不动 | 不动；缩容和更新时优先选它 | 不动 | 不动 |
| 容器反复重启 | 不动。滚动会卡住 | 不动 | 不动 | 不动 | 不动 | 不动 | 不动，让容器自己重启 |
| 长期 Pending | 不动 | 不动 | 不动 | 不动 | 不动 | 不动 | 不动 |
| 长期停在终止中 | OrderedReady 下阻塞 | 不计，立即补新 Pod | 跳过 | 阻塞 | 不计，补新 Pod | 同 StatefulSet | 不动。另一种策略下会触发整实例重建 |
| 被外部删除 | 同名重建 | 补新名字的 Pod | 在该节点重建 | 同序号重建 | 补新名字的 Pod | 同 StatefulSet | 同名重建 |
| 防热循环 | 没有 | 没有（kubernetes#72593 未解决） | `failedPodsBackoff`，1 秒到 15 分钟，内存 | 没有 | 没有 | 不适用 | `None` 下没有 |
| 谁来做 | reconcile worker | worker | worker | worker | worker | StatefulSet 的 worker | reconcile |

来源：StatefulSet `pkg/controller/statefulset/stateful_set_control.go:421-453`；`IsPodActive` 在 `pkg/controller/controller_utils.go:1092-1096`；DaemonSet `pkg/controller/daemon/daemon_controller.go:868-905` 和 `cmd/kube-controller-manager/app/apps.go:56`；Advanced StatefulSet `pkg/controller/statefulset/stateful_set_control.go:1098-1110`（kruise）；CloneSet `pkg/controller/cloneset/utils/cloneset_utils.go:100-115`；LeaderWorkerSet `api/leaderworkerset/v1/leaderworkerset_types.go:736-738`；RoleBasedGroup `pkg/reconciler/roleinstance/sync/instance_scale.go:152-159` 和 `keps/inactive-pod-handling/README.md`。

ModelServing 的当前实现和本设计放在同一张表里：

| Pod 的情形 | 当前实现，`None` | 本设计，`None` | 当前实现和本设计，`RoleRecreate`、`ServingGroupRecreate` |
| --- | --- | --- | --- |
| `Failed` | 不删除，永久不可用 | 删，同名重建，立即，带退避 | grace 到期后重建范围 |
| `Succeeded` | 不在故障判定里 | 同 `Failed` | 当前实现不处理；本设计同 `Failed` |
| 未就绪，没有重启 | 不动 | 不动 | 不动 |
| 容器反复重启 | 不动 | 不动 | grace 到期后重建范围 |
| 长期 Pending | 不动 | 不动，报告 | 不动；本设计增加报告 |
| 长期停在终止中 | 占着名字 | 占着名字，报告 | 同左 |
| 被外部删除 | 只补这个 Pod | 只补这个 Pod | 不等 grace，重建范围 |
| 防热循环 | 不适用 | 按身份退避 | 当前实现没有；本设计见待决项 O3 |
| 谁来做 | — | worker | 当前实现是 informer 回调和 grace 的 goroutine；本设计是 worker |

当前实现各格的证据见 [02](02-pod-state-entry-review.md)。

### 3.2 组级恢复策略

| | LeaderWorkerSet | RoleBasedGroup | Volcano Job | JobSet | ModelServing |
| --- | --- | --- | --- | --- | --- |
| 取值 | `RecreateGroupOnPodRestart`（默认）、`RecreateGroupAfterStart`、`None` | `RecreateRoleInstanceOnPodRestart`、`None` | `policies` 把事件映射到动作：`RestartJob`、`RestartTask`、`RestartPod` 等 | `failurePolicy` 的规则：`RestartJobSet`、`RestartJob`、`FailJobSet` 等 | `ServingGroupRecreate`、`RoleRecreate`（默认）、`None` |
| 触发 | 容器重启，或 Pod 被删除。不看 `Failed` | `Failed`、被观察到在终止的成员、计划外的容器重启。未就绪和 `Succeeded` 不触发 | `PodFailed`、`PodEvicted`、`PodPending` 加超时等。没有配置 policy 就不触发 | 子 Job 失败 | 未就绪且有重启，或 `Failed`；经过 grace。Pod 被删除则立即 |
| 范围 | 整个组 | 整个 Role 实例 | Job、Task、Pod、分区四档 | 整个 JobSet 或单个子 Job | ServingGroup 或 Role 实例 |
| 时机 | 立即；可选退避 10 秒到 300 秒 | 先过退避，默认 30 秒到 600 秒 | 立即，或 policy 的超时 | 立即 | grace |
| `None` 的含义 | 底层 StatefulSet 的行为：终态 Pod 单独重建，未就绪不动 | `Failed` 单独重建；其余不动 | 没有 `None`。不配置 policy 时终态 Pod 不处理 | 没有 `None` | 本设计：终态 Pod 单独重建，未就绪不动 |

RoleBasedGroup 的 KEP-173 把处理分成两层，可以直接对应本设计：第一层是任何策略下都删除 `Failed` 的 Pod，「follows native K8s StatefulSet behavior」；第二层是策略决定要不要把范围扩大到整个实例。

## 4. 与 StatefulSet 的行为差异

采用 D10 之后，`None` 下对终态 Pod 的处理和 StatefulSet 相同。下表列出其余值得比较的地方。

| 方面 | StatefulSet | ModelServing（本设计） | 差别的原因 |
| --- | --- | --- | --- |
| `Failed` 的 Pod | 删除，同名重建，立即 | `None`：相同。`RoleRecreate`、`ServingGroupRecreate`：等 grace，然后重建整个范围 | ModelServing 的一个单元是多个 Pod 一起工作，策略表达的是「一个坏了，哪些要一起重启」。StatefulSet 没有这个概念 |
| `Succeeded` 的 Pod | 与 `Failed` 相同 | 相同 | — |
| 重建之前等旧 Pod 消失 | 等。旧 Pod 从 etcd 消失后的下一轮才创建 | 相同。同名的 Pod 存在期间不创建 | — |
| 防热循环 | 没有 | 按身份退避 | DaemonSet 的经验；见第 1 节第 4 条 |
| 事件 | 1.27 到 1.30 每轮发 `RecreatingFailedPod`，一次驱逐产生过十条事件（kubernetes#122709）；1.31 起不再发 | 每次实际删除发一条，写明 Pod 的 UID | 需要可观察，又不能刷屏 |
| 未就绪的 Pod 对滚动的影响 | 没有 `maxUnavailable` 时，滚动遇到不可用的 Pod 就停。把模板改对之后，坏的 Pod 也不会被自动替换，要手工删除（文档列出的 forced rollback 限制） | 旧版本的未就绪单元优先被替换，不消耗健康额度（R12）。目标版本的未就绪单元占 `notReadyTarget`，阻塞继续替换（R8） | 行为规范的选择，与 Deployment 的 `cleanupUnhealthyReplicas` 相同 |
| 未就绪的 Pod 对缩容的影响 | OrderedReady 下被未就绪和终止中的 Pod 阻塞；只删最高序号 | 缩容优先选未就绪的单元，允许留下空洞（R20） | 行为规范 D5 |
| 长期停在终止中 | OrderedReady 下阻塞一切；Parallel 下不等 | 占着名字和容量，不阻塞其他单元，在 `status` 里报告 | 单元之间互不等待 |
| 终态 Pod 是否占滚动预算 | `maxUnavailable` 路径把它算作不可用 | 所在单元是未就绪的，按 R8 计数。受保护的单元故障也占用（D2） | D2 记录了 StatefulSet 在 1.38 的变化 |
| `minReadySeconds` | 有。可用要求 Ready 持续一段时间 | 没有。就绪即可用 | 不在这次的范围内；记一笔 |
| 被外部删除 | 同名重建这一个 Pod | `None`：相同。其余策略：重建整个范围 | 同第一行 |
| 存储 | 管理 PVC 的生命周期 | 不涉及 | 不在范围内 |

## 5. 恢复怎样跨重启：先例

| 机制 | 意图放在哪里 | 重启后怎样继续 | 计时器 |
| --- | --- | --- | --- |
| LeaderWorkerSet：删除 leader Pod | leader Pod 的 `deletionTimestamp` 和 foreground 删除；worker StatefulSet 的 owner 是 leader Pod，由 kube-controller-manager 的垃圾回收级联删除 | 不需要 controller 参与 | 退避由持久化的计数和 leader 的创建时间现算 |
| JobSet：递增计数 | `status.restarts`，加子 Job 上的 `restart-attempt` 标签 | 标签小于计数的子 Job 一律删除。完全是水平触发 | 没有 |
| Volcano Job：`RestartJob`、`RestartTask` | Job 的 `status`（`Restarting`、`retryCount`、`version`）；删除前给每个 Pod 写 `volcano.sh/controller-out-of-sync` 注解 | 看到带注解的 Pod 就删除，再补缺失的 | 延迟动作在内存里，重启后丢失 |
| RoleBasedGroup：整实例重建 | `RoleInstance` 的 `status`：`Restarting` condition、`restartCount`、`lastRestartTime` | 每轮先读 `status` | 退避由 `status` 里的计数和时间现算 |
| DaemonSet：`Failed` 的退避 | — | — | 只在内存里，重启后清零 |

来源：lws `pkg/controllers/pod_controller.go:546-731`；jobset `pkg/controllers/failure_policy.go:186-216`、`pkg/controllers/jobset_controller.go:485-520`；volcano `pkg/controllers/job/job_controller_actions.go:179-191`、`:485-487`；rbg `pkg/reconciler/roleinstance/sync/instance_scale.go:423-530`、`:818-881`。

本设计选的是 Volcano Job 这一种：在 Pod 上写一个注解，先写后删（[状态机](04-state-machine.md) 6.3）。另外两种没有选的原因：

| 做法 | 为什么没有选 |
| --- | --- |
| LeaderWorkerSet 的 owner 级联 | ModelServing 的 Pod 都直接属于 ModelServing。要靠垃圾回收级联，得让 worker Pod 只属于 entry Pod，这会改变所有权结构，也覆盖不了 ServingGroup 范围 |
| JobSet 的计数加标签 | 需要给每个 Role 实例和每个 ServingGroup 在 `status` 里各存一个计数，并给每个 Pod 加一个代次标签。ModelServing 的 `status` 目前没有逐单元的字段 |

这些先例都有同一类缺口，本设计也有：controller 没有看到的删除，重启之后无法知道。LeaderWorkerSet 依赖观察到 `deletionTimestamp` 或重启次数；RoleBasedGroup 把「没有被观察到就消失的成员」明确推迟到后续版本处理。

## 6. D10 的决策记录

### 6.1 已经定下来的

| 项 | 决定 | 先例 |
| --- | --- | --- |
| `None` 下未就绪的 Pod，包括容器反复重启的 | 不处理 | 第 1 节第 3 条 |
| `None` 下 `Failed` 的 Pod | 立即删除，按原名字重建，只动这一个 Pod | StatefulSet、RoleBasedGroup `None` |
| `Succeeded` 的 Pod | 与 `Failed` 相同。它在所有策略下都是恢复的触发条件 | StatefulSet 1.29 起、Advanced StatefulSet、KubeRay。RoleBasedGroup 是反例 |
| 失败退避 | 要有。按身份退避，只在内存里 | DaemonSet |

这个决定相对已发布的 v1.1.0 是一次反转，版本之间的经过和升级时要说明的事项见[重构方案](05-refactoring-plan.md) 6.2。

### 6.2 退避的形状

取 DaemonSet `failedPodsBackoff` 的形状，参数取 LeaderWorkerSet 的量级。

| 设计点 | 取法 | 理由 |
| --- | --- | --- |
| 推迟哪个动作 | 推迟删除，不推迟创建 | 等待期间终态的 Pod 留在原地，现场还在。终态的 Pod 不占节点资源 |
| 什么时候清零 | 按时间过期：上限的两倍时间里没有再失败 | 就绪即清零的话，「起来几十秒又被驱逐」的 Pod 会一直按最小间隔循环。DaemonSet 不在就绪时清零 |
| 键 | ModelServing 的 UID、Pod 的名字、单元的 revision | 各位置互不影响；修好模板之后从头开始。DaemonSet 的键里带 `observedGeneration` |
| 第一次 | 不等待 | 与 D10 的「立即」一致 |
| `Succeeded` | 计入 | 一条规则。DaemonSet 对 `Succeeded` 不退避 |
| 间隔 | 10 秒起，翻倍，上限 5 分钟 | 模型服务的 Pod 启动慢、占 GPU，1 秒的起点没有意义。LeaderWorkerSet 用 10 秒到 300 秒，DaemonSet 用 1 秒到 15 分钟 |
| 存放 | 内存 | D9。重启后归零，等待中的终态 Pod 会在新 leader 的第一轮里被删除 |

### 6.3 待定的三个细节

编号与[状态机改进方案](04-state-machine.md)第 15 节相同。

**O1 `restartGracePeriodSeconds: -1` 下的终态 Pod。**当前实现里 `-1` 和 `None` 对故障的处理完全相同，都不删除。D10 只改了 `None`，没有说 `None` 加 `-1` 时谁优先。

| 做法 | 结果 |
| --- | --- |
| (a) `-1` 在所有策略下都表示故障一律不删，包括 `None` 下的终态 Pod | `-1` 的含义不变，用了 `-1` 的用户不受影响。`None` 加 `-1` 就是 v1.1.0 的 `None`，升级说明里可以写「想保留失败的 Pod，加上 `-1`」。代价：`-1` 下终态 Pod 造成的问题（占预算、原地更新卡住）仍然存在，需要 condition 说明原因 |
| (b) `-1` 像 `None` 那样单独重建终态 Pod | 终态 Pod 在任何配置下都不会永久占着位置。代价：没有任何配置能保留失败的 Pod；同一个策略下「终态只换一个 Pod、被删除则重建整个范围」要靠区分删除的来源，重启之后分不清；`ServingGroupRecreate` 表达的是组内一起启动，只换一个 Pod 可能得到「Pod 都就绪但组不可用」 |
| (c) `-1` 下立即重建配置的范围 | `-1` 只对重启故障有意义。改变已发布的含义 |

建议 (a)。

**O2 `RoleRecreate`、`ServingGroupRecreate` 下终态 Pod 是否跳过 grace。**grace 的含义是等待原地恢复，终态 Pod 不可能原地恢复。

| 做法 | 结果 |
| --- | --- |
| (a) 保持不变，等满 grace | 与已发布的行为相同。等待是白等，但客观上起了去抖的作用。D10 之后，`None` 对终态 Pod 的反应会比 `RoleRecreate` 加正数 grace 更快 |
| (b) 跳过 grace，立即重建范围 | 恢复更快。终态 Pod 和被外部删除的 Pod 得到同样的处理：两者都是「这个 Pod 不会回来了」。终态可以随时重新观察，跨重启不需要任何记忆。去抖交给退避 |

建议 (b)，和 O3 一起做。它改变已发布的行为，需要契约条款的批准。

**O3 范围重建是否也加退避。**grace 为 0 时，范围重建和单 Pod 重建有同样的热循环：重建出来的 Pod 立刻又失败，又重建整个范围。当前实现没有保护，探针里 50 毫秒完成了 8 轮。采纳 D10 之后，最轻的策略有保护，更重的策略反而没有。LeaderWorkerSet 和 RoleBasedGroup 都给整组重建加了退避。建议加，键是范围的身份，形状与 6.2 相同。

## 7. 调研范围和缺口

- 没有找到先例的：在「不恢复」一类的策略下删除未就绪的 Pod；为终态 Pod 的重建持久化退避起点；StatefulSet 对 `Failed` 重建的专属退避。
- 读代码得出、没有运行验证的：LeaderWorkerSet 默认策略下 `Failed` 的 worker 经由 StatefulSet 的删除间接触发整组重建；RoleBasedGroup 里 `Succeeded` 的 Pod 继续占着名字；Volcano Job 和 RoleBasedGroup「先删 Pod、后写 status」在崩溃窗口里丢计数。
- 没有确认的：Job 的 `podFailurePolicy` 内部；kubelet 的 `ContainerRestartRules` 对终态判定的影响；`MaxUnavailableStatefulSet` 门控在所读提交上的默认状态；KubeRay 是否有退避；本地 Volcano 检出的提交是否在上游。
- Argo Rollouts、KServe 建立在 ReplicaSet 和 Deployment 之上，没有单独调研。
