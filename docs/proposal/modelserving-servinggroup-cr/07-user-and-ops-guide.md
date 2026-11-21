# 使用与运维说明

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 读者 | 使用 ModelServing 的人，和升级、运维 kthena 的人。不需要读过别的几份 |
| 状态 | **这是设计提案的一部分，功能还没有实现。**下面的命令、列名、注解名都是示意，以实现为准。它的用处是让评审的人看到设计落到用户手上是什么样子 |
| 细节 | 规则在 [02](02-design.md)，状态在 [03](03-state-machine.md)，行为的逐项对照在 [04](04-behavior-expectations.md)，升级的完整设计在 [06](06-live-upgrade.md) |

## 1. 一句话

ModelServing 的每个副本（ServingGroup）变成了一个看得见的对象。你写 ModelServing 的方式不变；变的是组和 Pod 的名字、组一级 `partition` 的含义，以及几处恢复和滚动的细节。

## 2. 你会看到什么

多了一种对象：

```text
$ kubectl get servinggroup -l modelserving.volcano.sh/name=llama
NAME          PHASE      REVISION       READY   AGE
llama-7k2qd   Ready      llama-5c4d8    True    3d
llama-b9x4m   Ready      llama-6b7f9    True    2h
llama-q2w8n   Creating   llama-6b7f9    False   40s
```

| 列 | 含义 |
| --- | --- |
| PHASE | Creating：还有成员没建好。Ready。NotReady：建成过，现在有成员不就绪。Failed：丢了成员，等着被整组替换。Terminating。Adopting：正在从旧版本接管 |
| REVISION | 这个组的版本 |

被 `partition` 留在旧版本的是哪几个组，这张表里没有：它是 controller 对全部组分类的结果，组对象上没有放它的字段。看 ModelServing 的 condition 的 message（第 5 节）。

`kubectl describe servinggroup` 里有每个 Role 实例的阶段，和这个组不就绪或者失败的原因。

ModelServing 自己的 `status`：

- 字段没有增减。`replicas`、`updatedReplicas`、`currentReplicas`、`availableReplicas` 的口径不变。
- `UpdateInProgress`、`Progressing`、`Available` 三个 condition 的含义不变，同一时刻仍然至多一个为真。`Available` 仍然是「全部组就绪，没有在进行的变更」。
- 新增一个 condition `RolloutBlocked`：有事要做而做不了，并且同一个原因持续超过设定的期限时为真。reason 是 `BaselineUnknown`、`WaitingForDeletion`、`GroupNotObserved`、`TargetNotReady`、`BudgetExhausted` 之一（第 6 节）。它可以和 `UpdateInProgress` 或 `Progressing` 同时为真。正常的缩容、滚动里短暂的等待不会让它变成真。
- 别的新事实写在已有 condition 的 message 里：正在接管、有组在整组恢复、灰度停在 `partition` 上。灰度停点上 `Available` 为真，message 列出受保护的组名。

判定式在 [03](03-state-machine.md) 第 6 节。

**ServingGroup 是 controller 管的对象，像 Deployment 名下的 ReplicaSet。**不要手工创建它；不要改它的 `spec`（会被写回去）；不要改它的 `status`；删除它的后果见第 5 节。

## 3. 名字

| | 以前 | 现在 |
| --- | --- | --- |
| 组 | `llama-0`、`llama-1` | `llama-7k2qd`：后缀是随机的 |
| Pod | `llama-0-prefill-0-0` | `llama-7k2qd-prefill-0-0`：前缀跟着组名；Role 实例的序号和 worker 的序号不变 |
| 标签 `modelserving.volcano.sh/group-name` | `llama-0` | `llama-7k2qd` |
| headless Service 的名字 | 前缀是组名：`llama-0-...` | 前缀跟着组名：`llama-7k2qd-...` |
| PodGroup 的名字 | 组名：`llama-0` | `llama-7k2qd` |
| Pod 的 hostname 和 DNS 名 | `llama-0-prefill-0-0.<Service 名>` | 跟着 Pod 名和 Service 名一起变 |
| plugin 按组名生成的对象 | 名字里带 `llama-0` | 带 `llama-7k2qd` |
| 滚动替换一个组之后 | 名字不变 | **名字变了**，上面各行跟着变 |
| 整组恢复之后 | 名字不变 | **名字变了**，上面各行跟着变 |

