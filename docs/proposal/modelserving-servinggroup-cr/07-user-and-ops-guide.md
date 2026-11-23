# 使用与运维说明

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 读者 | 使用 ModelServing 的人，和升级、运维 kthena 的人。不需要读过别的几份 |
| 状态 | **这是设计提案的一部分，功能还没有实现。**下面的命令、列名、注解名都是示意，以实现为准。它的用处是让评审的人看到设计落到用户手上是什么样子。没有在集群上验证过 |
| 相对上一版 | 上一版（提交 `e35b2e37`）里组名是随机后缀，组一级按数量滚动。这一版保留组的序号：组名、Pod 名、组一级的 `partition`、带 `maxSurge` 的滚动、缩容的次序都回到今天的样子（4.1） |
| 「今天」 | 指上游当前的实现（`main` @ `79c455ef`）和行为契约 2.12。在网版本不同的地方单独注明 |
| 细节 | 规则在 [02](02-design.md)，状态在 [03](03-state-machine.md)，行为的逐项对照在 [04](04-behavior-expectations.md)，证据在 [05](05-inference-verification.md)，升级的完整设计在 [06](06-live-upgrade.md) |

## 1. 一句话

ModelServing 的每个副本（ServingGroup）变成了一个看得见的对象。你写 ModelServing 的方式不变。组和 Pod 的名字不变。组一级的滚动、`partition`、缩容的规则不变。变的是几处恢复的细节（第 4 节），和一些事实记在哪里。

## 2. 你会看到什么

多了一种对象：

```text
$ kubectl get servinggroup -l modelserving.volcano.sh/name=llama
NAME      PHASE      REVISION       READY   SURGE           AGE
llama-0   Ready      llama-5c4d8    True                    3d
llama-1   Ready      llama-5c4d8    True                    3d
llama-2   Creating   llama-6b7f9    False                   40s
llama-3   Ready      llama-6b7f9    True    serving-group   6m
```

这是 N = 3、`maxSurge` 是 1 的一次滚动的中途：`llama-3` 是临时多出来的组，`llama-2` 刚被替换、正在按新版本重建。

| 列 | 含义 |
| --- | --- |
| NAME | `<ModelServing 名>-<序号>`，和今天的组名相同（第 3 节） |
| PHASE | Creating：还有成员没建好。Ready。NotReady：建成过，现在有成员不就绪。Failed：等着被整组替换。Terminating |
| REVISION | 这个组的版本 |
| SURGE | 有值的是滚动期间临时多出来的 surge 组。滚动做完它会被回收；副本数调大、它的序号落进正式范围时它留下来，这一列变空。取自组对象上的注解 `modelserving.volcano.sh/surge`。`maxSurge` 是 0 时也可能看到：一个正式的位置建不出来（序号被别的对象占着，或者那个位置要按基线版本建而基线读不到）而又在滚动、还有换得动的旧组时，controller 在范围外临时建一个带标记的新版本的组顶着，总数不超过 N，滚动做完回收 |
| AGE | 这个对象存在了多久。同一个名字的组被替换或整组恢复之后是一个新对象，`AGE` 从头算 |

被 `partition` 留在旧版本的是序号小于 P 的组，和今天一样：看名字里的序号和 REVISION 一列。灰度停下时 ModelServing 的 `Available` 的 message 也列出它们。

`kubectl describe servinggroup` 里有每个 Role 实例的阶段，和这个组不就绪或者失败的原因。

Pod 上（`kubectl get pod -o yaml`）：

| 项 | 今天 | 这个设计 |
| --- | --- | --- |
| 名字、hostname、已有的标签 | — | 不变 |
| `ownerReferences` | ModelServing | 所在的 ServingGroup |
| 标签 `modelserving.volcano.sh/instance-generation` | 没有 | 有：所在的 Role 实例是第几代 |
| 注解 `modelserving.volcano.sh/surge: serving-group` | 临时 surge 组的 Pod 上有 | **还在，是副本。**以 ServingGroup 上的注解为准（4.6） |
| 注解 `modelserving.volcano.sh/surge: role` | Role 一级临时实例的 Pod 上有 | 不变 |
| 在网版本的删除标记（`deletion-scope` 注解） | 在网版本有 | 没有。「正在整组恢复」「正在清空一个实例」写在 ServingGroup 的 `status` 里 |

ModelServing 自己的 `status`：

- 字段没有增减。`replicas`、`updatedReplicas`、`currentReplicas`、`availableReplicas` 的口径不变。各 Role 的副本数都是 0 的组算就绪，正在终止的组不计入 `updatedReplicas`，都与今天相同。
- `UpdateInProgress`、`Progressing`、`Available` 三个 condition 的含义不变，同一时刻仍然至多一个为真。`Available` 仍然是「全部组就绪，没有在进行的变更」。
- 新增一个 condition `RolloutBlocked`：有事要做而做不了，并且同一个原因持续超过设定的期限时为真。reason 是 `BaselineUnknown`、`WaitingForDeletion`、`GroupNotObserved`、`TargetNotReady`、`BudgetExhausted`、`OrdinalOccupied` 之一（第 6 节）。它可以和 `UpdateInProgress` 或 `Progressing` 同时为真。正常的缩容、滚动里短暂的等待不会让它变成真。`BaselineUnknown` 和 `OrdinalOccupied` 不等期限，立刻报。
- 别的新事实写在已有 condition 的 message 里：正在接管、有组在整组恢复、灰度停在 `partition` 上。范围内的空洞和范围外的正式的组照旧在 message 里报告。

判定式在 [03](03-state-machine.md) 第 6 节。

**ServingGroup 是 controller 管的对象，像 Deployment 名下的 ReplicaSet。**不要手工创建它；不要改它的 `spec`（会被写回去）；不要改它的 `status`；删除它的后果见第 5 节。

## 3. 名字

名字没有变。

| | 今天 | 这个设计 |
| --- | --- | --- |
| 组 | `llama-0`、`llama-1` | 相同 |
| Pod | `llama-0-prefill-0-0` | 相同 |
| 标签 `modelserving.volcano.sh/group-name` | `llama-0` | 相同 |
| headless Service、PodGroup 的名字 | 跟着组名 | 相同 |
| Pod 的 hostname 和 DNS 名 | `llama-0-prefill-0-0.<Service 名>` | 相同 |
| plugin 按组名生成的对象 | 名字里带 `llama-0` | 相同 |
| 滚动替换一个组之后 | 名字不变 | 名字不变。ServingGroup 是一个新的对象：UID 变了，`AGE` 从头算 |
| 整组恢复之后 | 名字不变 | 同上 |
| 滚动途中临时多出来的组 | 序号集合连续时是序号 N，例如 N = 3 时的 `llama-3` | 相同 |

