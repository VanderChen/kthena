# 在网版本的不断服升级

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 用途 | 已经在运行负载的集群，怎样从旧版本升级到本设计，而已运行的 Pod 不重启、不重建、Ready 不掉。也评估回退 |
| 覆盖的源版本 | 两个：**在网版本**（生产环境运行的分支）和 **v1.1.0**（上游发布版）。它们在对象上的形态、对 owner 变化的反应、选主的行为都不一样，下面分开写。源版本的代码位置和逐项盘点在任务目录的 `subagents/proposal-analysis-upgrade.md`，不在本文里 |
| 状态 | 设计提案。**没有在任何集群上做过升级演练。**关于旧版本行为的结论来自读代码；关于垃圾回收器、Deployment、Helm 的结论按文档和源码。要在集群上确认的事列在[第 12 节](#12-留给真实验证的) |
| 「不断服」的含义 | 升级期间：已运行的 Pod 的 UID 不变、容器不重启、Ready 不掉；没有人改 `spec` 的 ModelServing，升级之后没有 Pod 被替换。控制面（创建和更新 ModelServing、滚动、恢复）允许有一段不工作的时间 |

## 1. 结论

1. **接管只改元数据。**每个存量的组建一个同名的 ServingGroup 对象，再把 Pod、PodGroup、Service 的 controller owner 从 ModelServing 换成它。Pod 的名字、标签、hostname、DNS、PodGroup 的名字都不变。kubelet 不看 owner，容器不受影响。
2. **新旧进程不能有任何重叠，并且这一点要由新版本自己保证，不能靠操作步骤。**旧进程按 owner 认 Pod。owner 被换掉之后，在网版本会在下一轮把被接管的 Pod 当成占名字的外来 Pod **删掉**，并按恢复策略删同范围的其余 Pod；v1.1.0 把换 owner 当成一次 Pod 删除事件，**立刻删掉**同组或同实例里还没换 owner 的 Pod。重叠的后果不是报错，是删 Pod。
3. **「没有人改 `spec`，升级之后不替换任何 Pod」是一条单独的验收条件。**接管本身不删 Pod，不等于接管之后的第一轮正常 reconcile 不删。版本的身份、基线、partition 保护的集合在接管前后要对得上。
4. **回退是新版本的一个受控模式，不是把旧 chart 装回去。**不做反向接管就启动旧版本：在网版本把全部 Pod 逐个删掉重建，v1.1.0 卡住。
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
| partition 的语义 | 按序号升序的前 P 个现存的组 | 绝对序号 `[0, P)` |
| revision 的算法 | 与上游不同 | 与上游 `main` 相同 |
| headless Service 的布局 | 每个实例一个 | 每个实例两个，worker 的 subdomain 指向另一个 |
| 独有的 plugin 和 API 字段 | 有 | 有（原地更新等） |
| 选主 | 默认关。开了之后失主会退出 | 默认关。开了之后失主**不退出**，探针照样通过 |
| 部署 | 单副本，Deployment 滚动更新，webhook 都是 `failurePolicy: Fail`（驱逐 webhook 可配），CRD 在 chart 的 `crds/` | 相同 |

## 3. 升级的前提

对目标版本的要求。任何一条不满足，这个源版本就不能不断服地升上来。

| # | 前提 | 不满足时 |
| --- | --- | --- |
| P1 | **目标版本是源版本的超集**：API 字段、已注册的 plugin、ControllerRevision 的数据格式、校验规则 | 少一个 plugin：plugin 链构造失败，用到它的 ModelServing 建不了 Pod。少一个字段：下一次写的时候被裁掉。在网版本有上游没有的 plugin 和字段，目标版本要带上 |
| P2 | **版本的身份连续**：存量 Pod 上的 revision 标签，在目标版本看来与没有改过的 `spec` 是同一个版本。靠的是已有的机制：哈希不等时经 ControllerRevision 解出模板按语义比较，目标版本的解析先沿用已有的身份 | 接管之后第一轮，全部组都是过期的，按滚动预算逐个替换。在网版本的 revision 算法与上游不同，这一条对它是实打实的风险 |
| P3 | **存量实例保留老的 headless Service 布局**，直到它自己被重建 | 在网版本上用了 headless Service plugin、Role 带 worker 的负载：老 worker 的 DNS 记录消失，或者 plugin 报告要迁移而停止同步 |
| P4 | 新增的或收紧的校验只拦新引入的值 | 存量对象的任何更新被拒，包括 autoscaler 的 PATCH |
| P5 | 驱逐 webhook 认两种归属（owner 是 ModelServing，或者是它名下的 ServingGroup） | 接管到一半时，已接管的 Pod 的驱逐保护无声失效 |

## 4. 独占

接管开始之前，旧进程必须已经退出；之后也不能再有旧进程起来。四道防线，前三道是新版本自带的：

| # | 防线 | 挡住什么 | 挡不住什么 |
| --- | --- | --- | --- |
| X1 | **启动自查。**新 chart 给 controller 的 Pod 模板加一个版本标记。新进程在自己的 namespace 里还能看到「带 controller-manager 的 selector、不带这个标记」的 Pod 时，不启动任何 controller，只应答 webhook；看不到了才启动 | 直接 `helm upgrade`：Deployment 滚动更新，新旧 Pod 并存。新进程等旧 Pod 消失。webhook 没有空窗 | 不在这个 namespace 的旧进程；Pod 对象被强制删除而容器还在跑 |
| X2 | **CRD 和 RBAC 的闸。**ServingGroup 的 CRD 或者对它的权限不在时，新进程不就绪（就绪探针和存活探针分开），不启动 controller，也不退回旧的工作方式。chart 用 `lookup` 在升级开始之前就失败 | Helm 升级不处理 `crds/`。没有这道闸：新 Pod 就绪，旧 Pod 被停掉，控制循环从此没有人做，而且没有任何信号 | — |
| X3 | **换凭据。**新 chart 给 controller 换一个 ServiceAccount 和 ClusterRoleBinding 的名字。Helm 升级时删掉清单里不再有的旧对象，旧进程的凭据随之失效 | 卡住之后又恢复的旧进程、另一个 release 里的旧进程、有人在本地跑的旧版本：它们的写是 401 或 403 | Helm 先建新的、最后删旧的，中间有很短的次序不定。所以它不单独用。回退方向无效 |
| X4 | **选主，并且失主退出。**新版本默认打开 | 之后的每一次升级和重启 | 这一次升级：旧版本默认没开选主，租约排除不了它 |

- X1 是必须有的：它让「直接 `helm upgrade`」从事故变成安全的操作。
- X3 对在网版本尤其要做：重叠的后果在它上面最重。
- 控制面的空窗：从旧 Pod 开始终止到新进程过了接管屏障。这期间 ModelServing 的创建和更新照常被 webhook 接受，只是没有人执行；崩溃的 Pod 不被恢复；驱逐 webhook 照常工作。
- 源版本如果已经开了选主（在网版本改一个 values 就能开，并且失主会退出），X4 对这一次升级也生效。v1.1.0 失主之后不退出，不能靠它。

**评估过、没有采用的**：接管时保留两条 ownerReference（ServingGroup 是 controller，ModelServing 是非 controller）。它能让旧进程和旧 webhook 仍然认这些 Pod、让误删组和 CRD 不删 Pod、让回退不依赖反向接管先做完。没有采用，因为对象上还有另一个存在的 owner 时前台级联不删它，组的删除（02 的 5.4）在这段时间里失效；并且独占仍然是必须的（在网版本会继续按自己的规则管这些 Pod）。

## 5. 接管

接管按 ModelServing 进行。它幂等，任何一步停下都可以续做。

### 5.1 多出来的两个状态

| 层 | 状态 | 判定 | 动作 |
| --- | --- | --- | --- |
| Pod 成员 | 待接管 | 标签是这个组的；controller owner 还是 ModelServing，或者没有 controller owner | 是成员。**不为它的名字创建，也不删除它**；换 owner |
| ServingGroup | 接管中 | 带 `legacy-ordinal` 注解，Adopted 不为真 | ServingGroup controller 只换 owner、起账本。ModelServing controller 不把它当缩容和替换的候选 |

ModelServing 上有一个注解记接管的进度：没有（不知道）、`InProgress`、`Done`。用注解而不是 `status`：旧版本的 CRD 会裁掉它不认识的 status 字段。**没有这个注解不等于不需要接管**：它的含义是「还没查过」。

### 5.2 步骤

| 步 | 谁 | 做什么 | 写 | 这一步之后停下 |
| --- | --- | --- | --- | --- |
| T1 | ModelServing controller | 这个 ModelServing 没有注解，或者是 `InProgress`：**对 apiserver 直读**名下的存量对象（带它的名字标签、owner 是它或者没有 owner 的 Pod、PodGroup、Service）。一个都没有：写 `Done`。有：写 `InProgress`。`status` 已经被旧版本写过、或者有存量对象的，另外记下它是存量的（命名策略用序号，第 8 节） | PATCH ModelServing 的注解 | 重做：直读的结果相同 |
| T2 | ModelServing controller | 带删除标记而还活着的存量 Pod（只有在网版本有）：按 UID 删除。它们不接管 | DELETE Pod，带 UID | 重做：还在的再删 |
| T3 | ModelServing controller | 每个还有「活着的、不带删除标记的」存量 Pod 的组名：创建同名的 ServingGroup，带 `legacy-ordinal`。Pod 全部在终止的组不接管，等它们消失；这期间它在 `active` 里占一个位置 | 创建 ServingGroup | 重做：撞名字，核对 owner 是这个 ModelServing |
| T4 | ServingGroup controller | 每个待接管的成员：先**直读这个 ServingGroup**，确认 UID、确认没有 `deletionTimestamp`；然后把 controller owner **换成**它（不是追加），补上 `blockOwnerDeletion`，摘掉组一级的 surge 标记。PodGroup、Service 同样处理 | PATCH，带对象的 UID 和 resourceVersion | 一个组换到一半：剩下的仍然是待接管的成员，不会被当成缺失 |
| T5 | ServingGroup controller | 账本从零值开始。一个实例的成员（按它已经应用的模板布局）全部在缓存里、直读确认过：写 Established。不齐的留在 Creating | UPDATE status | 重做 |
| T6 | ServingGroup controller | 直读：这个组名下没有待接管的成员了。写 Adopted | UPDATE status | 重做 |
| T7 | ModelServing controller | 直读：没有存量对象了，接管来的组都是 Adopted。写 `Done` | PATCH 注解 | 重做 |

`InProgress` 期间，ModelServing controller 对这个 ModelServing 不做 G1 到 G6 的任何写（[03](03-state-machine.md) 的 M3）。`status` 照常写。

T3 里 ServingGroup 的 `spec` 怎样填：

| 情形 | 取值 |
| --- | --- |
| `ServingGroupRollingUpdate` | `revision` 取这个组的 Pod 的 revision 标签。组正在原地更新（只有 v1.1.0 有）：取进度注解里的目标版本 |
| `RoleRollingUpdate` | `targetRevision`、`baselineRevision` 取 ModelServing 的 `status`；各实例的版本从成员的标签读 |
| `roles`、`recoveryPolicy` 等 | ModelServing 当前的 `spec` |
| 代际号 | 存量 Pod 没有这个标签，按 0 |

### 5.3 为什么每一步都是安全的

| 担心 | 为什么不会 |
| --- | --- |
| Pod 缓存还空着，闸门误放行，按数量新建 N 个组 | T1、T7 是直读 |
| 一个组换 owner 换到一半，其余成员被当成缺失去建同名 Pod | 待接管的成员是成员 |
| 同名的 ServingGroup 被删了重建，缓存里是旧的 UID，写进 Pod 的 owner 是一个不存在的对象，垃圾回收器删掉这个 Pod | T4 之前直读。存量 Pod 还在时不删除、不重建同名的 ServingGroup |
| 接管来的实例被当成「建成过」，第一次观察到的不齐被当成丢失 | 账本从零值开始，零值只补 |
| 带删除标记的 Pod 所在的组或实例被整个删掉（标记是逐个打的，可能只打了一半） | 只删带标记的 UID，范围正好是原来选定的 |

模型里的对应：接管完成之前不创建、不删除任何组；去掉闸门之后这条性质被违反（[05](05-inference-verification.md) 的 G8、M7）。成员一级的步骤是推演。

## 6. 升级那一刻的状态

旧进程停下时负载可以处在任何状态。每一种怎样接过来：

| 升级时的状态 | 对象上是什么 | 接过来之后 | 相对旧进程继续做，差在哪 |
| --- | --- | --- | --- |
| 稳态 | N 个组，全部就绪 | N 个接管来的组，全部 Established。没有动作 | — |
| 滚动中，有一个 surge 组 | N + 1 个组，一个带 surge 标记 | 都接管，surge 标记摘掉。之后按 02 的 G1–G6：还有过期的组，上限是 N + S，继续替换；最后一个旧组删掉时正好是 N | 少建一个、少删一个（04 的 X4） |
| partition 灰度停点 | 序号小于 P 的是旧版本 | 接管来的组按存量序号排次序，受保护的是最靠前的 P 个非目标组：就是原来那几个。没有动作 | — |
| 保护的序号里有一个不是基线的旧版本（P 调大之后又提交过新版本） | — | 它是非目标的组，次序靠前，受保护。没有动作 | — |
| 稀疏序号 | 例如 {0, 3} | 两个组照常接管。组一级没有「洞」这个概念 | 之后扩容不再「补最低的洞」，回退窗口内新建的组仍然取最小的空闲序号 |
| 整组恢复或删除进行中，在网版本 | 一部分 Pod 带 `ServingGroup` 的删除标记 | T2 按 UID 删掉带标记的。没带标记的照常接管；实例不齐，留在 Creating，补 | 标记只打了一半时，没带标记的成员留下，与契约 2.11 相同 |
| 同上，v1.1.0 | 没有标记；组里只剩一部分 Pod，PodGroup 可能已经没了 | 接管，只补。第一轮确保 PodGroup、Service 存在 | 与 v1.1.0 自己重启的结果相同：不续删 |
| 实例恢复或删除进行中 | 同上，范围是一个实例 | 同上 | 同上 |
| Pod 正在终止 | — | 不接管，等它消失。所在的位置不创建 | — |
| 缩容进行中 | 多于 N 个组，有的在终止 | 活着的接管；闸门放行之后 G2 按次序再选 | v1.1.0 没有标记，再选的不一定是原来那个 |
| 原地更新进行中（v1.1.0） | 一部分 Pod 的标签是旧版本，带进度注解 | 组的版本取注解里的目标；成员分类先看进度注解。续做 | — |
| 全零的组（N 大于 0，各 Role 的副本数都是 0） | 没有 Pod；v1.1.0 可能留着 PodGroup | 没有可接管的 Pod。T1 仍然把这个 ModelServing 记成存量的：它没有接管注解，而 `status` 已经被旧版本写过（`status.observedGeneration` 不是 0），或者留有 PodGroup；闸门放行之后按 N 建空的组；存量的 PodGroup 按名字接过来 | — |
| roleCoordination 滚动进行中 | 基线在 ControllerRevision 的注解上 | 由 ModelServing controller 读出来，下发到组的 `spec` | — |
| owner 是 LeaderWorkerSet 的 ModelServing | — | 序号命名，一直如此 | — |
| 没有 controller owner 的带标签的 Pod（有人用 orphan 删过） | — | 待接管的成员 | — |

接管本身在每一行里都不创建、不删除健康的 Pod。T2 删的是旧进程已经决定要删、并且标在 Pod 上的那些。

## 7. 操作步骤

| 步 | 做什么 | 为什么 |
| --- | --- | --- |
| 1 | **升级前检查**（只读）。查出：进行中的滚动和恢复、带删除标记的 Pod、进行中的原地更新、第 10 节的负载、长名字的对象、驱逐 webhook 是否开着和它的 `failurePolicy`、源版本是否开了选主 | 有些状态升级那一刻有特殊后果；有些负载做不到不断服 |
| 2 | 装 CRD：ServingGroup 的 CRD，以及更新过的 ModelServing 的 CRD。用 `kubectl apply --server-side`（ModelServing 的 CRD 很大，客户端 apply 会因为注解太长失败） | Helm 升级不处理 `crds/`。X2 在这一步没做时会拦住 |
| 3 | `helm upgrade` 到新版本 | 新的 Deployment、RBAC、ServiceAccount、webhook 配置 |
| 4 | 等。新 Pod 起来只应答 webhook（X1）；旧 Pod 被停掉；新进程看不到旧 Pod 之后，过接管屏障，开始接管 | — |
| 5 | 验收：每个 ModelServing 的接管注解是 `Done`；Pod 的 UID 和升级前相同、重启计数没有变；没有人改过 `spec` 的 ModelServing 没有组在终止 | 第 1 节的第 3 条 |
| 6 | 观察一段时间之后，关闭回退窗口（第 8 节） | 之后新建的组才用随机名字 |

第 2 步和第 3 步的次序反了、或者第 2 步漏了：X2 让升级在开始前失败，或者新 Pod 不就绪、旧 Pod 不被停掉，旧版本照常工作。

不要做的事：任何步骤里都不删 CRD。chart 里 CRD 的位置不要变（位置一变，Helm 升级会删掉它，连带全部实例）。

## 8. 回退窗口

接管完成之后、运维确认不再需要回退到旧版本之前，是回退窗口。它由人显式关闭（ModelServing 上的注解，或者 controller 的一个开关），不按时间自动关。

| | 窗口之内 | 窗口关闭之后 |
| --- | --- | --- |
| 新建的组的名字 | `<名>-<最小的空闲序号>` | `<名>-<随机后缀>` |
| 次序键 | 序号 | 接管来的在前、按存量序号；新建的按创建时间 |
| 能不能不重建 Pod 回退 | 满足 9.2 的条件时能 | 不能 |

窗口内名字仍然是序号，是为了旧版本能解析它。算法在窗口内外相同，都按数量。

## 9. 回退

### 9.1 步骤

回退由新版本的一个受控模式（或者随它发布的工具）执行。旧版本做不了，它不认识 ServingGroup；手工做不现实。

| 步 | 做什么 | 为什么在这个位置 |
| --- | --- | --- |
| 1 | 回退前检查 9.2 的条件，不满足就拒绝 | — |
| 2 | 新版本的两个 controller 停止正常的 reconcile | 否则换回 owner 的 Pod 会被当成待接管的成员再接回来 |
| 3 | 对每个 ServingGroup：把名下的 Pod、PodGroup、Service 的 owner 换回 ModelServing。先直读 ModelServing | 与正向对称，幂等 |
| 4 | 直读核对：这个 ServingGroup 名下没有任何对象了 | 第 5 步之前必须确认 |
| 5 | 用 orphan 方式删除 ServingGroup | 即使第 4 步有遗漏也不删 Pod |
| 6 | 摘掉新版本写的、旧版本会误读的标记（ModelServing 上的接管注解；Pod 上的代际号标签可以留着，旧版本不认识） | 再次升级时从「没查过」开始 |
| 7 | 停掉新版本，确认已经退出，再装旧版本 | 独占，反方向 |

第 3 到 5 步中间停下，再次运行可以续。回退做了一半又决定不回退：一部分 Pod 已经归 ModelServing、对应的 ServingGroup 还在，这正是正向接管的 T4 的正常情形。

### 9.2 干净回退的条件

| # | 条件 | 不满足时旧版本会 |
| --- | --- | --- |
| 1 | 在回退窗口之内（没有随机名字的组） | 在网版本：按解析不出来的序号乱放 Pod。v1.1.0：整个忽略这些组，另建 N 个，容量翻倍 |
| 2 | 组的个数等于 N，没有在途的创建和删除 | 扩容或缩容 |
| 3 | 没有带组一级 surge 标记的 Pod（T4 已经摘掉） | 把这个健康的组当临时容量回收 |
| 4 | 没有生效的 partition；或者非目标的组正好是序号最小的那几个 | v1.1.0 按绝对序号：滚掉序号不小于 P 的旧版本组 |
| 5 | 版本的身份在旧版本看来是连续的（P2 的反方向） | 全量滚动 |
| 6 | 没有不带 controller owner 的带标签的 Pod | 在网版本：删掉。v1.1.0：卡住 |
| 7 | owner 是 LWS 的：序号集合是 `[0, N)` | `group-index` 越界 |

灰度进行中（第 4 条不满足）不能干净地回退到 v1.1.0。

### 9.3 升级、回退、再升级

回退的第 6 步摘掉了接管注解，第 5 步删掉了 ServingGroup 对象。再次升级时每个 ModelServing 都从「没查过」开始，走一遍完整的接管。没有残留的标记会被误读。

## 10. 做不到不断服的

| 情形 | 原因 | 怎么办 |
| --- | --- | --- |
| 目标版本缺源版本的 plugin 或字段（P1） | — | 目标版本补齐之前不升级 |
| 在网版本上：用了 headless Service plugin 并且 Role 带 worker，而目标版本不保留老布局（P3） | worker 的 DNS | 目标版本保留老布局；否则把这类负载排除在不断服的范围之外，另行迁移 |
| 版本的身份不连续（P2） | 全部组被当成过期的 | 升级前在预发环境验证：没有改 `spec` 的 ModelServing 升级之后不替换任何 Pod |
| 升级那一刻正在整组恢复或删除，v1.1.0 | 没有删除标记，删到一半的不续删，只补 | 可以接受；或者等它做完再升级 |
| 回退窗口关闭之后回退 | 随机名字 | 滚动重建 |
| 从在网版本来、在新版本里做过原地更新的 Pod，回退到在网版本 | 它的重启计数大于 0，在网版本之后会把任何一次不就绪当成不健康重启 | 回退前检查 |

## 11. 危险操作

| 操作 | 后果 | 防护 |
| --- | --- | --- |
| `kubectl delete servinggroup`（默认后台级联） | 这个组的 Pod 被删。ModelServing controller 随即补一个新的组 | chart 不把 `servinggroups` 的写权限聚合进内置的 admin、edit 角色。文档写明它相当于删一个副本 |
| 删除 ServingGroup 的 CRD | 集群里全部 ServingGroup 被删，名下的 Pod 随之被删。量级与删 ModelServing 的 CRD 相同，但这个 CRD 看起来像内部对象，更容易被当成可以清理的 | CRD 留在 `crds/` 并带 `helm.sh/resource-policy: keep`；任何文档步骤里不出现删 CRD |
| `helm uninstall` | CRD 不删，ModelServing、ServingGroup、Pod 都留着，没有人管。重装之后接着管 | 与今天相同 |
| `helm rollback` 到旧 chart | 第 9 节：不做反向接管就是事故 | 文档写明回退的入口是 9.1，不是 `helm rollback`。X3 在这个方向不起作用：旧 chart 会把旧的 ServiceAccount 建回来 |
| 在新版本运行时启动一个旧版本的进程 | 第 1 节第 2 条 | X3 |
| 改 `ServingGroup.status` | 账本回到零值：之后的丢失只补。Drained 被写成真：失败的组被提前删 | `servinggroups/status` 不授给用户 |

开了 `OwnerReferencesPermissionEnforcement` 的集群上，换 owner 需要对 Pod、PodGroup、Service 的 delete 权限；写 `blockOwnerDeletion` 需要对 `servinggroups/finalizers` 的 update 权限。新 chart 的 ClusterRole 按新 controller 的需要重写，包括对 PodGroup 的 patch。

## 12. 留给真实验证的

每个源版本各做一遍。前四项决定这套办法成不成立。

1. **owner 换掉之后旧进程的反应。**在旧进程运行时手工把一个 Pod 的 controller owner 换成别的对象，看它删什么。确认第 1 节第 2 条。
2. **不断服。**稳态的负载升级：Pod 的 UID、重启计数、Ready 的时间线全程不变。
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
13. 用百分比的 PDB、HPA 的 selector 在接管前后的行为。