受影响的是依赖固定名字的东西：写死 `llama-0-...` 的脚本和告警规则；写死某个 Pod 的 DNS 名的客户端；从组名里解析序号的程序。按标签选 Pod、通过 Service 访问的不受影响。

日志和监控：按 Pod 名聚合的日志流和监控序列，每滚动一次、每整组恢复一次新增一批，旧的一批不再更新。以前替换之后 Pod 名不变，同一条序列接着用。按标签（ModelServing 的名字、Role）聚合的不受影响。`kubectl logs`、`kubectl exec` 用的名字每次要重新找。

两种情况下组名仍然是序号：

- 由 LeaderWorkerSet 转换来的 ModelServing。它的 Pod 的 hostname 和 `group-index` 标签要一个整数序号，所以一直用序号命名。序号可以不连续：缩容会留下洞，今天也是这样。
- 回退窗口开着的时候（第 7 节）。回退窗口是 controller 的一个集群级设置，从旧版本升级时开着，由运维关闭。开着时全部 ModelServing 新建的组都取最小的空闲序号，包括升级之后才创建的 ModelServing；存量的组保留原来的名字。不关闭就一直如此。序号可以有洞。

## 4. 行为上的变化

ModelServing 的字段没有增减。下面是同样的配置在行为上的差别。

### 4.1 组一级的 `partition` 按数量

`rolloutStrategy` 是 `ServingGroupRollingUpdate` 时，顶层的 `partition`：

| | 以前 | 现在 |
| --- | --- | --- |
| 含义 | 序号小于 P 的组留在旧版本 | 留 P 个组在旧版本 |
| 留的是哪几个 | 序号 0 到 P−1 | 名额先给基线版本（`status.currentRevision`，上一次完整滚完的版本）的组，再给别的旧版本的组；各自里面取次序靠前的：序号名字的组在前，按序号；随机名字的组按创建时间，老的在前 |
| 灰度停点上扩容，P 是整数 | 新组是新版本 | 相同 |
| 灰度停点上扩容，P 是百分比 | 新组都是新版本 | P 跟着 N 变大：不够 P 个的部分，新组是旧版本。例如 N=4、P=50%，2 旧 2 新；扩到 6：以前 2 旧 4 新，现在 3 旧 3 新。旧版本的镜像要还在。灰度期间有 autoscaler 或 HPA 在动副本数的，P 用整数 |
| 灰度已经停在 P 上，一个受保护的组被整组恢复 | 看它的序号：序号小于 P 的按旧版本重建 | 旧版本的组少于 P 个了，补出来的是旧版本（基线版本），不论丢的是哪个组 |
| 灰度还没滚完（旧版本的组多于 P 个），一个受保护的组被整组恢复 | 同上 | 补出来的是新版本；留在旧版本的换成另一个组，个数相同 |
| 把模板改回基线版本（回滚） | 序号小于 P 的组不动，包括已经换成新版本的 | `partition` 不保护任何组：不是基线版本的组都被换回去，按滚动预算。例如把 P 调到 N 冻结现状，再把模板改回去：以前什么都不动，现在已经换成新版本的组被换回基线版本 |
| 想让指定的某几个组留在旧版本 | 可以 | 不可以，只能指定个数 |

Role 一级的 `partition`（`RoleRollingUpdate`）没有变，仍然按 Role 实例的序号。

### 4.2 带 `maxSurge` 的滚动少建少删

以前：先多建 S 个组，逐个替换，最后把多出来的 S 个删掉。现在：先多建 S 个，逐个替换，最后一个旧组删掉时数量正好是 N。新版本的组一共只建 N 个，先就绪的那几个留下。

一个后果：新版本的组里有一个一直起不来（例如落在坏节点上），它留在 N 个里面，不会因为滚动结束而被换掉。以前它是临时多出来的那个，结束时被回收，正式的位置上另建一个。现在滚动停在「还差一个没就绪」：

