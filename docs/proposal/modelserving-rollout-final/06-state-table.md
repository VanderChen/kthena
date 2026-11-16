# ModelServing 状态总表：对照代码

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 读 controller 代码时对照的一张表。每一种状态变化占一行：谁入队，worker 里哪个函数判断、哪个函数动手，之后是什么状态，本设计里怎么变。这些内容原来分散在几份文档的状态图里 |
| 状态 | 索引，不是规则。「入队侧」「worker 侧」「之后」三列是当前代码的走读结果；「本设计」一列没有实现 |
| 代码基线 | `volcano-sh/kthena` `main` @ `b4e536bb` |
| 以谁为准 | 当前实现以基线代码为准，更细的说明在[状态设计](../modelserving-rollout-state-design.md)第 3 节和 [Pod 状态入口的检视](02-pod-state-entry-review.md)；目标设计以[状态机改进方案](04-state-machine.md)为准；重构后的代码结构以[重构方案](05-refactoring-plan.md)第 3 节为准。本文和它们有出入时是本文的错误 |
| 范围 | ServingGroup 层和独立 Role。协调 Role（roleCoordination）只标出入口函数，内部算法不展开。plugin、PodGroup、Service 只标出入口 |

代码位置的缩写沿用其他几份文档，都在 `pkg/model-serving-controller/controller/` 下：`msc.go` 是 `model_serving_controller.go`，`ipu.go` 是 `inplace_update.go`，`surge.go` 是 `surge_rollout.go`，`aux.go` 是 `auxiliary_cleanup.go`，`obs.go` 是 `coordinated_role_observation.go`，`coord.go` 是 `role_rolling_update_coordinator.go`，`rh.go` 是 `revision_history.go`，`pdt.go` 是 `pod_deletion_targets.go`。本文另用 `binpack.go` 指 `binpack_scaledown.go`。[第 4 节](#4-函数索引)的位置写成 `文件:行`，行号是函数定义所在的行；4.6 和[第 5 节](#5-入队点)的行号是判断和调用所在的行。

## 1. 代码骨架

controller 只有两侧。入队侧是 informer 回调，出队侧是 worker。两侧之间是一个 workqueue 和一份内存里的 datastore：回调和 worker 都读写 datastore 里的单元状态。

```text
入队侧：informer 回调，都在 msc.go
  ModelServing    addModelServing、updateModelServing ─► 入队
                  deleteModelServing                   不入队，清内存记录
  Pod 新增、更新   addPod ─► updatePod ─► 五个分支之一                                   P1～P19
                    原地更新故障、故障   handleErrorPod ─► go handlePodAfterGraceTime ─► DELETE Pod   F1～F3
                    原地更新中           handleInPlaceUpdatingPod
                    就绪                 handleReadyPod
                    其他                 只在协调模式下入队
  Pod 删除         deletePod ─► handleDeletedPod ─► deleteServingGroup 或 DeleteRole，在回调里删除   P12、F4～F7
  Service、PodGroup 删除   deleteService、deletePodGroup ─► 入队
  启动             Run ─► syncAll：对每个 Pod 调 addPod，再把每个 ModelServing 入队

          │  enqueueModelServing、enqueueModelServingAfter
          ▼
  workqueue：key 是 namespace/name，同一个 key 串行

出队侧：worker
  worker ─► processNextWorkItem ─► syncModelServing
    1  cleanupOrphanServices
    2  finishPendingDeletions             对 Deleting 的单元再执行一次删除          U6
    3  desiredRevision                    解析并持久化目标 revision
    4  syncServingGroupReplicas           ServingGroup 的转正、扩缩容、建 surge     R1～R4
    5  syncRoleReplicas                   逐组逐 Role 调 manageRoleReplicasPerGroup  R5～R7、U2、U3
    6  manageRollingUpdateWithPolicy      原地更新收尾，替换，回收 surge            R8～R11
    7  updateModelServingStatusInRound    写 status                                 R12、R13
```

行尾是第 2 节里对应的行。三件事读代码时要先知道。

- **datastore 里存着单元状态。**ServingGroup 有 `Creating`、`Running`、`Scaling`、`Deleting`，Role 实例有 `Creating`、`Running`、`Deleting`。回调、worker 和启动时的 `syncAll` 都在写。本设计不存这些状态，每轮从 Pod 推导（[04](04-state-machine.md) 第 3 节）。
- **因故障而发的删除不经过 worker。**它们出自 `handlePodAfterGraceTime` 这个 goroutine 和 `deletePod` 回调。例外只有原地更新超期：`handleInPlaceTimeout` 在 worker 里直接删除没打上补丁的 Pod（P16）。本设计里所有写请求都由 worker 发出（04 的 C1）。
- **worker 是边判断边执行的。**第 4、5、6 步各自读 datastore、各自发请求，先创建后删除，一步出错返回就跳过后面的步骤和 `status`，例外见 T2。本设计先只读地观察、分类、记账、计划，再统一执行，最后写 `status`（04 第 4 节）。

回调和 worker 共用的内存状态有下面几样，读函数时会反复遇到。

| 载体 | 内容 | 谁写 | 什么时候清 | 相关的行 |
| --- | --- | --- | --- | --- |
| datastore 的单元记录 | 每个 ServingGroup 和 Role 实例的状态和 revision | 回调、worker、`syncAll` | 单元删完之后移除；ModelServing 删除时清空 | U1～U6 |
| datastore 的 Running 集合 | 每个 ServingGroup 里走过「就绪」分支的 Pod 名字。它不是单元的 `Running` 状态：worker 判断单元是否 Ready 时按 informer 里的 Pod 重算，不读这个集合 | `handleReadyPod` 加入；`markPodUnavailable`、`deletePod` 移出 | 随单元记录 | P2、P12、U3 |
| `initialSync` | 初始同步是否已经做完。为假时 `addServingGroupAndRoleFromPod` 才登记单元 | `syncAll` 结束时置为真 | 不清 | P1、P19、U1 |
| `graceMap` | 正在计时的故障 Pod，键是名字加 UID | `handleErrorPod` | `handlePodAfterGraceTime` 结束时删除。Pod 恢复 Ready 时不删除 | P5、F1、F2 |
| `podDeletions` | 一次单元删除要删的 Pod：第一次调用时 LIST 一次，记下名字和 UID，之后的重试只删这些，不再取快照。快照之外又出现的同范围 Pod 留着不删，发 `DeletionIdentityChanged` 事件 | `deleteServingGroup`、`DeleteRole` | 单元从 datastore 移除时；ModelServing 删除时 | U5、U6、F4、F5 |
| `roleCleanups` | plugin 的删除钩子要用的现场，在删除 Pod 之前由 `prepareRoleCleanup` 记下 | `deleteServingGroup`、`DeleteRole` | 同上 | U5、U6 |

## 2. 状态总表

各段的列相同。

| 列 | 内容 |
| --- | --- |
| 看到什么 | 状态变化，或 worker 里成立的条件 |
| 入队侧 | 当前代码里 informer 回调和它启动的 goroutine 做什么。写「入队」指调用 `enqueueModelServing`；全部入队点列在[第 5 节](#5-入队点) |
| worker 侧 | 当前代码里 `syncModelServing` 的哪个函数判断、哪个函数动手 |
| 之后 | datastore 里的单元状态，接下来走哪一行 |
| 本设计 | 状态机改进方案里对应的处理，括号里是 04 的章节 |

行号的前缀：M 是 ModelServing 的变化，P 是 Pod 的变化，U 是单元状态，R 是扩缩容和滚动，F 是故障恢复，T 是时间、错误和重启。

### 2.1 ModelServing 的变化

| # | 看到什么 | 入队侧 | worker 侧 | 之后 | 本设计 |
| --- | --- | --- | --- | --- | --- |
| M1 | ModelServing 新建 | `addModelServing`：入队 | 整轮 `syncModelServing`。还没有单元，走 R2 | 见 R2 | 相同。第一轮看到正式单元少于 N（04 5.1） |
| M2 | `spec` 变化：`replicas`、模板、partition、`roles[].replicas`、策略 | `updateModelServing`：新旧 `spec` 不同才入队。去掉网络拓扑时在回调里清理 PodGroup | 整轮。没有「改了什么」的记录：每一步拿最新的 `spec` 和 datastore 里的单元比较，落到 R 段的某一行 | 见 R 段 | `generation` 变化就入队，回调不调用 API（04 第 9 节）。同样不记改了什么，每轮按观察判断（04 5.1） |
| M3 | 只有 `status`、标签或注解变化 | `updateModelServing`：`spec` 相同，返回 | — | — | 相同 |
| M4 | ModelServing 删除 | `deleteModelServing`：清掉 datastore、`podDeletions`、`roleCleanups` 里它的记录，不入队 | 队列里还有它的 key 时，`syncModelServing` 读不到对象，返回 nil | Pod 靠 ownerReference 级联删除 | 入队（04 第 9 节） |

### 2.2 Pod 的变化

行号与 04 2.4、[02](02-pod-state-entry-review.md) 第 3 节的表相同，可以逐行对照。

`updatePod` 在分类之前有四个直接返回的检查：Pod 带 `deletionTimestamp`；事件已经过期，也就是 lister 里没有这个 Pod、UID 不同或它正在删除；找不到所属的 ModelServing，也就是 Pod 没有 ModelServing 名或组名的标签，或者 lister 里已经没有这个 ModelServing，M4 之后它的 Pod 的事件都在这里返回；`shouldSkipHandling` 为真，也就是 owner 的 UID 不符，或组名、Role 实例名不是 controller 生成的形式。通过之后按顺序落进五个分支之一：原地更新故障、原地更新中、就绪、故障、其他。

| # | 看到什么 | 入队侧 | worker 侧 | 之后 | 本设计 |
| --- | --- | --- | --- | --- | --- |
| P1 | 创建后还没 Ready | `updatePod` 的「其他」分支：只在协调模式下入队。初始同步期间 `addServingGroupAndRoleFromPod` 登记单元，之后什么都不做 | `manageRoleReplicasPerGroup` 用 `rolePodsComplete` 判断：在位，不 Ready | 实例和组 `Creating` | 成员「未就绪」。入队，等待（04 2.4 第 1 行） |
| P2 | 第一次 Ready | 「就绪」分支，`handleReadyPod`：调 plugin 的 `OnPodReady`，出错时直接返回，不记 Running 集合也不入队；记入 Running 集合；实例的 Pod 都 Ready 时实例置 `Running`；全组 Ready 并且组不是 `Deleting` 时组置 `Running` 并入队，每个这样的 Ready 事件都入队；组还没 Ready 而实例这次刚变 `Running` 时也入队。读不到 Pod 的历史 revision 时跳过 plugin，1 秒后再入队 | `syncRoleReplicas` 每轮重算实例和组的 Ready；`manageRollingUpdateWithPolicy` 按 `Running` 的数量重算额度，走 R8～R10 | 实例和组 `Running` | 成员「就绪」。回调只入队；单元状态和额度每轮现算，不等同批的其他单元（04 第 3 节、R10） |
| P3 | 不再 Ready，没有重启 | 「其他」分支：不改 datastore，不入队，协调模式除外 | 有别的事件带来一轮时，`syncRoleReplicas` 把实例和组降为 `Creating` | datastore 和 `status` 在下一轮之前仍把它算作可用 | 成员「未就绪」。入队，`status` 随即反映；不删除（04 2.4 第 3 行） |
| P4 | 容器计划外重启，并且不 Ready | 「故障」分支，`handleErrorPod`，走 F1 或 F3 | 不参与删除。下一轮看到单元 `Creating` | 见 F1、F3 | 成员「重启故障」。回调只入队；恢复生效时由 `recover` 计时，不生效时不处理（04 6.1） |
| P5 | 重启之后恢复 Ready | 「就绪」分支，同 P2。`graceMap` 里的记录不清除 | 同 P2 | 实例和组 `Running`。计时的 goroutine 结束之前同一个 Pod 再次故障，沿用旧的计时 | 成员「就绪」。结束这次计时，再次故障重新计（04 6.4） |
| P6 | `phase` 变成 `Failed` | 同 P4 | 认为它在位，不补建 | 恢复开启时走 F1、F2；`None` 或 grace 为 `-1` 时永久保留 | 成员「终态」。`None` 下立即删除并重建这一个；范围策略下不等 grace，重建范围（04 6.1，D13） |
| P7 | `phase` 变成 `Succeeded` | 「其他」分支：不改 datastore，不入队，协调模式除外 | 下一轮单元变成 `Creating`，之后一直如此 | 任何策略下都不处理 | 同 P6 |
| P8 | 长期 Pending，没有重启 | 同 P1 | 同 P1 | 没有恢复，没有专门的报告 | 成员「未就绪」。不处理，`status` 里报告 `PodNotRecovered`（04 5.1） |
| P9 | 被本进程以外的操作者请求删除 | `updatePod` 第一个检查就返回：不改 datastore，不入队 | — | 没有别的事件带来一轮时，终止期间 `status` 仍把它算作可用。消失时走 P12 | 成员「终止中」。回调生成外部删除记录并入队，由 `recover` 按策略处理（04 6.5） |
| P10 | 被本进程请求删除 | 同 P9 | 删除整个单元时（缩容、替换、回收 surge、范围恢复），发 DELETE 之前 `deleteServingGroup` 或 `DeleteRole` 已经把单元置为 `Deleting`。只删一个 Pod 的两处不改单元状态：F2 的 goroutine，P16 里没打上补丁的 Pod | 整单元删除时单元 `Deleting`，见 U5；只删一个 Pod 时它消失后走 P12 | 成员「终止中」。UID 记进「不算外部删除的删除」，等它消失（04 6.5） |
| P11 | 长期停在终止中 | 没有事件 | 有一轮时：本进程删的，`finishPendingDeletions` 对 `Deleting` 的单元再执行一次删除；别人删的，`rolePodsComplete` 认为它在位、不 Ready | 本进程删的单元停在 `Deleting`，别人删的停在 `Creating`；都占着序号，不重建 | 成员「终止中」，占着名字和容量。不强制删除，报告 `PodStuckTerminating`（04 2.4 第 11 行、5.1） |
| P12 | 对象消失，controller 看到了这次删除 | `deletePod`，依次是：`recreatedSinceDeletion` 为真只入队；移出 Running 集合；`shouldSkipHandling` 为真返回；`handleDeletionInProgress` 发现单元是 `Deleting`，入队后返回；否则 `handleDeletedPod`，走 F4～F6 | `finishPendingDeletions` 接着删；单元从 datastore 移除之后走 R2、R5 重建，或走 R6 补 Pod | 见 F4～F6 | 回调只记录删除并入队。`recover` 核对记录、承诺范围，`execute` 发删除（04 6.5、6.6） |
| P13 | 对象消失，controller 没有看到 | 没有事件 | 下一轮走 R6 补建 | 实例 `Creating` | 成员「缺失」。只补这一个 Pod（04 2.4 第 13 行） |
| P14 | 原地更新的正常过程 | 打过补丁、还没完成时走「原地更新中」分支，`handleInPlaceUpdatingPod`：`markPodUnavailable`，入队。成员完成后走「就绪」分支并入队 | R11 | 单元从 `Creating` 回到 `Running`，revision 更新 | 成员「原地更新中」，完成后「就绪」。单元全部成员完成后翻转版本（04 第 7 节） |
| P15 | 原地更新期间计划外重启，或 `Failed` | 「原地更新故障」分支，排在最前，Pod 当前 Ready 也进这里；`handleErrorPod`，走 F1 或 F3 | R11 | 见 F1、F3 | 成员「重启故障」或「终态」，同 P4、P6 |
| P16 | 原地更新超过进度期限 | 没有 Pod 事件。期限由 `reconcileInPlaceUnit` 和 `enforceInPlaceDeadline` 用 `enqueueModelServingAfter` 定时入队 | `handleInPlaceTimeout`：发 `InPlaceUpdateTimeout` 事件。恢复开启时，没打上补丁的 Pod 直接 DELETE，没完成的 Pod 交给 `handleErrorPod`；恢复关闭时只发事件 | 见 F1、F2 | 恢复的一种触发，由 `recover` 处理（04 6.1） |
| P17 | 标签或注解变化 | 按当时的健康状况走普通的更新事件 | — | — | 入队。用来确认在途的写操作（04 4.1） |
| P18 | 外来的同名 Pod；ownerReference 被摘掉；身份标签被改 | 外来 Pod 的事件被处理器的 owner 过滤和 `shouldSkipHandling` 跳过。摘掉 owner 被 `FilteringResourceEventHandler` 转成一次 `deletePod` | `createPod` 遇到 AlreadyExists 而身份不符：返回错误，并在 1 秒后再入队 | 整个 ModelServing 的这一轮中止，`status` 不更新 | 成员「外来」。只阻塞所在的单元，报告 `IdentityConflict`；不算删除（04 2.3、6.5） |
| P19 | controller 启动后第一次看到 | `Run` 等缓存同步之后调 `syncAll`：对每个 Pod 调 `addPod`，再把每个 ModelServing 入队，然后才启动 worker。informer 的 Add 事件也会对同一个 Pod 调 `addPod`，每个 Pod 处理两遍 | 第一轮从 `syncAll` 留下的 datastore 开始。故障的和终止中的 Pod 不登记单元，全是这类 Pod 的单元第一轮不在 datastore 里（状态设计 3.9） | 故障的 Pod 在 `syncAll` 期间就开始计时，grace 为 0 时 DELETE 早于第一轮 | 没有 `syncAll`。第一轮和之后的每一轮相同（04 第 10 节） |

### 2.3 单元状态

当前代码把 ServingGroup 和 Role 实例的状态存在 datastore 里，下面是每一种转移由谁写。本设计没有这些字段，「本设计」一列是每轮推导时对应的单元状态（04 第 3 节）。

| # | 看到什么 | 入队侧 | worker 侧 | 之后 | 本设计 |
| --- | --- | --- | --- | --- | --- |
| U1 | 单元不存在，变成 `Creating` | 初始同步期间 `addServingGroupAndRoleFromPod`；之后是第一个 Ready 的 Pod，`handleReadyPod` | ServingGroup：`scaleUpServingGroups` 创建之后登记。Role 实例：`scaleUpRoles`、`CreatePodsForServingGroup` | `Creating` | 不需要登记。单元状态每轮按成员推导 |
| U2 | `Creating` 或 `Scaling`，变成 `Running` | `handleReadyPod`：`checkRoleReady`、`checkServingGroupReady` 通过 | `syncRoleReplicas` 每轮重算；`manageRoleReplicasPerGroup` 用 `rolePodsComplete` 重算实例；`updateModelServingStatusInRound` 再查一次 | `Running`，计入可用 | 「就绪」：全部必需成员就绪 |
| U3 | `Running`，变成 `Creating` | `markPodUnavailable`，调用方是 `handleErrorPod`、`handleInPlaceUpdatingPod`、`handleDeletedPod` 的 `None` 分支；另有 `handleDeletedPod` 的 `RoleRecreate` 分支 | `syncRoleReplicas` 每轮重算；历史 revision 读不到时；`startInPlaceUpdate` 打补丁时 | `Creating`，不计入可用 | 「未就绪」：任一成员不再就绪或缺失。原地更新期间是「原地更新中」 |
| U4 | 组内 Role 扩缩容，组变成 `Scaling` | — | `scaleUpRoles`、`scaleDownRoles` | `Scaling`，Ready 之后回到 `Running` | 没有这个状态。新增的实例就绪之前组是「未就绪」（R22） |
| U5 | 变成 `Deleting` | `handleDeletedPod` 调 `deleteServingGroup` 或 `DeleteRole`，在回调线程里 | `deleteServingGroup`、`DeleteRole`，调用方是缩容、替换、回收 surge | `Deleting`：不参与 Ready 重算，不补 Pod，Ready 事件不会把它改回 `Running` | 「终止中」：现存成员全部在终止；或所在范围的恢复已经开始；或本轮已经决定删除它 |
| U6 | `Deleting`，Pod 全部消失；ServingGroup 还要等 PodGroup 消失 | `deletePod` → `handleDeletionInProgress`：入队 | `finishPendingDeletions` 再调一次 `deleteServingGroup` 或 `DeleteRole`：`isServingGroupDeleted` 或 `isRoleDeleted` 为真时从 datastore 移除，并入队 | 单元不存在，下一轮可以重建 | 「不存在」：没有任何 Pod |

### 2.4 扩缩容和滚动

这一段的判断都在 worker 里。「入队侧」一列写的是通常由哪一行的入队带来这一轮。配置了 roleCoordination 时，worker 侧另有 `withCoordinatedRound`、`persistCoordinatedRoleRevision`、`resolveRoleRolloutPolicy`、`refreshCoordinatedAvailabilityInRound`、`finishCoordinatedSurge`、`retireCoordinatedSurge`，不在这张表里展开。

| # | 看到什么 | 入队侧 | worker 侧 | 之后 | 本设计 |
| --- | --- | --- | --- | --- | --- |
| R1 | 带 surge 标记的单元，序号小于 N | M2，调大了 N | `syncServingGroupReplicas` → `adoptServingGroupSurge` → `clearSurgeMarker`。Role 层是 `manageRoleReplicasPerGroup` → `adoptRoleSurge` | 标记清除，成为正式单元。清除失败时 1 秒后再入队 | 转正：从这一轮起算正式单元，执行阶段最先清标（04 4.3，R3） |
| R2 | ServingGroup 数少于期望数 | M1、M2；U6，被删的组移除之后入队 | `syncServingGroupReplicas`：组数是 datastore 里的全部组，包含 `Deleting` 的。期望数是 N；R4 成立时是 N + S；没有可更新的旧组而还有带标记的 surge 组时由 `expectedWithSurge` 决定，通常是 N 加上还没回收的 surge 数，这样最后腾出的序号才补得回来。组数不足时 `scaleUpServingGroups`：`forEachMissingOrdinal` 从低到高补缺失的序号，序号小于 P 用 `status.currentRevision`，它为空时和其余序号一样用目标 revision；`CreatePodsForServingGroup` → `createPod` | 新组 `Creating` | 阶段 2：正式单元少于 N 时，在 `[0, N)` 的空闲序号上创建，补到正式单元数等于 N 或 `active` 达到 N + S 为止（04 5.1） |
| R3 | ServingGroup 数多于期望数。组数包含 `Deleting` 的组，缩容的候选不包含 | M2，调小了 N | `syncServingGroupReplicas` → `scaleDownServingGroups`：跳过 `Deleting` 的组，`calculateServingGroupScore` 按未就绪、deletion cost、序号排序，对多出的调 `deleteServingGroup` | 被选中的组 `Deleting` | 阶段 1：只在正式单元里选。被删的单元消失之前不建 surge、不替换（04 5.1，R24） |
| R4 | 有可更新的旧组，也就是序号不小于 P、模板不同；S 大于 0；不是原地更新 | M2，改了模板或调低了 P | `syncServingGroupReplicas`：`hasUpdateableOutdatedServingGroup` 为真时期望数加 S，`scaleUpServingGroups` 在 `[0, N + S)` 里补最低的缺失序号，序号不小于 N 的带 surge 标记。Role 模式对应的是 `manageRoleReplicasPerGroup`：有可更新的旧实例时期望的实例数加该 Role 的 S，再走 R5 | surge 组或 surge 实例 `Creating` | 阶段 3 建 surge：只建在序号不小于 N 的位置；要求 `workRecreate` 不空，不在缩容门控中（04 5.1、5.3） |
| R5 | 组里某种 Role 的实例数不等于期望数 | M2，改了 `roles[].replicas`；U6 | `syncRoleReplicas` → `manageRoleReplicasPerGroup`：多了调 `scaleDownRoles` → `DeleteRole`；少了调 `scaleUpRoles`，每创建一个实例入队一次。`scaleUpRoles` 选 revision：序号小于该 Role 的 P 时用 `status.currentRevision`，它为空或等于传入的 revision 时看现存的受保护实例：有别的 revision 就用它，不止一个时不建，一个都没有时用传入的 revision；否则用传入的 revision，ServingGroup 模式下是组已应用的 revision，Role 模式下是目标 revision | 组 `Scaling`；实例 `Deleting` 或 `Creating` | Role 层的阶段 1、2（04 5.1、第 8 节，R22） |
| R6 | 实例里缺 Pod | P12、P13 | `manageRoleReplicasPerGroup`：`rolePodsComplete` 为假时 `createPodsByRoleWithContext` → `createPod` | 实例 `Creating` | 补员，版本按 04 6.7 选（04 4.2 第 5 步） |
| R7 | 单元的记录还在，一个 Pod 都没有 | U6、P12 | ServingGroup 模式：`syncRoleReplicas` 用 `unitHasPods` 判断，按序号和 P 重新选 revision；选出的和记录的不同时换掉 datastore 里的记录、刷新 PodGroup、调 `CreatePodsForServingGroup` 重建整组，相同时不在这里处理，由 R6 逐实例补 Pod。Role 模式：`manageRoleReplicasPerGroup` 对实例同样重新选 revision，不同时换掉记录并入队，Pod 由 R6 补 | `Creating`，revision 可能改变 | 单元「不存在」，由阶段 2 按序号和 P 选版本创建（R15）。空掉的是临时单元时由阶段 3 重新判断要不要它（04 6.7） |
| R8 | ServingGroup 模式，有可更新的旧组 | M2；P2，组就绪后入队，额度随之释放 | `manageRollingUpdateWithPolicy`：`maxScaleDown` 是组数减去 N − U，再减去不可用数；不可用数是序号不小于 P、不是 `Running`、模板也不是「不同」的组，加上序号小于 P、不是 `Running`、模板读不出的组。没有额度就返回。`deleteOutdatedServingGroups` 先取不是 `Running` 的旧组，再取 `Running` 的，各自从序号高的取 → `updateServingGroup` → `decideInPlace`：重建调 `deleteServingGroup`，原地调 `startInPlaceUpdate`。`Deleting` 的旧组和已经打过补丁的旧组都在不是 `Running` 的旧组里，每轮被重新选中、各占一个额度：`deleteServingGroup` 对前者只是再执行一次删除，`decideInPlace` 对后者直接返回。当前代码靠这个给在途的替换记账，预算里没有单独的在途项；`decideInPlace` 返回等待时同样占一个额度 | 组 `Deleting`，或原地更新中 | 阶段 4 替换：`maxScaleDown` 和 `maxHealthyScaleDown` 两个额度，未就绪的候选先选（04 5.3） |
| R9 | Role 模式，有可更新的旧实例 | 同 R8 | `manageRollingUpdateWithPolicy` → `deleteOutdatedRoles`：逐组调 `rolesToDeleteForRoleRollingUpdateInRound`，额度是 `roleRolloutBudget`，候选是 `selectRoleRolloutCandidates` → `updateRole` → `DeleteRole` 或 `startInPlaceUpdate` | 实例 `Deleting`，或原地更新中 | 同 R8，范围是一个 ServingGroup 里的一种 Role（04 第 8 节） |
| R10 | 没有可更新的旧单元，还有带 surge 标记的单元 | P2 | ServingGroup 模式：`manageRollingUpdateWithPolicy` → `retireSurgeServingGroups`，`Running` 的 surge 要有额度才删；在这之前 `syncServingGroupReplicas` 用 `expectedWithSurge` 让期望数暂时留着 surge 的位置。Role 模式：`rolesToDeleteForRoleRollingUpdateInRound` → `surgeRolesToRetire` → `DeleteRole` | surge `Deleting` | 阶段 3 回收 surge：未就绪的直接删除，就绪的要求删除之后 `ready` 不低于 `minAvailable`（04 5.1） |
| R11 | 单元在原地更新中 | P14；进度期限的定时入队 | `manageRollingUpdateWithPolicy` 开头的 `completeInPlaceUpdates` → `reconcileInPlaceUnit`，依次是：有成员在终止就等；目标改回了 Pod 标签上的 revision 调 `recreateInPlaceUnit`；还有没打补丁的成员调 `resumeInPlaceUnit`，超期并且恢复开启时改调 `handleInPlaceTimeout`；目标换成了别的版本时不完成，只按 `enforceInPlaceDeadline` 看期限；全部完成则翻转标签并入队；其余情况 `enforceInPlaceDeadline`，到期调 `handleInPlaceTimeout` | 完成后单元的 revision 更新 | 续补和完成是计划的输出 `continueInPlace`、`completeInPlace`；超期并且恢复生效时交给恢复，不生效时继续续补、只报告超期（04 5.2、2.4 第 16 行） |
| R12 | 每一轮的末尾 | — | `updateModelServingStatusInRound`：`replicas` 数 datastore 里的全部组，包含 `Deleting` 的；`availableReplicas` 数 `Running` 的组；`updatedReplicas` 数不是 `Deleting`、模板与目标等价的组；写 `status`。前面的步骤出错返回时这一步不执行，例外见 T2 | — | `status` 放在最后，出错的轮次也写（04 4.4） |
| R13 | 没有待回收的 surge，各组的 Role 实例数齐全，`updatedReplicas`、`availableReplicas`、组数都等于 N | — | `updateModelServingStatusInRound`：把 `currentRevision` 推进到目标 revision。条件里没有单独判断 P。`currentRevision` 为空时不等这个条件：没有没更新的组就直接取目标 revision，新建的 ModelServing 第一轮就是这样；有没更新的组时取其中数量最多的那个 revision。之后只在条件成立时推进 | 之后序号小于 P 的新单元用新的基线 | 基线只在全量完成时推进：P 为 0，N 大于 0，单元数等于 N，全部是正式单元、没在删除、就绪、目标版本（04 5.2，R17） |

### 2.5 故障恢复

「恢复开启」指 `recoveryDisabled` 为假：策略不是 `None`，并且 grace 不是 `-1`。

| # | 看到什么 | 入队侧 | worker 侧 | 之后 | 本设计 |
| --- | --- | --- | --- | --- | --- |
| F1 | 故障的 Pod，恢复开启 | `handleErrorPod`：`graceMap` 里已有它的记录就返回；否则写入记录，`markPodUnavailable`，启动 goroutine `handlePodAfterGraceTime`，入队 | 不参与 | 单元 `Creating`，计时中 | 故障起点记在内存里，每轮由 `recover` 现算是否到期，没到期用 `AddAfter`（04 6.4） |
| F2 | grace 到期，或 grace 是 0 | goroutine `handlePodAfterGraceTime`：grace 大于 0 时睡到期，重读 Pod，仍然不 Ready 或还在原地更新中才 DELETE；grace 是 0 时立即 DELETE，不重读。DELETE 带 UID 前置条件，失败只打日志 | 不参与 | Pod 终止中，消失时走 P12 | DELETE 由 worker 发出，失败会重试。范围策略下先给范围内的 Pod 写恢复标记，再删到期的故障成员（04 6.3 的 M1、M2） |
| F3 | 故障的 Pod，恢复关闭 | `handleErrorPod`：只 `markPodUnavailable`，入队 | 不删除，不补建 | 单元 `Creating`。重启故障的 Pod 自己恢复 Ready 时走 P5；`Failed` 的 Pod 一直留着，直到有人删除它 | 重启故障：不处理，报告 `PodNotRecovered`。终态：照样处理，见 P6（D13） |
| F4 | 被删的 Pod 消失，策略是 `ServingGroupRecreate` | `handleDeletedPod` → `deleteServingGroup`：在回调里把组置为 `Deleting`，删除 PodGroup 和组里其余的 Pod | `finishPendingDeletions` 接着删；全部消失后走 U6、R2 | 组 `Deleting`，之后重建 | `recover` 承诺 ServingGroup 这个范围，分两步删除，清空后按 04 6.7 重建（04 6.3、6.6） |
| F5 | 同上，策略是 `RoleRecreate` | `handleDeletedPod`：组是 `Deleting` 时调 `deleteServingGroup`，通常走不到，`deletePod` 前一步的 `handleDeletionInProgress` 已经拦住；否则组若是 `Running` 先降为 `Creating`，再调 `DeleteRole` 删除实例里其余的 Pod | `finishPendingDeletions` 接着删；实例移除后走 U6、R5 | 实例 `Deleting`，之后重建 | 同 F4，范围是 Role 实例 |
| F6 | 同上，策略是 `None` | `handleDeletedPod`：`markPodUnavailable`，入队 | R6 补这一个 Pod | 实例 `Creating` | 相同：只补这一个 Pod。两种例外见 04 6.3：已经开始的范围照样做完；范围带着残留标记时发生的外部删除 |
| F7 | 迟到或重复的删除事件 | `deletePod`：`recreatedSinceDeletion` 为真时只入队；`handleDeletionInProgress` 为真时入队后返回 | — | — | 记录按 Pod 的 UID 去重，由 worker 核对之后丢弃或承诺（04 6.5） |
| F8 | 恢复做到一半 controller 重启 | `syncAll` 重放每个 Pod 的事件，`graceMap` 是空的，重新计满 grace | `Deleting` 的标记已经丢失，按缺口补建 | 取决于重启的时刻：故障的 Pod 还在终止就做完整个范围，已经消失就只补缺的 | 已经开始的范围靠 Pod 上的恢复标记做完；计时重新计满（04 6.3 的 M3，第 10 节） |

### 2.6 时间、错误和重启

| # | 看到什么 | 入队侧 | worker 侧 | 之后 | 本设计 |
| --- | --- | --- | --- | --- | --- |
| T1 | 一轮返回 nil | — | `processNextWorkItem`：`Forget` | 等下一次入队 | 相同 |
| T2 | 一轮返回错误 | — | `processNextWorkItem`：`AddRateLimited`。出错那一步之后的步骤和 `status` 都被跳过。两类错误例外，记下来、把这一轮做完、最后才返回：`cleanupOrphanServices` 的错误；`syncRoleReplicas` 里只是历史 revision 读不到的错误，由 `onlyRevisionErrors` 判断，以及本轮各步记在 revision 历史里的同类错误 | 限速重试 | 错误只阻塞出错的单元，其他单元和 `status` 照常；最后汇总返回，限速重试（04 4.3、4.4） |
| T3 | 要等一段时间再看 | grace 是 goroutine 里的 `time.Sleep` | `enqueueModelServingAfter`：原地更新的期限用剩余时间，PodGroup 重试用管理器给出的间隔；清标失败、身份冲突、`handleReadyPod` 读不到 Pod 的历史 revision、协调模式的几处用固定的 1 秒 | — | 都用 `AddAfter` 剩余时间，没有固定 1 秒的轮询，没有 goroutine 里的睡眠（04 第 9 节） |
| T4 | Service 或 PodGroup 被删除 | `deleteService`、`deletePodGroup`：入队 | 下一轮补建。PodGroup 由 `syncServingGroupReplicas` 末尾的 `createOrUpdatePodGroupByServingGroup` 补建或刷新 | — | 入队 owner（04 第 9 节） |
| T5 | 周期 resync | 没有，两个 informer factory 的 resync 周期都是 0 | — | 丢了事件就没有下一轮 | 有，10 分钟量级，只作兜底（04 第 9 节） |
| T6 | controller 启动 | 见 P19 | — | — | 见 P19 |

## 3. worker 一轮的顺序

当前代码和本设计各一列，按执行顺序。编号与第 1 节的骨架相同，「协调」几行只在配置了 roleCoordination 时有内容。

| 顺序 | 当前代码 | 做什么 | 本设计的对应 |
| --- | --- | --- | --- |
| 入口 | `processNextWorkItem` | 取 key，调 `syncModelServing`，按返回值 `Forget` 或 `AddRateLimited` | 相同 |
| 入口 | `syncModelServing` 开头 | 从 lister 读 ModelServing，读不到返回 nil | 观察（04 4.1）：读 `spec`，从 informer 取全部 Pod，叠加在途的写操作 |
| 1 | `cleanupOrphanServices` | 清理孤儿 Service，错误合并进返回值 | 辅助资源不在 04 的范围内 |
| 2 | `finishPendingDeletions` | 对每个 `Deleting` 的组和 Role 实例再执行一次删除。出错返回 | 没有这一步。滚动、缩容的删除靠每轮重新选中，恢复的删除靠恢复标记（04 6.3） |
| 协调 | `withCoordinatedRound` | 只在协调模式下有内容：建立这一轮直读的观察 | 协调用的观察与其他部分是同一份（04 第 8 节） |
| 3 | `desiredRevision` | 解析并持久化目标 revision。失败时只做 `scaleDownOnRevisionError` 然后返回错误 | 观察的一部分。解析不出目标时只写副本计数和一个 condition（04 4.4）。当前的缩容是否保留是 04 的待决项 O12 |
| 协调 | `persistCoordinatedRoleRevision`、`scaleDownServingGroups`、`refreshCoordinatedAvailabilityInRound` | 只在协调模式下有内容：记副本数的基线；先做 ServingGroup 的缩容；刷新各 Role 实例的可用性 | 协调内部的算法不在 04 的范围内 |
| 4 | `syncServingGroupReplicas` | 转正；算期望的组数；`scaleUpServingGroups` 或 `scaleDownServingGroups`；刷新 PodGroup。出错返回 | 分类、记账之后，计划 ServingGroup 层的阶段 1、2 和建 surge（04 5.2） |
| 协调 | `resolveRoleRolloutPolicy` | 算各组各 Role 的限额。没有配置协调时也调，返回空的策略 | 协调模式的候选函数（04 第 8 节） |
| 5 | `syncRoleReplicas` | 逐组：ServingGroup 模式下整组没有 Pod、并且该换 revision 时重建；每种 Role 调 `manageRoleReplicasPerGroup`，依次是转正、算期望数、缩容、逐实例（重算 Ready、补 Pod、调 plugin 的 `OnRoleSync`）、扩容；最后重算组的 Ready | 分类（04 第 3 节）；计划 Role 层的阶段 1、2；补员（04 4.2） |
| 6 | `manageRollingUpdateWithPolicy` | `completeInPlaceUpdates`；ServingGroup 模式算 `maxScaleDown`，回收 surge 或替换；Role 模式调 `deleteOutdatedRoles` | 记账（04 第 4 节的图、5.3）；计划阶段 3 的回收、阶段 4 和在途的原地更新 |
| — | — | — | 执行（04 4.3）：按固定的顺序发请求，逐个动作复核，错误隔离到单元 |
| 7 | `updateModelServingStatusInRound` | 算副本计数和 revision，写 `status` | 写 `status`（04 4.4）：正常和出错两条路径都经过 |

故障恢复基本不在当前的 worker 里：计时和删除都在入队侧（F 段）。例外是原地更新超期，`handleInPlaceTimeout` 在 worker 里直接删除没打上补丁的 Pod，并对没完成的 Pod 调 `handleErrorPod`（P16）。本设计把恢复放进计划：带标记的范围排在两层的滚动计划之前，新的触发排在之后（04 4.2）。

## 4. 函数索引

「线程」一列：回调指 informer 回调，worker 指 `syncModelServing` 之内，两者都调的写两个。「重构后」一列是[重构方案](05-refactoring-plan.md)第 3 节的文件名，决定和发请求分到了不同的块；它只说明这份职责归到哪一块，不表示保留当前的每一个分支。「见」一列是第 2 节里用到这个函数的行，从函数找回状态用。

### 4.1 入队侧

| 函数 | 位置 | 线程 | 做什么 | 重构后 | 见 |
| --- | --- | --- | --- | --- | --- |
| `NewModelServingController` | `msc.go:122` | 启动 | 注册四类事件处理器。Pod、Service、PodGroup 的处理器先按 ownerReference 过滤 | `events.go` | 第 1 节 |
| `addModelServing` | `msc.go:267` | 回调、启动 | 入队 | `events.go` | M1 |
| `updateModelServing` | `msc.go:277` | 回调 | `spec` 变了才入队 | `events.go` | M2、M3 |
| `deleteModelServing` | `msc.go:308` | 回调 | 清内存记录，不入队 | `events.go` | M4 |
| `addPod` | `msc.go:343` | 回调、启动 | 调 `updatePod` | `events.go` | P 段、P19 |
| `updatePod` | `msc.go:347` | 回调 | 四个前置检查，五个分支 | 入队在 `events.go`，分类在 `classify.go` | P 段 |
| `shouldSkipHandling` | `msc.go:2508` | 回调 | owner 的 UID 和身份形式的检查 | `classify.go` 的「外来」 | P12、P18 |
| `handleReadyPod` | `msc.go:2137` | 回调 | 调 plugin；记入 Running 集合；实例和组置 `Running`；入队 | 入队在 `events.go`，就绪的判断在 `classify.go` | P2、P5、U1、U2 |
| `checkRoleReady` | `msc.go:3614` | 回调 | `checkRoleReadyInRound` 的包装 | `classify.go` | P2、U2 |
| `checkServingGroupReady` | `msc.go:2388` | 回调 | 组里每种 Role 的实例是否都 `Running`，是 `checkServingGroupReadyWithContext` 的包装 | `classify.go` | P2、U2 |
| `handleErrorPod` | `msc.go:2247` | 回调、worker | 标记不可用；写 `graceMap`；启动 grace 的 goroutine；入队 | `recover.go` | P4、P6、P15、P16、F1、F3 |
| `recoveryDisabled` | `msc.go:2242` | 回调、worker | 策略是 `None`，或 grace 是 `-1` | `recover.go` | P16、F1、F3 |
| `markPodUnavailable` | `msc.go:2278` | 回调、worker | 移出 Running 集合；实例和组从 `Running` 降为 `Creating` | 去掉，单元状态不存储 | P14、U3、F1、F3、F6 |
| `handlePodAfterGraceTime` | `msc.go:2308` | goroutine | 睡到 grace 到期，重读，DELETE Pod | 计时在 `recover.go`，DELETE 在 `execute.go` | F2 |
| `handleInPlaceUpdatingPod` | `msc.go:435` | 回调 | 标记不可用，入队 | 入队在 `events.go` | P14、U3 |
| `addServingGroupAndRoleFromPod` | `msc.go:421` | 回调、启动 | 初始同步期间登记单元 | 去掉，由 `observe.go` 取代 | P1、P19、U1 |
| `deletePod` | `msc.go:443` | 回调 | 过期判断；移出 Running 集合；单元在删除就入队；否则 `handleDeletedPod` | 记录删除并入队在 `events.go` | P12、F4～F7 |
| `recreatedSinceDeletion` | `msc.go:510` | 回调 | 范围内的 Pod 是否都是这次删除之后创建的 | `recover.go` | P12、F7 |
| `handleDeletionInProgress` | `msc.go:2563` | 回调 | 单元是 `Deleting` 时入队，并让调用方返回 | 去掉 | P12、U6、F7 |
| `handleDeletedPod` | `msc.go:2352` | 回调 | 按 `recoveryPolicy` 调 `deleteServingGroup`、`DeleteRole`，或只标记并入队 | `recover.go` | P12、F4～F6 |
| `deleteService` | `msc.go:534` | 回调 | 入队 | `events.go` | T4 |
| `deletePodGroup` | `msc.go:570` | 回调 | 入队 | `events.go` | T4 |
| `enqueueModelServing` | `msc.go:607` | 两侧 | `workqueue.Add` | 不变 | 各行的「入队」 |
| `enqueueModelServingAfter` | `msc.go:617` | 两侧 | `workqueue.AddAfter` | 不变 | P16、T3 |
| `Run` | `msc.go:736` | 启动 | 启动 informer，等缓存同步，`syncAll`，启动 worker | 去掉 `syncAll` 这一步 | P19、T6 |
| `syncAll` | `msc.go:767` | 启动 | 对每个 Pod 调 `addPod`，把每个 ModelServing 入队 | 去掉 | P19、F8 |

### 4.2 出队侧：主流程

| 函数 | 位置 | 做什么 | 重构后 | 见 |
| --- | --- | --- | --- | --- |
| `worker` | `msc.go:627` | 循环调 `processNextWorkItem` | 不变 | 第 3 节 |
| `processNextWorkItem` | `msc.go:632` | 取 key，调 `syncModelServing`，`Forget` 或 `AddRateLimited` | 不变 | T1、T2 |
| `syncModelServing` | `msc.go:651` | 一轮 reconcile，顺序见第 3 节 | 按管线拆开 | 第 3 节 |
| `cleanupOrphanServices` | `aux.go:77` | 清理孤儿 Service | 不在 04 的范围内 | 第 3 节、T2 |
| `finishPendingDeletions` | `aux.go:45` | 对 `Deleting` 的单元再执行一次删除 | 去掉 | P11、P12、U6、F4、F5 |
| `desiredRevision` | `rh.go:339` | 解析并持久化目标 revision | 解析在 `observe.go`。持久化 ControllerRevision 是写请求，05 第 3 节没有写它归哪一块 | 第 3 节 |
| `scaleDownOnRevisionError` | `msc.go:893` | 目标 revision 解析不出时只做缩容 | 当前的特殊路径。本设计在目标解析不出时只写副本计数和一个 condition（04 4.4），是否保留这条缩容是 04 的待决项 O12 | 第 3 节 |
| `onlyRevisionErrors` | `rh.go:48` | 一个错误是否只由历史 revision 读不到组成 | 不需要：错误按单元隔离（04 4.3） | T2 |
| `updateModelServingStatusInRound` | `msc.go:2679` | 算副本计数和 revision，写 `status` | `status.go` | R12、R13、U2 |

### 4.3 出队侧：扩缩容、创建和删除

这一段里有几个函数回调线程也在调：`deleteServingGroup`、`DeleteRole` 以及它们调用的 `prepareRoleCleanup`、`isServingGroupDeleted`、`isRoleDeleted`，调用方是 `handleDeletedPod`（F4、F5）；`rolePodsComplete`、`checkRoleReadyInRound`、`checkServingGroupReadyWithContext`，调用方是 `handleReadyPod`（P2）。其余只在 worker 里。

| 函数 | 位置 | 做什么 | 重构后 | 见 |
| --- | --- | --- | --- | --- |
| `syncServingGroupReplicas` | `msc.go:795` | ServingGroup 的转正、期望数、扩容、缩容、PodGroup | `plan.go`，请求在 `execute.go` | R1～R4、R10 |
| `hasUpdateableOutdatedServingGroup` | `msc.go:874` | 有没有序号不小于 P、模板不同的组 | `plan.go` | R4 |
| `compareServingGroupTemplate` | `rh.go:391` | 一个组的模板和目标比较，结果有三种：等价、不同、未知。未知不授权替换，未就绪时在预算里按不可用计 | `classify.go` 的版本 | R4、R8、R12 |
| `scaleUpServingGroups` | `msc.go:982` | 补缺失的序号，按序号和 P 选 revision，创建整组 | `plan.go`，请求在 `execute.go` | R2、R4、U1 |
| `forEachMissingOrdinal` | `msc.go:3012` | 从低到高枚举缺失的序号 | `plan.go` | R2、R4 |
| `scaleDownServingGroups` | `msc.go:3040` | 排序后删除多出的组 | `plan.go`，请求在 `execute.go` | R3 |
| `calculateServingGroupScore` | `binpack.go:158` | 缩容的排序依据：未就绪、deletion cost、序号 | `plan.go` | R3 |
| `syncRoleReplicas` | `msc.go:1029` | 逐组处理 Role；ServingGroup 模式下整组没有 Pod、并且该换 revision 时重建；重算组的 Ready | `plan.go`、`classify.go` | R5、R7、U2、U3 |
| `manageRoleReplicasPerGroup` | `msc.go:1378` | 一个组里一种 Role：转正、算期望数、缩容、逐实例（重算 Ready、补 Pod、调 plugin 的 `OnRoleSync`）、扩容 | `plan.go`、`classify.go` | R1、R4～R7、U2 |
| `scaleUpRoles` | `msc.go:1198` | 创建 Role 实例 | `plan.go`，请求在 `execute.go` | R5、U1、U4 |
| `scaleDownRoles` | `msc.go:1135` | 排序后删除多出的 Role 实例 | `plan.go`，请求在 `execute.go` | R5、U4 |
| `rolePodsComplete` | `rh.go:274` | 实例的 Pod 是否在位，是否都 Ready | `classify.go` | P1、R6、U2 |
| `unitHasPods` | `rh.go:295` | 单元是否还有 Pod：先查缓存，缓存里没有时直读 apiserver 确认 | `observe.go` | R7 |
| `checkServingGroupReadyWithContext` | `msc.go:2392` | 组里每种 Role 的实例是否都 `Running` | `classify.go` | U2 |
| `checkRoleReadyInRound` | `msc.go:2424` | 用 `rolePodsComplete` 判断实例的 Pod 是否在位并且都 Ready；协调模式下按直读的 Pod | `classify.go` | P2、U2 |
| `CreatePodsForServingGroup` | `msc.go:3131` | 创建一个组的全部 Role 实例 | `execute.go` | R2、R7、U1 |
| `createPodsByRoleWithContext` | `msc.go:3186` | 创建一个 Role 实例的 entry 和 worker Pod | `execute.go` | R6 |
| `createPod` | `msc.go:3218` | 发 CREATE；AlreadyExists 时核对身份 | `execute.go` | P18、R2、R6 |
| `createOrUpdatePodGroupByServingGroup` | `msc.go:3389` | 创建或刷新一个组的 PodGroup；需要稍后重试时定时入队 | 辅助资源，不在 04 的范围内 | T3、T4 |
| `deleteServingGroup` | `msc.go:3318` | 组置为 `Deleting`；删除 PodGroup 和 Pod；全部消失后从 datastore 移除并入队 | `execute.go` | R3、R8、R10、F4、U5、U6 |
| `DeleteRole` | `msc.go:1660` | 实例置为 `Deleting`；删除 Pod；全部消失后从 datastore 移除并入队 | `execute.go` | R5、R9、R10、F5、U5、U6 |
| `prepareRoleCleanup` | `aux.go:215` | 删除 Pod 之前记下 plugin 删除钩子要用的现场 | plugin 钩子，不在 04 的范围内 | 第 1 节的载体表 |
| `isServingGroupDeleted` | `msc.go:2574` | 缓存里是否已经没有这个组的 Pod 和 PodGroup | `observe.go` | U6 |
| `isRoleDeleted` | `msc.go:2598` | 缓存里是否已经没有这个 Role 实例的 Pod | `observe.go` | U6 |

### 4.4 出队侧：滚动、surge 和原地更新

| 函数 | 位置 | 做什么 | 重构后 | 见 |
| --- | --- | --- | --- | --- |
| `manageRollingUpdateWithPolicy` | `msc.go:1729` | 原地更新收尾；候选分类；ServingGroup 模式的额度、回收 surge、替换；Role 模式转给 `deleteOutdatedRoles` | `budget.go`、`plan.go` | R8～R11 |
| `deleteOutdatedServingGroups` | `msc.go:1858` | 从尾部取 `maxScaleDown` 个组，调 `updateServingGroup` | `plan.go` | R8 |
| `deleteOutdatedRoles` | `msc.go:1881` | 逐组选出要替换的 Role 实例，调 `updateRole` 或 `DeleteRole` | `plan.go` | R9 |
| `rolesToDeleteForRoleRollingUpdateInRound` | `msc.go:1958` | 一个组里各 Role 的候选：替换的，和要回收的 surge | `plan.go` | R9、R10 |
| `outdatedRolesInRound` | `msc.go:2095` | 一种 Role 里模板不同的实例和不可用数，受 P 保护的由调用方去掉。独立 Role 把不是 `Running` 的排在前面，再按序号从高到低；它会把入参里正在删除的实例改标成 `Deleting` | `plan.go` | R9 |
| `compareRoleTemplate` | `rh.go:478` | 一个 Role 实例的模板和目标比较，结果同样是等价、不同、未知 | `classify.go` 的版本 | R9 |
| `hasUpdateableOutdatedRoleInRound` | `msc.go:1580` | 一种 Role 有没有序号不小于 P、模板不同的实例 | `plan.go` | R4、R9、R10 |
| `roleRolloutBudget` | `obs.go:128` | Role 的两个额度 | `budget.go` | R9 |
| `selectRoleRolloutCandidates` | `coord.go:154` | 在额度内选 Role 实例；协调模式的入口也在这里 | `plan.go`、`plan_coordination.go` | R9 |
| `adoptServingGroupSurge` | `surge.go:135` | 序号小于 N 的带标记的组，清除标记 | `plan.go` | R1 |
| `adoptRoleSurge` | `surge.go:150` | 同上，Role 实例 | `plan.go` | R1 |
| `clearSurgeMarker` | `surge.go:168` | 发 PATCH 清除 surge 注解 | `execute.go` | R1、T3 |
| `surgeServingGroups` | `surge.go:91` | 哪些组是 surge：序号不小于 N 并且有带标记的 Pod | `classify.go` | R4、R10 |
| `surgeRoles` | `surge.go:112` | 哪些 Role 实例是 surge：序号不小于该 Role 的 N 并且有带标记的 Pod | `classify.go` | R4、R10 |
| `expectedWithSurge` | `surge.go:193` | 没有可更新的旧单元时，期望数还要不要留着 surge | `plan.go` | R10 |
| `retireSurgeServingGroups` | `surge.go:204` | 回收 surge 组 | `plan.go` | R10 |
| `surgeRolesToRetire` | `surge.go:237` | 回收 surge 的 Role 实例 | `plan.go` | R10 |
| `updateServingGroup` | `ipu.go:144` | 替换一个组：原地还是重建 | `plan.go`，请求在 `execute.go` | R8、R11 |
| `updateRole` | `ipu.go:173` | 替换一个 Role 实例：原地还是重建 | `plan.go`，请求在 `execute.go` | R9、R11 |
| `decideInPlace` | `ipu.go:203` | 一个单元能不能原地更新：等待、原地、重建 | `classify.go` 的更新方式 | R8、R9 |
| `startInPlaceUpdate` | `ipu.go:309` | 给单元的 Pod 打补丁 | `execute.go` | R8、R9、U3 |
| `completeInPlaceUpdates` | `ipu.go:406` | 逐个单元调 `reconcileInPlaceUnit`，没有在途更新的单元只做修复 | `plan.go` | R11 |
| `reconcileInPlaceUnit` | `ipu.go:462` | 等待、目标改回、续补、目标变化、完成、期限 | `plan.go`，超期在 `recover.go` | P16、R11 |
| `resumeInPlaceUnit` | `ipu.go:758` | 给没打上补丁的成员续补 | `plan.go` | R11 |
| `recreateInPlaceUnit` | `ipu.go:765` | 目标改回原版本时删除整个单元 | `plan.go` | R11 |
| `enforceInPlaceDeadline` | `ipu.go:717` | 没到期就定时入队，到期调 `handleInPlaceTimeout` | `recover.go` | P16、R11 |
| `handleInPlaceTimeout` | `ipu.go:732` | 发事件；没打上补丁的 Pod 直接 DELETE，没完成的交给 `handleErrorPod` | `recover.go` | P16、R11 |

### 4.5 协调模式的入口

| 函数 | 位置 | 做什么 | 重构后 | 见 |
| --- | --- | --- | --- | --- |
| `resolveRoleRolloutPolicy` | `coord.go:770` | 算各组各 Role 的限额 | `plan_coordination.go` | 2.4 的说明 |
| `refreshCoordinatedAvailabilityInRound` | `obs.go:64` | 按直读的 Pod 刷新实例的可用性 | `observe.go` | 2.4 的说明 |
| `finishCoordinatedSurge` | `obs.go:278` | 没有可更新的旧实例而还有 surge 时：先补回缺的正式实例，再调 `retireCoordinatedSurge`。建 surge 不在这里，在 `scaleUpRoles` 里 | `plan.go`，与独立 Role 合并成一套 surge；协调的门控在 `plan_coordination.go`，请求在 `execute.go` | 2.4 的说明 |
| `retireCoordinatedSurge` | `obs.go:316` | 协调模式回收 surge | `plan.go`，与独立 Role 合并成一套 surge；协调的门控在 `plan_coordination.go`，请求在 `execute.go` | 2.4 的说明 |
| `withCoordinatedRound` | `coordinated_round.go:59` | 建立协调模式的一轮：缓存一次直读的 Pod LIST 和读到的 ControllerRevision | `observe.go` | 2.4 的说明 |
| `persistCoordinatedRoleRevision` | `coord.go:317` | 协调模式下把各 Role 当前的 `replicas` 记到目标 ControllerRevision 的注解上，作为副本数的基线；内容没变就不写 | 05 第 3 节没有写。它是写请求，不在 `plan_coordination.go` 的纯函数里 | 2.4 的说明 |

### 4.6 大函数里常查的位置

第 2 节提到的几处判断在比较长的函数里，这里给出行的范围。

| 找什么 | 位置 | 见 |
| --- | --- | --- |
| `updatePod` 的五个分支 | `msc.go:384-419` | P 段 |
| ServingGroup 的期望数 | `msc.go:801-828` | R2～R4、R10 |
| 整组没有 Pod 时重新选 revision | `msc.go:1072-1107` | R7 |
| 每轮重算组的 Ready | `msc.go:1114-1126` | U2、U3 |
| Role 的期望实例数 | `msc.go:1408-1429` | R4、R5 |
| 每轮重算 Role 实例的 Ready，补 Pod | `msc.go:1527-1543` | U2、R6 |
| 替换候选的分类和不可用数 | `msc.go:1765-1788` | R8 |
| `maxScaleDown` | `msc.go:1808-1837` | R8、R10 |
| 候选的拼接和取法 | `msc.go:1840`、`msc.go:1868` | R8 |
| 基线推进的条件，`currentRevision` 为空时的取法 | `msc.go:2829-2857` | R13 |
| `status` 的副本计数 | `msc.go:2866-2871` | R12 |

## 5. 入队点

非测试代码里调用 `enqueueModelServing` 的有 22 处，调用 `enqueueModelServingAfter` 的有 18 处，加上 `processNextWorkItem` 里的 `AddRateLimited`。下表一处一行或几处并一行，位置是调用所在的行。标「回调」的 Pod 入队点，启动时也由 `syncAll` → `addPod` 走到。

| 函数 | 位置 | 线程 | 什么时候 | 方式 | 见 |
| --- | --- | --- | --- | --- | --- |
| `addModelServing` | `msc.go:274` | 回调、启动 | ModelServing 新建；启动时 `syncAll` 对每个 ModelServing 调一次 | 立即 | M1、P19 |
| `updateModelServing` | `msc.go:305` | 回调 | `spec` 变了 | 立即 | M2 |
| `updatePod` | `msc.go:399` | 回调 | 「就绪」分支，Pod 带在途的原地更新 | 立即 | P14 |
| `updatePod` | `msc.go:414` | 回调 | 「其他」分支，配置了 roleCoordination | 立即 | P1、P3 |
| `handleInPlaceUpdatingPod` | `msc.go:440` | 回调 | 「原地更新中」分支 | 立即 | P14 |
| `handleReadyPod` | `msc.go:2147` | 回调 | 读不到 Pod 的历史 revision | 1 秒后 | P2、T3 |
| `handleReadyPod` | `msc.go:2201` | 回调 | 协调 Role 的实例这次刚变 `Running` | 立即 | P2 |
| `handleReadyPod` | `msc.go:2226` | 回调 | 全组 Ready，组不是 `Deleting` | 立即 | P2 |
| `handleReadyPod` | `msc.go:2233` | 回调 | 组还没 Ready，实例这次刚变 `Running` | 立即 | P2 |
| `handleErrorPod` | `msc.go:2253` | 回调 | 恢复关闭 | 立即 | F3 |
| `handleErrorPod` | `msc.go:2271` | 回调、worker | 恢复开启，开始计时 | 立即 | F1 |
| `deletePod` | `msc.go:469` | 回调 | `recreatedSinceDeletion` 为真 | 立即 | F7 |
| `handleDeletionInProgress` | `msc.go:2568` | 回调 | 单元是 `Deleting` | 立即 | P12、U6 |
| `handleDeletedPod` | `msc.go:2383` | 回调 | 策略是 `None` | 立即 | F6 |
| `deleteService` | `msc.go:567` | 回调 | Service 被删除 | 立即 | T4 |
| `deletePodGroup` | `msc.go:604` | 回调 | PodGroup 被删除 | 立即 | T4 |
| `deleteServingGroup` | `msc.go:3337` | 回调、worker | 删除过程出错 | 立即 | U5 |
| `deleteServingGroup` | `msc.go:3384` | 回调、worker | 组从 datastore 移除之后 | 立即 | U6 |
| `DeleteRole` | `msc.go:1681` | 回调、worker | 删除过程出错 | 立即 | U5 |
| `DeleteRole` | `msc.go:1714` | 回调、worker | 实例从 datastore 移除之后 | 立即 | U6 |
| `scaleUpRoles` | `msc.go:1292` | worker | 每创建一个 Role 实例 | 立即 | R5 |
| `scaleUpRoles` | `msc.go:1290` | worker | 同上，协调模式另加一次 | 1 秒后 | R5 |
| `scaleUpRoles` | `msc.go:1227`、`msc.go:1242`、`msc.go:1310`、`msc.go:1316`、`msc.go:1321`、`msc.go:1362` | worker | 协调模式下读观察或创建实例出错 | 1 秒后 | 2.4 的说明 |
| `manageRoleReplicasPerGroup` | `msc.go:1400` | worker | 协调模式下转正出错 | 1 秒后 | R1 |
| `manageRoleReplicasPerGroup` | `msc.go:1515` | worker | 空的 Role 实例换了 revision | 立即 | R7 |
| `createPod` | `msc.go:3300` | worker | 同名 Pod 已存在而身份不符 | 1 秒后 | P18 |
| `createOrUpdatePodGroupByServingGroup` | `msc.go:3404` | worker | PodGroup 需要稍后重试 | 管理器给出的间隔 | T3 |
| `clearSurgeMarker` | `surge.go:183` | worker | 清除 surge 标记失败 | 1 秒后 | R1 |
| `reconcileInPlaceUnit` | `ipu.go:514` | worker | 还有没打补丁的成员，期限没到 | 剩余时间后 | P16、R11 |
| `reconcileInPlaceUnit` | `ipu.go:555` | worker | 单元的原地更新完成 | 立即 | R11 |
| `enforceInPlaceDeadline` | `ipu.go:720` | worker | 期限没到 | 剩余时间后 | P16 |
| `finishCoordinatedSurge` | `obs.go:281`、`obs.go:288` | worker | 读带标记的 Pod 出错（`obs.go:281`）；还有带标记的 surge 实例时每轮都定时一次（`obs.go:288`） | 1 秒后 | 2.4 的说明 |
| `retireCoordinatedSurge` | `obs.go:319`、`obs.go:372` | worker | 读带标记的 Pod 出错（`obs.go:319`）；`DeleteRole` 出错（`obs.go:372`） | 1 秒后 | 2.4 的说明 |
| `processNextWorkItem` | `msc.go:646` | worker | 一轮返回错误 | `AddRateLimited` | T2 |
