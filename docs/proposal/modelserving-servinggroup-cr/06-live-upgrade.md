# 在网版本的不断服升级

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 已经在运行负载的集群，怎样从旧版本升级到本设计，而已运行的 Pod 不重启、不重建、Ready 不掉。也评估回退 |
| 覆盖的源版本 | 两个：**在网版本**（生产环境运行的分支）和 **v1.1.0**（上游发布版）。它们在对象上的形态、对 owner 变化的反应、选主的行为都不一样，下面分开写。源版本的代码位置和逐项盘点在任务目录的 `subagents/proposal-analysis-upgrade.md`，组一级 surge 标记的盘点在 `subagents/r4-proposal-analysis.md` 第 7.1 节，都不在本文里 |
| 相对上一版 | 上一版（提交 `e35b2e37`）里组的名字是随机后缀，组一级按数量滚动。升级那一刻名字的规则和 `partition` 的含义都要换，所以有一个「回退窗口」。这一版保留组的序号：名字、`partition` 的含义、组一级 surge 的身份在升级前后相同。没有了的和改了的列在[第 8 节](#8-升级前后相同的东西) |
| 状态 | 设计提案。**没有在任何集群上做过升级演练。**关于旧版本行为的结论来自读代码；关于垃圾回收器、Deployment、Helm 的结论按文档和源码；接管和回退的每一步是按规则推演。要在集群上确认的事列在[第 12 节](#12-留给真实验证的) |
| 「不断服」的含义 | 范围是升级和回退。期间：已运行的 Pod 的 UID 不变、容器不重启、Ready 不掉；Service、Endpoints、Pod 的 DNS 记录不变；还有活 Pod 的组，PodGroup 不被重建；没有人改 `spec` 的 ModelServing，升级之后没有 Pod 被替换。控制面（创建和更新 ModelServing、滚动、恢复）允许有一段不工作的时间 |
| 控制面空窗的长度 | 估算式：旧 Pod 的终止时间 + 取得租约 + 缓存同步 + 每个 ModelServing 的接管。数值没有测量，留给真实验证。空窗里会怎样见 [4.1](#41-控制面的空窗) |

## 1. 结论

1. **接管只改元数据。**每个还有活 Pod 的存量的组建一个同名的 ServingGroup 对象，再把 Pod、PodGroup、Service 的 controller owner 从 ModelServing 换成它。Pod 的名字、标签、hostname、DNS、PodGroup 的名字都不变。kubelet 不看 owner，容器不受影响。变的是 owner 的 kind：按 owner 聚合的监控和策略在升级那一刻就受影响（[07](07-user-and-ops-guide.md) 第 7 节）。
2. **新旧进程不能有任何重叠，并且这一点要由新版本自己保证，不能靠操作步骤。**旧进程按 owner 认 Pod。owner 被换掉之后，在网版本会在下一轮把被接管的 Pod 当成占名字的外来 Pod **删掉**，并按恢复策略删同范围的其余 Pod；v1.1.0 把换 owner 当成一次 Pod 删除事件，**立刻删掉**同组或同实例里还没换 owner 的 Pod。重叠的后果不是报错，是删 Pod。新版本自己挡不住的几种（别处的旧进程、本地跑的旧版本）列在第 4 节和第 11 节，只有文档。
3. **「没有人改 `spec`，升级之后不替换任何 Pod」是一条单独的验收条件。**接管本身不删 Pod，不等于接管之后的第一轮正常 reconcile 不删。版本的身份和基线在接管前后要对得上（P2）。`partition` 保护的序号、哪个组是临时的 surge 组，由前后相同的规则保证（第 8 节）。已知的例外有两种，都列在第 6 节和第 10 节：升级那一刻不带 surge 标记的组多于 N；升级那一刻已经有旧版本在容忍的 `Failed` 的 Pod。
4. **组一级的 surge 标记在接管时搬到组对象上，Pod 上的留作副本。**判定只做一次，在建组对象的那一刻，规则与两个源版本自己的判定相同。标记不会后加：判错了没有办法补（5.2、5.3）。
5. **回退是新版本的一个持久的受控模式，不是把旧 chart 装回去。**不做反向接管就启动旧版本：在网版本把全部 Pod 逐个删掉重建，v1.1.0 卡住。回退在名字上没有期限：名字一直是序号，旧版本任何时候都解析得了。回退到在网版本另有两条随新版本运行的时间变多的限定（第 10 节）。灰度进行中可以回退；带 surge 的滚动途中回退时，surge 的组带着标记回去。条件见 9.2。
6. **有几种负载做不到不断服**，列在[第 10 节](#10-做不到不断服的)，升级之前要能查出来。

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
| 组一级 surge 标记的载体 | Pod 注解 `modelserving.volcano.sh/surge`，值 `serving-group`。Role 一级用同一个 key，值 `role` | 相同 |
| 标记什么时候写 | 建组时序号不小于 N：这个组的每个 Pod 都带 | 相同，并且要求是 `ServingGroupRollingUpdate` |
| 之后在这个组里补建的 Pod | 实例里缺了一部分 Pod：从同实例还在的 Pod 继承。整个实例重建、Role 扩容新增的实例：不带 | 不带 |
| 一个组算不算临时的 surge 组 | 序号不小于 N，并且名下至少一个 Pod 带值为 `serving-group` 的注解。终止中的 Pod 也算 | 相同 |
| 序号小于 N 而 Pod 带标记 | 每轮把这些 Pod 的注解摘掉 | 相同 |
| 标记用在哪 | 缩容时把临时的组排除在正式的组之外；建组时算正式的名额；回收临时的组 | 算期望的组数；回收临时的组；判断有没有没回收完的 |
| `RoleRollingUpdate` 下的组一级标记 | 摘标记和「临时的组」的判定不看滚动模式 | 不看标记：带标记的组按正式的组算 |
| surge 的容量建在哪个序号上 | 正式的组不够 N 个、或者有一个范围外的旧版本的组等着被换掉时，建在小于 N 的空洞里，**不带标记**；否则建在不小于 N 的序号上 | 在 `[0, N + S)` 里最低的缺失序号上。稀疏的序号集合下会落在小于 N 的空洞里，那样的组**不带标记**。上游 `main` @ `79c455ef` 相同 |
| revision 的算法 | 与上游不同 | 与上游 `main` 相同 |
| headless Service 的布局 | 每个实例一个 | 每个实例两个，worker 的 subdomain 指向另一个 |
| 独有的 plugin 和 API 字段 | 有 | 有（原地更新等） |
| plugin 框架的钩子 | 七个：比上游多 Pod 运行、Pod 删除、整组删除三个 | 四个，与上游 `main` 相同 |
| 选主 | 默认关。开了之后失主会退出 | 默认关。开了之后失主**不退出**，探针照样通过 |
| 部署 | 单副本，Deployment 滚动更新，webhook 都是 `failurePolicy: Fail`（驱逐 webhook 可配），CRD 在 chart 的 `crds/` | 相同 |

组一级 surge 标记的几行合起来有两个后果，接管时都要照顾到（5.2 的 T3）：

- 一个临时的 surge 组里可以只有一部分 Pod 带标记。单 Pod 的组里那个 Pod 被换过之后，一个都不带：这样的组在两个源版本眼里已经是正式的组。
- 稀疏的序号集合下，滚动途中多出来的那份容量可以在小于 N 的序号上，不带标记。这时不带标记的组多于 N（第 6 节）。

## 3. 升级的前提

对目标版本的要求。任何一条不满足，这个源版本就不能不断服地升上来。

| # | 前提 | 不满足时 |
| --- | --- | --- |
| P1 | **目标版本是源版本的超集**：API 字段、已注册的 plugin、plugin 框架的钩子集合、ControllerRevision 的数据格式、校验规则 | 少一个 plugin：plugin 链构造失败，用到它的 ModelServing 建不了 Pod。少一个字段：下一次写的时候被裁掉。少一个钩子：靠它做事的 plugin 不再被调到。在网版本有上游没有的 plugin 和字段，plugin 框架也比上游多三个钩子，目标版本都要带上（表下） |
| P2 | **版本的身份连续**：存量 Pod 上的 revision 标签，在目标版本看来与没有改过的 `spec` 是同一个版本。靠的是已有的机制：哈希不等时经 ControllerRevision 解出模板按语义比较，目标版本的解析先沿用已有的身份 | 接管之后第一轮，序号不小于 P 的组都是可替换的（[02](02-design.md) 的 4.1），按滚动预算逐个替换。在网版本的 revision 算法与上游不同，这一条对它是实打实的风险 |
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
- 控制面的空窗：从旧 Pod 开始终止，到新进程取得租约、缓存同步完、这个 ModelServing 接管完成（第 5 节）。新进程里两个 controller 之间没有先后的要求（[02](02-design.md) 的 [6.3](02-design.md#63-直读和复核)）。期间会怎样见 [4.1](#41-控制面的空窗)。
- 源版本如果已经开了选主（在网版本改一个 values 就能开，并且失主会退出），X4 对这一次升级也生效。v1.1.0 失主之后不退出，不能靠它。

**评估过、没有采用的**：接管时保留两条 ownerReference（ServingGroup 是 controller，ModelServing 是非 controller）。它能让旧进程和旧 webhook 仍然认这些 Pod、让误删组和 CRD 不删 Pod、让回退不依赖反向接管先做完。没有采用，因为对象上还有另一个存在的 owner 时前台级联不删它，组的删除（02 的 5.4）在这段时间里失效，而替换和整组恢复都靠「旧对象和它的 Pod 消失之后，同名的新对象才建得出来」（02 的 [4.5](02-design.md#45-名字确定创建幂等同名重建)）；并且独占仍然是必须的（在网版本会继续按自己的规则管这些 Pod）。

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
| 容器崩溃 | kubelet 按 `restartPolicy` 重启容器。没有按策略的重建 | 升级上来的这一次没有故障起点可读：重启故障的 grace 从新进程第一次看到它时起算，与今天相同。起点从这时起写在组的 `status` 里，之后的每次重启接着算（[02](02-design.md) 的 5.3；第 11 节） |
| 进行到一半的滚动、恢复、缩容 | 停在原地。多出来的组继续占资源 | 第 6 节 |
| `ModelServing.status` | 不更新 | 接管注解是 `InProgress` 期间只更新 condition，计数保留旧版本写的值（5.2）；`Done` 之后重算 |
| 旧的 controller Pod 停不下来 | 以上各行一直持续。空窗没有上界（第 10 节） | — |

空窗里 webhook 一直有进程应答：新 Pod 就绪之后旧 Pod 才被停掉。没有请求因为没有人应答而被拒绝。

回退的空窗是从创建回退开关到旧版本的 controller 起来（9.1）。「空窗里」一栏对它同样成立；之后由旧版本按它自己的规则接着做。

## 5. 接管

接管按 ModelServing 进行。它幂等，任何一步停下都可以续做。

### 5.1 多出来的一个状态

只有成员一级多一个状态。组一级没有「接管中」这个状态，组对象上也没有为接管加的注解或 condition：存量的成员凭 Pod 自己认。

| 层 | 状态 | 判定 | 动作 |
| --- | --- | --- | --- |
| Pod 成员 | 待接管（存量） | 标签是这个组的；不在终止中；名字是这个组某个期望成员的名字；并且 controller owner 是这个 ModelServing（按 UID），或者没有 controller owner 而所属 ModelServing 的接管还没有做完（没有接管注解，或者注解是 `InProgress`） | 是成员。**不为它的名字创建，也不删除它**；换 owner（T4） |

- **不看版本标签。**原地更新到一半、标签还是旧版本的 Pod 照样是待接管的成员。放弃回退时（9.1）也接得回来。
- **不要求代际号标签。**旧版本不写它。T4 换 owner 时补上。
- controller owner 是这个 ModelServing 的 Pod 只有两个来源：升级之前的存量；回退时换回去的（9.1）。
- **接管期间没有 controller owner 的带标签的 Pod 也接管。**旧版本下有人摘过 owner、或者用 orphan 方式删过 ModelServing 又建了同名的，会留下这样的 Pod。它们没有代际号标签。ServingGroup controller 看所属 ModelServing 的接管注解：没有注解或者是 `InProgress` 时按存量接管，不要求代际号标签。这是成员状态「待接管」的第三种（[03](03-state-machine.md) 第 3 节）。与上一版相同。
- `Done` 之后才出现的、没有 controller owner 的成员 Pod 是另一种待接管，要求带代际号标签，判定见 5.4。

**存量对象**：带这个 ModelServing 的名字标签、controller owner 是它（按 UID）的 Pod、PodGroup、Service；接管还没有做完时（没有接管注解，或者注解是 `InProgress`）另包括带这个标签、没有 controller owner、名字对得上期望成员的 Pod。T1 做判断的那一刻注解还没有，也按这个口径数：名下只有这种 Pod 的 ModelServing，T1 写的是 `InProgress`，不是 `Done`。T1、T7 数的是它们。终止中的存量 Pod 不算。

「期望成员的名字」按名字的格式判断：`<组名>-<role>-<实例序号>-<worker 序号>`。role 按这个组自己的版本判断：Pod 的 revision 标签指向的 ControllerRevision 里的 Role；那个版本读不到时，按当前 `spec` 的 Role。这样旧版本的组里、已经从最新 `spec` 删掉的 Role 的 Pod 仍然是成员。实例序号超出当前 `replicas` 的也算：它所在的实例接管之后按 [03](03-state-machine.md) 的 4.3 的 E4 清空。

仍然对不上的带标签的 Pod：不接管，不删除，不计入存量对象（T1、T7 不数它），发事件报告，由人处理。owner 还是 ModelServing 的也一样。`kubectl debug --copy-to` 加 `--keep-labels` 复制出来的 Pod 属于这一种：它带着原 Pod 的标签，没有 owner。升级前检查把它们报出来（7.1）。

ModelServing 上有一个注解记接管的进度：没有（不知道）、`InProgress`、`Done`、`RollingBack`（回退中，9.1）。用注解而不是 `status`：旧版本的 CRD 会裁掉它不认识的 status 字段。**没有这个注解不等于不需要接管**：它的含义是「还没查过」。接管在 ModelServing 上只记这一个注解。

### 5.2 步骤

| 步 | 谁 | 做什么 | 写 | 这一步之后停下 |
| --- | --- | --- | --- | --- |
| T1 | ModelServing controller | 这个 ModelServing 没有注解，或者是 `InProgress`，或者是 `RollingBack` 而回退开关不在（9.1）：**对 apiserver 直读**名下的存量对象（5.1）。一个都没有：写 `Done`。有：写 `InProgress`。判据只有这一个事实：直读到了存量对象。终止中的存量 Pod 不算，与 T7 同一个口径 | PATCH ModelServing 的注解 | 重做：直读的结果相同 |
| T2 | ModelServing controller | 带删除标记而还活着的存量 Pod（只有在网版本有）：按 UID 删除。它们不接管 | DELETE Pod，带 UID | 重做：还在的再删 |
| T2′ | ModelServing controller | 没有「活着的、不带删除标记的」存量 Pod 的组名：它名下存量的 PodGroup、Service，直读确认之后按 UID 删掉。确认的内容：对象的 UID 没变，这个组名下仍然没有活着的、不带删除标记的存量 Pod | DELETE PodGroup、Service，带 UID | 重做：还在的再读再删 |
| T3 | ModelServing controller | 每个还有「活着的、不带删除标记的」存量 Pod 的组名：创建同名的 ServingGroup。名字就是存量的名字，序号从名字解析。创建之前直读 ModelServing 和这个组名下的存量 Pod（终止中的也读），判定建出来的对象带不带 surge 注解（表下）。Pod 全部在终止的组不接管，也不等它们消失（T7） | 创建 ServingGroup | 重做：撞名字。直读那个对象，核对 controller owner 的 UID 是这个 ModelServing。对象上有没有 surge 注解以第一次创建的为准，不重算 |
| T4 | ServingGroup controller | 每个待接管的成员：先**直读这个 ServingGroup**，确认 UID、确认没有 `deletionTimestamp`；然后把 controller owner **换成**它（不是追加），补上 `blockOwnerDeletion`，没有代际号标签的补上（值是 0）。**不摘** Pod 上的 surge 注解。PodGroup、Service 同样换 owner | PATCH，带对象的 UID 和 resourceVersion | 一个组换到一半：剩下的仍然是待接管的成员，不会被当成缺失 |
| T5 | ServingGroup controller | 账本从零值开始。一个实例的成员（按它已经应用的模板布局）全部在缓存里、直读确认过：写 Established。不齐的留在 Creating | UPDATE status | 重做 |
| T7 | ModelServing controller | 直读：名下没有存量对象了。**终止中的存量 Pod 不算存量对象**，不等它消失。并且接管来的组都已观测（表下）。满足之后写 `Done` | PATCH 注解 | 重做 |

没有 T6。上一版在这个位置由 ServingGroup controller 在组上写一个 condition，表示这个组接管完了，T7 等它。这一版没有那个 condition，T7 自己直读。编号空着，别处引用的 T7 不变号。

`InProgress` 期间，ModelServing controller 对这个 ModelServing 不做 S1′、G0 到 G6 的任何写（[03](03-state-machine.md) 第 7 节的升级闸门，M3）。`status` 的计数不重算，保留旧版本写的值，只更新 condition（[03](03-state-machine.md) 第 6 节）：刚接管来的组还没有写出 Ready，重算会让可用数掉到 0 再回来。

闸门只停组一级的写。`InProgress` 期间，ServingGroup controller 对已经换完 owner 的组照常做 [03](03-state-machine.md) 第 8 节的全部步骤：补缺的成员，接着做组内没滚完的 Role 一级的滚动和扩缩容，按策略恢复。动手恢复之前，恢复策略和 grace 从 ModelServing 上直读（[02](02-design.md) 的 5.3）：接管没做完时把策略改成 `None`、`-1` 或者更长的 grace 同样生效。反方向（打开策略、缩短 grace）要等 G5 下发，`InProgress` 期间 G5 不做，不生效。

**T3 怎样判定 surge 注解。**标记只在创建对象时写，之后只会被摘掉，不会后加（[02](02-design.md) 的 2.1）。所以判定必须在 T3 做完，不能留给 T4，也不能留给 `Done` 之后的 G0。

| 直读到的 | 建出来的对象 | 理由 |
| --- | --- | --- |
| `ServingGroupRollingUpdate`；序号不小于直读到的 N；名下至少一个 Pod 带值为 `serving-group` 的 surge 注解，终止中的 Pod 也算 | 带 surge 注解 | 与两个源版本自己的判定逐条相同（第 2 节）：任一 Pod、不排除终止中的、比对值、按当时的 N |
| 同上，只有一部分 Pod 带 | 带 | 按「至少一个」。之后标记在对象上，不再有「打了一半」 |
| 序号小于 N，而 Pod 带标记 | 不带 | 旧版本下一轮就会把这些 Pod 上的注解摘掉，在它眼里这已经是正式的组。不留给 G0：G0 另有一个条件「正式的组少于 N」，在接管来的集合里它可以不成立（02 的 4.2） |
| 序号不小于 N，没有一个 Pod 带（带标记的 Pod 都被换掉过） | 不带。它是一个范围外的正式的组 | 旧版本也已经把它当成正式的组。正式的组多于 N 时按缩容处理 |
| `RoleRollingUpdate` | 不带，不论 Pod 上有没有 | 这个模式下组一级的标记不生效（02 的 4.6）。不从接管这里造出一个带标记的组 |
| Pod 上这个 key 的值是 `role` | 不算 | 那是 Role 一级的标记。同一个 key 有两种值，读的地方比对值（02 的 4.9） |

- **T3 重做时不重算。**撞名字说明对象已经在了。它带不带注解已经定了，不会后加。N 在第一次创建之后变了的话，由 `Done` 之后的 G0 处理：`InProgress` 期间不做 G0 到 G6。
- **Pod 上的注解留着，是副本。**T4 之后，ServingGroup controller 按 02 的 2.3 维护它：组带注解，名下每个 Pod 都补上值为 `serving-group` 的注解；组不带，把 Pod 上值为 `serving-group` 的摘掉。值为 `role` 的不是副本，接管不动它。
- **在 T3、T4、副本同步之间的任何一处停下都没有影响。**新版本的决定只读组对象上的那一个注解。Pod 上的副本只在两处被读，都是建组对象的那一刻：这里的 T3，和找回（5.4）。

**T3 里 ServingGroup 的 `spec` 怎样填：**

| 情形 | 取值 |
| --- | --- |
| `ServingGroupRollingUpdate` | `revision` 取这个组的 Pod 的 revision 标签。组正在原地更新（只有 v1.1.0 有）：取进度注解里的目标版本。一个组的 Pod 标签不一致、又没有进度注解（例如从 Role 模式切过来之后没滚完的组）：`revision` 留空，按 [02](02-design.md) 的 4.7 取 `status.revision`，为空算可替换 |
| `RoleRollingUpdate` | `targetRevision` 取 ModelServing 的 `status`；各实例的版本从成员的标签读 |
| `baselineRevision` | 两种模式都带，取 ModelServing 的 `status.currentRevision` |
| `roles`、`recoveryPolicy` 等 | ModelServing 当前的 `spec`，与 G5 会下发的相同 |
| `expectedPods` | 按这个组的版本的模板算（[02](02-design.md) 的 8.5）。T3 建的组版本取自 Pod 的标签，可以不是目标版本 |
| 代际号 | 存量 Pod 没有这个标签，按 0 |

几点说明。

- **T7 的第二个条件。**T7 如果只看「没有存量对象了」，owner 换完而 ServingGroup controller 还没写出第一次 `status` 的那一段里就会写 `Done`。`Done` 之后计数重算，可用数只数报告过就绪的组：还没写出 `status` 的组不计入，可用数会先掉下去再回来；缩容的次序这时也没有就绪可排。所以 T7 另外等接管来的组都已观测（`status.observedGeneration` 不小于 `metadata.generation`，02 的 A1）。它用的是已有的约定，不是新的 condition。一个组的 `status` 一直写不出来时接管停在 `InProgress`，`Progressing` 的 message 写明是哪个组。
- **新进程里两个 controller 之间没有先后的要求。**上一版要求 ServingGroup controller 先把每个组过一遍，ModelServing controller 才对它发组一级的写。这一版没有这一步。停机期间变坏或者恢复而 `status` 没有人写的组由复核挡住：组的 `status` 要是按 ModelServing controller 在 Pod 缓存里现在看到的这些 Pod 算的才算数，不新鲜的按未观测算（02 的 [6.3](02-design.md#63-直读和复核)）。接管来的组在写出第一次 `status` 之前是未观测：占位置，不算确认的就绪，不是替换的候选。
- **接管注解丢了**（例如对象被整体替换）。按「还没查过」重走 T1，结果由直读决定。组的名字是存量的名字，标记在组对象上，都不记在 ModelServing 上，不受影响。
- **T2′ 处理的三种存量对象。**v1.1.0 全零的组留下的 PodGroup；升级那一刻 Pod 全部在终止的组的 PodGroup 和 Service；在网版本上被 T2 删光的组的 PodGroup 和 Service。它们没有可以换过去的 ServingGroup，留着的话 T7 永远过不了。
- **T7 之后还在终止的存量 Pod。**它消失之前，名字相同的新组的那个成员位置等着，不创建（[03](03-state-machine.md) 的 4.3，E7）。
- T2、T2′ 是 ModelServing controller 直接删 Pod、PodGroup、Service，只在接管期间有。

### 5.3 为什么每一步都是安全的

| 担心 | 为什么不会 |
| --- | --- |
| Pod 缓存还空着，闸门误放行：ModelServing controller 在序号 0 到 N − 1 上自己建组，版本按序号和 `partition` 选，不看存量 Pod 的版本、surge 标记和删除标记 | T1、T7 是直读 |
| 一个组换 owner 换到一半，其余成员被当成缺失去建同名 Pod | 待接管的成员是成员 |
| 同名的 ServingGroup 被删了重建，缓存里是旧的 UID，写进 Pod 的 owner 是一个不存在的对象，垃圾回收器删掉这个 Pod | T4 之前直读这个 ServingGroup 的 UID 和 `deletionTimestamp`。这一版里每次替换之后同名的对象都换 UID，所以这条直读不是升级独有的：收养无主的成员 Pod 时同样要做（02 的 5.5）。它挡不全：PATCH 的前置条件是 Pod 的，不是组的，直读之后、PATCH 之前组换了代的那一小段挡不住（02 的 6.5）。`InProgress` 期间闸门关着，ModelServing controller 不删组，这个窗口只剩用户自己删组 |
| 接管来的实例被当成「建成过」，第一次观察到的不齐被当成丢失 | 账本从零值开始，零值只补 |
| 带删除标记的 Pod 所在的组或实例被整个删掉（标记是逐个打的，可能只打了一半） | 只删带标记的 UID，范围正好是原来选定的 |
| 没有活 Pod 的组留下的 PodGroup、Service，或者一个卡在终止中的存量 Pod，让接管永远做不完 | T2′ 删掉前者；T7 不等后者 |
| T2′ 删掉了一个还有活 Pod 的组的 PodGroup | 删之前直读。闸门关着，这期间没有人给这个组建 Pod |
| 带着标签的无关 Pod（调试用的副本）被接管成成员 | 待接管的成员要求名字是期望成员的名字 |
| 一个临时的 surge 组被建成了正式的组，或者反过来。标记不会后加，判错了没有办法补 | T3 的判定是直读，不走缓存；终止中的 Pod 也读；按「至少一个」；比对注解的值；N 取直读到的。T3 重做不重算 |
| T3 撞名字，而那个对象不是这个 ModelServing 的 | 核对 controller owner 的 UID。不是自己的：不建，不删它，不收为己有，发事件（02 的 [4.5](02-design.md#45-名字确定创建幂等同名重建)）。这个组名下的存量 Pod 留在待接管，接管停在 `InProgress`，`Progressing` 的 message 写明被哪个对象挡住，等人删掉那个对象。升级之前集群里没有 ServingGroup 对象，这种情形只可能来自一次没做完的升级之后 ModelServing 被删了重建 |
| Pod 上的 surge 注解没有摘，新版本把它当成决定的输入 | 新版本的决定只读组对象上的注解。Pod 上的只在建组对象的那一刻被读（T3、找回） |
| 新进程的第一轮按停机之前的 Ready 删健康的组 | `InProgress` 期间不做 G0 到 G6。`Done` 之后：未观测的组不是替换的候选；组的 `status` 要是按现在这些 Pod 算的才算数（02 的 6.3） |

证据的类别：

- 模型：组级模型把一个存量的组接成一个同序号、同版本、同标记的对象。接管完成之前不创建、不删除任何组；去掉闸门之后这条性质被违反（[05](05-inference-verification.md) 第 3 节）。
- 推演：标记怎样从 Pod 上判定，副本怎样同步，成员一级的各步，T2′，5.4。
- 走读：两个源版本自己怎样判定临时的组（第 2 节）。

### 5.4 `Done` 之后出现的无主成员 Pod

接管注解已经是 `Done` 之后，仍然可能出现没有 controller owner 的成员 Pod：有人用 `kubectl delete servinggroup --cascade=orphan` 删了一个组。它们的找回和收养是正常 reconcile 的一部分，规则在 [02](02-design.md) 的 [5.5](02-design.md#55-有人直接删了一个组)：ModelServing controller 为它们重建同名的组对象（S1′），ServingGroup controller 换 owner，版本不同的回到重建。不动接管注解，也不停下这个 ModelServing 的其余动作。

这里只列它与升级接管的差别：

| | 升级接管（5.1、5.2） | `Done` 之后的找回和收养（02 的 5.5） |
| --- | --- | --- |
| Pod 的 controller owner | 这个 ModelServing | 没有 |
| 代际号标签 | 不要求 | 要求。它是 ServingGroup controller 管过这个 Pod 的痕迹；升级接管时 T4 给存量的 Pod 补上 |
| 谁建组对象，在什么时候 | T3。闸门关着，S1′、G0 到 G6 都不做 | S1′。在正常的一轮里，先于 G0 |
| 组的版本 | T3 的表：取 Pod 的标签或进度注解，不一致时留空 | Pod 的 revision 标签一致并且 ControllerRevision 还在时用它；否则按序号和 `partition` |
| 成员的版本与组的 `spec.revision` 不同 | **不比较。**存量的成员不看版本 | `ServingGroupRollingUpdate` 下、组里没有未完成的原地更新时，写 `RecreateRequired`，在预算之内替换 |
| surge 注解 | `ServingGroupRollingUpdate`，序号不小于 N，至少一个 Pod（含终止中的）带值为 `serving-group` 的注解 | 同一条规则 |
| 接管注解 | `InProgress` | 不动 |
| 带删除标记的 Pod | T2 按 UID 删 | 没有：新版本不写这个标记 |

「版本不同回到重建」不能用在 owner 是 ModelServing 的 Pod 上。v1.1.0 原地更新到一半的组里，Pod 的标签还是旧版本，组的 `spec.revision` 取的是进度注解里的目标。对它比较版本的话，升级那一刻这样的组会被替换，而没有人改过 `spec`。

升级那一刻就没有 controller owner 的带标签的 Pod 不属于上面任何一种，见第 6 节的末行。

## 6. 升级那一刻的状态

旧进程停下时负载可以处在任何状态。每一种怎样接过来：

| 升级时的状态 | 对象上是什么 | 接过来之后 | 相对旧进程继续做，差在哪 |
| --- | --- | --- | --- |
| 稳态 | N 个组，全部就绪 | N 个同名的 ServingGroup，全部 Established。没有动作 | — |
| 滚动中，有一个临时的 surge 组 | N + 1 个组。序号不小于 N 的那个组至少一个 Pod 带值为 `serving-group` 的注解 | 都接管。T3 建的那个对象带 surge 注解；Pod 上的注解不摘，之后 ServingGroup controller 把副本补到它名下的每个 Pod 上。`Done` 之后照旧版本的方式做完（[02](02-design.md) 的 4.2）：在预算之内替换旧组，G4 在腾出来的序号上按序号和 `partition` 重建；没有可替换的正式组之后回收 surge 组 | —。多建又多删的那一个组与旧版本相同。那个 surge 组一直没就绪时，没有滚动要做之后它直接被回收，不会留在 N 个里面 |
| 同上，N 刚调大，surge 组的序号已经小于 N，旧版本还没来得及摘 Pod 上的注解 | 序号小于 N 的组里有 Pod 带标记 | T3 建的对象不带注解，它是正式的组。Pod 上的由 ServingGroup controller 摘掉 | —。旧版本下一轮也摘 |
| 同上，surge 组里带标记的 Pod 都被换掉过 | 序号不小于 N 的组，没有一个 Pod 带标记 | 对象不带注解，是范围外的正式的组。正式的组多于 N：按缩容处理，不替换，不建 surge，G2 删一个 | 旧版本同样已经把它当成正式的组。之后删哪一个，两边各按自己的缩容次序，没有逐例比 |
| 稀疏的序号集合上滚动中，多出来的那份容量建在小于 N 的空洞里 | 例如 `{0, 3}`、N = 2、S = 1，两个组都是旧版本；旧版本在序号 1 上建了目标版本的组。三个组都不带标记，不带标记的组多于 N。两个源版本都会这样（第 2 节） | 三个组都接管，都是正式的组，正式的组多于 N：缩容中，不建 surge，不替换，G2 删一个。`sg-1` 已经就绪：按 deletion cost、再按序号高的先，删 `sg-3`。`sg-1` 还没就绪：未就绪的先，删的是刚建的 `sg-1`；之后在序号 2 上建带注解的 surge 组，重新来 | 前一种与旧版本的意图相同（留下 `sg-1`，换掉范围外的 `sg-3`）。后一种：**没有人改 `spec`，一个还在起的组被删掉重建。**没有健康的组被多删。接受这个结果；升级前检查报出这样的 ModelServing，建议等滚动做完再升级（7.1、第 10 节） |
| `RoleRollingUpdate` 下，Pod 上留着组一级的标记（从 `ServingGroupRollingUpdate` 切过来时留下的） | 序号不小于 N 的组里有 Pod 带值为 `serving-group` 的注解 | 对象不带注解；Pod 上的由 ServingGroup controller 摘掉。它是正式的组，多于 N 时由 G2 缩掉（02 的 4.6） | v1.1.0 在这个模式下不看这个标记，相同。在网版本仍然把它当临时的组；接管之后多出来的组按缩容次序删，删的不一定是它。没有验证 |
| Role 一级的滚动进行中，有临时的 Role 实例 | Pod 上这个 key 的值是 `role` | 接管不动它。`RoleRollingUpdate` 下它是 Role 一级标记的来源，ServingGroup controller 按 Role 一级的规则续做。`ServingGroupRollingUpdate` 下留着的（切换模式留下的）：ServingGroup controller 摘掉，多出来的实例按 Role 的缩容处理（02 的 4.6） | 前一种没有差别。后一种旧版本怎样做没有读 |
| partition 灰度停点 | 序号小于 P 的是旧版本 | 序号小于 P 的组受保护，就是原来那几个。没有动作 | — |
| 保护的序号里有一个不是基线的旧版本（P 调大之后又提交过新版本） | — | 序号小于 P 的组不论是哪个版本都不动（02 的 4.3） | —。两个源版本也按绝对序号 |
| 稀疏序号 | 例如 `{0, 3}` | 两个组照常接管，序号从名字解析。之后扩容补小于 N 的最低空洞。`partition` 生效时受保护的仍然是序号小于 P 的组 | 这几条与旧版本相同。范围内的组被替换或整组恢复之后可能落到更低的空洞上（02 的 4.2）；两个源版本在这种布局下各自落在哪，没有逐例比 |
| 整组恢复或删除进行中，在网版本 | 一部分 Pod 带 `ServingGroup` 的删除标记 | T2 按 UID 删掉带标记的。没带标记的照常接管；实例不齐，留在 Creating，补。整个组都带标记的：Pod 被 T2 删光，PodGroup、Service 由 T2′ 删掉，之后按下面「整个组的 Pod 都在终止」一行 | 标记只打了一半时，没带标记的成员留下，与契约 2.11 相同 |
| 同上，v1.1.0 | 没有标记；组里只剩一部分 Pod，PodGroup 可能已经没了 | 接管，只补。第一轮确保 PodGroup、Service 存在 | 与 v1.1.0 自己重启的结果相同：不续删 |
| 实例恢复或删除进行中 | 同上，范围是一个实例 | 同上 | 同上 |
| 个别 Pod 正在终止 | — | 不接管。T7 不等它；它消失之前所在的位置不创建（[03](03-state-machine.md) 的 4.3，E7） | — |
| 整个组的 Pod 都在终止 | — | 这个组不接管，PodGroup、Service 由 T2′ 删掉。闸门放行之后这个序号空着，补不补按当时的规则（02 的 4.2）。正式的组少于 N 时 G4 补一个，建在小于 N 的最低空闲序号上。就是这个序号时（连续的序号集合下总是）：补出来的组与它同名，各成员位置等这些 Pod 消失，新旧两批 Pod 不并存。更低处另有空洞时（稀疏布局）：建在那个更低的序号上，新组的 Pod 在旧 Pod 停完之前就开始创建 | 稀疏布局的那一种里，旧 Pod 停完之前容量短时间多出一个组：这些 Pod 没有组对象，不在 `active` 里。契约 0.3 里删除中的组在消失之前占着容量；两个源版本在这一点上的实现没有核对 |
| 缩容进行中 | 多于 N 个组，有的在终止 | 活着的接管；闸门放行之后 G2 按次序再选 | v1.1.0 没有标记，再选的不一定是原来那个 |
| 原地更新进行中（v1.1.0） | 一部分 Pod 的标签是旧版本，带进度注解 | 组的版本取注解里的目标；成员分类先看进度注解。续做。续不下去的（超期、被作废、有终态成员、plugin 不支持）：ServingGroup controller 把组的 `RecreateRequired` 写成真，这个组按可替换的算，在预算之内删除，在原序号上重建（[02](02-design.md) 第 9 节） | 没有验证：原地更新没有模型 |
| 存量 Pod 已经是 `Failed`，旧版本在容忍它（grace 是 `-1`，或者还没到期） | 一个终态的 Pod；同一个实例、同一个组里别的 Pod 是健康的 | 照常接管。之后的第一轮：终态的成员是故障，不看账本是不是零值、不等 grace（[03](03-state-machine.md) 的 4.3，E5）。`RoleRecreate` 重建这个实例，`ServingGroupRecreate` 换掉整个组 | **健康的 Pod 被重建，而没有人改过 `spec`。**这不满足不断服（第 10 节）。它是 005 已批准的 D10、D13 在升级那一刻生效，不是本设计新加的规则；旧版本的容忍取自契约 2.12 的文字，没有读源版本的代码 |
| 存量 Pod 在重启故障里，grace 还没到期 | 容器在反复重启，Pod 不 Ready | 照常接管。没有故障起点可读，grace 从新进程第一次看到它时起算 | 与旧版本自己重启一次相同 |
| 全零的组（N 大于 0，各 Role 的副本数都是 0） | 没有 Pod；v1.1.0 可能留着 PodGroup | 没有可接管的 Pod。留着 PodGroup 的：T1 直读到存量对象，T2′ 把这些 PodGroup 按 UID 删掉。什么都没留的：T1 直接写 `Done`。两种情形闸门放行之后 G4 都在序号 0 到 N − 1 上建空的组，ServingGroup controller 建 PodGroup。空组的版本按序号指派：序号小于 P 并且基线不等于目标时是基线，否则是目标（02 的 4.2） | 存量的 PodGroup 被删了重建，UID 变了，它名下没有 Pod。各 Role 的副本数调回来时，各序号的版本与旧版本按序号新建的结果相同 |
| roleCoordination 滚动进行中 | 基线在 ControllerRevision 的注解上 | 由 ModelServing controller 读出来，下发到组的 `spec` | — |
| owner 是 LeaderWorkerSet 的 ModelServing | — | 与别的 ModelServing 相同。组名一直是序号，plugin 照旧从组名里取 `group-index`（02 的 2.2） | — |
| `Done` 之后才出现的无主成员 Pod（`kubectl delete servinggroup --cascade=orphan`） | 带代际号标签，没有 controller owner | 按 5.4 找回、收养 | 连续的序号集合下没有多出来的组。稀疏集合下有一个竞态：先在更低的序号上多建一个组，再缩掉一个（02 的 5.5） |
| 升级那一刻就没有 controller owner 的带标签的 Pod（有人摘过 owner；用 orphan 方式删过 ModelServing 又建了同名的） | 没有 controller owner，也没有代际号标签 | 名字对得上期望成员的：接管期间按存量接管（5.1）。对不上的：不接管，不删除，发事件报告。升级前检查把两种都报出来 | 与上一版相同。两个源版本对它：在网版本删掉，v1.1.0 卡住。它在升级之前已经是异常 |

接管本身在每一行里都不创建、不删除健康的 Pod。T2 删的是旧进程已经决定要删、并且标在 Pod 上的那些。T2′ 删的是名下没有活 Pod 的 PodGroup、Service。

相对上一版变好的一处：整个组的 Pod 都在终止的那一行。上一版里补出来的组是另一个名字，新旧两批 Pod 总是并存；这一版在连续的序号集合下不并存。

## 7. 操作步骤

| 步 | 做什么 | 为什么 |
| --- | --- | --- |
| 1 | **升级前检查**（只读）。查出：进行中的滚动和恢复、哪些组是临时的 surge 组、不带标记的组多于 N 的 ModelServing、带删除标记的 Pod、进行中的原地更新、第 10 节的负载、驱逐 webhook 是否开着和它的 `failurePolicy`、源版本是否开了选主、已 cordon 或正在排空的节点。规定的输出见 [7.1](#71-升级前检查的输出) | 有些状态升级那一刻有特殊后果；有些负载做不到不断服 |
| 2 | 装 CRD：ServingGroup 的 CRD，以及更新过的 ModelServing 的 CRD。用 `kubectl apply --server-side`（ModelServing 的 CRD 很大，客户端 apply 会因为注解太长失败） | Helm 升级不处理 `crds/`。X2 在这一步没做时会拦住 |
| 3 | `helm upgrade` 到新版本。从这一步到第 5 步验收完，不要排空节点，不要做会删 Pod 的操作 | 新的 Deployment、RBAC、ServiceAccount、webhook 配置。空窗里被删的 Pod 没有人补（4.1），验收的基线也会被打乱 |
| 4 | 等。新 Pod 起来只应答 webhook（X1），它的事件和日志写着在等哪个旧 Pod；旧 Pod 被停掉；新进程看不到旧 Pod 之后取得租约，缓存同步完，各个 ModelServing 开始接管 | — |
| 5 | 验收：每个 ModelServing 的接管注解是 `Done`；每个还有活 Pod 的存量的组有一个同名的 ServingGroup；带 surge 注解的 ServingGroup 与升级前检查记下的临时的组是同一批；Pod 的 UID 和升级前相同、重启计数没有变；没有人改过 `spec` 的 ModelServing 没有组在终止（升级前检查已经报出会缩掉一个组的除外）；`status` 的可用数全程不低于升级前；存量 Pod 的 `hostname.subdomain` 仍然解析；还有活 Pod 的组，PodGroup 的 UID 不变 | 第 0 节的定义，第 1 节的第 3 条 |

验收之后没有别的步骤。升级之后组的名字、Pod 的名字不变，回退在名字上没有期限（第 8、9 节；回退到在网版本的限定见第 10 节）。

第 2 步和第 3 步的次序反了、或者第 2 步漏了：X2 让升级在开始前失败，或者新 Pod 不就绪、旧 Pod 不被停掉，旧版本照常工作。

不要做的事：

- 任何步骤里都不删 CRD。chart 里 CRD 的位置不要变（位置一变，Helm 升级会删掉它，连带全部实例）。
- 升级和重启 controller 的同时不要排空节点，不要做会删 Pod 的操作。这一次升级：空窗里丢的 Pod 没有人补，接管之后只补。按用户的口径升级不赦免，这一次没有做到，与今天旧版本自己升级时相同（[README](README.md) 的 OP8）。之后新版本的每一次升级和重启：停机期间丢了成员的已建成实例，回来之后按 `recoveryPolicy` 的范围恢复，重建的 Pod 比丢的多；停机之前就在重启故障里的成员，grace 接着算，停机超过 grace 时回来的第一轮就恢复（第 11 节；[07](07-user-and-ops-guide.md) 的 4.2、4.3）。

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
| 集群 | 已经存在的 ServingGroup 对象，和它们的 owner | T3 撞名字：不属于对应 ModelServing 的同名对象会让接管做不完（5.3） |
| 每个 ModelServing | 滚动模式、N、各组的序号（有没有洞）、有效的 partition | 第 6 节；第 5 步验收的基线 |
| 每个 ModelServing | 组一级的 surge 标记：每个组名下带值为 `serving-group` 的注解的 Pod 有几个、不带的有几个；按 T3 的规则这个组接管之后带不带注解。单独报出：只有一部分 Pod 带标记的组；序号小于 N 而带标记的 Pod；序号不小于 N 而一个 Pod 都不带的组；`RoleRollingUpdate` 下带标记的 Pod | T3；第 5 步验收的基线 |
| 每个 ModelServing | **不带标记的组多于 N。**结论写明：升级之后会缩掉一个组，按那一刻的就绪情况可能是哪一个。建议等滚动做完再升级 | 第 6 节「多出来的那份容量建在小于 N 的空洞里」一行 |
| 每个 ModelServing | 进行中的滚动（可替换的组数）、缩容、恢复（带删除标记的 Pod、正在终止的 Pod、`Failed` 的 Pod、在重启故障里的 Pod）、原地更新。对每个 `Failed` 的 Pod 给出结论：升级之后会重建哪个实例或哪个组 | 第 6 节的各行 |
| 每个 ModelServing | **版本的身份**：按目标版本的算法算出当前 `spec` 的版本，与存量 Pod 的 revision 标签比。结论是「升级后不滚动」或者「升级后会滚动 k 个组」。**这一项必须有** | P2。运维自己看不出来 |
| 每个 ModelServing | 用到的 plugin 和字段里目标版本没有的；用到的 plugin 里登记了删除钩子、而资源不挂在 ServingGroup 名下的；headless Service 的布局（是否带 worker） | P1、P3 |
| 每个 ModelServing | 会被目标版本新增或收紧的校验拦住的值 | P4 |
| 每个 ModelServing | 带它的标签而名字对不上任何期望成员的 Pod（5.1），包括 owner 还是 ModelServing 的；带它的标签而没有 controller owner 的 Pod | 它们不接管，不计入存量对象，由人处理（第 6 节末行） |
| 每个 ModelServing | `status` 的可用数 | 第 5 步验收的基线 |
| 每个 Pod | UID、各容器的重启计数、Ready 的状态和时间；PodGroup、Service 的 UID | 第 5 步验收的基线 |

组名的长度不用另查：升级前后组名的格式相同（02 的 8.4）。

## 8. 升级前后相同的东西

组的序号留着，所以下面几样在升级前后是同一个东西。上一版这一节是「回退窗口」，这一版没有那个机制。

| 项 | 两个源版本 | 升级之后 |
| --- | --- | --- |
| 组名 | `<ModelServing 名>-<序号>` | 相同。接管来的组用存量的名字；之后新建的组按 [02](02-design.md) 的 4.2 取序号 |
| Pod 名、hostname、headless Service 名、PodGroup 名、Pod 的 DNS 名 | 跟着组名 | 相同 |
| 替换、整组恢复之后的名字 | 不变 | 不变。稀疏布局下的例外见 02 的 4.2 |
| 顶层 `partition` 的含义 | 绝对序号 `[0, P)` | 相同（02 的 4.3） |
| 哪个组是临时的 surge 组 | 序号不小于 N，并且至少一个 Pod 带值为 `serving-group` 的注解 | 接管时按同一条规则判定一次，写在组对象上；Pod 上留着副本（02 的 2.3） |
| surge 组的转正和回收 | 序号小于 N 而带标记的组被摘掉标记，成为正式的组；临时的组在滚动做完之后回收（第 2 节） | 同样的两件事，规则在 02 的 4.2。临时的容量建在哪个序号上，稀疏集合下有一处不同（第 6 节） |

因此没有了的：

| 上一版（提交 `e35b2e37`）有的 | 这一版 |
| --- | --- |
| 回退窗口：controller 的一个集群级设置，升级时打开，由人关闭；窗口关着而有存量对象时不接管；第 7 节多一步「关闭窗口」 | 没有。旧版本任何时候都解析得了组名，回退在名字上没有期限 |
| 命名策略：建组时看窗口开没开、owner 是不是 LeaderWorkerSet，决定用序号还是随机后缀 | 没有。全部 ModelServing 的组名都是序号 |
| 升级那一刻 `partition` 换一种含义；接管来的稀疏集合上滚动停在与升级前不同的地方 | 没有。前后都是绝对序号 |
| 接管时把组一级的 surge 标记摘掉，多出来的组成为 N 个里的一个 | 标记搬到组对象上，照旧版本的方式做完 |
| 灰度进行中不能干净回退 | 可以（9.2） |
| 升级前检查按随机后缀算名字长度的余量 | 不用查 |

## 9. 回退

### 9.1 步骤

回退是新版本的一个持久的受控模式。旧版本做不了，它不认识 ServingGroup；手工做不现实。

| 步 | 做什么 | 为什么在这个位置 |
| --- | --- | --- |
| 1 | 人运行回退前检查（只读），看一遍 9.2 的条件。这一遍只供参考：检查之后 controller 还在正常工作，条件可以变 | — |
| 2 | 人创建**回退开关**：controller 所在命名空间里的一个 ConfigMap。它是集群级的、持久的，由人创建和删除。它在的时候，新版本的两个 controller 不做正常的 reconcile，也不做正向接管 | 否则换回 owner 的 Pod 会被当成待接管的成员再接回来。开关在对象上，不在进程参数里：新版本进程中途重启，读到开关，继续回退 |
| 2′ | 对每个 ModelServing：开关已经在，正常的 reconcile 已经停下，这时 controller 按 9.2 的条件再检查一遍。有终止中的组：等它们消失，这一步由垃圾回收器推进，不需要 kthena。其余条件不满足的不动它，写事件报告；人看到之后处理，或者删掉开关放弃。满足的，回退的第一个写：把接管注解改成 `RollingBack` | 之后在任何一步停下或放弃，这个 ModelServing 都不会被当成接管完成的，不会另建组 |
| 3 | 对每个 ServingGroup：直读这个组和它名下的 Pod，把名下的 Pod、PodGroup、Service 的 owner 换回 ModelServing，换之前直读 ModelServing。**同一次 PATCH 里把 Pod 上组一级 surge 注解的副本写成与组对象一致**：`ServingGroupRollingUpdate` 下组带注解的，写上值 `serving-group`；其余的，摘掉值为 `serving-group` 的 | 与正向对称，幂等。旧版本从 Pod 上读组一级的标记。副本平时由 ServingGroup controller 维护，但回退开关在的时候正常的 reconcile 停了，所以这一步自己把它写对，不依赖之前同步好没有 |
| 4 | 直读核对：这个 ServingGroup 名下没有任何对象了；带 surge 注解的组，换回去的每个 Pod 都带着值为 `serving-group` 的注解 | 第 5 步之后对象没有了，标记唯一的载体就是这些 Pod |
| 5 | 用 orphan 方式删除 ServingGroup | 即使第 4 步有遗漏也不删 Pod。必须在第 4 步之后：先删对象的话，中间停下标记就丢了 |
| 6 | 一个 ModelServing 名下的 ServingGroup 都删掉之后，摘掉新版本写的、旧版本会误读的标记：ModelServing 上的接管注解。Pod 上的代际号标签可以留着，旧版本不认识。Pod 上的 surge 注解留着，旧版本读的就是它 | 再次升级时从「没查过」开始 |
| 7 | 全部 ModelServing 做完之后，新版本停在只应答 webhook：开关还在，两个 controller 不工作。再装旧版本。Deployment 滚动：旧版本的 Pod 就绪之后，新版本的 Pod 才停 | 没有 webhook 空窗。独占，反方向：旧版本的 controller 起来时，新版本的 controller 已经不写了，对象已经是存量形态 |

第 2′ 到第 6 步中间停下：开关还在，新版本进程重启之后读到它，接着做。每一步幂等。

**标记在换回 owner 的那一次写里带回去。**组带 surge 注解时，名下每个 Pod 平时就带着同样的注解（[02](02-design.md) 的 2.3），多数时候这次写不改它。开关创建的那一刻副本正好没有同步（刚转正，刚接管完，刚切换过滚动模式）时，这次写把它改对。

**Pod 上这个 key 的值已经是 `role`。**同一个 key 只能有一个值。按 02 的 4.6 推，这不会成为「副本写不上去」：

| 滚动模式 | 组对象 | 这个 Pod |
| --- | --- | --- |
| `ServingGroupRollingUpdate` | 可以带 surge 注解 | Role 一级是不生效的一层。值为 `role` 的标记由 ServingGroup controller 摘掉，多出来的实例按 Role 的缩容处理。组带注解时，这个 Pod 随后带上值为 `serving-group` 的副本。结果是这个 key 上的值从 `role` 换成 `serving-group` |
| `RoleRollingUpdate` | 组一级是不生效的一层。G0 无条件摘掉组的注解 | 组不带注解，Pod 上不需要副本。值为 `role` 的留着：它是 Role 一级标记的来源，回退之后旧版本照常读 |

所以稳定之后，一个 Pod 的这个 key 上至多需要一个值。切换模式之后还没稳定的那一段，第 3 步按上表的终态写：`ServingGroupRollingUpdate` 下组带注解时写 `serving-group`，盖掉还没摘的 `role`；`RoleRollingUpdate` 下不写副本，`role` 留着。这是推演，没有验证。

放弃回退：删掉开关。两种 ModelServing 落回正向接管的 T1：注解是 `RollingBack` 的；已经没有注解而对象是存量形态的（做完了第 6 步）。T1 直读到存量对象，重新接管。ServingGroup 还在的组是 T4 的正常情形，换回 owner；ServingGroup 已经被删的组（做完了第 5 步），T3 按组名重建同名的对象，surge 注解按 5.2 的规则从 Pod 上的副本读回。这与正向接管是同一条路，没有新的状态。还没被碰到的 ModelServing 注解仍是 `Done`，照常 reconcile。

开关在的时候 `ModelServing.status` 不更新：正常的 reconcile 停了。控制面的空窗从创建开关算起，到旧版本的 controller 起来为止（4.1）。

第 7 步不能提前：对象还没有全部回到存量形态时让旧版本起来，就是第 1 节第 2 条的重叠。

### 9.2 干净回退的条件

| # | 条件 | 不满足时旧版本会 |
| --- | --- | --- |
| 1 | 没有终止中的组。有的话等它们消失，再做第 3 步 | 终止中的组的 Pod 正在被前台级联删除。换回 owner 之后，删到一半的组会被旧版本当成缺员去补 |
| 2 | 第 3 步做完之后，组一级 surge 注解的副本与组对象一致：带注解的组，名下每个 Pod 都带值为 `serving-group` 的注解；不带注解的组，名下没有 Pod 带。第 4 步核对，不一致不做第 5 步 | 带注解的组一个 Pod 都不带：旧版本把这个临时的组当成正式的组，正式的组多于 N，缩掉一个，被缩掉的不一定是它。不带注解的组（转正过的）还有 Pod 带：序号不小于 N 时旧版本把这个正式的组当成临时容量回收，序号小于 N 时旧版本自己摘掉。带注解的组只有一部分 Pod 带：旧版本按「至少一个」仍然认得出，但与它自己建出来的形态不一致 |
| 3 | 版本的身份在旧版本看来是连续的（P2 的反方向） | 全量滚动 |
| 4 | 没有不带 controller owner 的带标签的 Pod | 在网版本：删掉。v1.1.0：卡住 |

上一版的前四条在这一版的去向：

| 上一版的条件 | 这一版 |
| --- | --- |
| 没有随机名字的组 | 不存在这种组 |
| 组的个数等于 N，没有在途的创建和删除 | 收窄成第 1 条。个数不必等于 N：surge 的组带着标记回去，旧版本接着滚 |
| 没有带组一级 surge 标记的 Pod | 反过来了：第 2 条 |
| 没有生效的 `partition`，或者非目标的组的序号都小于 P | 不需要。`partition` 前后都是绝对序号，序号小于 P 的组是同一批 |

- **灰度进行中可以回退。**受保护的组在新旧版本眼里都是序号小于 P 的那几个。上一版里「回退之前先把 P 调到 N」的做法不再需要。
- **带 surge 的滚动途中可以回退**，前提是上表的各条，尤其是第 2 条。临时的组带着标记回去，旧版本按自己的规则把滚动做完。
- **稀疏的序号集合上、带 surge 的滚动途中回退，没有分析。**这一版把临时的容量建在不小于 N 的最低空闲序号上并带标记；两个源版本在同样的集合下把这份容量建在小于 N 的空洞里（第 2 节）。回退之后旧版本看到的是「一个带标记的临时的组，加范围内的空洞」，它接着怎样做没有读代码核对。回退前检查报出这样的 ModelServing（第 12 节）。
- **回退时丢掉的两种进度**，上一版也丢：Failed 写上而还没删的组，旧版本只看到一个带故障 Pod 的组；清空到一半的 Role 实例，旧版本只补。
- **账本和故障起点随 ServingGroup 对象一起没有了。**旧版本按它自己的方式重新计 grace。
- owner 是 LeaderWorkerSet 的 ModelServing 没有单独的条件。`group-index` 可以不连续：缩容按未就绪的先、deletion cost 低的先，会留下洞，今天也是这样。

### 9.3 升级、回退、再升级

回退的第 6 步摘掉了接管注解，第 5 步删掉了 ServingGroup 对象。组一级的 surge 标记留在 Pod 上的副本里（第 4 步核对过）。再次升级时每个 ModelServing 都从「没查过」开始，走一遍完整的接管：T3 按同一条规则从 Pod 上读回标记；Pod 上留着的代际号标签不妨碍接管（owner 是 ModelServing 的 Pod 不看这个标签），T4 保留它的原值；账本和故障起点从零值开始。没有残留的标记会被误读。

再次升级之前要删掉回退开关。它还在的话，新版本起来之后不做正向接管，也不做正常的 reconcile。升级前检查报告它（7.1）。

## 10. 做不到不断服的

| 情形 | 原因 | 怎么办 |
| --- | --- | --- |
| 目标版本缺源版本的 plugin、字段或框架的钩子（P1） | — | 目标版本补齐之前不升级 |
| 在网版本上：用了 headless Service plugin 并且 Role 带 worker，而目标版本不保留老布局（P3） | worker 的 DNS | 目标版本保留老布局；否则把这类负载排除在不断服的范围之外，另行迁移 |
| 版本的身份不连续（P2） | 序号不小于 P 的组都被当成可替换的 | 升级前在预发环境验证：没有改 `spec` 的 ModelServing 升级之后不替换任何 Pod |
| 升级那一刻不带 surge 标记的组多于 N（稀疏的序号集合上滚动中） | 接管之后正式的组多于 N，缩掉一个。多出来的那个组还没就绪时，被缩掉的是它：没有人改 `spec`，一个还在起的组被删掉重建。没有健康的组被多删（第 6 节） | 升级前检查报出这样的 ModelServing（7.1）。等滚动做完再升级；或者接受 |
| 升级那一刻正在整组恢复或删除，v1.1.0 | 没有删除标记，删到一半的不续删，只补 | 可以接受；或者等它做完再升级 |
| 升级那一刻已经有 `Failed` 的 Pod，旧版本在容忍它，策略是 `RoleRecreate` 或 `ServingGroupRecreate` | 接管之后终态的成员是故障，不等 grace：同一个实例或整个组里健康的 Pod 被重建（第 6 节） | 升级前检查报出每个这样的 Pod 和会被重建的范围。升级之前先处理掉它（删掉这个 Pod，让旧版本只补这一个）；或者接受。要不要让第一次接管对已有的终态 Pod 只删这一个再补，留给评审定 |
| 升级那一刻就没有 controller owner 的带标签的 Pod，名字对不上任何期望成员 | 不接管。它不占成员的名字，留在原处 | 升级前检查报出来，由人处理 |
| 从在网版本来、在新版本里做过原地更新的 Pod，回退到在网版本 | 它的重启计数大于 0，在网版本之后会把任何一次不就绪当成不健康重启 | 回退前检查 |
| 新版本建的 Pod，回退到在网版本 | 它们没有在网版本的实例身份注解。回退之后，有这种 Pod 的组里按范围的恢复不再触发，丢了成员只补：`recoveryPolicy` 对它们不起作用。按读代码的推演，没有验证 | 回退前检查报出这些组 |
| 新版本重建过的实例，回退到在网版本 | 存量实例保留老的 headless Service 布局，直到它自己被重建（P3）；新版本重建过的实例是新的布局。在网版本回来之后怎样对待新布局的 Service，没有分析 | 回退前检查报出这些实例；留给真实验证（第 12 节） |
| 旧的 controller Pod 停不下来（节点失联、finalizer） | 新进程一直等（X1）。数据面不受影响，控制面的空窗没有上界 | X1 的事件、日志和超时告警；由人处理那个 Pod。不要只把 Pod 对象强制删掉：容器还在跑时 X1 看不到它 |
| 用百分比或 `maxUnavailable` 的 PDB | 不是断服，但升级那一刻允许的驱逐数变了：期望数从组的个数变成这个组里 Pod 的个数（[02](02-design.md) 的 8.5），方向是变严。接管到一半时是两种口径之和 | 升级前检查列出这些 PDB（7.1），升级之后核对。没有验证（第 12 节第 13 项） |

这一次升级不受影响、之后新版本自己的每一次重启和升级才有的两种，列在第 11 节：停机期间丢了成员的已建成实例按范围恢复；停机超过 grace 时到期的重启故障在回来的第一轮恢复。

## 11. 危险操作

| 操作 | 后果 | 防护 |
| --- | --- | --- |
| `kubectl delete servinggroup`（默认后台级联） | 这个组的 Pod 被删。组对象立刻消失，同名的新对象随即建出来，它的成员位置等旧 Pod 被回收之后才建 Pod。不会有两批同名的 Pod（[02](02-design.md) 的 [5.5](02-design.md#55-有人直接删了一个组)） | 没有权限上的防护：chart 把 `servinggroups` 的 delete 聚合进内置的 edit、admin 角色，能删 Pod 的人本来就能触发恢复（02 的 8.10）。create、update、patch 不给。文档写明它相当于删一个副本 |
| `kubectl delete servinggroup --all` | 这个 namespace 里全部 ModelServing 的全部副本同时被删、同时重建，不受滚动预算限制 | 没有，只有文档 |
| `kubectl delete servinggroup --cascade=orphan` | Pod 留下，没有 owner，照常服务。controller 重建同名的组对象把它们收回来（5.4）。连续的序号集合下没有多出来的组。稀疏集合下有竞态：先在更低的序号上多建一个组，再缩掉一个，未就绪的先。被删的是临时的 surge 组时，身份凭 Pod 上的副本找回。受保护的序号上是第三个版本的组、又遇上同一个竞态时，这个组被换成基线版本 | 没有，只有文档：不要用 orphan 方式删组 |
| 给 ServingGroup 加 finalizer，或者别的原因让它卡在终止中 | 它的序号一直被占着，不能重建。它是正式的组时，N 调小之后还会挡住滚动。带 S 的滚动照常做完，做完之后 `maxUnavailable` 不小于 1 时一直少一个组，直到它消失（02 的 4.5）。报 `RolloutBlocked`，reason 是 `WaitingForDeletion`，message 写明序号 | 没有。与今天一个 Pod 卡在终止中相同 |
| 垃圾回收器停了或者很慢（kube-controller-manager 不工作） | 整组删除的 Pod 由垃圾回收器删。被删的组停在终止中，名下的 Pod 还在跑、还在服务；这个序号上的替换和恢复停住。今天 controller 自己删 Pod，不依赖它 | 没有。超过期限报 `WaitingForDeletion` |
| 命名空间里有同名而不属于这个 ModelServing 的 ServingGroup（用 orphan 方式删 ModelServing 之后又建同名的；手工建的） | 这个序号建不出来。controller 不删它，不收为己有。全部序号都被占着时这个 ModelServing 一个组也建不出来。报 `RolloutBlocked`，reason 是 `OrdinalOccupied` | 它的 owner 是一个已经不存在的 UID 时垃圾回收器会删掉它；没有 owner 的要人删 |
| `kubectl delete modelserving --cascade=orphan` | ServingGroup 对象和 Pod 都留下，组没有 owner。ServingGroup controller 不管没有 owner 的组：不补，不恢复，不删，发事件（[02](02-design.md) 的 2.4）。与今天 orphan 之后 Pod 没有人管相同。之后建同名的 ModelServing 时，这些组占着序号 | 用户文档写明；要清掉就删这些 ServingGroup |
| 删除 ServingGroup 的 CRD | 集群里全部 ServingGroup 被删，名下的 Pod 随之被删。量级与删 ModelServing 的 CRD 相同，但这个 CRD 看起来像内部对象，更容易被当成可以清理的 | CRD 留在 `crds/` 并带 `helm.sh/resource-policy: keep`；任何文档步骤里不出现删 CRD |
| `helm uninstall` | CRD 不删，ModelServing、ServingGroup、Pod 都留着，没有人管。重装之后接着管 | 与今天相同 |
| `helm rollback`、`kubectl rollout undo` 到旧版本 | 第 9 节：不做反向接管就是事故。旧版本的 Pod 先起来并且就绪，新版本的 Pod 才被停掉，中间两个版本同时在跑 | X1 的持续观察只停得下新版本一侧的写；旧版本一侧挡不住，它照样删 Pod。文档写明回退的入口是 9.1。X3 在这个方向不起作用：旧 chart 会把旧的 ServiceAccount 建回来 |
| 在新版本运行时启动一个旧版本的进程（另一个 namespace、另一个 release、本地调试） | 第 1 节第 2 条 | 没有，只有文档。X1 只看自己 namespace 里的 Pod；X3 只挡用这个 release 的 ServiceAccount 的旧进程 |
| 回退开关留着没有删（9.1） | 新版本的两个 controller 不做正常的 reconcile，也不做正向接管 | 升级前检查报告它（7.1） |
| 重启或升级新版本的 controller，同时排空节点或删 Pod | 停机期间丢了成员的已建成实例，回来之后按 `recoveryPolicy` 的范围恢复。`ServingGroupRecreate` 下的整组恢复不受滚动预算限制。从旧版本升上来的那一次不这样：账本从零值开始，只补（README 的 OP8） | 驱逐预算（配了 `evictionStrategy` 并且驱逐 webhook 可用时）。其余只有文档 |
| 新版本的 controller 停机超过 `restartGracePeriodSeconds`（重启、升级、换主卡住） | 停机之前已经在重启故障里、回来时还是同一段故障的成员，起点延续。回来的第一轮它们同时到期，按 `recoveryPolicy` 的范围同时恢复，没有上限；与上一行叠加。要不要给第一轮加一个上限，留给评审定。今天是回来之后再等一个 grace。从旧版本升上来的那一次没有起点，不受影响；之后的每次重启受影响 | 没有，只有文档。这一条改了已批准的决定，要批准（[04](04-behavior-expectations.md) 第 11 节的 N1）；不批准时起点留在内存里，这一行不成立 |
| 改 `ServingGroup.status` | 账本回到零值：重新建账之前的丢失只补；重启故障的起点丢失，重新计。Drained 被写成真：失败的组被提前删 | `servinggroups/status` 不授给用户 |
| 手工改 surge 注解 | 组对象上的注解由 controller 在创建时写，之后只会被它摘掉。手工摘掉：这个临时的组成为正式的组，不再被回收；正式的组多于 N 时按缩容删一个，删的不一定是它。手工加上：序号不小于 N 的正式的组会被当成临时容量，没有滚动要做时被回收；序号小于 N 的下一轮又被摘掉。Pod 上的副本：手工改了之后被 ServingGroup controller 按组对象写回去 | `servinggroups` 的 patch 不授给用户。「只会被摘掉、不会后加」不能由 API 强制，靠没有人有 patch 权限（02 的 8.10） |

开了 `OwnerReferencesPermissionEnforcement` 的集群上，换 owner 需要对 Pod、PodGroup、Service 的 delete 权限；写 `blockOwnerDeletion` 需要对 `servinggroups/finalizers` 的 update 权限。新 chart 的 ClusterRole 按新 controller 的需要重写，包括对 PodGroup 的 patch 和 T2′ 用到的 delete。

## 12. 留给真实验证的

每个源版本各做一遍。前四项决定这套办法成不成立。

1. **owner 换掉之后旧进程的反应。**在旧进程运行时手工把一个 Pod 的 controller owner 换成别的对象，看它删什么。确认第 1 节第 2 条。
2. **不断服。**稳态的负载升级：Pod 的 UID、重启计数、Ready 的时间线全程不变；存量 Pod 的 `hostname.subdomain` 全程解析，Service、Endpoints 不变；PodGroup 的 UID 不变；`ModelServing.status` 的可用数全程不低于升级前；每个存量的组有一个同名的 ServingGroup。
3. **版本的身份。**没有改 `spec` 的 ModelServing，升级之后目标版本等于存量组的版本，没有组被替换。
4. **直接 `helm upgrade`。**不装 CRD：升级失败或新 Pod 不就绪，旧版本照常。装了 CRD：新进程等旧 Pod 消失才开始接管。
5. 垃圾回收器：owner 被 PATCH 成一个存在的对象时不动 Pod；PATCH 成一个 UID 不存在的对象时删掉 Pod，同名而 UID 不同的 owner 按不存在处理；ServingGroup 用 orphan 删除时 Pod 留下。
6. PodGroup 的 owner 和 `blockOwnerDeletion` 变化时 Volcano 的反应；PodGroup 的 `minMember` 在接管前后不变。
7. 第 6 节的每一行：在那个状态下升级。尤其是滚动中带 surge、不带标记的组多于 N、带删除标记的恢复、原地更新进行中、灰度停点。
8. 接管的每一步之后杀掉新进程，再起来能续做。
9. 驱逐：接管到一半时，对已接管和未接管的 Pod 各发一次驱逐，预算都算对。
10. 回退：9.1 的步骤走一遍，旧版本起来之后不动 Pod。然后再升级一次。
11. 9.2 的每一条不满足时，旧版本实际做了什么。
12. 换凭据：Helm 升级之后旧 ServiceAccount 的令牌多久失效。
13. 用百分比的 PDB、HPA 的 selector 在接管前后的行为。接管到一半时 PDB 的期望数是不是两种口径之和。
14. 控制面空窗的长度：旧 Pod 的终止时间、取得租约、缓存同步、每个 ModelServing 的接管各占多少。T1、T3、T7 的直读随组数和 Pod 数怎样增长。
15. X1 的持续观察：运行中出现不带版本标记的 controller Pod（`helm rollback`、`kubectl rollout undo`）时，新版本多久停下两个 controller，这段时间旧版本删了什么。等旧 Pod 消失期间的事件、日志和超时告警。
16. X3 的范围：另一个 release 里的旧进程能不能并存，Helm 对同名集群级对象怎样处理。
17. T2′：没有活 Pod 的组（v1.1.0 全零的组留下的 PodGroup；Pod 全部在终止的组；T2 删光的组）接管能做完。PodGroup 被删掉、之后重建时 Volcano 的反应。T7 之后还在终止的存量 Pod：同名的新组等它消失。
18. `Done` 之后的无主成员 Pod（5.4）：`--cascade=orphan` 删组之后同名的对象被重建，Pod 被收回来，UID 不变；连续的序号集合下没有多出来的组；稀疏集合下的竞态里被缩掉的是哪一个；被删的是 surge 组时重建的对象带注解；`kubectl debug --copy-to` 的副本不被收养，有事件。
19. 回退的持久模式（9.1）：中途杀掉新版本进程，起来之后读到开关接着做；中途删掉开关，落回正向接管，没有多建组；最后一步两个版本的 Pod 并存时没有 webhook 空窗，旧版本不动 Pod。
20. 在网版本多出的三个钩子在目标版本里的调用点；登记了删除钩子的 plugin 在 `ServingGroupRollingUpdate` 下滚动之后有没有泄漏。
21. 回退到在网版本之后，新版本建的 Pod 所在的组丢了成员时旧版本的恢复范围（第 10 节）。
22. 期望成员按组自己的版本判断：旧版本的组里、已经从最新 `spec` 删掉的 Role 的 Pod 被接管；仍然对不上的 Pod 不接管，有事件，升级前检查报得出来。
23. 升级那一刻已经有 `Failed` 的 Pod：两个源版本各自容忍它的条件；接管之后第一轮重建的范围。
24. 原地更新到一半的组：标签还是旧版本的 Pod 照样换 owner，接管做得完，不因为版本不同被替换。
25. 收养来的无主成员版本与组的不同：写 `RecreateRequired`，在预算之内替换；`RoleRollingUpdate` 下不写；升级接管不走这一条（5.4）。
26. 回退的 2′：开关创建之后 controller 的再检查；不满足条件的 ModelServing 不被动到；有终止中的组时等它们消失。
27. **T3 对 surge 注解的判定。**下面每种形态各造一次，看建出来的对象带不带注解，并与旧版本当时的判定对照：全部 Pod 带标记；只有一部分带；只有终止中的 Pod 带；序号小于 N 而带；序号不小于 N 而一个都不带；`RoleRollingUpdate` 下带；Pod 上是值为 `role` 的注解。T3 在创建之后停下、再重做，对象上的注解不变。
28. **副本的同步。**接管之后带注解的组名下每个 Pod 都补上了注解；不带注解的组，Pod 上值为 `serving-group` 的被摘掉；值为 `role` 的在生效的模式下不动。转正之后 Pod 上的副本多久被摘掉。
29. **回退时的标记。**开关创建时副本没有同步的组，第 3 步把它写对；第 4、5 步之间停下，标记不丢；旧版本起来之后对每个组的判定（临时还是正式）与回退之前对象上的注解相同，带 surge 的滚动由旧版本接着做完。
30. 切换过滚动模式、Pod 上留着值为 `role` 的注解时回退（9.1）。
31. **同名的 PodGroup、Service 先删后建时 Volcano 的反应。**前台级联下旧的 PodGroup 和旧 Pod 都消失之后才建同名的新 PodGroup；用后台方式删组时，同名的新 PodGroup 出现而旧 Pod 还在终止，旧 Pod 会不会被算进新 PodGroup 的成员数。
32. **不带标记的组多于 N**（第 6 节）：多出来的组已经就绪、还没就绪两种情况下，接管之后被缩掉的各是哪一个；升级前检查报得出来。
33. 整个组的 Pod 都在终止时升级：连续的序号集合下补出来的同名的组等旧 Pod 消失；稀疏布局下落到更低的空洞上，新旧两批 Pod 并存多久。
34. 灰度进行中升级，再回退：序号小于 P 的组前后是同一批，新旧版本起来之后都不动 Pod。
35. 稀疏的序号集合上、带 surge 的滚动途中回退：旧版本怎样接着做（9.2，没有分析）。
36. 重启故障的计时：从旧版本升上来的那一次从第一次看到起算；之后重启新版本的 controller，起点延续；停机超过 grace 时回来的第一轮恢复的范围。
37. T7 与第一次 status 写的先后（5.2）：`Done` 写上的那一刻，接管来的组都已观测，可用数没有掉下去。
38. T3 撞上不属于这个 ModelServing 的同名 ServingGroup：不建、不删、有事件；那个对象被删掉之后接管做得完。
39. 升级那一刻就没有 controller owner 的带标签的 Pod：名字对得上的被接管，对不上的不接管、有事件；升级前检查两种都报得出来。
40. 回退到在网版本之后，新版本重建过的实例（新的 headless Service 布局）：旧版本怎样对待它们的 Service，DNS 记录有没有中断（第 10 节）。
41. 名下只有「没有 controller owner 的带标签的 Pod」的 ModelServing：T1 写的是 `InProgress`，这些 Pod 被接管（5.1）。
42. `InProgress` 期间把恢复策略改成 `None`：已经换完 owner 的组不再恢复。反方向（打开策略）在 `Done` 之后才生效（5.2）。
43. 升级空窗里丢 Pod：从旧版本升上来的这一次只补；之后新版本到新版本的升级、主备切换、删掉主的 Pod 或让主所在的节点失联，已建成的实例都按范围恢复。