- 怎样确认：`RolloutBlocked` 为真，reason 是 `TargetNotReady`；`kubectl get servinggroup` 里有一个新版本的组一直是 Creating 或 NotReady。
- `recoveryPolicy` 只管成员丢失和故障。成员是 Pending（调度不上、拉不到镜像）时它帮不上。
- 处理办法：删掉那个组，controller 会补一个新的。

  ```sh
  kubectl delete servinggroup <组名> --cascade=foreground
  ```

### 4.3 controller 不在的时候丢了 Pod

以前：controller 重启或升级期间，一个 Pod 被删了（例如节点排空）。controller 回来之后只补这一个，不论 `recoveryPolicy` 是什么。

现在：已经建成的 Role 实例丢了成员，回来之后照样按 `recoveryPolicy` 的范围恢复：`RoleRecreate` 重建这个实例，`ServingGroupRecreate` 换掉整个组。**升级 controller 的同时排空节点，重建的 Pod 会比以前多。**

- 正在终止的 Pod 也算丢失。优雅终止时间长的 Pod 在 controller 回来时还没消失，同样触发。
- `ServingGroupRecreate` 下的整组恢复不受滚动预算限制。几个组各丢了一个成员，它们在同一轮被换掉。
- 例外：从旧版本升到这个版本的那一次。接管来的实例的记录从头开始，停机期间丢的只补（[06](06-live-upgrade.md) 的 5.2）。之后的每一次重启和升级都按上面的规则。
- 现成的缓解：配了 `evictionStrategy` 并且驱逐 webhook 可用时，节点排空造成的丢失不超过驱逐预算；webhook 在 controller 不工作时照常应答。节点故障和直接删 Pod 不受它限制。

还在创建中的实例（成员还没全部建出来过）被删了成员，只补，三种策略都一样。成员是 `Failed` 的不在此列，照样按范围恢复。

### 4.4 整组恢复是换一个新的组

`ServingGroupRecreate` 下一个成员丢了或者故障：

1. 这个组变成 Failed。故障的成员先被删掉；这期间组里其余的成员继续运行。
2. 故障的成员消失之后，整个组被删除。
3. 组里的 Pod 都消失之后，建一个新的组。名字是新的。版本按数量选（4.1）：旧版本的组不够 `partition` 个时是旧版本，否则是新版本。

组变成 Failed 之后不撤销。这时把 `recoveryPolicy` 改成 `None` 拦不住，这个组照样被换掉。从 Failed 到被删要等故障的成员停下来，这段时间可以很长（取决于 Pod 的优雅终止时间），期间没有叫停的办法。`RoleRecreate` 下一个 Role 实例开始清空之后同样不撤销。

### 4.5 缩容删谁

次序：不就绪的；反正要被滚动替换的旧版本的组；不受 `partition` 保护的；`pod-deletion-cost` 低的；最新的。

与以前的差别：滚动途中缩容，先删的是旧版本的组，不看 deletion cost；有 `partition` 时受保护的组最后删。deletion cost 是汇总到组上之后才被读到的：刚设完马上缩容，可能还没生效。「不就绪」按组上一次报告的状态算：一个组刚变坏、状态还没写上去时，可能选错。

### 4.6 PodDisruptionBudget

Pod 的 owner 从 ModelServing 变成 ServingGroup。用整数 `minAvailable` 的 PDB 不受影响。用 `maxUnavailable` 或百分比的 PDB，期望数以前按「组的个数」算，现在按「这个组里 Pod 的个数」算。

**方向是变严。**以前期望数远小于实际的 Pod 数，允许的驱逐数偏大；现在按 Pod 数算，以前能通过的节点排空可能被挡住。升级之后请核对这类 PDB 允许的驱逐数。接管到一半时，期望数是两种口径之和。这一条是按 Kubernetes 的 disruption controller 的源码推出来的，没有在集群上验证。

### 4.7 没有变的