写死 `llama-0-...` 的脚本和告警规则、写死某个 Pod 的 DNS 名的客户端、从组名里解析序号的程序都不受影响。按 Pod 名聚合的日志流和监控序列在替换之后接着用，和今天一样。

**名字是位置，UID 是这一代。**同一个名字先后可以是两个 ServingGroup 对象。这带来几件要知道的事：

| 事 | 说明 |
| --- | --- |
| 删掉一个组，它会自己回来 | `kubectl delete servinggroup llama-1` 之后，同名的组被重新建出来，`AGE` 从头算。和删 StatefulSet 的 Pod 一样 |
| `kubectl get servinggroup -w` | 替换时先一条 DELETED，再一条 ADDED，名字相同 |
| 事件 | `kubectl describe servinggroup llama-1` 只显示当前这个对象的事件。上一代对象的事件在保留期之内按名字查：`kubectl get events --field-selector involvedObject.name=llama-1` |
| 按 UID 聚合的监控 | 每次替换断一次。按名字聚合的不断 |
| `kubectl wait` | 见 4.8：同名重建时的行为没有核实 |

**稀疏布局下的例外。**序号集合可以有洞：按 `pod-deletion-cost` 缩容会留下洞，今天也是这样。有洞的时候，一个组被替换或整组恢复之后，新的组建在小于 N 的最低空闲序号上，不一定是原来的序号。例如组是 `llama-1`、`llama-3`，N = 2：替换 `llama-1` 之后是 `llama-0`、`llama-3`。洞的个数不增加，名字变了。序号集合连续时（没有按 deletion cost 缩过容的 ModelServing 都是）总是原来的名字。规则在 [02](02-design.md) 的 [4.2](02-design.md#42-一轮的计划)。

## 4. 行为上的变化

ModelServing 的字段没有增减。组一级的行为和今天相同（4.1）。有变化的几处：

| 变化 | 今天 | 这个设计 | 详见 |
| --- | --- | --- | --- |
| controller 不在的时候丢了 Pod | 回来之后只补这一个 | 已经建成的 Role 实例按 `recoveryPolicy` 的范围恢复 | 4.2 |
| 实例建成之前丢了成员 | 上游按范围恢复；在网版本只补 | 只补 | 4.2 |
| 恢复开始之后改 `recoveryPolicy` | 契约：还没开始删的要取消。已批准的决定：已经开始删的做完 | 组变成 Failed、实例开始清空之后不撤销。还在等 grace 的照旧可以取消 | 4.4 |
| 整个组的 Pod 都没了 | 按序号和当时的 `partition` 重建这个组的 Pod | 结果相同。多一步：组对象先被换掉，UID 和 `AGE` 变 | 4.5 |
| 组一级 surge 标记记在哪 | 组的每个 Pod 的注解 | ServingGroup 的注解；Pod 上留副本 | 4.6 |
| 用百分比或 `maxUnavailable` 的 PDB | 期望数按组的个数 | 按 Pod 的个数（选中的各组之和），变严 | 4.7 |
| 整组删除时的 `OnRoleDelete` | 对每个实例调 | 只尽力调一次，不保证 | 第 8 节 |
| 事件记在哪 | 全部在 ModelServing 上 | Role 实例和 Pod 一级的在 ServingGroup 上 | 第 6 节 |
| Pod 的 owner | ModelServing | ServingGroup | 第 7 节 |

前三行改的是行为契约里写明的话，要评审批准，条款在 [04](04-behavior-expectations.md) 第 11 节；第一行的口径已经定了（4.2）。重启故障的 grace 遇上 controller 重启时怎样计，没有变（4.3）。另有两条要批准的不直接改变你看到的行为，改的是事实记在哪：每个 Role 实例在 ServingGroup 的 `status` 里有一条记录（建成过没有，在不在清空）；删到一半的恢复靠这条记录接着做，不靠 Pod 上的标记。

### 4.1 没有变的

| 项 | 今天 | 这个设计 |
| --- | --- | --- |
| 组名、Pod 名、DNS 名 | 第 3 节 | 相同 |
| 组一级的 `partition`（`ServingGroupRollingUpdate`） | 序号小于 P 的组留在旧版本 | 相同。可以指定哪几个组留在旧版本 |
| 灰度停点上扩容 | 新组的版本按它的序号：序号小于 P 的用基线版本，其余用新版本 | 相同 |
| `partition` 调大，已经是新版本的组落进保护的范围 | 保留，不换回旧版本 | 相同 |
| 把模板改回基线版本（回滚） | 序号小于 P 的组不动 | 相同 |
| 带 `maxSurge` 的滚动 | 先多建 S 个临时的组，逐个替换，做完之后回收 | 相同 |
| 一个一直起不来的临时的组 | 滚动做完时被回收，不留在 N 个里面 | 相同 |
| 滚动途中把副本数调大 | 序号落进新范围的临时的组留下来当正式的组，不重建 | 相同 |
| 滚动途中把副本数调小 | 先缩容，缩容做完再继续滚动 | 相同 |
| 缩容删谁 | 不就绪的；`pod-deletion-cost` 低的；序号高的。`partition` 不改变次序 | 相同 |
| 缩容留下的洞 | 之后的扩容先补最低的洞 | 相同 |
| 整组恢复之后 | 名字不变；版本按序号和当时的 `partition` | 相同（稀疏布局下的例外见第 3 节） |
| Role 一级的滚动 | Role 实例的序号、Role 一级的 `maxUnavailable`、`maxSurge`、`partition`、`roleCoordination` | 相同 |
| 由 LeaderWorkerSet 转换来的 ModelServing | 组名是序号，`group-index` 从组名里取 | 相同，不需要特殊处理 |

另外：

- ModelServing 的字段和默认值没有变。组名的格式没有变，按它估计名字长度的校验写法不变；有 `maxSurge` 时序号可以到 N + S − 1，位数的估计要把它算进去（[02](02-design.md) 的 8.4）。`partition` 字段的 API 注释（按序号保护、不改变缩容的次序）仍然成立。
- `recoveryPolicy` 三个值的含义、`restartGracePeriodSeconds` 的含义没有变。触发的时机和能不能撤销变了，见 4.2 到 4.5。
- 原地更新：接口不变，行为留给真实验证。一个组不能原地更新时（plugin 不支持、有终态成员、超期、被作废），这个组回到删除替换，在原序号上重建。
- autoscaler、HPA、router 的用法没有变。一处细节：每改一次 Role 的副本数，各组要先确认新的配置，这一小段时间里滚动不往前走。
- 序号集合有洞、又带 `maxSurge` 滚动时，临时的组建在哪个序号上与旧版本的实现不完全一样：这个设计总是建在不小于 N 的序号上并带标记；旧版本的实现可能建在小于 N 的空洞里，不带标记（[06](06-live-upgrade.md) 第 2、6 节）。

依据：行为契约 2.12 的 35 个组合场景和补充脚本在模型里逐步重放，过程和终态与契约相同（[05](05-inference-verification.md) 第 2 节）。没有在集群上验证。

与契约的文字有出入、要声明的三处：

| 处 | 契约的文字 | 这个设计 | 影响 |
| --- | --- | --- | --- |
| 稀疏布局下，范围内的组被替换或整组恢复 | 优先在原序号上重建 | 建在小于 N 的最低空闲序号上 | 名字可能换成更低的序号（第 3 节） |
| 替换腾出来的位置落在受保护的空洞上、按基线版本建 | 等它就绪，再继续清理未就绪的旧组 | 不等 | 只影响本来就未就绪的旧组，就绪的组数不因此再降 |
| 组一级 surge 标记的位置 | Pod 上 | ServingGroup 上；Pod 上留副本 | 4.6 |

前两处是组一级那套按序号的规则本来就带的，契约的 35 个组合场景碰不到。

### 4.2 controller 不在的时候丢了 Pod

今天：controller 重启或升级期间，一个 Pod 被删了（例如节点排空）。controller 回来之后只补这一个，不论 `recoveryPolicy` 是什么。

这个设计：已经建成的 Role 实例丢了成员，回来之后照样按 `recoveryPolicy` 的范围恢复：`RoleRecreate` 重建这个实例，`ServingGroupRecreate` 换掉整个组，在原序号上重建。**升级 controller 的同时排空节点，重建的 Pod 会比今天多。**

| controller 为什么不在 | 应当怎样 | 今天 | 这个设计 |
| --- | --- | --- | --- |
| 升级、重启 | 照 `recoveryPolicy` 的范围处理 | 只补 | 按范围恢复 |
| 主备切换；多副本时主被删，或者主所在的节点故障 | 照范围处理 | 只补 | 按范围恢复 |
| 完全故障；controller 被删除之后 | 允许只补，照范围处理也可以 | 只补 | 按范围恢复。不区分 controller 为什么不在 |

前两行算在正常运行的容灾范围里：恢复策略在这些窗口里同样要成立。

- 正在终止的 Pod 也算丢失。优雅终止时间长的 Pod 在 controller 回来时还没消失，同样触发。
- `ServingGroupRecreate` 下的整组恢复不受滚动预算限制。几个组各丢了一个成员，它们在同一轮被换掉。
- 例外：从旧版本升到这个版本的那一次。接管来的实例的记录从头开始，升级的空窗里丢的只补（[06](06-live-upgrade.md) 的 5.2）。这一次没有做到上表的第一行，与今天旧版本自己升级时相同，是已经接受的缺口；所以这一次升级期间不要排空节点。之后的每一次重启和升级都按上面的规则。
- 现成的缓解：配了 `evictionStrategy` 并且驱逐 webhook 可用时，节点排空造成的丢失不超过驱逐预算；webhook 在 controller 不工作时照常应答。节点故障和直接删 Pod 不受它限制。

**建成之前只补。**还在创建中的实例（成员还没全部建出来过）被删了成员，只补，三种策略都一样。成员是 `Failed` 的不在此列，照样按范围恢复。上游今天在这种时候按范围恢复；在网版本已经是只补。这段时间很短：从实例开始创建到它的成员第一次全部出现。

### 4.3 重启故障的 grace：不变

`restartGracePeriodSeconds` 是「一个成员在重启故障里多久之后按 `recoveryPolicy` 恢复」。它从 controller 第一次看到故障起算，起点在 controller 的内存里。这一点与今天相同：

| | 今天和这个设计 |
| --- | --- |
| controller 重启、升级或换主之后 | 从新进程第一次看到起，重新计满一个 grace |
| grace 是 60 秒：t = 0 看到故障，t = 40 controller 退出，t = 50 新进程起来 | 等到 t = 110 |
| controller 每隔不到一个 grace 就重启一次 | 一直不到期 |
| 成员自己恢复过，之后又坏了 | 重新计 |
| `-1`、`None` | 不计时。改回有限的 grace 之后，从 controller 下一次看到故障的时刻起算，不会立刻到期 |

它和 4.2 不同：4.2 说的是恢复的范围，升级和主备切换的窗口里必须成立；这里说的是恢复什么时候开始：每换一次 controller 进程，从新进程第一次看到起再等一个完整的 grace。恢复一旦开始（组变成 Failed，或者实例开始清空），换了 controller 也接着做完，不再等 grace（4.4）。清空一个实例时（缩容、滚动、恢复都一样），里面正在反复重启的 Pod 先删，等它消失再删其余的。

变的只有一处，在改策略和 grace 的时候：

- **打开策略、缩短 grace，要等下发到 ServingGroup 才生效。**今天改了就按新的。这个设计里往保守的方向仍然立刻生效：controller 按策略动手之前读 ModelServing 上最新的 `recoveryPolicy` 和 grace，改成 `None`、`-1`、更长的 grace，或者改范围，动手那一刻就按新的，不用等下发到每个 ServingGroup，升级接管没做完时也生效。另一个方向不是：打开策略（从 `None`、`-1` 改成别的）、缩短 grace，要等下发到 ServingGroup 才生效（看组的 `spec` 确认）；升级接管没做完时不生效。`None`、`-1` 还没下发到组就改回来的不在此列：改回来之后重新计满一个 grace 就恢复。

### 4.4 整组恢复，和提交之后不撤销

`ServingGroupRecreate` 下一个成员丢了或者故障：

1. 这个组变成 Failed。故障的成员先被删掉；这期间组里其余的成员继续运行。
2. 故障的成员消失之后，整个组被删除。其余的 Pod 由 Kubernetes 的垃圾回收器删。
3. 组和它的 Pod 都消失之后，在原序号上建一个同名的新组（稀疏布局下的例外见第 3 节）。版本按序号和当时的 `partition`：序号小于 P 的用基线版本，其余用新版本。

过程和结果与今天相同。不同的两点：

- **组变成 Failed 之后不撤销。**这时把 `recoveryPolicy` 改成 `None` 拦不住，这个组照样被换掉。从 Failed 到被删要等故障的成员停下来，这段时间可以很长（取决于 Pod 的优雅终止时间），期间没有叫停的办法。`RoleRecreate` 下一个 Role 实例开始清空之后同样不撤销。还在等 grace、组还没有变成 Failed 时改策略，照旧可以取消。
- **第 2 步依赖垃圾回收器**（4.8）。

### 4.5 整个组的 Pod 都没了

`ServingGroupRollingUpdate` 下，一个建成过的组名下一个 Pod 都没有了（节点故障、驱逐、手工删），终止中的也没有：

- 这个组变成 Failed，对象被删掉，在这个序号上重建。`kubectl get servinggroup` 里它的 `AGE` 从头算。
- 任何 `recoveryPolicy` 下都这样，包括 `None`。`None` 的含义是不因为故障删健康的 Pod；这里没有 Pod 可删，不冲突。你会看到一个 Failed 的组，ModelServing 的 `Progressing` 为真（整组恢复中）。
- 这个组的 PodGroup、headless Service 和 plugin 挂在组名下的资源跟着删了重建。今天这两样的 owner 是 ModelServing，Pod 丢了它们不动。gang 调度下 PodGroup 重新入队，排队的集群里可能要等；Service 的 Endpoints 和 DNS 记录消失再出现。Volcano 对同名 PodGroup 先删后建的反应没有验证。
- 新的 Pod 按序号和当时的 `partition` 选版本，与今天相同：滚动途中丢了整个旧版本的组，直接按新版本起，不会先按旧版本起一遍再被换掉。
- 每组只有一个 Pod 的 ModelServing：这个 Pod 每丢一次，组对象换一个。比今天多几步，Pod 的名字和版本与今天相同。
- 整个组的 Pod 是逐个丢的（节点排空逐个驱逐）时，走不走这一条取决于 controller 有没有一轮正好看到「一个都没有」。看到了，按序号和 `partition` 重建；没看到，逐个按组自己的版本补。滚动途中两种结果的版本可以不同。

`RoleRollingUpdate` 下不这样：缺的成员按 Role 的规则补。

相关的两种没有 Pod 的组：

| 情形 | 会怎样 |
| --- | --- |
| 组对象建了，Pod 一个都还没建出来，这时改了模板或 `partition` | controller 还没有开始给它建 Pod（刚建出来的组；PodGroup 建不出来而卡住的组）：组的版本跟着改，放开之后 Pod 按改过之后的版本起，组对象不换。已经开始建 Pod 了（例如 Pod 的创建被拒绝）：它是一个旧版本的组，在滚动预算之内被替换，组对象换一个 |
| 各 Role 的副本数都调成了 0 | 序号小于 N 的空组对象留着，`kubectl get servinggroup` 里还看得到。序号不小于 N 的空组被删掉，在小于 N 的最低空洞上补一个空组（低位补不上时不删）。这些组算就绪，`availableReplicas` 和 `Available` 与今天相同；这期间提交了新版本的话，基线不推进，等副本数调回来。副本数调回来时各序号的版本按「现在新建」的规则：序号小于 P 的用基线版本，其余用新版本 |

### 4.6 组一级 surge 标记的来源在 ServingGroup 上

| | 今天 | 这个设计 |
| --- | --- | --- |
| 标记在哪 | 临时 surge 组的每个 Pod 上：注解 `modelserving.volcano.sh/surge`，值 `serving-group` | ServingGroup 对象上同名、同值的注解。controller 的决定只读这一个；Pod 上的只在给现成的 Pod 建组对象的那一刻被读（升级接管，和 orphan 删组之后的找回） |
| Pod 上 | 来源 | 副本：组带注解，名下每个 Pod 都带；组不带，摘掉 |
| 转正（临时的组留下来当正式的组） | 逐个摘掉每个 Pod 的注解，可以摘到一半 | 摘掉组对象上的注解，一次写。Pod 上的副本随后摘掉，中间有一小段两边不一样 |

- 从 Pod 注解认临时组的脚本和看板继续能用。要准确的话读 ServingGroup：`kubectl get servinggroup` 的 SURGE 一列。
- 手工改 Pod 上的这个注解没有用，下一轮被写回去。
- Role 一级的 surge 标记（同一个 key，值 `role`）没有变，仍然只在 Pod 上。

### 4.7 PodDisruptionBudget

Pod 的 owner 从 ModelServing 变成 ServingGroup。用整数 `minAvailable` 的 PDB 不受影响。用 `maxUnavailable` 或百分比的 PDB，期望数今天按「组的个数」算，这个设计按 Pod 的个数算：PDB 选中多个组时，是这些组的 Pod 数之和。

**方向是变严。**今天期望数远小于实际的 Pod 数，允许的驱逐数偏大；按 Pod 数算之后，今天能通过的节点排空可能被挡住。升级之后请核对这类 PDB 允许的驱逐数。接管到一半时，期望数是两种口径之和。这一条是按 Kubernetes 的 disruption controller 的源码推出来的，没有在集群上验证。

### 4.8 已知的局限

- **序号被占着时，这个位置建不出来。**两种情形：

  | 情形 | 后果 | `RolloutBlocked` 的 reason |
  | --- | --- | --- |
  | 一个组卡在 Terminating（有人给它加了 finalizer；垃圾回收器停了） | 它的序号不能重建。副本数调小之后它还会挡住滚动。带 `maxSurge` 的滚动照常做完，做完之后 `maxUnavailable` 不小于 1 时一直少一个组，直到它消失 | `WaitingForDeletion`，message 写明序号 |
  | 命名空间里有一个同名而不属于这个 ModelServing 的 ServingGroup（用 orphan 方式删过 ModelServing 又建了同名的；别人建的） | 这个序号建不出来。controller 不删它，也不把它收为己有 | `OrdinalOccupied` |

  第一种与今天一个 Pod 卡在终止中相同。
- **整组删除依赖 Kubernetes 的垃圾回收器。**替换、缩容、整组恢复都是删掉组对象，名下的 Pod 由 kube-controller-manager 的垃圾回收器删。它停了或者很慢时，组一直是 Terminating，名下的 Pod 还在跑、还在服务，这个序号上的替换和恢复停住。今天 controller 自己删 Pod，不依赖它。好的一面：kthena 不在时组照样删得掉。
- **不要用 orphan 方式删组**（`kubectl delete servinggroup --cascade=orphan`）。Pod 会留下、照常服务，controller 重建同名的组把它们收回来。但有两种分不清的情形：序号集合有洞时，可能先在更低的序号上多建一个组，再缩掉一个，未就绪的先，被缩掉的不一定是多建的那个；受 `partition` 保护的序号上是一个既不是基线也不是新版本的组时，它可能被换成基线版本。
- **状态晚一跳。**组的就绪要先由 controller 写到 ServingGroup 上，管组的那一层才看到。
  - 滚动的可用性下限不因为这一跳受影响：算「还能删几个组」之前，controller 核对每个组的状态是不是按现在这些 Pod 算的。Pod 变过而状态还没跟上的组（刚坏，或者刚恢复）先不算数：不算就绪，也不会被当成坏的组删掉，等它的状态跟上。剩下的滞后是 Pod 缓存自己的，今天也有。这是按规则推出来的，没有在集群上验证。
  - 「Pod 变过」只看 controller 判断成员状态用到的那些字段（就绪、阶段、重启次数、是否在终止、版本标签等）。别的组件改 Pod 上别的东西不算：周期性地写 `pod-deletion-cost` 不会让组停下来等。成员 Pod 的状态每变一次，它的 ServingGroup 的 `status` 写一次；Pod 启动期间会写几次。抄了标签的 Pod（调试用的副本）不是成员，它怎样变都不影响所在的组。
  - 缩容删谁：未就绪的先，`pod-deletion-cost` 低的先。两样都直接看 Pod，不受这一跳影响，与今天相同。cost 只数这个组自己的成员 Pod；今天按组名标签数，调试用的副本抄来的 cost 也算在里面。
  - 失败的组清空之后、被删之前又有成员出故障：controller 看到 Pod 变了会等状态跟上；它还没看到那次故障时，新出故障的成员和其余的成员一起被删，没有被单独等。
- **一个组的状态一直写不出来时**，它不会被替换，超过期限报 `GroupNotObserved`。它上一次报告的就绪一直算在 `availableReplicas` 和 `Available` 里，可用数可能偏大；拿 `Available` 当发布门禁时请同时看 `RolloutBlocked`。
- **没有 owner 的 ServingGroup 没有人管。**用 orphan 方式删 ModelServing（`kubectl delete modelserving --cascade=orphan`）之后，组和 Pod 都留下。controller 不补、不恢复、不删这些组的 Pod，与今天 orphan 之后 Pod 没有人管相同。要清掉就删这些 ServingGroup。之后建同名的 ModelServing 时它们占着序号（上表第二行）。
- **`kubectl wait` 在同名重建时的行为没有核实。**`kubectl wait --for=delete servinggroup/llama-1` 等的是这个名字；替换时旧对象消失、同名的新对象随即出现，这条命令能不能看到中间的「没有」，没有核实。`kubectl wait --for=condition=Ready servinggroup/llama-1` 在替换开始之前执行时，旧对象的 Ready 是真，预计立刻返回，不表示替换做完了；同样没有核实。脚本里要等一次替换做完，请比对 UID，或者看 ModelServing 的 condition。
- **每一步多一跳。**滚动和恢复的每一步都多经过一个对象；每次替换要等旧的组对象和它的 Pod 都消失，才建同名的新组。耗时没有测量。

## 5. 常见操作

按名字做的事都和今天一样：`kubectl logs`、`kubectl exec`、固定的 DNS 名、按 Pod 名的告警。

| 想做的事 | 怎么做 |
| --- | --- |
| 看滚动进行到哪 | `kubectl get servinggroup -l modelserving.volcano.sh/name=<名>`：REVISION 一列有几个新、几个旧；SURGE 一列有值的是临时的组 |
| 看滚动为什么不动 | `kubectl describe modelserving <名>` 的 conditions。常见的原因见第 6 节 |
| 重建一个副本 | `kubectl delete servinggroup <组名>`。同名的组会自己回来；序号集合有洞时回来的是小于 N 的最低空闲序号，不一定是同一个名字（第 3 节）。回来的组按序号和当时的 `partition` 选版本：删的是序号不小于 P 的旧版本的组时，回来的是新版本，等于手工滚了这一个，不占滚动预算。加 `--cascade=foreground` 时旧对象等名下的 Pod 都消失才消失；不加时对象立刻消失，同名的新对象随即建出来，它的 Pod 等旧 Pod 被回收之后才建。两种都不会有两批同名的 Pod |
| 重建一个 Pod | `kubectl delete pod`。之后发生什么由 `recoveryPolicy` 决定，和今天一样。组里只有这一个 Pod 时见 4.5 |
| 让某个组优先被缩掉 | 和今天一样：给它的 Pod 设 `controller.kubernetes.io/pod-deletion-cost`，下一次缩容按它排 |
| 查一个组上一代的事件 | `kubectl get events --field-selector involvedObject.name=<组名>` |

权限。chart 把 `servinggroups` 的 get、list、watch 聚合进内置的 view、edit、admin 角色；delete 聚合进 edit、admin，能删 Pod 的人本来就能触发恢复；create、update、patch 不给，`status` 的写也不给。

不要做的：

| 操作 | 后果 |
| --- | --- |
| 手工创建 ServingGroup | 带着正确的 owner 时，它被算成多出来的一个组，按缩容的次序删掉一个；老的组里有 deletion cost 更低的，被删的就是老的组。没有 owner 或者 owner 不对时，它占着那个序号，这个位置建不出来（4.8） |
| 改 ServingGroup 的 `spec` | 下一轮被写回去 |
| 改或者清空 ServingGroup 的 `status` | 这个组「建成过」的记录丢失：成员齐全的实例会被重新记成建成，在那之前丢了成员只补，不按范围恢复 |
| 改 ServingGroup 或 Pod 上的 surge 注解 | ServingGroup 上的：摘掉之后这个临时的组成为正式的组，不再被回收，组多于 N 时按缩容删一个。Pod 上的：被写回去 |
| 给 ServingGroup 加 finalizer | 它被删除时卡在 Terminating，序号一直被占着（4.8） |
| `kubectl delete servinggroup --all` | 这个 namespace 里全部 ModelServing 的全部副本同时被删、同时重建，不受滚动预算限制 |
| `kubectl delete servinggroup --cascade=orphan` | 4.8。名字对不上任何成员位置的 Pod（例如 `kubectl debug --copy-to --keep-labels` 的副本）不收、不删，发事件报告 |
| `kubectl delete modelserving --cascade=orphan` | 留下没有 owner 的 ServingGroup 和它们的 Pod，没有人管（4.8） |
| 把 HPA、`kubectl scale` 指向 ServingGroup | ServingGroup 有 `scale` 子资源，只是给 PDB 读期望数用的。改它没有用，下一轮被写回去。扩缩容仍然改 ModelServing 的副本数和 Role 的 `replicas` |
| **删除 ServingGroup 的 CRD** | **集群里全部 ModelServing 的全部 Pod 被删除。**这个 CRD 不是可以清理的内部对象 |

## 6. 排障

事件去哪里看：

- Role 实例和 Pod 一级的事件记在 ServingGroup 上：`kubectl describe servinggroup <组名>`。它只显示当前这个对象的事件。
- ModelServing 上是组一级的事件：建了哪个组、删了哪个组、为什么，带组名和 UID。`kubectl describe modelserving <名>` 看得到每个组的来去，看不到组里面的细节。
- 已经消失的组、同一个名字上一代的组，事件在保留期之内按名字查：`kubectl get events --field-selector involvedObject.name=<组名>`。

| 现象 | 看哪里 | 含义和处理 |
| --- | --- | --- |
| 滚动停住，还有旧版本的组，`RolloutBlocked` 的 reason 是 `TargetNotReady` | 新版本的组里哪个是 NotReady 或 Creating | 新版本起不来，占着不可用的额度。修模板，或者回滚。一个组未就绪超过设定的期限才报 |
| 没有旧版本的组了，滚动一直不结束，reason 同样是 `TargetNotReady` | 同上 | 一个正式位置上的新组一直没就绪。它是不是故障由成员的状态和 `recoveryPolicy` 决定；成员是 Pending（调度不上、拉不到镜像）时 `recoveryPolicy` 帮不上。修模板；或者删掉这个组让它重建 |
| reason 是 `BudgetExhausted` | 就绪的组数和 `maxUnavailable` | 就绪的组已经在下限上，健康的旧组不能再删。未就绪的是受保护的组，或者旧版本的组自己。等它们就绪 |
| reason 是 `BaselineUnknown` | `status.currentRevision` 和对应的 ControllerRevision | 要在受保护的序号上按基线版本建一个组，而基线版本的记录读不到。controller 不会猜成新版本，正式的组也不会跳到更高的序号上去建，会一直等。还有换得动的旧组时，它可能在范围外临时建一个带 SURGE 的新版本的组顶着（第 2 节）。一个要被替换的组，如果替它的新组得建在受保护的序号上，这时也不会被删。恢复那个 ControllerRevision，或者把 `partition` 调到 0 |
| reason 是 `WaitingForDeletion` | 哪个组在 Terminating，或者是 Failed；message 里的序号 | 被删的组的 Pod 还没停完，或者失败的组还没清空，它的序号没有空出来。不在滚动的时候也会出现。副本数调小之后，一个卡住的组会一直挡住滚动。超过期限才报。看那些 Pod 为什么停不下来，组上有没有 finalizer，垃圾回收器在不在工作 |
| reason 是 `OrdinalOccupied` | 事件和 message 里的组名；`kubectl get servinggroup <组名> -o yaml` 的 `ownerReferences` | 有一个同名而不属于这个 ModelServing 的 ServingGroup。controller 不删它。小于 N 的序号被这样的对象占满时，范围外的旧组也不会被替换（删了补不上）。它的 owner 已经不存在时，垃圾回收器会删掉它，等一会儿。它没有 owner 时，确认不要了之后由人删掉 |
| reason 是 `GroupNotObserved` | message 里的组名和原因。三种：这个组的 `status.observedGeneration` 落后于 `metadata.generation`；`observedGeneration` 不落后，但状态不是按现在这些 Pod 算的（Pod 变过，状态没跟上）；这个组名下有一直收不回来的无主 Pod | controller 一直写不出这个组的状态，或者一直改不了那些 Pod 的 owner。这个组不会被替换。看写状态、改 Pod 为什么被拒（webhook、权限）。实在不行，删掉这个组 |
| 没有改模板，一个组被换掉了 | 这个组的 condition `RecreateRequired` 和事件 | 这个组只能删掉重建。两种来源：配了原地更新而它不能原地更新（plugin 不支持、超期、有终态的成员）；orphan 删组之后收回来的 Pod 版本与组的不同。在滚动预算之内替换 |
| 协调的 Role 滚动不动 | ModelServing 的 `CoordinatedRoleRolloutBlocked`，message 里的组名；`kubectl describe servinggroup <组名>` 里同名的 condition | 这个 condition 现在按组报告，ModelServing 上是汇总。原因和处理与今天相同 |
| 有事件说一个带标签的 Pod 没有被收 | 事件里的 Pod 名 | 名字对不上任何成员位置的 Pod（调试用的副本）。controller 不收、不删、不数它。不要了就删掉 |
| controller 的事件里反复出现同一个写被拒绝（建组、删组），condition 的 message 里也写着 | 事件和 message 里的对象和原因 | 组一级的写被对象配额或者准入 webhook 拒绝。没有专门的 condition。Role 副本数的变化照常下发到已有的组。解除拒绝的原因 |
| 新建的组卡在 Creating，没有 Pod，`describe servinggroup` 有 `ReconcileFailed` | 事件里 PodGroup 的创建为什么失败（队列名、配额、Volcano 的 webhook） | PodGroup 建不出来。这时改模板，组的版本直接跟着改，修好之后按新模板起，不用等滚动 |
| 一个组的 condition 里有 `ReconcileFailed` | `describe servinggroup` 的事件 | controller 处理这个组时出错，例如它引用的版本记录不在了。它的 Ready 仍然按 Pod 的实际情况报告；是旧版本的话照常被滚动替换 |
| 滚动停着，`Available` 为真，message 写着停在 `partition` 上 | message 里受保护的组名 | 不是故障。这是灰度停点：序号小于 `partition` 的组留在旧版本。把 `partition` 调小才继续 |
| 一个组长时间是 Failed | `describe servinggroup` 的 Drained | 故障的成员还没停下来。它停下之前组不会被删 |
| 一个组长时间是 Terminating | 它名下的 Pod、PodGroup、Service；它的 `finalizers`；kube-controller-manager | 等它们都消失。这一步由 Kubernetes 的垃圾回收器做，和 kthena 在不在无关 |
| 一个 Role 实例一直在清空，不删 Pod 也不重建 | `describe servinggroup` 的 `DrainBlocked` 和事件 | plugin 的 `OnRoleDelete` 一直失败。它挡住这个实例的恢复、Role 缩容和按重建的滚动。修 plugin |
| 组的个数比 N 多 | SURGE 一列 | 滚动途中多出不超过 `maxSurge` 个带 SURGE 的组是正常的。不带 SURGE 而多出来的：副本数刚调小，正在缩容；或者刚从旧版本升上来（第 7 节）；或者有人用 orphan 方式删过组。都会缩回。一直不缩回：看有没有组卡在 Terminating，是不是有两个 controller 进程在跑 |
| 组的序号有洞，例如只有 `llama-0`、`llama-3` | ModelServing 的 condition 的 message | 按 `pod-deletion-cost` 缩容留下的，正常，今天也是这样。之后的扩容先补最低的洞 |
| 删掉的组又回来了，`AGE` 很小 | — | 正常（第 3 节） |
| `describe servinggroup` 里看不到以前的事件 | — | 它只显示当前这个对象的。按名字查（上面） |
| controller 重启之后，一批实例或者组同时被重建 | 重启期间有没有排空节点，有没有 Pod 被删 | 4.2 |
| 一个在重启故障里的成员比预期晚被恢复 | 这期间 controller 有没有重启、换主 | 每个新进程重新计满一个 grace（4.3） |
| controller 的日志里有 AlreadyExists 和 409 Conflict | — | 正常路径，不是错误。重复的创建撞名字；对上一代对象、或者对已经变过的对象的写被拒绝，下一轮重新计划。同一个组名一直 AlreadyExists 而组一直建不出来：看 `OrdinalOccupied` |
| 升级之后新的 controller Pod 就绪了，迟迟不接手 | 新 Pod 的事件和日志；同一个 namespace 里有没有旧版本的 controller Pod | 它在等旧版本的 Pod 消失，事件和日志里写着在等哪个；等待超过设定的时间有告警。旧 Pod 卡在 Terminating 时一直等：处理那个 Pod。不要只把 Pod 对象强制删掉 |
| 升级之后新的 controller Pod 不就绪，旧的还在工作 | 两个 CRD 在不在；新 Pod 的日志 | ServingGroup 的 CRD 或者对它的权限不在。先装 CRD（第 7 节） |
| 运行中的 controller 停下了，只应答 webhook | controller Pod 的事件和日志；同一个 namespace 里的 controller Pod | 它看到了旧版本的 controller Pod：有人做了 `helm rollback` 或 `kubectl rollout undo`。见第 7 节「不要做的」 |
| 升级之后 ModelServing 一直在接管中（`Progressing` 为真，message 写着接管中） | 它的接管注解；名下还有没有 owner 是 ModelServing 的 Pod、PodGroup、Service；有没有同名而不属于它的 ServingGroup | 接管没做完。看 controller 的事件和日志里卡在哪个对象上 |

## 7. 升级

完整的设计和每一步的理由在 [06](06-live-upgrade.md)。这里是操作的人要知道的。

**升级之后会怎样。**已经在运行的 Pod 不重启、不重建。每个还有活 Pod 的存量的组多出一个同名的 ServingGroup 对象。组名、Pod 名、DNS 名、`partition` 的含义都和升级前相同。滚动途中临时多出来的 surge 组，升级之后仍然是临时的组：它的标记搬到 ServingGroup 上，Pod 上的留着。升级过程中有一小段时间（旧进程停下到新进程接手）没有人做滚动和恢复，已经在运行的 Pod 不受影响。

有一样东西在升级那一刻就变了，不用等到 Pod 重建：存量 Pod、PodGroup、Service 的 owner 的 kind 从 ModelServing 变成 ServingGroup。按 owner 聚合的监控（例如按 owner 的 kind 统计 Pod 的指标）、成本分摊、按 owner 写的准入策略受影响。

接管期间 ModelServing 的 `status` 的计数不重算，停在旧版本写的值上，接管完成之后才更新。

**步骤。**

1. 升级前检查（只读）。它会报告：正在进行的滚动和恢复；哪些组是临时的 surge 组；做不到不断服的负载；会被新的校验拦住的对象；升级之后会不会滚动（版本对不对得上）；升级之后会缩掉一个组的 ModelServing（见下）；没有 owner 的带标签的 Pod；已 cordon 或正在排空的节点。完整的输出在 [06](06-live-upgrade.md) 的 7.1。
2. 装 CRD。Helm 升级不会装新的 CRD，也不会更新旧的：

   ```sh
   kubectl apply --server-side -f <chart>/crds/
   ```

3. `helm upgrade`。从这一步到验收完，不要排空节点，不要做会删 Pod 的操作。
4. 等每个 ModelServing 接管完成。新的 controller Pod 先只应答 webhook，等旧的 Pod 停掉；在等什么写在它的事件和日志里。然后验收：
   - 每个 ModelServing 的接管注解是 `Done`；
   - 每个还有活 Pod 的存量的组有一个同名的 ServingGroup，SURGE 一列有值的就是升级前临时的那几个；
   - Pod 的 UID 和重启计数没有变；
   - **没有改过 `spec` 的 ModelServing 没有组在终止**（升级前检查已经报出会缩掉一个组的除外）；
   - `status` 的可用数全程不低于升级前；
   - 存量 Pod 的 DNS 名仍然解析，PodGroup 的 UID 没有变。

验收之后没有别的步骤。

第 2 步漏了：升级会在开始前失败，或者新的 controller 不就绪、旧的继续工作。不会出现「新的起来了却不干活，又没有任何信号」。新的 controller Pod 就绪之后迟迟不接手：它在等旧的 Pod 停掉，见第 6 节。

**升级之后会缩掉一个组的情形。**序号集合有洞、并且正在带 `maxSurge` 滚动的 ModelServing：旧版本可能把临时多出来的那个组建在了小于 N 的空洞里，不带临时的标记。升级之后它是正式的组，正式的组比 N 多一个，controller 缩掉一个。多出来的那个组已经就绪时，按缩容的次序删一个：没有设 deletion cost 时是序号最高的那个旧组，与旧版本本来要做的相同。它还没就绪时，被缩掉的是它，之后重新来。没有健康的组被多删。升级前检查会报出这样的 ModelServing；等它滚动做完再升级。

**升级和排空节点。**这一次升级期间丢的 Pod 只补（4.2 的例外）；已经是 `Failed` 的 Pod 除外，见下一段。之后新版本的每一次升级和重启都按 4.2：停机期间丢了成员，回来之后按 `recoveryPolicy` 的范围恢复。所以以后升级和重启 controller 的同时也不要排空节点。

**升级之前已经有 `Failed` 的 Pod。**旧版本在容忍它的话（`restartGracePeriodSeconds` 是 `-1`，或者还没到期），新版本接管之后不容忍：终态的 Pod 不等 grace，`RoleRecreate` 重建它所在的实例，`ServingGroupRecreate` 换掉整个组，健康的 Pod 跟着被重建。升级前检查会列出这样的 Pod 和会被重建的范围。升级之前先处理掉它们。

**不要做的。**

- **接管之后，任何旧版本的 controller 进程都不能再起来。**旧进程按 owner 认 Pod，看到 owner 被换掉，不是报错，是删 Pod：在网版本会把全部 Pod 逐个删掉重建；v1.1.0 在接管途中会立刻删掉同组里还没换 owner 的 Pod，接管完成之后卡住。会让旧进程起来的操作：
  - `helm rollback` 到旧版本；
  - `kubectl rollout undo` controller 的 Deployment；
  - 别处的旧进程：另一个 namespace、另一个 release 里的旧版本；
  - 本地调试：有人用自己的 kubeconfig 在集群外跑旧版本的 controller。

  前两种，新版本看到旧版本的 Pod 之后会停下自己的 controller，但旧进程照样删 Pod。后两种没有防护，只有这条规则。
- 任何时候都不要删 CRD。

**回退。**要用新版本提供的回退步骤（[06](06-live-upgrade.md) 第 9 节）：先创建回退开关，新版本把 Pod 的归属换回去；全部换完之后再装旧版本。次序不能反。再次升级之前要删掉回退开关。

- 回退在名字上没有期限：组名一直是序号，旧版本任何时候都认得。
- 回退到在网版本另有两条限定，都随新版本运行的时间变多（06 第 10 节）。新版本建的 Pod 没有在网版本的实例身份注解：回退之后这些组里丢了成员只补，`recoveryPolicy` 对它们不起作用。在新版本里做过原地更新的 Pod：回退之后任何一次不就绪都被当成不健康重启。回退前检查会报出这些组和 Pod。新版本重建过的实例是新的 headless Service 布局，在网版本怎样对待它没有分析。
- 灰度进行中（`partition` 生效）可以回退：受保护的在新旧版本眼里都是序号小于 P 的组。
- 带 `maxSurge` 的滚动途中可以回退：临时的组带着 Pod 上的标记回去，旧版本接着滚。序号集合有洞时的这种回退没有分析过，等滚动做完再回退。
- 回退的条件（06 的 9.2）：没有正在终止的组，有的话 controller 会等它们消失；版本对得上（旧版本把现在的 Pod 认作它自己算出来的那个版本，否则它会全量滚动）；没有不带 owner 的带标签的 Pod。临时的组在 Pod 上的标记由回退步骤自己写齐，不用人管。

**部署上的变化。**新版本默认打开选主，失去租约的进程会退出。controller 换了 ServiceAccount 的名字。运行中的 controller 看到旧版本的 controller Pod 会停下自己，只应答 webhook（第 6 节）。整组删除的 Pod 由 kube-controller-manager 的垃圾回收器删（4.8）。

## 8. 写 plugin 的人

- 钩子（`OnPodCreate`、`OnPodReady`、`OnRoleSync`、`OnRoleDelete`）现在由 ServingGroup controller 调。在网版本的框架多出的三个钩子（Pod 运行、Pod 删除、整组删除）也是。
- `HookRequest.ServingGroup` 仍然是 `<ModelServing 名>-<序号>`，可以从里面取序号，和今天一样。
- `HookRequest` 多带两样：组的 UID，和实例的代际号。同一个名字上会先后有新旧两批：组被替换之后是同名的新对象，UID 不同；一个 Role 实例被重建之后代际号加一。**代际号只在一个 UID 之内有意义**：同名的新组的代际号又从头数。分辨新旧两批要用「组的 UID 加代际号」，不要只用组名，也不要只用代际号。
- plugin 自己在进程里以组名为键记的东西，在同名重建之后会把上一代的决定用到新一代上。要改成对象上的事实，并带上组的 UID。
- 钩子可能被重复调用（controller 换主之后），要幂等。接管之后 `OnPodReady` 可能对已经就绪的 Pod 再调一次。
- `OnRoleSync` 传入的 owner 是 ServingGroup。plugin 创建的集群内对象请把 owner 设成它，让它们随组一起被回收。
- `OnRoleDelete`：一个 Role 实例被清空时，在删它的 Pod **之前**调，失败会重试。**整个组被删时不保证调到。**整组删除的钩子同样只尽力调一次。
- **`ServingGroupRollingUpdate` 下不要依赖 `OnRoleDelete`。**这个模式下滚动、缩容、整组恢复全部是整组删除。钩子不是偶尔调不到，是从来不保证。在钩子里清理、又不挂在 ServingGroup 名下的资源，每滚动一次泄漏一批。
- `OnRoleDelete` 一直失败：实例停在清空中，不删 Pod，也不重建。它会挡住这个实例的恢复、Role 缩容和按重建的滚动。今天钩子在删 Pod 之后调，失败不挡删除。

钩子的调用点和保证的完整写法在 [02](02-design.md) 第 7 节。
