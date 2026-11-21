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
NAME          PHASE      REVISION       PROTECTED   READY   AGE
llama-7k2qd   Ready      llama-5c4d8    true        True    3d
llama-b9x4m   Ready      llama-6b7f9                True    2h
llama-q2w8n   Creating   llama-6b7f9                False   40s
```

| 列 | 含义 |
| --- | --- |
| PHASE | Creating：还有成员没建好。Ready。NotReady：建成过，现在有成员不就绪。Failed：丢了成员，等着被整组替换。Terminating。Adopting：正在从旧版本接管 |
| REVISION | 这个组的版本 |
| PROTECTED | 滚动途中被 `partition` 留在旧版本的组 |

`kubectl describe servinggroup` 里有每个 Role 实例的阶段，和这个组不就绪或者失败的原因。

ModelServing 自己的 `status` 字段不变。`conditions` 里多了几种原因：正在接管、正在整组恢复、滚动被一个起不来的新版本的组挡住、基线版本读不到。

**ServingGroup 是 controller 管的对象，像 Deployment 名下的 ReplicaSet。**不要改它的 `spec`（会被写回去）；不要改它的 `status`；删除它的后果见第 5 节。

## 3. 名字

| | 以前 | 现在 |
| --- | --- | --- |
| 组 | `llama-0`、`llama-1` | `llama-7k2qd`：后缀是随机的 |
| Pod | `llama-0-prefill-0-0` | `llama-7k2qd-prefill-0-0`：前缀跟着组名；Role 实例的序号和 worker 的序号不变 |
| 标签 `modelserving.volcano.sh/group-name` | `llama-0` | `llama-7k2qd` |
| 滚动替换一个组之后 | 名字不变 | **名字变了** |
| 整组恢复之后 | 名字不变 | **名字变了** |

受影响的是依赖固定名字的东西：写死 `llama-0-...` 的脚本和告警规则；写死某个 Pod 的 DNS 名的客户端；从组名里解析序号的程序。按标签选 Pod、通过 Service 访问的不受影响。

两种情况下组名仍然是序号：

- 由 LeaderWorkerSet 转换来的 ModelServing。它的 Pod 的 hostname 和 `group-index` 标签要一个从 0 起的整数，所以一直用序号命名。
- 从旧版本升级过来的 ModelServing，在运维关闭回退窗口之前（第 7 节）。存量的组保留原来的名字，直到它自己被替换。

## 4. 行为上的变化

ModelServing 的字段没有增减。下面是同样的配置在行为上的差别。

### 4.1 组一级的 `partition` 按数量

`rolloutStrategy` 是 `ServingGroupRollingUpdate` 时，顶层的 `partition`：

| | 以前 | 现在 |
| --- | --- | --- |
| 含义 | 序号小于 P 的组留在旧版本 | 留 P 个组在旧版本 |
| 留的是哪几个 | 序号 0 到 P−1 | 最老的 P 个（按创建时间）。从旧版本升级来的组按原来的序号排 |
| 灰度停点上扩容，P 是整数 | 新组是新版本 | 相同 |
| 灰度停点上扩容，P 是百分比 | 新组都是新版本 | P 跟着 N 变大：不够 P 个的部分，新组是旧版本。例如 N=4、P=50%，2 旧 2 新；扩到 6：以前 2 旧 4 新，现在 3 旧 3 新 |
| 旧版本的组少于 P 时（例如一个受保护的组被整组恢复） | 看它的序号 | 补出来的是旧版本，不论是哪个组 |
| 想让指定的某几个组留在旧版本 | 可以 | 不可以，只能指定个数 |

Role 一级的 `partition`（`RoleRollingUpdate`）没有变，仍然按 Role 实例的序号。

### 4.2 带 `maxSurge` 的滚动少建少删

以前：先多建 S 个组，逐个替换，最后把多出来的 S 个删掉。现在：先多建 S 个，逐个替换，最后一个旧组删掉时数量正好是 N。新版本的组一共只建 N 个，先就绪的那几个留下。

一个后果：如果新版本的组里有一个一直起不来，它不会因为滚动结束而被换掉。它算不算故障、要不要重建，由 `recoveryPolicy` 决定。滚动会停在「还差一个没就绪」，condition 里写着原因。

### 4.3 controller 不在的时候丢了 Pod

以前：controller 重启或升级期间，一个 Pod 被删了（例如节点排空）。controller 回来之后只补这一个，不论 `recoveryPolicy` 是什么。

现在：已经建成的 Role 实例丢了成员，回来之后照样按 `recoveryPolicy` 的范围恢复：`RoleRecreate` 重建这个实例，`ServingGroupRecreate` 换掉整个组。**升级 controller 的同时排空节点，重建的 Pod 会比以前多。**

还在创建中的实例（成员还没全部起来过）丢了成员，只补，三种策略都一样。

### 4.4 整组恢复是换一个新的组

`ServingGroupRecreate` 下一个成员丢了或者故障：

1. 这个组变成 Failed。故障的成员先被删掉；这期间组里其余的成员继续运行。
2. 故障的成员消失之后，整个组被删除。
3. 组里的 Pod 都消失之后，建一个新的组。名字是新的。

### 4.5 缩容删谁

次序：不就绪的；反正要被滚动替换的旧版本的组；不受 `partition` 保护的；`pod-deletion-cost` 低的；最新的。

与以前的差别：滚动途中缩容，先删的是旧版本的组，不看 deletion cost；有 `partition` 时受保护的组最后删。deletion cost 是汇总到组上之后才被读到的：刚设完马上缩容，可能还没生效。

### 4.6 PodDisruptionBudget

Pod 的 owner 从 ModelServing 变成 ServingGroup。用整数 `minAvailable` 的 PDB 不受影响。用 `maxUnavailable` 或百分比的 PDB，期望数以前按「组的个数」算，现在按「这个组里 Pod 的个数」算。升级之后请核对这类 PDB 允许的驱逐数。

### 4.7 没有变的

- ModelServing 的全部字段、默认值、校验。
- `RoleRollingUpdate` 下的一切：Role 实例的序号、Role 一级的 `maxUnavailable`、`maxSurge`、`partition`、`roleCoordination`。
- `recoveryPolicy` 三个值的含义，`restartGracePeriodSeconds`。
- 原地更新。
- autoscaler、HPA、router 的用法。

## 5. 常见操作

| 想做的事 | 怎么做 |
| --- | --- |
| 看滚动进行到哪 | `kubectl get servinggroup -l modelserving.volcano.sh/name=<名>`：REVISION 一列有几个新、几个旧；PROTECTED 是被 `partition` 留下的 |
| 看滚动为什么不动 | `kubectl describe modelserving <名>` 的 conditions。常见的原因见第 6 节 |
| 重建一个副本 | `kubectl delete servinggroup <组名> --cascade=foreground`。controller 会补一个新的组。不带 `--cascade=foreground` 也可以，只是新组可能在旧组的 Pod 还没停完时就开始创建，短时间内 Pod 数超过 N + S |
| 重建一个 Pod | `kubectl delete pod`。之后发生什么由 `recoveryPolicy` 决定，和以前一样 |
| 让某个组优先被缩掉 | 给它的 Pod 设 `controller.kubernetes.io/pod-deletion-cost`，和以前一样 |

不要做的：

| 操作 | 后果 |
| --- | --- |
| 改 ServingGroup 的 `spec` | 下一轮被写回去 |
| 改或者清空 ServingGroup 的 `status` | 这个组「建成过」的记录丢失：之后丢了成员只补一个，不按范围恢复 |
| `kubectl delete servinggroup --cascade=orphan` | Pod 留下没人管，直到 controller 把它们重新收回来 |
| **删除 ServingGroup 的 CRD** | **集群里全部 ModelServing 的全部 Pod 被删除。**这个 CRD 不是可以清理的内部对象 |

## 6. 排障

| 现象 | 看哪里 | 含义和处理 |
| --- | --- | --- |
| 滚动停住，condition 的原因是 `TargetNotReady` | 新版本的组里哪个是 NotReady 或 Creating | 新版本起不来，占着不可用的额度。修模板，或者回滚 |
| 原因是 `BudgetExhausted` | 就绪的组数和 `maxUnavailable` | 就绪的组已经在下限上。等不就绪的组恢复 |
| 原因是 `BaselineUnknown` | `status.currentRevision` 和对应的 ControllerRevision | 要按旧版本建一个组，而旧版本的记录读不到。controller 不会猜成新版本，会一直等。恢复那个 ControllerRevision，或者把 `partition` 调到 0 |
| 原因是 `WaitingForDeletion` | 有组在 Terminating | 被删的组的 Pod 还没停完，位置没有空出来。看那些 Pod 为什么停不下来 |
| 一个组长时间是 Failed | `describe servinggroup` 的 Drained | 故障的成员还没停下来。它停下之前组不会被删 |
| 一个组长时间是 Terminating | 它名下的 Pod、PodGroup、Service | 等它们都消失。这一步由 Kubernetes 的垃圾回收器做，和 kthena 在不在无关 |
| 组的个数比 N 多 | 最新的那个组 | 两个 controller 进程短暂并存时可能多建一个，之后会被缩回。一直不缩回：看是不是有两个进程在跑 |
| 升级之后 ModelServing 一直是 `Adopting` | 它的接管注解；名下还有没有 owner 是 ModelServing 的 Pod | 接管没做完。看 controller 的日志里卡在哪个对象上 |

## 7. 升级

完整的设计和每一步的理由在 [06](06-live-upgrade.md)。这里是操作的人要知道的。

**升级之后会怎样。**已经在运行的 Pod 不重启、不重建。每个存量的组多出一个同名的 ServingGroup 对象，Pod 的名字不变。升级过程中有一小段时间（旧进程停下到新进程接手）没有人做滚动和恢复，已经在运行的 Pod 不受影响。

**步骤。**

1. 升级前检查（只读）。它会报告：正在进行的滚动和恢复；做不到不断服的负载；会被新的校验拦住的对象。
2. 装 CRD。Helm 升级不会装新的 CRD，也不会更新旧的：

   ```sh
   kubectl apply --server-side -f <chart>/crds/
   ```

3. `helm upgrade`。
4. 等每个 ModelServing 接管完成，核对 Pod 的 UID 和重启计数没有变。
5. 运行一段时间、确认不需要回到旧版本之后，关闭回退窗口。之后新建的组才用随机名字。

第 2 步漏了：升级会在开始前失败，或者新的 controller 不就绪、旧的继续工作。不会出现「新的起来了却不干活」。

**不要做的。**

- 不要用 `helm rollback` 回到旧版本。旧版本不认识 ServingGroup：在网版本会把全部 Pod 逐个删掉重建，v1.1.0 会卡住。回退要用新版本提供的回退步骤：先把 Pod 的归属换回去，再停新版本，再装旧版本。
- 关闭回退窗口之后不能再不重建 Pod 地回退。
- 灰度进行中（`partition` 生效）不要回退到 v1.1.0：它按序号保护，会滚掉一些旧版本的组。
- 任何时候都不要删 CRD。

**部署上的变化。**新版本默认打开选主，失去租约的进程会退出。controller 换了 ServiceAccount 的名字。

## 8. 写 plugin 的人

- 钩子（`OnPodCreate`、`OnPodReady`、`OnRoleSync`、`OnRoleDelete`）现在由 ServingGroup controller 调。
- 传进来的组名可能是随机后缀，不要从里面解析序号。
- 钩子可能被重复调用（controller 换主之后），要幂等。
- `OnRoleDelete`：一个 Role 实例被清空时，在删它的 Pod **之前**调，失败会重试。**整个组被删时不保证调到。**plugin 创建的集群内对象请把 owner 设成 ServingGroup，让它们随组一起被回收。