- ModelServing 的字段和默认值。校验变了一处：名字长度按随机后缀估计，新的校验只拦新引入的值，存量对象的更新不受影响。`partition` 字段的 API 注释写的是按序号保护、不改变缩容的次序，这两句在组一级不再成立（4.1、4.5）。
- Role 一级的滚动规则：Role 实例的序号、Role 一级的 `maxUnavailable`、`maxSurge`、`partition`、`roleCoordination`。`RoleRollingUpdate` 下组名、恢复、钩子的调用者同样变了（第 3 节、4.3、4.4、第 8 节）。
- `recoveryPolicy` 三个值的含义，`restartGracePeriodSeconds`。触发的时机和能不能撤销变了，见 4.3、4.4。
- 原地更新：接口不变，行为留给真实验证。一个组不能原地更新时（plugin 不支持、有终态成员、超期、被作废），这个组回到删除替换。
- autoscaler、HPA、router 的用法。router 的行为没有变。autoscaler 和 HPA 在行为上有几处不同：百分比 `partition` 的停点上扩容会建旧版本的组（4.1）；缩容删谁的次序（4.5）；每改一次 Role 的副本数，各组要先确认新的配置，这一小段时间里滚动不往前走。

### 4.8 已知的局限

- **滚动途中可能多删健康的组。**一个组刚失去就绪（节点故障、驱逐、探针抖动）时，它的状态要先写到 ServingGroup 上才被看到，晚一跳。这一小段时间里 controller 按旧的就绪数算额度，可用的组数短时间低于 N − `maxUnavailable`。界是瞬时的：任一时刻可用的组数不低于 N − `maxUnavailable` − 状态还没写出来的组数。它不是累计的界：一个组的状态一直写不出来时，滚动会按它旧的就绪数接着删，直到状态写出来；这种情形超过期限会报 `GroupNotObserved`。以前也有这种滞后，少一跳。瞬时的界在模型里成立；窗口多长没有测量。
- **每一步多一跳。**滚动和恢复的每一步都多经过一个对象，可能比以前慢一点。controller 重启之后，一个 ModelServing 名下的组要先被过一遍，才开始处理对它的修改。耗时没有测量。

## 5. 常见操作

| 想做的事 | 怎么做 |
| --- | --- |
| 看滚动进行到哪 | `kubectl get servinggroup -l modelserving.volcano.sh/name=<名>`：REVISION 一列有几个新、几个旧。被 `partition` 留下的是哪几个：`kubectl describe modelserving <名>`，灰度停下时 `Available` 的 message 列出受保护的组名 |
| 看滚动为什么不动 | `kubectl describe modelserving <名>` 的 conditions。常见的原因见第 6 节 |
| 重建一个副本 | `kubectl delete servinggroup <组名> --cascade=foreground`。controller 会补一个新的组。不带 `--cascade=foreground` 也可以，只是新组可能在旧组的 Pod 还没停完时就开始创建，短时间内 Pod 数超过 N + S |
| 重建一个 Pod | `kubectl delete pod`。之后发生什么由 `recoveryPolicy` 决定，和以前一样 |
| 让某个组优先被缩掉 | 稳态下和以前一样：给它的 Pod 设 `controller.kubernetes.io/pod-deletion-cost`。滚动途中和有 `partition` 时次序不同，并且晚一跳，见 4.5 |

权限。chart 把 `servinggroups` 的 get、list、watch 聚合进内置的 view、edit、admin 角色；delete 聚合进 edit、admin，能删 Pod 的人本来就能触发恢复；create、update、patch 不给，`status` 的写也不给。

不要做的：

| 操作 | 后果 |
| --- | --- |
| 手工创建 ServingGroup | 它被算成多出来的一个组，按缩容的次序删掉一个。老的组里有 deletion cost 更低的，被删的就是老的组 |
| 改 ServingGroup 的 `spec` | 下一轮被写回去 |
| 改或者清空 ServingGroup 的 `status` | 这个组「建成过」的记录丢失。成员齐全的实例会被重新记成建成；在那之前丢了成员只补，不按范围恢复 |
| `kubectl delete servinggroup --all` | 这个 namespace 里全部 ModelServing 的全部副本同时被删、同时重建，不受滚动预算限制 |
| `kubectl delete servinggroup --cascade=orphan` | Pod 留下，没有 owner。controller 看到之后按组名重建同名的组，把它们收回来。有一个竞态：controller 可能先看到少了一个组、按数量建了一个新组，收回来之后多一个，再按缩容的次序删掉一个，删的不一定是后建的那个。名字对不上任何成员位置的 Pod（例如 `kubectl debug --copy-to` 的副本）不收，发事件报告 |
| **删除 ServingGroup 的 CRD** | **集群里全部 ModelServing 的全部 Pod 被删除。**这个 CRD 不是可以清理的内部对象 |

