# 在网版本的不断服升级

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 已经在运行负载的集群，怎样从旧版本升级到本设计，而已运行的 Pod 不重启、不重建、Ready 不掉。也评估回退 |
| 覆盖的源版本 | 两个：**在网版本**（生产环境运行的分支）和 **v1.1.0**（上游发布版）。它们在对象上的形态、对 owner 变化的反应、选主的行为都不一样，下面分开写。源版本的代码位置和逐项盘点在任务目录的 `subagents/proposal-analysis-upgrade.md`，不在本文里 |
| 状态 | 设计提案。**没有在任何集群上做过升级演练。**关于旧版本行为的结论来自读代码；关于垃圾回收器、Deployment、Helm 的结论按文档和源码。要在集群上确认的事列在[第 12 节](#12-留给真实验证的) |
| 「不断服」的含义 | 范围是升级和回退。期间：已运行的 Pod 的 UID 不变、容器不重启、Ready 不掉；Service、Endpoints、Pod 的 DNS 记录不变；还有活 Pod 的组，PodGroup 不被重建；没有人改 `spec` 的 ModelServing，升级之后没有 Pod 被替换。控制面（创建和更新 ModelServing、滚动、恢复）允许有一段不工作的时间 |
| 控制面空窗的长度 | 估算式：旧 Pod 的终止时间 + 取得租约 + 缓存同步 + 每个 ModelServing 的接管。数值没有测量，留给真实验证。空窗里会怎样见 [4.1](#41-控制面的空窗) |

## 1. 结论

1. **接管只改元数据。**每个还有活 Pod 的存量的组建一个同名的 ServingGroup 对象，再把 Pod、PodGroup、Service 的 controller owner 从 ModelServing 换成它。Pod 的名字、标签、hostname、DNS、PodGroup 的名字都不变。kubelet 不看 owner，容器不受影响。变的是 owner 的 kind：按 owner 聚合的监控和策略在升级那一刻就受影响（[07](07-user-and-ops-guide.md) 第 7 节）。
2. **新旧进程不能有任何重叠，并且这一点要由新版本自己保证，不能靠操作步骤。**旧进程按 owner 认 Pod。owner 被换掉之后，在网版本会在下一轮把被接管的 Pod 当成占名字的外来 Pod **删掉**，并按恢复策略删同范围的其余 Pod；v1.1.0 把换 owner 当成一次 Pod 删除事件，**立刻删掉**同组或同实例里还没换 owner 的 Pod。重叠的后果不是报错，是删 Pod。新版本自己挡不住的几种（别处的旧进程、本地跑的旧版本）列在第 4 节和第 11 节，只有文档。
3. **「没有人改 `spec`，升级之后不替换任何 Pod」是一条单独的验收条件。**接管本身不删 Pod，不等于接管之后的第一轮正常 reconcile 不删。版本的身份、基线、partition 保护的集合在接管前后要对得上。
4. **回退是新版本的一个持久的受控模式，不是把旧 chart 装回去。**不做反向接管就启动旧版本：在网版本把全部 Pod 逐个删掉重建，v1.1.0 卡住。
5. **有几种负载做不到不断服**，列在[第 10 节](#10-做不到不断服的)，升级之前要能查出来。

## 2. 两个源版本

对升级有影响的差别：

| 项 | 在网版本 | v1.1.0 |
| --- | --- | --- |
| Pod、Service 的 owner | ModelServing，`controller`、`blockOwnerDeletion` | 相同 |
| PodGroup 的 owner | ModelServing，`controller`，没有 `blockOwnerDeletion` | 相同 |
| 认「自己的」Pod | owner 里有这个 ModelServing（不要求是 controller）；事件过滤另外放行带两个标签的 Pod | owner 里有 ModelServing；删除时要求是 controller owner |
| 名字对、标签对、owner 不是自己的 Pod | **主动删除** | 报身份不符，重试 |
| Pod 的 owner 被换掉 | 下一轮删掉这个 Pod，再按恢复策略删同范围的其余 Pod | 当成删除事件，立刻按恢复策略删同范围里还归自己的 Pod；`ServingGroupRecreate` 下连 PodGroup 一起删 |
| 计划内删除的持久标记 | Pod 上的 `deletion-scope` 注解 | 没有 |
| 实例身份注解 | 有 | 没有 |
| 原地更新 | 没有 | 有 |
| partition 的语义 | 绝对序号 `[0, P)`。API 字段的注释写的是「前 P 个现存的组」，controller 的行为是绝对序号 | 绝对序号 `[0, P)` |
| revision 的算法 | 与上游不同 | 与上游 `main` 相同 |
| headless Service 的布局 | 每个实例一个 | 每个实例两个，worker 的 subdomain 指向另一个 |
| 独有的 plugin 和 API 字段 | 有 | 有（原地更新等） |
| plugin 框架的钩子 | 七个：比上游多 Pod 运行、Pod 删除、整组删除三个 | 四个，与上游 `main` 相同 |
| 选主 | 默认关。开了之后失主会退出 | 默认关。开了之后失主**不退出**，探针照样通过 |
| 部署 | 单副本，Deployment 滚动更新，webhook 都是 `failurePolicy: Fail`（驱逐 webhook 可配），CRD 在 chart 的 `crds/` | 相同 |

## 3. 升级的前提

对目标版本的要求。任何一条不满足，这个源版本就不能不断服地升上来。

| # | 前提 | 不满足时 |
| --- | --- | --- |
| P1 | **目标版本是源版本的超集**：API 字段、已注册的 plugin、plugin 框架的钩子集合、ControllerRevision 的数据格式、校验规则 | 少一个 plugin：plugin 链构造失败，用到它的 ModelServing 建不了 Pod。少一个字段：下一次写的时候被裁掉。少一个钩子：靠它做事的 plugin 不再被调到。在网版本有上游没有的 plugin 和字段，plugin 框架也比上游多三个钩子，目标版本都要带上（表下） |
| P2 | **版本的身份连续**：存量 Pod 上的 revision 标签，在目标版本看来与没有改过的 `spec` 是同一个版本。靠的是已有的机制：哈希不等时经 ControllerRevision 解出模板按语义比较，目标版本的解析先沿用已有的身份 | 接管之后第一轮，全部组都是过期的，按滚动预算逐个替换。在网版本的 revision 算法与上游不同，这一条对它是实打实的风险 |
| P3 | **存量实例保留老的 headless Service 布局**，直到它自己被重建 | 在网版本上用了 headless Service plugin、Role 带 worker 的负载：老 worker 的 DNS 记录消失，或者 plugin 报告要迁移而停止同步 |
| P4 | 新增的或收紧的校验只拦新引入的值 | 存量对象的任何更新被拒，包括 autoscaler 的 PATCH |
| P5 | 驱逐 webhook 认两种归属（owner 是 ModelServing，或者是它名下的 ServingGroup） | 接管到一半时，已接管的 Pod 的驱逐保护无声失效 |

P1 的三点补充：

- **在网版本多出的三个钩子**（Pod 运行、Pod 删除、整组删除）。目标版本要带上它们，并规定调用点（[02](02-design.md) 第 7 节）：调用者和另外四个钩子一样，是 ServingGroup controller；前两个在对应的 Pod 事件上调；整组删除的钩子与组被删时的 `OnRoleDelete` 同一个时机，同样只尽力调一次，不保证。逐个钩子的调用点没有验证（第 12 节）。
- **登记了删除钩子的 plugin 要查一遍。**`ServingGroupRollingUpdate` 下的删除都是整组删除，删除钩子在这个模式下不保证调到（[07](07-user-and-ops-guide.md) 第 8 节）。P1 因此多一项检查：每个登记了删除钩子的 plugin，它创建的资源是否都挂在 ServingGroup 名下、能随组一起被回收。不是的，每滚动一次泄漏一批。
- **驱逐追踪用的 ConfigMap** 的 owner 仍然是 ModelServing。它不属于要接管的存量对象，T1、T7 不数它。

## 4. 独占

接管开始之前，旧进程必须已经退出；之后也不能再有旧进程起来。四道防线，前三道是新版本自带的：

| # | 防线 | 挡住什么 | 挡不住什么 |
| --- | --- | --- | --- |
| X1 | **自查，启动时做，运行中持续做。**新 chart 给 controller 的 Pod 模板加一个版本标记。新进程在自己的 namespace 里还能看到「带 controller-manager 的 selector、不带这个标记」的 Pod 时，不启动任何 controller，只应答 webhook；看不到了才启动。运行中看到这样的 Pod：停掉两个 controller（放掉租约），继续应答 webhook，写事件和日志。进程重启之后回到启动自查 | 直接 `helm upgrade`：Deployment 滚动更新，新旧 Pod 并存。新进程等旧 Pod 消失。webhook 没有空窗。`helm rollback`、`kubectl rollout undo` 把旧版本的 Pod 带起来：新版本一侧的写停下 | 不在这个 namespace 的旧进程；Pod 对象被强制删除而容器还在跑；不是 Pod 的旧进程（本地跑的）。运行中的那一种只停得下新版本一侧，旧进程照样按第 1 节第 2 条删 Pod |
| X2 | **CRD 和 RBAC 的闸。**ServingGroup 的 CRD 或者对它的权限不在时，新进程不就绪（就绪探针和存活探针分开），不启动 controller，也不退回旧的工作方式。chart 用 `lookup` 在升级开始之前就失败 | Helm 升级不处理 `crds/`。没有这道闸：新 Pod 就绪，旧 Pod 被停掉，控制循环从此没有人做，而且没有任何信号 | — |
| X3 | **换凭据。**新 chart 给 controller 换一个 ServiceAccount 和 ClusterRoleBinding 的名字。Helm 升级时删掉清单里不再有的旧对象，旧进程的凭据随之失效 | 只有用这个 release 的 ServiceAccount 的旧进程（例如卡住之后又恢复的旧 Pod）：它的写被拒绝 | 用自己的 kubeconfig 在本地跑的旧版本：凭据不是这个 ServiceAccount。另一个 release 里的旧进程：能不能并存、它的凭据会不会被这次升级删掉，取决于 Helm 对同名集群级对象的处理，没有验证。Helm 先建新的、最后删旧的，中间有很短的次序不定，所以它不单独用。回退方向无效 |
| X4 | **选主，并且失主退出。**新版本默认打开 | 之后的每一次升级和重启 | 这一次升级：旧版本默认没开选主，租约排除不了它 |

- X1 是必须有的：它让「直接 `helm upgrade`」从事故变成安全的操作。
- **X1 的等待要有信号。**新进程在等旧 Pod 消失时，在自己的 Pod 上写一个事件，并写日志，说明在等哪个 Pod；等待超过一个可配的时间之后告警。旧 Pod 卡在 Terminating（节点失联、finalizer）时新进程一直等，webhook 照常接受请求而没有人执行，没有信号的话看不出来。
- X3 对在网版本尤其要做：重叠的后果在它上面最重。
- 控制面的空窗：从旧 Pod 开始终止，到一个 ModelServing 过了接管屏障（[02](02-design.md) 的 6.3，按 ModelServing 设）并且接管完成（第 5 节）。期间会怎样见 [4.1](#41-控制面的空窗)。
- 源版本如果已经开了选主（在网版本改一个 values 就能开，并且失主会退出），X4 对这一次升级也生效。v1.1.0 失主之后不退出，不能靠它。

**评估过、没有采用的**：接管时保留两条 ownerReference（ServingGroup 是 controller，ModelServing 是非 controller）。它能让旧进程和旧 webhook 仍然认这些 Pod、让误删组和 CRD 不删 Pod、让回退不依赖反向接管先做完。没有采用，因为对象上还有另一个存在的 owner 时前台级联不删它，组的删除（02 的 5.4）在这段时间里失效；并且独占仍然是必须的（在网版本会继续按自己的规则管这些 Pod）。

### 4.1 控制面的空窗

正向升级的空窗里，用户的各种动作会怎样。除注明的以外都是按本文的机制推演，没有验证。

| 用户的动作或事件 | 空窗里 | 接管完成之后 |
| --- | --- | --- |
| 创建、更新 ModelServing | webhook 接受，没有人执行。新旧两个 Pod 并存时请求落到哪一个上不定，两边的校验规则不同；P4 保证存量对象的更新不会因此被拒 | 按最新的值做 |
| HPA、KEDA、autoscaler 改副本数 | 被接受并累积，没有人执行。负载突增时扩不上去 | 按最后的值做 |
| 删除 ModelServing | 被接受。垃圾回收器照常级联，Pod 被删 | — |
| 驱逐 | 驱逐 webhook 照常应答，预算照常算（P5）。被驱逐的 Pod 没有人补 | 补（下一行） |
| Pod 被删 | 没有人管 | 只补：账本从零值开始（T5） |
| Pod 变成 `Failed` | 没有人管 | 按 `recoveryPolicy` 的范围恢复：故障不看账本是不是零值（[03](03-state-machine.md) 的 4.3，E5） |
| 容器崩溃 | kubelet 按 `restartPolicy` 重启容器。没有按策略的重建 | 重启故障的 grace 从新进程看到它时重新算 |
| 进行到一半的滚动、恢复、缩容 | 停在原地。多出来的组继续占资源 | 第 6 节 |
| `ModelServing.status` | 不更新 | 接管注解是 `InProgress` 期间只更新 condition，计数保留旧版本写的值（5.2）；`Done` 之后重算 |
| 旧的 controller Pod 停不下来 | 以上各行一直持续。空窗没有上界（第 10 节） | — |

空窗里 webhook 一直有进程应答：新 Pod 就绪之后旧 Pod 才被停掉。没有请求因为没有人应答而被拒绝。

回退的空窗是从创建回退开关到旧版本的 controller 起来（9.1）。「空窗里」一栏对它同样成立；之后由旧版本按它自己的规则接着做。

## 5. 接管

接管按 ModelServing 进行。它幂等，任何一步停下都可以续做。

### 5.1 多出来的两个状态

| 层 | 状态 | 判定 | 动作 |
| --- | --- | --- | --- |
| Pod 成员 | 待接管 | 标签是这个组的；controller owner 还是 ModelServing，或者没有 controller owner；不在终止中；名字是这个组某个期望成员的名字。组不带 `adopt-members` 注解、Pod 又没有 controller owner 时另有两条（5.4）。带注解的组、owner 还是 ModelServing 的 Pod 都不看版本标签，所以原地更新到一半、标签还是旧版本的 Pod 照样是待接管的成员，放弃回退时也接得回来 | 是成员。**不为它的名字创建，也不删除它**；换 owner |
| ServingGroup | 接管中 | 带 `modelserving.volcano.sh/adopt-members: "true"` 注解，Adopted 不为真 | ServingGroup controller 只换 owner、起账本。ModelServing controller 不把它当缩容和替换的候选 |

`adopt-members` 注解由 T3 在创建组的时候写，之后不变。升级接管和 5.4 的收养用同一个 T3，建出来的组都带它。

「期望成员的名字」按名字的格式判断：`<组名>-<role>-<实例序号>-<worker 序号>`。role 按这个组自己的版本判断：Pod 的 revision 标签指向的 ControllerRevision 里的 Role；那个版本读不到时，按当前 `spec` 的 Role。这样旧版本的组里、已经从最新 `spec` 删掉的 Role 的 Pod 仍然是成员。实例序号超出当前 `replicas` 的也算：它所在的实例接管之后按 [03](03-state-machine.md) 的 4.3 的 E4 清空。

仍然对不上的带标签的 Pod：不接管，不删除，不计入存量对象（T1、T7 不数它），发事件报告，由人处理。owner 还是 ModelServing 的也一样。`kubectl debug --copy-to` 加 `--keep-labels` 复制出来的 Pod 属于这一种：它带着原 Pod 的标签，没有 owner。升级前检查把它们报出来（7.1）。

ModelServing 上有一个注解记接管的进度：没有（不知道）、`InProgress`、`Done`、`RollingBack`（回退中，9.1）。用注解而不是 `status`：旧版本的 CRD 会裁掉它不认识的 status 字段。**没有这个注解不等于不需要接管**：它的含义是「还没查过」。接管在 ModelServing 上只记这一个注解。新建的组用什么名字不记在 ModelServing 上，只看回退窗口开没开（第 8 节）。

### 5.2 步骤

| 步 | 谁 | 做什么 | 写 | 这一步之后停下 |
| --- | --- | --- | --- | --- |
| T1 | ModelServing controller | 这个 ModelServing 没有注解，或者是 `InProgress`，或者是 `RollingBack` 而回退开关不在（9.1）：**对 apiserver 直读**名下的存量对象（带它的名字标签、owner 是它或者没有 owner 的 Pod、PodGroup、Service）。一个都没有：写 `Done`。有：写 `InProgress`。判据只有这一个事实：直读到了存量对象。终止中的存量 Pod 不算，与 T7 同一个口径。直读到了存量对象而回退窗口关着：不接管，报告，等人处理（第 8 节） | PATCH ModelServing 的注解 | 重做：直读的结果相同 |
| T2 | ModelServing controller | 带删除标记而还活着的存量 Pod（只有在网版本有）：按 UID 删除。它们不接管 | DELETE Pod，带 UID | 重做：还在的再删 |
| T2′ | ModelServing controller | 没有「活着的、不带删除标记的」存量 Pod 的组名：它名下存量的 PodGroup、Service，直读确认之后按 UID 删掉。确认的内容：对象的 UID 没变，这个组名下仍然没有活着的、不带删除标记的存量 Pod | DELETE PodGroup、Service，带 UID | 重做：还在的再读再删 |
| T3 | ModelServing controller | 每个还有「活着的、不带删除标记的」存量 Pod 的组名：创建同名的 ServingGroup，带 `adopt-members` 注解。存量的序号不另外记，从名字解析。Pod 全部在终止的组不接管，也不等它们消失（T7） | 创建 ServingGroup | 重做：撞名字，核对 owner 是这个 ModelServing |
| T4 | ServingGroup controller | 每个待接管的成员：先**直读这个 ServingGroup**，确认 UID、确认没有 `deletionTimestamp`；然后把 controller owner **换成**它（不是追加），补上 `blockOwnerDeletion`，补上代际号标签（值是 0），摘掉组一级的 surge 标记。PodGroup、Service 同样处理 | PATCH，带对象的 UID 和 resourceVersion | 一个组换到一半：剩下的仍然是待接管的成员，不会被当成缺失 |
| T5 | ServingGroup controller | 账本从零值开始。一个实例的成员（按它已经应用的模板布局）全部在缓存里、直读确认过：写 Established。不齐的留在 Creating | UPDATE status | 重做 |
| T6 | ServingGroup controller | 直读：这个组名下没有待接管的成员了。写 Adopted | UPDATE status | 重做 |
| T7 | ModelServing controller | 直读：没有存量对象了，接管来的组都是 Adopted。写 `Done`。**终止中的存量 Pod 不算存量对象**，不等它消失 | PATCH 注解 | 重做 |

`InProgress` 期间，ModelServing controller 对这个 ModelServing 不做 G1 到 G6 的任何写（[03](03-state-machine.md) 的 M3）。`status` 的计数不重算，保留旧版本写的值，只更新 condition（[03](03-state-machine.md) 第 6 节）：接管中的组还没有写出 Ready，重算会让可用数掉到 0 再回来。

几点说明。

- **接管注解丢了**（例如对象被整体替换）。按「还没查过」重走 T1，结果由直读决定。组的名字和次序不受影响：它们不记在 ModelServing 上（第 8 节）。
- **T2′ 处理的三种存量对象。**v1.1.0 全零的组留下的 PodGroup；升级那一刻 Pod 全部在终止的组的 PodGroup 和 Service；在网版本上被 T2 删光的组的 PodGroup 和 Service。它们没有可以换过去的 ServingGroup，留着的话 T7 永远过不了。
- **T7 之后还在终止的存量 Pod。**它消失之前，名字相同的新组的那个成员位置等着，不创建（[03](03-state-machine.md) 的 4.3，E7）。
- T2、T2′ 是 ModelServing controller 直接删 Pod、PodGroup、Service，只在接管期间有。

T3 里 ServingGroup 的 `spec` 怎样填：

| 情形 | 取值 |
| --- | --- |
| `ServingGroupRollingUpdate` | `revision` 取这个组的 Pod 的 revision 标签。组正在原地更新（只有 v1.1.0 有）：取进度注解里的目标版本。一个组的 Pod 标签不一致、又没有进度注解（例如从 Role 模式切过来之后没滚完的组）：`revision` 留空，按 [02](02-design.md) 的 4.7 取 `status.revision`，为空算过期 |
| `RoleRollingUpdate` | `targetRevision`、`baselineRevision` 取 ModelServing 的 `status`；各实例的版本从成员的标签读 |
| `roles`、`recoveryPolicy` 等 | ModelServing 当前的 `spec` |
| 代际号 | 存量 Pod 没有这个标签，按 0 |

### 5.3 为什么每一步都是安全的

| 担心 | 为什么不会 |
| --- | --- |
| Pod 缓存还空着，闸门误放行，按数量新建 N 个组 | T1、T7 是直读 |
| 一个组换 owner 换到一半，其余成员被当成缺失去建同名 Pod | 待接管的成员是成员 |
| 同名的 ServingGroup 被删了重建，缓存里是旧的 UID，写进 Pod 的 owner 是一个不存在的对象，垃圾回收器删掉这个 Pod | T4 之前直读这个 ServingGroup 的 UID 和 `deletionTimestamp` |
| 接管来的实例被当成「建成过」，第一次观察到的不齐被当成丢失 | 账本从零值开始，零值只补 |
| 带删除标记的 Pod 所在的组或实例被整个删掉（标记是逐个打的，可能只打了一半） | 只删带标记的 UID，范围正好是原来选定的 |
| 没有活 Pod 的组留下的 PodGroup、Service，或者一个卡在终止中的存量 Pod，让接管永远做不完 | T2′ 删掉前者；T7 不等后者 |
| T2′ 删掉了一个还有活 Pod 的组的 PodGroup | 删之前直读。闸门关着，这期间没有人给这个组建 Pod |
| 带着标签的无关 Pod（调试用的副本）被接管成成员 | 待接管的成员要求名字是期望成员的名字 |

模型里的对应：接管完成之前不创建、不删除任何组；去掉闸门之后这条性质被违反（[05](05-inference-verification.md) 的 G8、M7）。成员一级的步骤是推演，T2′ 和 5.4 也是。

### 5.4 `Done` 之后出现的无主成员 Pod

接管注解已经是 `Done` 之后，仍然可能出现没有 owner 的成员 Pod：有人用 `kubectl delete servinggroup --cascade=orphan` 删了一个组（[02](02-design.md) 的 5.5）。它们被收养回来，用的是 T3 到 T6 这几步，但**不动接管注解**，也不停下这个 ModelServing 的其余动作。

| 项 | 内容 |
| --- | --- |
| 无主的成员 Pod | 带这个 ModelServing 的标签和代际号标签，没有 controller owner，不在终止中，名字是它的 `group-name` 标签那个组的一个期望成员的名字 |
| 谁发现 | ServingGroup controller 的 Pod 事件处理看到这样的 Pod，把所属的 ModelServing 入队 |
| 谁处理 | ModelServing controller 直读确认。组名对应的 ServingGroup 不存在，并且 Pod 的版本读得到：按 T3 建同名的组，带 `adopt-members`。ServingGroup controller 按 T4 到 T6 换 owner、起账本、写 Adopted。PodGroup、Service 同样换回来 |
| 期间 | 只有 G4 不放行：名下有接管中的组、或者有还没建出组的无主成员 Pod 时，不按数量新建组。G1、G2、G3 和 `status` 照常 |

代际号标签是 ServingGroup controller 管过这个 Pod 的痕迹（升级接管时 T4 给存量的 Pod 补上）。要求它，是为了不让随手建的一个带标签的 Pod 挡住扩容。

收养有出口：

| 情形 | 处理 |
| --- | --- |
| Pod 的版本指向的 ControllerRevision 已经不在 | 不建组，不接管，发事件，由人处理 |
| 名字对不上任何期望成员，或者不带代际号标签（例如 `kubectl debug --copy-to` 的副本） | 同上 |
| 接管中的组超过一个可配的期限还有换不了 owner 的 Pod | ServingGroup controller 照样写 Adopted，发事件；换不了的 Pod 之后对这个组是外来的 |

这三种 Pod 都不再挡 G4。

这是 ModelServing controller 读 Pod 的第二个例外，第一个是升级接管。它只读。

已知的竞态：ModelServing controller 先看到少了一个组，按数量建了一个新组，之后才收到无主 Pod 的事件。

- 新组的名字和被删的组不同：收养完成之后多一个组，按缩容的次序删掉一个（[02](02-design.md) 的 4.4）。删的不一定是后建的那个：新组已经就绪时，被删的可能是刚收回来的组。
- 新组的名字和被删的组相同：回退窗口开着、或者 owner 是 LeaderWorkerSet 时，新组取最小的空闲序号，可能正好是被删的那个。这时没有多出来的组。同名的新组建不出自己的成员（名字被无主的 Pod 占着），分两种：
  - 它的版本和无主 Pod 的版本相同：无主的 Pod 是它的待接管成员，换 owner。
  - 版本不同（新组的版本是按数量选的）：这些 Pod 对它是外来的，不换 owner。ModelServing controller 先直读确认这个同名的组是刚按数量建出来的：不带 `adopt-members`，没有任何 Established 的实例，没有就绪的成员。确认了才把它前台删除（它名下只有它自己建的对象，无主的 Pod 不受影响），等它消失，再按 T3 用 Pod 的版本建同名的组并接管。确认不了的不删，无主的 Pod 按上表第二行处理。

所以「待接管」在一种情形下另有两条：组不带 `adopt-members` 注解，并且 Pod 没有 controller owner。这时要求 Pod 带代际号标签，`ServingGroupRollingUpdate` 下 Pod 的 revision 标签等于组的 `spec.revision`。带注解的组是为这些 Pod 建的；owner 还是 ModelServing 的 Pod 只来自升级接管和回退。这两种都不加。

无主的 PodGroup、Service 同理：不带注解的组不把同名的无主对象收为己有。所以删掉那个同名的新组时，它们和无主的 Pod 一样不受影响。

## 6. 升级那一刻的状态

旧进程停下时负载可以处在任何状态。每一种怎样接过来：

| 升级时的状态 | 对象上是什么 | 接过来之后 | 相对旧进程继续做，差在哪 |
| --- | --- | --- | --- |
| 稳态 | N 个组，全部就绪 | N 个接管来的组，全部 Established。没有动作 | — |
| 滚动中，有一个 surge 组 | N + 1 个组，一个带 surge 标记 | 都接管，surge 标记摘掉。之后按 02 的 G1–G6：还有过期的组，上限是 N + S，继续替换；最后一个旧组删掉时正好是 N | 少建一个、少删一个（04 的 X4）。那个多出来的组一直没就绪的话，升级之后它是 N 个目标组之一，不再被回收，滚动可能落到 SG-C16 的终态：`RolloutBlocked`，reason 是 `TargetNotReady`（[07](07-user-and-ops-guide.md) 的 4.2） |
| partition 灰度停点 | 序号小于 P 的是旧版本 | 非目标的组不超过 P 个，全部受保护：就是原来那几个。没有动作 | — |
| 保护的序号里有一个不是基线的旧版本（P 调大之后又提交过新版本） | — | 非目标的组不超过 P 个时全部受保护，没有动作。多于 P 个（滚动还没到停点）时，受保护的名额先给基线版本的组，再给别的版本，各自按次序（[02](02-design.md) 的 4.1） | 滚动还没到停点、高序号上还有基线版本的组时，留下的不一定是序号小于 P 的那几个。例如 `{0:v1, 1:v2, 2:v1}`、基线 v1、P=2、已提交 v3：旧进程留 0、1，接管之后留 0、2。个数相同 |
| 稀疏序号 | 例如 {0, 3} | 两个组照常接管。组一级没有「洞」这个概念 | 之后扩容不再「补最低的洞」，回退窗口内新建的组仍然取最小的空闲序号 |
| 稀疏序号加生效的 partition（两个源版本都按绝对序号） | 例如 `{0, 3, 4}` 全是 v1，P=3，已提交 v2。升级前序号 3、4 可更新 | 三个组照常接管。按数量：三个非目标的组不超过 P，全部受保护。滚动停下 | 滚动在升级前后停在不同的地方。不替换任何 Pod，但用户要把 P 调小才继续。升级前检查报出这样的 ModelServing（7.1）；接管之后 `Available` 的 message 写明停在 partition 上，并列出受保护的组名（[03](03-state-machine.md) 第 6 节） |
| 整组恢复或删除进行中，在网版本 | 一部分 Pod 带 `ServingGroup` 的删除标记 | T2 按 UID 删掉带标记的。没带标记的照常接管；实例不齐，留在 Creating，补。整个组都带标记的：Pod 被 T2 删光，PodGroup、Service 由 T2′ 删掉，闸门放行之后按数量补一个组 | 标记只打了一半时，没带标记的成员留下，与契约 2.11 相同 |
| 同上，v1.1.0 | 没有标记；组里只剩一部分 Pod，PodGroup 可能已经没了 | 接管，只补。第一轮确保 PodGroup、Service 存在 | 与 v1.1.0 自己重启的结果相同：不续删 |
| 实例恢复或删除进行中 | 同上，范围是一个实例 | 同上 | 同上 |
| Pod 正在终止 | — | 不接管。T7 不等它；它消失之前所在的位置不创建（[03](03-state-machine.md) 的 4.3，E7）。整个组的 Pod 都在终止：这个组不接管，PodGroup、Service 由 T2′ 删掉；闸门放行之后按数量补一个组，名字和它相同时各成员位置等这些 Pod 消失 | 整个组都在终止、补出来的组名字不同时，新组的 Pod 在旧 Pod 停完之前就开始创建 |
| 缩容进行中 | 多于 N 个组，有的在终止 | 活着的接管；闸门放行之后 G2 按次序再选 | v1.1.0 没有标记，再选的不一定是原来那个 |
| 原地更新进行中（v1.1.0） | 一部分 Pod 的标签是旧版本，带进度注解 | 组的版本取注解里的目标；成员分类先看进度注解。续做。续不下去的（超期、被作废、有终态成员、plugin 不支持）：ServingGroup controller 把组的 `RecreateRequired` 写成真，这个组按过期的算，由 G3 删除替换（[02](02-design.md) 的 4.1、4.2） | 没有验证：原地更新没有模型 |
| 存量 Pod 已经是 `Failed`，旧版本在容忍它（grace 是 `-1`，或者还没到期） | 一个终态的 Pod；同一个实例、同一个组里别的 Pod 是健康的 | 照常接管。之后的第一轮：终态的成员是故障，不看账本是不是零值、不等 grace（[03](03-state-machine.md) 的 4.3，E5）。`RoleRecreate` 重建这个实例，`ServingGroupRecreate` 换掉整个组 | **健康的 Pod 被重建，而没有人改过 `spec`。**这不满足不断服（第 10 节）。它是 005 已批准的 D10、D13 在升级那一刻生效，不是本设计新加的规则；旧版本的容忍取自契约 2.12 的文字，没有读源版本的代码 |
| 全零的组（N 大于 0，各 Role 的副本数都是 0） | 没有 Pod；v1.1.0 可能留着 PodGroup | 没有可接管的 Pod。留着 PodGroup 的：T1 直读到存量对象，T2′ 把这些 PodGroup 按 UID 删掉。什么都没留的：T1 直接写 `Done`。两种情形闸门放行之后都按 N 建空的组，名字是序号（回退窗口开着），ServingGroup controller 建 PodGroup。空组的版本每一轮按新建的规则重新指派：P 个名额给基线，其余给目标，只改 `spec.revision`，不删对象，不占预算（[02](02-design.md) 的 4.2） | 存量的 PodGroup 被删了重建，UID 变了，它名下没有 Pod。各 Role 的副本数调回来时，各版本的个数与按序号新建的结果相同 |
| roleCoordination 滚动进行中 | 基线在 ControllerRevision 的注解上 | 由 ModelServing controller 读出来，下发到组的 `spec` | — |
| owner 是 LeaderWorkerSet 的 ModelServing | — | 新建的组一直用序号名字，不看回退窗口 | — |
| 没有 controller owner 的带标签的 Pod（有人用 orphan 删过） | — | 名字是所在组某个期望成员的名字：待接管的成员（5.1）。对不上的不接管，发事件报告，由人处理。`Done` 之后才出现的（`kubectl delete servinggroup --cascade=orphan`）：按 5.4 收回来 | `Done` 之后的那一种有竞态：可能先按数量多建一个组，收回来之后再按缩容的次序删掉一个（5.4） |

接管本身在每一行里都不创建、不删除健康的 Pod。T2 删的是旧进程已经决定要删、并且标在 Pod 上的那些。T2′ 删的是名下没有活 Pod 的 PodGroup、Service。

## 7. 操作步骤

| 步 | 做什么 | 为什么 |
| --- | --- | --- |
| 1 | **升级前检查**（只读）。查出：进行中的滚动和恢复、带删除标记的 Pod、进行中的原地更新、第 10 节的负载、长名字的对象、驱逐 webhook 是否开着和它的 `failurePolicy`、源版本是否开了选主、已 cordon 或正在排空的节点。规定的输出见 [7.1](#71-升级前检查的输出) | 有些状态升级那一刻有特殊后果；有些负载做不到不断服 |
| 2 | 装 CRD：ServingGroup 的 CRD，以及更新过的 ModelServing 的 CRD。用 `kubectl apply --server-side`（ModelServing 的 CRD 很大，客户端 apply 会因为注解太长失败） | Helm 升级不处理 `crds/`。X2 在这一步没做时会拦住 |
| 3 | `helm upgrade` 到新版本，回退窗口的设置开着（第 8 节）。从这一步到第 5 步验收完，不要排空节点，不要做会删 Pod 的操作 | 新的 Deployment、RBAC、ServiceAccount、webhook 配置。窗口开着，新建的组才用序号名字。空窗里被删的 Pod 没有人补（4.1），验收的基线也会被打乱 |
| 4 | 等。新 Pod 起来只应答 webhook（X1），它的事件和日志写着在等哪个旧 Pod；旧 Pod 被停掉；新进程看不到旧 Pod 之后，各个 ModelServing 过接管屏障，开始接管 | — |
| 5 | 验收：每个 ModelServing 的接管注解是 `Done`；Pod 的 UID 和升级前相同、重启计数没有变；没有人改过 `spec` 的 ModelServing 没有组在终止；`status` 的可用数全程不低于升级前；存量 Pod 的 `hostname.subdomain` 仍然解析；还有活 Pod 的组，PodGroup 的 UID 不变 | 第 0 节的定义，第 1 节的第 3 条 |
| 6 | 观察一段时间之后，关闭回退窗口（第 8 节） | 之后新建的组才用随机名字 |

第 2 步和第 3 步的次序反了、或者第 2 步漏了：X2 让升级在开始前失败，或者新 Pod 不就绪、旧 Pod 不被停掉，旧版本照常工作。

不要做的事：

- 任何步骤里都不删 CRD。chart 里 CRD 的位置不要变（位置一变，Helm 升级会删掉它，连带全部实例）。
- 升级和重启 controller 的同时不要排空节点，不要做会删 Pod 的操作。这一次升级：空窗里丢的 Pod 没有人补，接管之后只补。之后新版本的每一次升级和重启：停机期间丢了成员的已建成实例，回来之后按 `recoveryPolicy` 的范围恢复，重建的 Pod 比丢的多（[07](07-user-and-ops-guide.md) 的 4.3）。

### 7.1 升级前检查的输出

对每个 ModelServing 给一个结论：可以不断服地升级；要先等某件事做完；做不到，原因是什么。验收要用的基线一起存下来。

| 范围 | 输出的项 | 用途 |
| --- | --- | --- |
| 集群 | 源版本和它的启动参数（是否开了选主）；controller 的 Deployment 的副本数和更新策略；两个 CRD 在不在、版本对不对；各 webhook 的 `failurePolicy`，驱逐 webhook 是否开着 | X1、X2、X4；空窗里会怎样（4.1） |
| 集群 | 全部 namespace 里符合 controller selector 的 Pod | 独占：找别处的旧进程 |
| 集群 | 已 cordon 或正在排空的节点；正在终止的 ModelServing 的 Pod | 第 3 步；T7 |
| 集群 | 选中 ModelServing 的 Pod 的 PDB 里，用百分比或 `maxUnavailable` 的 | 第 10 节 |
| 集群 | 是否开了 `OwnerReferencesPermissionEnforcement` | 第 11 节 |
| 集群 | 回退开关在不在（9.1） | 它在的时候新版本不做正向接管 |
| 集群 | 这次升级的 values 里回退窗口的设置有没有打开（第 8 节） | 没有打开就报告。检查是只读的，拦不住 `helm upgrade`；真正拦的是 controller：窗口关着而有存量对象时它不接管（第 8 节） |
| 每个 ModelServing | 滚动模式、N、各组的序号（有没有洞）、有效的 partition（整数还是百分比）。**稀疏序号加生效的 partition 单独报出** | 第 6 节：升级之后滚动会停下 |
| 每个 ModelServing | 进行中的滚动（过期的组数、带 surge 标记的组）、缩容、恢复（带删除标记的 Pod、正在终止的 Pod、`Failed` 的 Pod）、原地更新。对每个 `Failed` 的 Pod 给出结论：升级之后会重建哪个实例或哪个组 | 第 6 节的各行 |
| 每个 ModelServing | **版本的身份**：按目标版本的算法算出当前 `spec` 的版本，与存量 Pod 的 revision 标签比。结论是「升级后不滚动」或者「升级后会滚动 k 个组」。**这一项必须有** | P2。运维自己看不出来 |
| 每个 ModelServing | 用到的 plugin 和字段里目标版本没有的；用到的 plugin 里登记了删除钩子、而资源不挂在 ServingGroup 名下的；headless Service 的布局（是否带 worker） | P1、P3 |
| 每个 ModelServing | 名字长度离上限还有多少，按随机后缀算 | 第 10 节：关闭回退窗口之后 |
| 每个 ModelServing | owner 是不是 LeaderWorkerSet | 它新建的组一直用序号名字 |
| 每个 ModelServing | 带它的标签而名字对不上任何期望成员的 Pod（5.1），包括 owner 还是 ModelServing 的 | 它们不接管，不计入存量对象，由人处理 |
| 每个 ModelServing | `status` 的可用数 | 第 5 步验收的基线 |
| 每个 Pod | UID、各容器的重启计数、Ready 的状态和时间；PodGroup、Service 的 UID | 第 5 步验收的基线 |

## 8. 回退窗口

接管完成之后、运维确认不再需要回退到旧版本之前，是回退窗口。它是 controller 的一个集群级的设置（chart 的一个值），由人显式关闭，不按时间自动关。默认是关的：全新安装没有要回退到的旧版本。从旧版本升级时，在升级的那一步显式打开它（第 7 节）。

忘了打开不会悄悄丢掉回退的路，由 controller 自己拦：T1 直读到存量对象而窗口关着时，不接管这个 ModelServing，在它上面写事件和 condition 的 message，等人处理。人有两个选择：打开窗口；或者用另一个设置显式声明「不需要回退」，这时在窗口关着的情况下接管。升级前检查也报告这一项（7.1），它是只读的工具，拦不住 `helm upgrade`，所以不靠它。

| | 窗口开着 | 窗口关闭之后 |
| --- | --- | --- |
| 新建的组的名字 | `<名>-<最小的空闲序号>`，全部 ModelServing 都是 | `<名>-<随机后缀>`。owner 是 LeaderWorkerSet 的仍然是序号 |
| 次序键 | 只看名字和创建时间：名字是 `<ModelServing 名>-<纯数字>` 的组在前，按序号；其余按创建时间，再按名字 | 相同。还留着的序号名字的组排在随机名字的组前面 |
| 能不能不重建 Pod 回退 | 满足 9.2 的条件时能 | 不能 |

- **命名不是某个 ModelServing 的属性。**建一个组的那一刻只看两件事：owner 是不是 LeaderWorkerSet，窗口开没开。ModelServing 上没有记命名的注解，组上也没有记存量序号的注解，序号从名字解析。
- **窗口开着时全部 ModelServing 都用序号名字。**包括升级之后才创建的 ModelServing，和全零而没有留下任何对象的 ModelServing。所以窗口之内没有随机名字的组，9.2 第 1 条对全部 ModelServing 成立。
- 随机后缀至少含一个字母，两种名字分得开。

窗口内名字仍然是序号，是为了旧版本能解析它。算法在窗口内外相同，都按数量。

关闭之前核对名字的长度：随机后缀比序号长，名字贴着长度上限的 ModelServing 在窗口关闭之后建不出新组（第 10 节）。

## 9. 回退

### 9.1 步骤

回退是新版本的一个持久的受控模式。旧版本做不了，它不认识 ServingGroup；手工做不现实。

| 步 | 做什么 | 为什么在这个位置 |
| --- | --- | --- |
| 1 | 人运行回退前检查（只读），看一遍 9.2 的条件。这一遍只供参考：检查之后 controller 还在正常工作，条件可以变 | — |
| 2 | 人创建**回退开关**：controller 所在命名空间里的一个 ConfigMap。它是集群级的、持久的，由人创建和删除。它在的时候，新版本的两个 controller 不做正常的 reconcile，也不做正向接管 | 否则换回 owner 的 Pod 会被当成待接管的成员再接回来。开关在对象上，不在进程参数里：新版本进程中途重启，读到开关，继续回退 |
| 2′ | 对每个 ModelServing：开关已经在，正常的 reconcile 已经停下，这时 controller 按 9.2 的条件再检查一遍。不满足的不动它，写事件报告；人看到之后处理，或者删掉开关放弃。满足的，回退的第一个写：把接管注解改成 `RollingBack` | 之后在任何一步停下或放弃，这个 ModelServing 都不会被当成接管完成的，不会按数量另建组 |
| 3 | 对每个 ServingGroup：把名下的 Pod、PodGroup、Service 的 owner 换回 ModelServing。先直读 ModelServing | 与正向对称，幂等 |
| 4 | 直读核对：这个 ServingGroup 名下没有任何对象了 | 第 5 步之前必须确认 |
| 5 | 用 orphan 方式删除 ServingGroup | 即使第 4 步有遗漏也不删 Pod |
| 6 | 一个 ModelServing 名下的 ServingGroup 都删掉之后，摘掉新版本写的、旧版本会误读的标记（ModelServing 上的接管注解；Pod 上的代际号标签可以留着，旧版本不认识） | 再次升级时从「没查过」开始 |
| 7 | 全部 ModelServing 做完之后，新版本停在只应答 webhook：开关还在，两个 controller 不工作。再装旧版本。Deployment 滚动：旧版本的 Pod 就绪之后，新版本的 Pod 才停 | 没有 webhook 空窗。独占，反方向：旧版本的 controller 起来时，新版本的 controller 已经不写了，对象已经是存量形态 |

第 2′ 到第 6 步中间停下：开关还在，新版本进程重启之后读到它，接着做。每一步幂等。

放弃回退：删掉开关。两种 ModelServing 落回正向接管的 T1：注解是 `RollingBack` 的；已经没有注解而对象是存量形态的（做完了第 6 步）。T1 直读到存量对象，重新接管。ServingGroup 还在的组是 T4 的正常情形，换回 owner；ServingGroup 已经被删的组（做完了第 5 步），T3 按组名重建同名的对象。还没被碰到的 ModelServing 注解仍是 `Done`，照常 reconcile。

开关在的时候 `ModelServing.status` 不更新：正常的 reconcile 停了。控制面的空窗从创建开关算起，到旧版本的 controller 起来为止（4.1）。

第 7 步不能提前：对象还没有全部回到存量形态时让旧版本起来，就是第 1 节第 2 条的重叠。

### 9.2 干净回退的条件

| # | 条件 | 不满足时旧版本会 |
| --- | --- | --- |
| 1 | 窗口一直开着，没有随机名字的组。窗口开着时全部 ModelServing 新建的组都是序号名字，包括升级之后才创建的（第 8 节） | 在网版本：按解析不出来的序号乱放 Pod。v1.1.0：整个忽略这些组，另建 N 个，容量翻倍 |
| 2 | 组的个数等于 N，没有在途的创建和删除 | 扩容或缩容 |
| 3 | 没有带组一级 surge 标记的 Pod（T4 已经摘掉） | 把这个健康的组当临时容量回收 |
| 4 | 没有生效的 partition；或者非目标的组的序号都小于 P | 两个源版本都按绝对序号保护 `[0, P)`：滚掉序号不小于 P 的旧版本组 |
| 5 | 版本的身份在旧版本看来是连续的（P2 的反方向） | 全量滚动 |
| 6 | 没有不带 controller owner 的带标签的 Pod | 在网版本：删掉。v1.1.0：卡住 |

第 4 条不满足时，不能干净地回退到任何一个源版本。灰度进行中容易落到这种情形：窗口内的算法按数量，新建的组取最小的空闲序号。例如 N=3、P=1，灰度还没滚完时序号 0 上受保护的组被整组恢复：补出来的是目标版本，留在旧版本的换成了序号 1 上的组，它不小于 P。

这种情形有一个待评估的做法：回退之前先把 P 调到 N。新版本下全部非目标的组受保护，没有动作；旧版本起来之后按绝对序号保护 `[0, N)`，序号集合落在 `[0, N)` 之内时它一个组也不动。有组的序号不小于 N 时不成立（窗口内带 S 滚动过，序号有洞）。它是推演，没有验证（第 12 节）；确认之前不要把它当成出路。

owner 是 LeaderWorkerSet 的 ModelServing 没有单独的条件。`group-index` 可以不连续：缩容按未就绪的先、deletion cost 低的先，会留下洞，今天也是这样。

### 9.3 升级、回退、再升级

回退的第 6 步摘掉了接管注解，第 5 步删掉了 ServingGroup 对象，它上面的 `adopt-members` 注解跟着没了。命名不记在对象上。再次升级时每个 ModelServing 都从「没查过」开始，走一遍完整的接管。没有残留的标记会被误读。

再次升级之前要删掉回退开关。它还在的话，新版本起来之后不做正向接管，也不做正常的 reconcile。升级前检查报告它（7.1）。

## 10. 做不到不断服的

| 情形 | 原因 | 怎么办 |
| --- | --- | --- |
| 目标版本缺源版本的 plugin、字段或框架的钩子（P1） | — | 目标版本补齐之前不升级 |
| 在网版本上：用了 headless Service plugin 并且 Role 带 worker，而目标版本不保留老布局（P3） | worker 的 DNS | 目标版本保留老布局；否则把这类负载排除在不断服的范围之外，另行迁移 |
| 版本的身份不连续（P2） | 全部组被当成过期的 | 升级前在预发环境验证：没有改 `spec` 的 ModelServing 升级之后不替换任何 Pod |
| 升级那一刻正在整组恢复或删除，v1.1.0 | 没有删除标记，删到一半的不续删，只补 | 可以接受；或者等它做完再升级 |
| 升级那一刻已经有 `Failed` 的 Pod，旧版本在容忍它，策略是 `RoleRecreate` 或 `ServingGroupRecreate` | 接管之后终态的成员是故障，不等 grace：同一个实例或整个组里健康的 Pod 被重建（第 6 节） | 升级前检查报出每个这样的 Pod 和会被重建的范围。升级之前先处理掉它（删掉这个 Pod，让旧版本只补这一个）；或者接受。要不要让第一次接管对已有的终态 Pod 只删这一个再补，留给评审定 |
| 回退窗口关闭之后回退 | 随机名字 | 滚动重建 |
| 从在网版本来、在新版本里做过原地更新的 Pod，回退到在网版本 | 它的重启计数大于 0，在网版本之后会把任何一次不就绪当成不健康重启 | 回退前检查 |
| 新版本建的 Pod，回退到在网版本 | 它们没有在网版本的实例身份注解。回退之后，有这种 Pod 的组里按范围的恢复不再触发，丢了成员只补：`recoveryPolicy` 对它们不起作用。按读代码的推演，没有验证 | 回退前检查报出这些组 |
| 关闭回退窗口之后，名字贴着长度上限的 ModelServing | 随机后缀比序号长。存量的组不受影响；下一次替换、扩容、整组恢复建不出新组 | 升级前检查按随机后缀算出余量（7.1）。窗口是集群级的，不能只对一个 ModelServing 留着：关闭之前把余量不够的 ModelServing 换一个短名字重建，否则不关闭窗口 |
| 旧的 controller Pod 停不下来（节点失联、finalizer） | 新进程一直等（X1）。数据面不受影响，控制面的空窗没有上界 | X1 的事件、日志和超时告警；由人处理那个 Pod。不要只把 Pod 对象强制删掉：容器还在跑时 X1 看不到它 |
| 用百分比或 `maxUnavailable` 的 PDB | 不是断服，但升级那一刻允许的驱逐数变了：期望数从组的个数变成这个组里 Pod 的个数（[02](02-design.md) 的 8.5），方向是变严。接管到一半时是两种口径之和 | 升级前检查列出这些 PDB（7.1），升级之后核对。没有验证（第 12 节第 13 项） |

## 11. 危险操作

| 操作 | 后果 | 防护 |
| --- | --- | --- |
| `kubectl delete servinggroup`（默认后台级联） | 这个组的 Pod 被删。ModelServing controller 随即补一个新的组 | 没有权限上的防护：chart 把 `servinggroups` 的 delete 聚合进内置的 edit、admin 角色，能删 Pod 的人本来就能触发恢复（[02](02-design.md) 的 8.10）。create、update、patch 不给。文档写明它相当于删一个副本 |
| `kubectl delete servinggroup --all` | 这个 namespace 里全部 ModelServing 的全部副本同时被删、同时重建，不受滚动预算限制 | 没有，只有文档 |
| `kubectl delete servinggroup --cascade=orphan` | Pod 留下，没有 owner。按 5.4 收回来，有竞态：可能先多建一个组，之后再缩掉一个 | 没有，只有文档 |
| 删除 ServingGroup 的 CRD | 集群里全部 ServingGroup 被删，名下的 Pod 随之被删。量级与删 ModelServing 的 CRD 相同，但这个 CRD 看起来像内部对象，更容易被当成可以清理的 | CRD 留在 `crds/` 并带 `helm.sh/resource-policy: keep`；任何文档步骤里不出现删 CRD |
| `helm uninstall` | CRD 不删，ModelServing、ServingGroup、Pod 都留着，没有人管。重装之后接着管 | 与今天相同 |
| `helm rollback`、`kubectl rollout undo` 到旧版本 | 第 9 节：不做反向接管就是事故。旧版本的 Pod 先起来并且就绪，新版本的 Pod 才被停掉，中间两个版本同时在跑 | X1 的持续观察只停得下新版本一侧的写；旧版本一侧挡不住，它照样删 Pod。文档写明回退的入口是 9.1。X3 在这个方向不起作用：旧 chart 会把旧的 ServiceAccount 建回来 |
| 在新版本运行时启动一个旧版本的进程（另一个 namespace、另一个 release、本地调试） | 第 1 节第 2 条 | 没有，只有文档。X1 只看自己 namespace 里的 Pod；X3 只挡用这个 release 的 ServiceAccount 的旧进程 |
| 回退开关留着没有删（9.1） | 新版本的两个 controller 不做正常的 reconcile，也不做正向接管 | 升级前检查报告它（7.1） |
| 改 `ServingGroup.status` | 账本回到零值：重新建账之前的丢失只补。Drained 被写成真：失败的组被提前删 | `servinggroups/status` 不授给用户 |

开了 `OwnerReferencesPermissionEnforcement` 的集群上，换 owner 需要对 Pod、PodGroup、Service 的 delete 权限；写 `blockOwnerDeletion` 需要对 `servinggroups/finalizers` 的 update 权限。新 chart 的 ClusterRole 按新 controller 的需要重写，包括对 PodGroup 的 patch 和 T2′ 用到的 delete。

## 12. 留给真实验证的

每个源版本各做一遍。前四项决定这套办法成不成立。

1. **owner 换掉之后旧进程的反应。**在旧进程运行时手工把一个 Pod 的 controller owner 换成别的对象，看它删什么。确认第 1 节第 2 条。
2. **不断服。**稳态的负载升级：Pod 的 UID、重启计数、Ready 的时间线全程不变；存量 Pod 的 `hostname.subdomain` 全程解析，Service、Endpoints 不变；PodGroup 的 UID 不变；`ModelServing.status` 的可用数全程不低于升级前。
3. **版本的身份。**没有改 `spec` 的 ModelServing，升级之后目标版本等于存量组的版本，没有组被替换。
4. **直接 `helm upgrade`。**不装 CRD：升级失败或新 Pod 不就绪，旧版本照常。装了 CRD：新进程等旧 Pod 消失才开始接管。
5. 垃圾回收器：owner 被 PATCH 成一个存在的对象时不动 Pod；PATCH 成一个 UID 不存在的对象时删掉 Pod；ServingGroup 用 orphan 删除时 Pod 留下。
6. PodGroup 的 owner 和 `blockOwnerDeletion` 变化时 Volcano 的反应；PodGroup 的 `minMember` 在接管前后不变。
7. 第 6 节的每一行：在那个状态下升级。尤其是滚动中带 surge、带删除标记的恢复、原地更新进行中、灰度停点。
8. 接管的每一步之后杀掉新进程，再起来能续做。
9. 驱逐：接管到一半时，对已接管和未接管的 Pod 各发一次驱逐，预算都算对。
10. 回退：9.1 的步骤走一遍，旧版本起来之后不动 Pod。然后再升级一次。
11. 9.2 的每一条不满足时，旧版本实际做了什么。
12. 换凭据：Helm 升级之后旧 ServiceAccount 的令牌多久失效。
13. 用百分比的 PDB、HPA 的 selector 在接管前后的行为。接管到一半时 PDB 的期望数是不是两种口径之和。
14. 控制面空窗的长度：旧 Pod 的终止时间、取得租约、缓存同步、每个 ModelServing 的接管各占多少。接管屏障的耗时随组数怎样增长。
15. X1 的持续观察：运行中出现不带版本标记的 controller Pod（`helm rollback`、`kubectl rollout undo`）时，新版本多久停下两个 controller，这段时间旧版本删了什么。等旧 Pod 消失期间的事件、日志和超时告警。
16. X3 的范围：另一个 release 里的旧进程能不能并存，Helm 对同名集群级对象怎样处理。
17. T2′：没有活 Pod 的组（v1.1.0 全零的组留下的 PodGroup；Pod 全部在终止的组；T2 删光的组）接管能做完。PodGroup 被删掉、之后重建时 Volcano 的反应。T7 之后还在终止的存量 Pod：同名的新组等它消失。
18. `Done` 之后的无主成员 Pod（5.4）：`--cascade=orphan` 删组之后被收回来；竞态下多出来的组被缩掉的是哪一个；回退窗口开着、新组正好同名时没有多出来的组；`kubectl debug --copy-to` 的副本不被接管，有事件。
19. 回退的持久模式（9.1）：中途杀掉新版本进程，起来之后读到开关接着做；中途删掉开关，落回正向接管，没有多建组；最后一步两个版本的 Pod 并存时没有 webhook 空窗，旧版本不动 Pod。
20. 灰度进行中回退的做法（9.2）：回退之前把 P 调到 N，序号集合在 `[0, N)` 之内时旧版本一个组也不动。
21. 稀疏序号加生效的 partition（第 6 节）：接管之后滚动停下，condition 的 message 写明停在 partition 上；把 P 调小之后继续。
22. 在网版本多出的三个钩子在目标版本里的调用点；登记了删除钩子的 plugin 在 `ServingGroupRollingUpdate` 下滚动之后有没有泄漏。
23. 回退到在网版本之后，新版本建的 Pod 所在的组丢了成员时旧版本的恢复范围（第 10 节）。
24. 关闭回退窗口之后，名字贴着长度上限的 ModelServing 建新组的结果。
25. 带 surge 的滚动途中升级，多出来的组一直没就绪：升级之后停在 `TargetNotReady`，删掉那个组之后收敛。
26. 回退窗口是集群级的设置：开着时，升级之后才创建的 ModelServing 和全零的 ModelServing 新建的组用序号名字；关闭之后新建的组用随机名字，序号名字的组排在前面。
27. 期望成员按组自己的版本判断：旧版本的组里、已经从最新 `spec` 删掉的 Role 的 Pod 被接管；仍然对不上的 Pod 不接管，有事件，升级前检查报得出来。
28. 回退窗口关着而有存量对象时，新版本不接管并报告；显式声明不需要回退之后接管。
29. 升级那一刻已经有 `Failed` 的 Pod：两个源版本各自容忍它的条件；接管之后第一轮重建的范围。
30. 原地更新到一半的组：标签还是旧版本的 Pod 照样换 owner，接管做得完。
31. 收养无主的成员 Pod 期间只有 G4 停下；三种出口各造一次；同名而版本不同的新组只在确认是刚建的时候才被删。
32. 回退的 2′：开关创建之后 controller 的再检查；不满足条件的 ModelServing 不被动到。