## 6. 排障

事件去哪里看：

- Role 实例和 Pod 一级的事件记在 ServingGroup 上：`kubectl describe servinggroup <组名>`。
- ModelServing 上是组一级的事件：建了哪个组、删了哪个组、为什么，带组名。`kubectl describe modelserving <名>` 看得到每个组的来去，看不到组里面的细节。
- 已经消失的组，事件在保留期之内仍然查得到：`kubectl get events --field-selector involvedObject.name=<组名>`。

| 现象 | 看哪里 | 含义和处理 |
| --- | --- | --- |
| 滚动停住，还有旧版本的组，`RolloutBlocked` 的 reason 是 `TargetNotReady` | 新版本的组里哪个是 NotReady 或 Creating | 新版本起不来，占着不可用的额度。修模板，或者回滚。一个组未就绪超过设定的期限才报 |
| 滚动只差一个新版本的组没就绪，一直不结束，reason 同样是 `TargetNotReady` | 同上 | 起不来的新组留在 N 个里面（4.2）。删掉那个组，controller 补一个新的：`kubectl delete servinggroup <组名> --cascade=foreground`。成员是 Pending 时 `recoveryPolicy` 帮不上 |
| reason 是 `BudgetExhausted` | 就绪的组数和 `maxUnavailable` | 就绪的组已经在下限上，健康的旧组不能再删。未就绪的是受保护的组、状态还没更新的组，或者旧版本的组自己。等它们就绪 |
| reason 是 `BaselineUnknown` | `status.currentRevision` 和对应的 ControllerRevision | 要按旧版本建一个组，而旧版本的记录读不到。controller 不会猜成新版本，会一直等。恢复那个 ControllerRevision，或者把 `partition` 调到 0 |
| reason 是 `WaitingForDeletion` | 有组在 Terminating，或者是 Failed | 被删的组的 Pod 还没停完，或者失败的组还没清空，位置没有空出来，替换和创建都不做。不在滚动的时候也会出现。副本数调小之后，一个卡住的组会一直挡住滚动。超过期限才报。看那些 Pod 为什么停不下来 |
| reason 是 `GroupNotObserved` | 哪个组的 `status.observedGeneration` 落后；controller 的日志 | controller 一直写不出这个组的状态，它不会被替换。看写状态为什么被拒。实在不行，删掉这个组 |
| 一个组的 condition 里有 `ReconcileFailed` | `describe servinggroup` 的事件 | controller 处理这个组时出错，例如它引用的版本记录不在了。它的 Ready 仍然按 Pod 的实际情况报告；是旧版本的话照常被滚动替换 |
| 滚动停着，`Available` 为真，message 写着停在 `partition` 上 | message 里受保护的组名 | 不是故障。这是灰度停点：留在旧版本的组数不超过 `partition`，都受保护。把 `partition` 调小才继续 |
| 一个组长时间是 Failed | `describe servinggroup` 的 Drained | 故障的成员还没停下来。它停下之前组不会被删 |
| 一个组长时间是 Terminating | 它名下的 Pod、PodGroup、Service | 等它们都消失。这一步由 Kubernetes 的垃圾回收器做，和 kthena 在不在无关 |
| 一个 Role 实例一直在清空，不删 Pod 也不重建 | `describe servinggroup` 的 `DrainBlocked` 和事件 | plugin 的 `OnRoleDelete` 一直失败。它挡住这个实例的恢复、Role 缩容和按重建的滚动。修 plugin |
| 组的个数比 N 多 | 最新的那个组 | 滚动途中多出不超过 `maxSurge` 个是正常的。超出的：两个 controller 进程短暂并存时可能多建一个，之后会被缩回。一直不缩回：看是不是有两个进程在跑 |
| 升级之后新的 controller Pod 就绪了，迟迟不接手 | 新 Pod 的事件和日志；同一个 namespace 里有没有旧版本的 controller Pod | 它在等旧版本的 Pod 消失，事件和日志里写着在等哪个；等待超过设定的时间有告警。旧 Pod 卡在 Terminating 时一直等：处理那个 Pod。不要只把 Pod 对象强制删掉 |
| 升级之后新的 controller Pod 不就绪，旧的还在工作 | 两个 CRD 在不在；新 Pod 的日志 | ServingGroup 的 CRD 或者对它的权限不在。先装 CRD（第 7 节） |
| 运行中的 controller 停下了，只应答 webhook | controller Pod 的事件和日志；同一个 namespace 里的 controller Pod | 它看到了旧版本的 controller Pod：有人做了 `helm rollback` 或 `kubectl rollout undo`。见第 7 节「不要做的」 |
| 升级之后 ModelServing 一直在接管中（`Progressing` 为真，message 写着接管中） | 它的接管注解；名下还有没有 owner 是 ModelServing 的 Pod、PodGroup、Service | 接管没做完。看 controller 的日志里卡在哪个对象上 |

## 7. 升级

完整的设计和每一步的理由在 [06](06-live-upgrade.md)。这里是操作的人要知道的。

**升级之后会怎样。**已经在运行的 Pod 不重启、不重建。每个还有活 Pod 的存量的组多出一个同名的 ServingGroup 对象，Pod 的名字不变。升级过程中有一小段时间（旧进程停下到新进程接手）没有人做滚动和恢复，已经在运行的 Pod 不受影响。

有一样东西在升级那一刻就变了，不用等到 Pod 重建：存量 Pod、PodGroup、Service 的 owner 的 kind 从 ModelServing 变成 ServingGroup。按 owner 聚合的监控（例如按 owner 的 kind 统计 Pod 的指标）、成本分摊、按 owner 写的准入策略受影响。

接管期间 ModelServing 的 `status` 的计数不重算，停在旧版本写的值上，接管完成之后才更新。

**步骤。**

1. 升级前检查（只读）。它会报告：正在进行的滚动和恢复；做不到不断服的负载；会被新的校验拦住的对象；升级之后会不会滚动（版本对不对得上）；升级之后滚动会停下的 ModelServing（序号有洞并且 `partition` 生效）；已 cordon 或正在排空的节点。完整的输出在 [06](06-live-upgrade.md) 的 7.1。
2. 装 CRD。Helm 升级不会装新的 CRD，也不会更新旧的：

   ```sh
   kubectl apply --server-side -f <chart>/crds/
   ```

3. `helm upgrade`，回退窗口的设置开着。它默认是关的，要显式打开；忘了打开时新版本不接管存量的负载，并在 ModelServing 上报告，打开之后才继续。从这一步到验收完，不要排空节点，不要做会删 Pod 的操作。
4. 等每个 ModelServing 接管完成。新的 controller Pod 先只应答 webhook，等旧的 Pod 停掉；在等什么写在它的事件和日志里。然后验收：
   - 每个 ModelServing 的接管注解是 `Done`；
   - Pod 的 UID 和重启计数没有变；
   - **没有改过 `spec` 的 ModelServing 没有组在终止**；
   - `status` 的可用数全程不低于升级前；
   - 存量 Pod 的 DNS 名仍然解析，PodGroup 的 UID 没有变。
5. 运行一段时间、确认不需要回到旧版本之后，关闭回退窗口。它是 controller 的一个集群级设置（chart 的一个值），对全部 ModelServing 一起生效。之后新建的组才用随机名字。关闭之前核对名字的长度：随机后缀比序号长，名字贴着长度上限的 ModelServing 关闭之后建不出新组。

第 2 步漏了：升级会在开始前失败，或者新的 controller 不就绪、旧的继续工作。不会出现「新的起来了却不干活，又没有任何信号」。新的 controller Pod 就绪之后迟迟不接手：它在等旧的 Pod 停掉，见第 6 节。

**升级和排空节点。**这一次升级期间丢的 Pod 只补（4.3 的例外）；已经是 `Failed` 的 Pod 除外，见下一段。之后新版本的每一次升级和重启都按 4.3：停机期间丢了成员，回来之后按 `recoveryPolicy` 的范围恢复。所以以后升级和重启 controller 的同时也不要排空节点。

**升级之前已经有 `Failed` 的 Pod。**旧版本在容忍它的话（`restartGracePeriodSeconds` 是 `-1`，或者还没到期），新版本接管之后不容忍：终态的 Pod 不等 grace，`RoleRecreate` 重建它所在的实例，`ServingGroupRecreate` 换掉整个组，健康的 Pod 跟着被重建。升级前检查会列出这样的 Pod 和会被重建的范围。升级之前先处理掉它们。

**不要做的。**

- **接管之后，任何旧版本的 controller 进程都不能再起来。**旧进程按 owner 认 Pod，看到 owner 被换掉，不是报错，是删 Pod：在网版本会把全部 Pod 逐个删掉重建；v1.1.0 在接管途中会立刻删掉同组里还没换 owner 的 Pod，接管完成之后卡住。会让旧进程起来的操作：
  - `helm rollback` 到旧版本；
  - `kubectl rollout undo` controller 的 Deployment；
  - 别处的旧进程：另一个 namespace、另一个 release 里的旧版本；
  - 本地调试：有人用自己的 kubeconfig 在集群外跑旧版本的 controller。

  前两种，新版本看到旧版本的 Pod 之后会停下自己的 controller，但旧进程照样删 Pod。后两种没有防护，只有这条规则。
- 回退要用新版本提供的回退步骤（[06](06-live-upgrade.md) 第 9 节）：先创建回退开关，新版本把 Pod 的归属换回去；全部换完之后再装旧版本。次序不能反。再次升级之前要删掉回退开关。
- 关闭回退窗口之后不能再不重建 Pod 地回退。
- 灰度进行中（`partition` 生效）不要回退，两个源版本都一样：旧版本按绝对序号保护，会滚掉一些旧版本的组。条件和一个待评估的做法见 [06](06-live-upgrade.md) 的 9.2。
- 任何时候都不要删 CRD。

**部署上的变化。**新版本默认打开选主，失去租约的进程会退出。controller 换了 ServiceAccount 的名字。运行中的 controller 看到旧版本的 controller Pod 会停下自己，只应答 webhook（第 6 节）。

## 8. 写 plugin 的人

- 钩子（`OnPodCreate`、`OnPodReady`、`OnRoleSync`、`OnRoleDelete`）现在由 ServingGroup controller 调。在网版本的框架多出的三个钩子（Pod 运行、Pod 删除、整组删除）也是。
- 传进来的组名可能是随机后缀，不要从里面解析序号。
- 钩子可能被重复调用（controller 换主之后），要幂等。
- 需要从组名里取序号的 plugin 只能用在由 LeaderWorkerSet 转换来的 ModelServing 上。别的 ModelServing 在回退窗口关上之后新建的组没有序号。
- `OnRoleDelete`：一个 Role 实例被清空时，在删它的 Pod **之前**调，失败会重试。**整个组被删时不保证调到。**整组删除的钩子同样只尽力调一次。plugin 创建的集群内对象请把 owner 设成 ServingGroup，让它们随组一起被回收。
- **`ServingGroupRollingUpdate` 下不要依赖 `OnRoleDelete`。**这个模式下滚动、缩容、整组恢复全部是整组删除。钩子不是偶尔调不到，是从来不保证。在钩子里清理、又不挂在 ServingGroup 名下的资源，每滚动一次泄漏一批。
- `HookRequest` 带实例的代际号。同一个序号上会先后有新旧两批 Pod；删除钩子要按代际号核对，不要把对旧一批的删除用到新一批上。
- `OnRoleDelete` 一直失败：实例停在清空中，不删 Pod，也不重建。它会挡住这个实例的恢复、Role 缩容和按重建的滚动。以前钩子在删 Pod 之后调，失败不挡删除。
