# 设计说明

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 读者 | 评审这份设计的人。想知道为什么这样做、好处是什么、代价是什么 |
| 内容 | 要解决的问题；为什么是 CR、为什么保留组的序号；优势，每条配一个当前实现的例子；代价；社区的先例；保留序号之后的三个问题 |
| 相对上一版 | 上一版（提交 `e35b2e37`）里组的名字是随机后缀，组一级按数量滚动。这一版保留组的序号。原因在 2.2，影响在第 6 节 |
| 依据 | 「当前实现」指 `volcano-sh/kthena` `main` @ `79c455ef`，逐行对照在 [005 的 06](../modelserving-rollout-final/06-state-table.md) 附录 A 和 [007 的 01](../modelserving-state-machine-v3-v4/01-nondeterminism-and-locks.md)。「在网版本」指生产环境运行的分支，这里只描述它的做法。优势的证据见 [05](05-inference-verification.md)。社区先例的完整调研在任务目录的 `subagents/community-research.md` |

## 1. 要解决的问题

ModelServing 的一个副本叫 ServingGroup，里面有若干 Role 实例，每个实例有若干 Pod。恢复和滚动的单位是 Role 实例或者整个 ServingGroup，都比一个 Pod 大。

问题出在：**这两级单位今天都不是对象。**它们只是 Pod 上的标签。于是「这个组正在被删除」「这个实例建成过」「这个组是临时多出来的」这样的事实没有地方放。它们要么在 controller 的内存里，要么拆开写在每个 Pod 上。

用户举的例子是最典型的一处：

> 对于 pod 缺失，有可能是没有创建完成，也有可能是需要整组删除恢复。

「某个成员的名字上没有 Pod」这一个观察，对应两种差得很远的正确反应：补这一个，或者把范围里其余的 Pod 也删掉重建。区分它们的事实是「这个 Pod 以前在不在」，而这件事不在对象上。

- 当前实现靠删除事件有没有送到本进程，和内存里的单元状态。重启、换主、事件晚到一拍，结果就不一样。
- 在网版本靠一个只在内存里的布尔值（本进程见没见过这个实例建成），再加一把按 ModelServing 的互斥锁把回调和 worker 串起来。它试过把这个事实放进一个私有的 ConfigMap，也试过写成 Pod 上的注解，两次都撤回了。

## 2. 为什么是 CR

### 2.1 事实放在哪

把 ServingGroup 做成对象，范围一级的事实就有了一个可以原子写入的地方。

| 事实放在哪 | 试过或评估过的 | 问题 |
| --- | --- | --- |
| 内存 | 当前实现；在网版本 | 重启就丢。回调和 worker 都要读写它，于是要锁 |
| 拆开写在范围里的每个 Pod 上 | v2 的恢复标记；在网版本的 `deletion-scope` 注解 | 不能原子完成：写到一半停下，范围里一部分有、一部分没有。要一组规则处理「写了一半」 |
| 一个私有的 ConfigMap | 在网版本（已撤回） | 是一张和对象并行的表，要自己保证它和对象一致 |
| 被删的 Pod 自己（finalizer） | 007 的 v3 | 不加对象就能确定。代价是 controller 不在时被删的 Pod 卡在终止中，卸载、回退都要按步骤来 |
| **范围自己的对象** | 本设计 | 一次写就是提交。代价在第 4 节 |

ServingGroup 做成 CR 之后，组一级的事实直接在对象上；Role 实例一级的事实写在组的 `status` 里的一张小表（账本）上，每个实例三个值。007 比较过三种粒度，结论是只把 ServingGroup 做成 CR：Role 实例的序号有实在的外部用途（HPA 的 selector、Volcano 的 task 名、Role 一级的 partition），而没有 worker 的实例只有一个 Pod，给它一个对象不划算。

### 2.2 组的序号保留

把组做成对象，并不要求改它的名字。这份设计的上一版同时去掉了组的序号：名字用随机后缀，组一级按数量滚动。这一版把序号留下，组的名字仍然是 `<ModelServing 名>-<序号>`，和今天相同。用户给的两条理由：

1. **按名字找组和 Pod 的习惯不用改。**随机后缀在每次替换、每次整组恢复之后都会变；序号不变。
2. **组一级和 Role 一级的滚动是同一套行为。**上一版里组一级按数量、Role 一级按序号，同一个 `partition` 字段在两层是两种含义。

上一版把两件事绑在了一起。它们其实可以分开：

| 问题 | 靠什么解决 | 和序号有没有关系 |
| --- | --- | --- |
| 范围一级的事实没有地方放（第 1 节） | 组是对象；账本在它的 `status` 里 | 没有 |
| 组一级那套按序号的规则多（surge 的标记、转正和回收，稀疏序号和补洞） | 上一版：去掉序号，规则少了，对外行为跟着变。这一版：规则留下，与契约 2.12、与 Role 一级相同；规则用到的事实放到对象上 | 有 |

第一个问题是这份设计要解决的，序号留不留都解决。第二个问题上，上一版用对外行为的变化换规则的简化，这一版不换。

保留序号还带来一个上一版没有的性质：**组的名字由规则从对象算出，所以创建是幂等的。**上一版为了挡住「随机名字的创建重复发出」，要在内存里登记在途的创建，新进程还要从一次一致性的 LIST 开始。这一版不需要这些：重复的创建撞名字，要发的写按直读重算，进程里不记自己发过什么（[02](02-design.md) 的 3.2、6.3）。

随之而来的三个问题在第 6 节回答：这样做可行吗；离契约 2.12 是不是更近；是否仍然满足 007 的 DFA 判据。

## 3. 优势

每条四栏：今天是什么样；会出什么问题；本设计里是什么样；证据。

### 3.1 「缺一个成员」在每个状态下只有一个含义

| | |
| --- | --- |
| 今天 | 一个 Role 实例里一部分成员在、一部分不在。是创建中、删除中、还是恢复中，要看内存里的单元状态（`datastore/store.go:66-98` 的 `Creating`、`Running`、`Scaling`、`Deleting`）。成员的名字上没有 Pod 时有四种可能的反应，由删除事件有没有送到本进程决定（`model_serving_controller.go:443-490`、`2352-2386`） |
| 问题 | 同一份对象，两个进程可以做出不同的事。一个刚建到一半的实例，如果删除事件恰好送到，会被整范围删掉重来 |
| 本设计 | 账本的值决定含义。创建中：没建完，补。已建成：丢失，按策略的范围恢复。清空中：接着清。每一格恰好一个反应（[03](03-state-machine.md) 的 4.3） |
| 证据 | 007 的组级模型，原样重跑：去掉账本，没有丢失也会整范围删除 |

### 3.2 换主、重启、被接管之后，做的事相同

| | |
| --- | --- |
| 今天 | 进程启动时把每个 Pod 当成一次新增事件重放，重建内存里的单元状态（`model_serving_controller.go:767-786`）；故障的和终止中的 Pod 不登记，全是这类 Pod 的单元第一轮不在内存里。正在计时的故障、一次删除选定的 UID 都丢失 |
| 问题 | 重启之后的行为取决于重启的时刻。恢复删到一半时重启：当前实现不续删；在网版本靠 Pod 上的标记续删，标记只打了一半时范围不完整 |
| 本设计 | 状态不存储，每一轮从对象判定。新进程读到的状态属于判定表里的某一格，那一格的动作就是原进程下一步要做的。把每一次写当作一个可能停下的点，逐行列出来了（03 的 10.2），列出的写里没有「取决于原进程记得什么」的格子。组的名字确定，重复的创建撞名字，新进程不需要知道原进程发过哪些创建 |
| 证据 | 组一级：模型在任何一次写之后换掉进程，终局的集合与不换相同。实例一级：007 的模型，任何两次写之间停下。钩子、owner、surge 标记、PodGroup 的各行是推演。plugin 自己的进程内记忆不在表里：内置的 headless Service plugin 有一份，要改成对象上的事实（02 第 7 节） |

### 3.3 controller 不在的时候发生的丢失，不被漏掉

| | |
| --- | --- |
| 今天 | controller 停着时一个成员被删掉（节点排空、有人手工删）。回来之后只看到一个缺口。当前实现和在网版本都只补这一个；契约 2.9 把它写成了允许的结果 |
| 问题 | `RoleRecreate`、`ServingGroupRecreate` 的含义是成员丢了要整范围重建（例如同一个实例的 entry 和 worker 之间有建立好的连接）。这个保证在 controller 升级或重启的窗口里不成立，而升级 controller 和排空节点常常是同一次运维 |
| 本设计 | 「建成过」写在组的 `status` 里。回来之后看到已建成的实例缺成员，直读确认，按范围恢复。与 controller 当时在不在无关 |
| 证据 | 007 的模型。这是一处有意的行为变化，要批准（[04](04-behavior-expectations.md) 第 11 节 C2） |

### 3.4 恢复和删除的提交是一次写

| | |
| --- | --- |
| 今天 | 在网版本删一个范围之前，给范围里每个 Pod 逐个打 `deletion-scope` 注解，再逐个删。v2 的设计是每个 Pod 一个恢复标记加七条规则（M1–M7） |
| 问题 | 标记是逐个写的。写到一半停下，范围里一部分有标记、一部分没有。每条规则都要说明「只有一部分带标记时怎么办」 |
| 本设计 | 整组：`status` 的 Failed，一次写。一个实例：账本的 Draining，一次写。这次写带 resourceVersion，两个进程同时写只有一个成功。写之前停下，下一个进程重新判断；写之后停下，下一个进程读到它，接着做 |
| 证据 | 模型：先删后写会在没有丢失时整范围删除 |

### 3.5 没有按资源的锁

| | |
| --- | --- |
| 今天 | 在网版本有一把按 ModelServing 的互斥锁。worker 的整轮 reconcile 持锁，包括其中全部的 API 请求；informer 的回调在锁里对 apiserver 做直读 |
| 问题 | 锁是结果不是原因：回调和 worker 都在读写生命周期状态，所以要把它们串起来。informer 对一个 handler 是单个 goroutine 顺序投递的，一个 Pod 的回调在等某个 ModelServing 的锁时，排在它后面的所有 Pod 事件都要等，包括别的 ModelServing 的 |
| 本设计 | 回调只入队。状态在对象上，每个写带前置条件。进程里留下的东西都不是决定的输入，只会让动作晚一点（02 的 3.2） |
| 证据 | 设计上的性质（[02](02-design.md) 第 6 节）。没有解决的并发问题也列在那里 |

### 3.6 组一级和 Role 一级是同一套规则，名字和契约都不变

| | |
| --- | --- |
| 今天 | 组一级按序号的规则已经在契约 2.12 里定下来：绝对序号的 partition、临时的 surge 单元、被副本数覆盖时转正、滚动结束时回收、补最低的空洞。35 个组合场景和 runner 的用例都按它写。surge 身份标在组里每个 Pod 的注解上，逐个打、逐个摘 |
| 问题 | 规则本身没有问题。问题在它用到的事实是逐 Pod 的：打到一半、摘到一半、组里一个 Pod 都没有的时候，「这个组是不是 surge」没有确定的答案，要另外的规则补 |
| 本设计 | 规则不变，事实换了地方。surge 身份是组对象上的一个注解：建组的那一次请求里带上，转正是一次写。组存在不存在、是哪个版本、是不是 surge、坏了没有，各是对象上的一个字段。两层的计划可以是同一个函数（[02](02-design.md) 的 4.9）。用户看到的名字、`partition` 的含义、过程表都和契约相同 |
| 证据 | 契约的 35 个组合场景和补充脚本在模型里逐步重放，结果见 [05](05-inference-verification.md) 第 2 节 |

### 3.7 不同的组可以并行

| | |
| --- | --- |
| 今天 | 队列的 key 是 ModelServing。一个 ModelServing 的全部组、全部实例、全部 Pod 的工作串在一个 key 上 |
| 问题 | 一个组里的恢复、另一个组里的扩容，互相等 |
| 本设计 | ServingGroup controller 的 key 是一个组。不同的组各走各的。ModelServing controller 只在组的个数、版本、就绪变化时才被唤醒 |
| 证据 | 设计上的性质。批量操作的总耗时由客户端限流决定，没有测量，不承诺更快 |

### 3.8 组看得见

| | |
| --- | --- |
| 今天 | 一个组在什么状态，要从它的 Pod 的标签、注解和 controller 的日志里拼出来。组在删除中、在恢复中，对象上没有一处直接写着 |
| 问题 | 排障靠读日志 |
| 本设计 | `kubectl get servinggroup` 一行一个组：阶段、版本、就绪。受 partition 保护的是哪几个组，写在 ModelServing 的 condition 的 message 里。`describe` 里有 condition 和原因，账本里有每个实例的阶段 |
| 证据 | — |

### 3.9 没有 finalizer

| | |
| --- | --- |
| 对照 | 007 的 v3 用 Pod finalizer 得到同样确定的状态机。代价是 controller 不在时受保护的 Pod 被删之后卡在终止中，连带 ModelServing、namespace 删不掉 |
| 本设计 | 没有 Pod finalizer，没有组的 finalizer。组的删除是前台级联，由 kube-controller-manager 推进，kthena 不在也照样完成 |
| 限定 | 同名重建有它自己的停法：旧对象没有消失之前，那个序号建不出新的。别人给组加了 finalizer、或者垃圾回收器停了，这个序号就一直空着（第 4 节） |

### 3.10 状态机读得出来

| | |
| --- | --- |
| 今天 | 状态分散在内存的几张表里，转换写在回调和 worker 的分支里 |
| 本设计 | 四层状态，每个状态有只用对象字段写的判定条件；每条转换有谁推动、条件、写和前置条件；两个 controller 各一条完整的 reconcile 线（03） |

### 3.11 决定的输入只来自对象

| | |
| --- | --- |
| 今天 | 进程里有会改变动作的状态：组和实例的状态机、「这个实例建成过」、重启故障从什么时候起算、发出去还没看到的创建和删除 |
| 问题 | 这些在换主、重启之后都没有了。同一份对象，新旧两个进程算出不同的动作 |
| 本设计 | 每一轮要发的写是对象快照的函数。进程里留下的四样东西（缓存、队列、钩子调过没有、一个组的 `status` 从什么时候起不新鲜）都不改变任何创建和删除，只推迟动作；最后一样决定一个 condition 什么时候报，是豁免项（02 的 3.2，本文 6.3）。新进程里没有要先等的东西。组的名字确定，重复的创建撞名字，所以连 ReplicaSet 那样的「发出了几个创建还没看到」都不用记：和 StatefulSet 是同一类 |
| 限定 | 「只来自对象」说的是持久的业务事实。缓存不是跨资源一致的快照，时钟也是输入。名字确定只保证创建不重复，不保证旧的视图选中的删除对象和新的视图相同：所以有写要发的轮次按直读重算，每个写另带前置条件（02 的 6.3）。原生 controller 也是这个范围（第 5 节） |
| 证据 | 穷举模型：新进程没有任何内存、缓存可以是旧的，按直读重算时没有多建、没有越过下限；按缓存直接发时两样都报出来（05 第 3 节）。其余是推演 |

## 4. 代价

| 代价 | 内容 | 怎样控制 |
| --- | --- | --- |
| 多一个 CRD | API 面、RBAC、生成的 client、文档、chart；一个 Go 类型要改名 | — |
| 多一个 controller 和两者之间的约定 | 组的就绪要经过组的 `status` 才到 ModelServing controller，晚一跳 | 约定写成六条（02 第 3 节）。额度靠复核：组的 `status` 要是按现在这些 Pod 算的才算数。次序和 deletion cost 直接看 Pod。三样都不受这一跳影响 |
| ModelServing controller 也读 Pod | 复核、找回无主的成员 Pod、缩容和回收的次序，只读缓存 | 复核只比摘要，不另算一遍就绪；找回只用来建对象；次序是偏好 |
| 有写的轮次多一次读 | 每个要发写的轮次直读一次名下的组 | 稳态下不读。没有测量 |
| `status` 当输入 | 约定里 `status` 应当能从观察重建，账本不能 | 零值是不会引起删除的状态；`status` 端点用 RBAC 保护 |
| 组一级的规则没有变少 | surge 的标记、转正、回收，稀疏序号和补洞都留着。每次带 S 的滚动仍然多建 S 个组、最后删掉 S 个 | 规则与契约、与 Role 一级相同，不是新的；用到的事实各是对象上的一个字段 |
| 同名重建 | 替换和整组恢复是「删掉，等旧对象完全消失，再建」。同一个名字先后是两个对象；一个卡在终止中的组、一个外来的同名对象会一直占着序号 | 前台级联；每一处按 UID（02 的 4.5、6.2）。今天的替换也要等旧 Pod 消失 |
| 故障的起点写在账本里 | 换主之后 grace 接着算：停机超过 grace 时，回来第一轮到期的范围同时恢复 | 这改变已批准的 D9，要批准（02 的 5.3） |
| 依赖垃圾回收器 | 原位重建要等旧对象消失；垃圾回收器停了，组停在终止中，Pod 还在跑 | 今天 controller 自己删 Pod。没有缓解 |
| 部署前提 | 选主打开，失主退出 | 今天默认不开，要改 |
| plugin | 整个组被删时 `OnRoleDelete` 不保证调到 | plugin 的资源挂在 ServingGroup 名下，靠垃圾回收 |
| 稳态下的写 | 成员 Pod 的判定用到的字段每变一次，它的组一次 status 写（复核用的摘要变了）；就绪变了，ModelServing 再写一次；每个实例建成时多一次；每个组从没有 Pod 到有 Pod 多一次。Pod 不变时不写；别的组件改 Pod 上无关的字段不写。对象多 N 个 | 没有测量 |
| 一次升级 | 存量负载要接管；新旧进程不能重叠；回退要走受控的步骤 | [06](06-live-upgrade.md)。名字和 `partition` 的含义在升级前后相同 |
| 行为变化 | 契约 2.12 在字面上被碰到的条款 | 04 第 11 节，逐条等批准 |

Role 一级和原地更新没有从这个设计里受益：它们的规则原样搬进新的 controller。

## 5. 社区的先例

| 做法 | 先例 | 来源 |
| --- | --- | --- |
| 父对象管个数和版本，子对象管自己里面的事，通过 `status` 交接 | Deployment 和 ReplicaSet。Deployment controller 启动时不等 ReplicaSet controller 先把 `status` 重算一遍；Recreate 策略下它另外直接看 Pod | [`deployment/sync.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controller/deployment/sync.go)、[`deployment_controller.go`](https://github.com/kubernetes/kubernetes/blob/v1.32.0/pkg/controller/deployment/deployment_controller.go#L152-L177)（`Run`、`getPodMapForDeployment`） |
| 两个额度的滚动算式：未就绪的旧副本先清，健康的受可用数限制 | Deployment 的 `reconcileOldReplicaSets`、`cleanupUnhealthyReplicas` | [`deployment/rolling.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controller/deployment/rolling.go) |
| 子对象的名字确定，controller 里不记「发出去还没看到的创建」 | StatefulSet：按序号填空，没有 expectations。Deployment：ReplicaSet 的名字是模板的哈希加持久化的 `collisionCount`，撞名字时核对 owner 的 UID 和模板 | [`stateful_set_control.go`](https://github.com/kubernetes/kubernetes/blob/v1.32.0/pkg/controller/statefulset/stateful_set_control.go#L388-L428)、[`deployment/sync.go`](https://github.com/kubernetes/kubernetes/blob/v1.32.0/pkg/controller/deployment/sync.go#L188-L256) |
| 名字是位置，版本是标签，基线和目标是两个 ControllerRevision；目标副本全部就绪之后推进基线 | StatefulSet 的 `currentRevision`、`updateRevision` | [`stateful_set_control.go`](https://github.com/kubernetes/kubernetes/blob/v1.32.0/pkg/controller/statefulset/stateful_set_control.go#L215-L274) |
| 按序号的 surge：临时的高序号单元，滚动做完缩回 | LeaderWorkerSet：把 leader StatefulSet 的 `replicas` 扩到至多 N + surge，再按预算缩回 | [`leaderworkerset_controller.go`](https://github.com/kubernetes-sigs/lws/blob/v1.0.0/pkg/controllers/leaderworkerset_controller.go#L588-L618) |
| 稀疏序号 | OpenKruise Advanced StatefulSet 的 `reserveOrdinals` | [`statefulset_types.go`](https://github.com/openkruise/kruise/blob/v1.9.3/apis/apps/v1beta1/statefulset_types.go#L88-L101) |
| 同名重建：前台删除，旧对象消失之前不建同名的新对象 | LeaderWorkerSet 的整组恢复；JobSet 的子 Job | [`pod_controller.go`](https://github.com/kubernetes-sigs/lws/blob/v1.0.0/pkg/controllers/pod_controller.go#L391-L409)、[`jobset_controller.go`](https://github.com/kubernetes-sigs/jobset/blob/v0.13.1/pkg/controllers/jobset_controller.go#L1214-L1229) |
| 计时的起点取对象上的时间戳 | Deployment 的进度期限用 `Progressing` 的 `lastUpdateTime`；`minReadySeconds` 用 Pod Ready 的 `lastTransitionTime`；Job 按索引的退避用失败 Pod 上的注解和结束时间（KEP-3850 明确不用内存里的计时） | [`deployment_util.go`](https://github.com/kubernetes/kubernetes/blob/v1.32.0/pkg/controller/deployment/util/deployment_util.go#L751-L790)、[KEP-3850](https://github.com/kubernetes/enhancements/blob/cac930cb9d7c70192016865425f943c339414011/keps/sig-apps/3850-backoff-limits-per-index-for-indexed-jobs/README.md#L341-L369) |
| 做破坏性决定之前不走缓存读一次 | `ControllerRefManager` 收养之前的复核；Cluster API 的 `APIReader` | [`controller_ref_manager.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controller/controller_ref_manager.go)、[kubernetes#42639](https://github.com/kubernetes/kubernetes/issues/42639) |
| `status` 里放不能从观察重建的状态 | Job 的 `uncountedTerminatedPods`（KEP-2307） | 007 的调研报告 |
| 按 UID 原位替换 owner | Cluster API 的 `ReplaceOwnerRef`，用在 Secret 上 | [`util.go`](https://github.com/kubernetes-sigs/cluster-api/blob/67ee2f32340593d8b0c70ddb216718cd6326564e/util/util.go#L331-L348) |

**原生 controller 的状态来源。**用户的判断是「原生 controller 的状态只来自对象」。读源码之后要加限定：

| controller | 持久的业务事实 | 进程里影响动作的东西 |
| --- | --- | --- |
| StatefulSet | Pod 的名字、revision 标签；自己的 `status` | 没有 expectations。队列的限速 |
| Deployment | ReplicaSet 的 `spec`、`status`、注解；`collisionCount` | 没有 expectations。队列的限速 |
| ReplicaSet、DaemonSet | Pod | expectations：发出了几个创建和删除还没看到。重启就丢，五分钟过期 |
| Job | Pod；`status` 里的计数和 `uncountedTerminatedPods`；按索引的失败次数在 Pod 的注解上 | expectations；全局的失败退避在内存里（[`backoff_utils.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/controller/job/backoff_utils.go#L35-L40)） |
| 节点污点驱逐 | 污点上有 `timeAdded` | 驱逐的计时用本进程的时钟和内存里的队列，重启之后重新排期（[`taint_eviction.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/controller/tainteviction/taint_eviction.go#L479-L489)）。这是反例 |

所以准确的说法是：原生 controller 把持久的业务事实放在对象上；名字确定的那两个（StatefulSet、Deployment）进程里没有影响动作的记忆；名字随机的那几个另有一份记忆（expectations），主要用来推迟动作；等待期间 `spec`、Pod 的状态和删除的候选都可能变，所以不能说它只改时间、不改动作；计时有放在对象上的，也有放在内存里的。本设计的名字是确定的，属于前一类；计时的起点也放在对象上。

和先例不同的地方：

- **逐组的 surge 标记放在子对象上，扩容时摘掉转正。**没有找到先例。LWS 的 surge 状态是 leader StatefulSet 的 `replicas`、`partition` 和一个记着上一次 N 的注解，不是逐组的标记；它的高序号组落进新的 `[0, N)` 时自动成为正式的。本设计要逐组的标记，是因为契约规定 surge 身份和序号无关（稀疏序号下，序号不小于 N 的可以是正式的组）。项目内的先例是当前实现：标记在 Pod 的注解上，转正时带 UID 摘掉。
- **按序号的 surge 本身。**StatefulSet 没有 `maxSurge`（KEP-961 加的是 `maxUnavailable`）；Kruise 的 Advanced StatefulSet 也没有。LWS 的讨论里指出稳定的序号让临时的高序号单元最后要清理（[lws#3](https://github.com/kubernetes-sigs/lws/issues/3)）。这是契约已有的行为，本设计沿用。
- **删除带 UID 的前置条件。**StatefulSet 删 Pod 不带 UID（[`stateful_pod_control.go`](https://github.com/kubernetes/kubernetes/blob/v1.32.0/pkg/controller/statefulset/stateful_pod_control.go#L97-L99)）。本设计每个删除都带，比先例严。
- **确认的就绪另看 Pod。**Deployment 的滚动只看 ReplicaSet 的 `status`，重启之后那份 `status` 可能是旧的，它接受这个窗口：多删的是一个 Pod。这里多删的是一个组（一组 Pod、一次 gang 调度、一次模型加载），所以组的 `status` 要是按 Pod 缓存里现在这些 Pod 算的才算数（02 的 6.3）。两者启动时都没有先后的要求。
- **有写的轮次按直读重算。**原生 controller 按缓存行动，名字随机的靠 expectations 补。这里要发写时读一次对象。
- **滚动途中改副本数。**Deployment 在新旧两边按比例分摊。这里先把规模做完再继续滚动，与契约相同。
- **按组的 deletion cost。**没有找到先例；LWS 明确不做。这里由 ModelServing controller 从 Pod 缓存现算，与今天相同。
- **给运行中的 Pod 换 controller owner。**上游的 `ControllerRefManager` 对已经有别的 controller owner 的对象一律不碰。读到的就地迁移（Kubernetes 文档、PingCAP、Strimzi）都是先让旧 owner 以 orphan 方式删除、再由新 owner 收养，中间有一段 Pod 没有 controller owner。这里一次 PATCH 原位替换，没有那一段，但「两个 controller 不打架」不再由对象上的状态保证，完全靠独占保证（06 第 4 节）。
- **同类项目在类似的变化上多数选择不迁移存量。**RBG 重建 Pod；LWS 要求删除重建；Job 的 KEP-2307 让存量的 Job 留在旧机制上。这里要求不断服，目标比多数先例高。

同名重建在先例里出过的问题，本设计逐条对照：

| 先例里的问题 | 原因 | 本设计 |
| --- | --- | --- |
| 同名的新 Job 继承了旧 Job 没有满足的 expectations，不再协调（[kubernetes#142142](https://github.com/kubernetes/kubernetes/issues/142142)） | 进程里的记忆以名字为键 | 进程里没有以名字为键的、影响动作的记忆（02 的 3.2） |
| LWS 漏掉整组恢复：旧 worker 的删除和同名新 worker 的创建在按名字的队列里合成了一项（[lws#998](https://github.com/kubernetes-sigs/lws/issues/998)） | 故障只存在于事件里，事件被合并就丢了 | 故障不靠事件：账本里已建成的实例缺成员，任何一轮都看得到（02 的 5.2） |
| LWS 在 partition 以下重建的组用了新模板（[lws#280](https://github.com/kubernetes-sigs/lws/issues/280)） | 重建时没有按位置选版本 | 序号小于 P 的位置按基线建；基线读不到时等待（02 的 4.3、4.8） |
| JobSet 逐个重建时新旧两代互相连上（[jobset#684](https://github.com/kubernetes-sigs/jobset/issues/684)） | 新的建出来时旧的还在 | 组的删除是前台级联，旧对象消失之前同名的建不出来。垃圾回收器不知道的残留 Pod 按「外来」等它消失（02 的 4.5） |
| 同名 PVC 新旧缓存交错（[kubernetes#128164](https://github.com/kubernetes/kubernetes/issues/128164)，原因是报告者的分析） | 按名字读到了另一代 | 每个写带 UID |

没有找到先例的：一个带不可逆失败状态的组 CR、由父对象删除之后在原名字上重建。LWS 和 JobSet 的同名重建最接近，但它们的子对象是内置类型，没有「失败」这个由子 controller 写、父 controller 读的 condition。

## 6. 保留序号之后的三个问题

### 6.1 可行吗

可行。对象模型不变：ServingGroup 是 CR，账本在它的 `status` 里，两个 controller。组一级换回契约 2.12 已有的按序号规则。规则用到的每个事实各是对象上的一个字段：

| 规则要知道的 | 今天在哪 | 本设计 |
| --- | --- | --- |
| 这个序号上有没有组 | 有没有带这个组名标签的 Pod | 有没有这个名字的对象 |
| 它是哪个版本 | Pod 的标签，可以不齐 | `spec.revision` |
| 它是不是临时的 surge | 组里至少一个 Pod 带标记 | 对象上的一个注解 |
| 它是不是在删除中 | 内存；Pod 上的标记 | `deletionTimestamp` |
| 它坏了没有 | 内存 | Failed、Drained |

规则表在「按最新的对象算」的前提下用模型重放和穷举过（[05](05-inference-verification.md) 第 2、3 节）。分析找出的没有定义的情形都补了规则：外来的同名对象、`RoleRollingUpdate` 下留着的 surge 标记、没有成员的组、无主的成员 Pod 与补位的先后（[02](02-design.md) 的 4.2、4.5、4.6、5.5）。评审另指出的几处（收养不了的带标签 Pod、复核里两边的定义、没有成员的组与建 Pod 的先后、Role 一级 surge 实例的定义）在定稿里改了规则；这些改动是推演，没有模型。

### 6.2 离契约 2.12 是不是更近

是。

| 量 | 上一版（随机名字，按数量） | 这一版 | 依据 |
| --- | --- | --- | --- |
| 要批准的契约条款 | 14 条 | 6 条：账本一类 4 条（C1–C4），提交之后不撤销（C12），重启之后 grace 不重新计（N1，新增） | [04](04-behavior-expectations.md) 第 11 节 |
| 契约的 35 个组合场景加补充脚本，共 43 段 | 32 段一致，11 段有意变化 | 43 段一致，终态每个序号上的版本也相同 | 模型重放，05 第 2 节 |
| runner 的用例，在模型里逐步重放 | 只单独重放了 5 个停点 | 组合场景 35 例的 188 个停点全部到得了；`normal` 套件 SG 模式「滚动中扩缩容」「滚动中再次变更」的 68 例全部符合 | 05 的 2.2、2.3 |
| 用例目录里规则与契约不同的 | 148 项 | 4 项（`plugin-retry`：整组删除时 `OnRoleDelete` 只尽力调）。这是按归类规则的结果：1013 项里逐例重放的是 164 项，推演 121 项，728 项沿用上一版的归类 | 04 的 8.5 |
| 对外的名字 | 每次替换、每次整组恢复都换 | 不变 | 02 的 2.2 |
| `partition` 的含义 | 组一级按数量，Role 一级按序号 | 两层都按序号 | 02 的 4.3 |

变小的是组一级：去掉的 9 条全是组一级的（partition、稀疏序号、surge、次序、名字、容量）。恢复、重启和持久记录这一面没有变小：上一版的 C1–C4、C12 原样留下，另加一条 N1。

还剩的差别分三类：

- **账本一类**（C1–C4）：范围一级的事实写在对象上带来的。controller 不在时发生的丢失回来之后按范围恢复；实例建成之前的丢失只补。这是把组做成对象的目的，与序号无关。
- **N1**：重启故障的起点写在对象上，换主之后 grace 接着算。它改了已批准的 D9，是「状态只来自对象」的直接后果，可以单独不批准（02 的 5.3）。
- **C1、C2、N1 的来历**：这三条恢复的是契约 2.4、2.8 有过、2.9 按用户的明确取舍删掉的要求。批准它们是撤回那一次取舍里的三样。来历、这一版的载体与当时有什么不同、三条怎样分开批，写在 04 的 11.1。
- **字面出入，不改变结果的大类**：稀疏布局下范围内的组不保证原位重建；替换的容量按基线建时不占在途额度。这两处是 v2 的规则本来就有的，契约的场景碰不到。

用例目录里另有 51 项记「待定」，都不是规则不同：契约自己有 8 例与 SG-P11 对同一种形状的期望相反，另有 6 例可能是同一处，要契约的维护者裁决；37 例时序用例的夹具对准「组刚丢了成员、`status` 还没写出来」的那一跳，这一版组的 `status` 要是按现在这些 Pod 算的才进额度，机制上不再受它影响，但要在集群上才能确认（04 第 8 节）。

### 6.3 是否满足 DFA 的判据

逐条对照 [007 的 02](../modelserving-state-machine-v3-v4/02-dfa-criteria-and-tradeoffs.md) 的 J1–J8：

| 判据 | 上一版 | 这一版 |
| --- | --- | --- |
| J1 写只取决于 `spec`、快照和现在的时间；时间只和快照里的时间比 | 故障的起点在内存里，是豁免项 | 创建和删除的写满足：故障的起点在账本里。一处豁免：`RolloutBlocked` 的 `GroupNotObserved` 什么时候报，取决于 ModelServing controller 在内存里记的「从什么时候起看到这个组不新鲜或者待收养」。condition 也是写；它不决定任何创建和删除 |
| J2 内存只能扣留，扣留的上界与重启次数无关 | 扣留项：在途登记和在途屏障、接管屏障（每次重启重新等一遍）、故障起点（豁免） | 进程里剩四样：缓存、队列、钩子调过没有、一个组从什么时候起不新鲜或者待收养。前三样的上界与重启次数无关。第四样是豁免项：每次重启重新计，两次重启的间隔一直短于期限时 `GroupNotObserved` 一直不报。它与上一版的故障起点是同一种形状，后果只有一个 condition 的早晚 |
| J3 证据持久 | 建成之后满足 | 相同。另外两样成了对象上的事实：组一级的 surge 身份、故障的起点 |
| J4 提交是一次原子的写，写到一半停下也是合法的状态 | 满足 | 相同。转正是一个对象上的一次写；替换的中间状态「删了、还没建」就是「序号空着」 |
| J5 按旧的视图行动是安全的 | 创建不幂等，靠在途登记和启动时的一致性 LIST | 创建幂等。有写的轮次按直读重算。每个写带 UID，删除另带 resourceVersion |
| J6 环境里有另一个实例、另一个版本 | 两个进程并存时多建组；升级那一刻名字和 partition 的语义都换 | 不多建；升级前后名字、partition、surge 身份相同 |
| J7 进展 | 没有因为名字卡住的情形 | **多几种会一直停住的状态**，各有 reason。因为名字：序号被卡在终止中的组、外来的同名对象占着（`WaitingForDeletion`、`OrdinalOccupied`）；成员、PodGroup、Service 的名字被不属于这个组的对象占着（组上 `ReconcileFailed`，ModelServing 上 `TargetNotReady`）。因为复核和找回：组的 `status` 一直写不出来；待收养一直收养不完（都是 `GroupNotObserved`）。因为补不上：要替换的组删掉之后低位建不出来（`BaselineUnknown`、`OrdinalOccupied`）。进展的假设是 API 的写最终成功；组一级的写一直被拒绝时只有事件和 message，没有 reason |
| J8 每个状态都有定义 | 满足 | 分析指出的 7 处没有定义的情形都补了。评审另指出 5 处定义不全或者互相矛盾的（无主的成员 Pod、复核里两边的定义、没有成员的组、Role 一级的 surge 实例在不在期望之内、外来对象的三处），定稿里各改了规则。复查又指出 3 处（摘要的范围与入队的范围不同、「没有成员」要不要已观测、替换之前看不看补得上），也改了。与 Pod 有关的改动没有模型；与规则函数有关的重跑过 |

仍然列为豁免的（两版相同）：预算是对多个对象求和，写不成一个对象上的条件；两个进程并存时一个额度可能删两个；Drained 晚一跳（这一版缩到 Pod 缓存的滞后）。

### 6.4 状态是不是只来自对象

用户的要求是「状态来源最好只来自 CR」。做到的范围：

| | 内容 |
| --- | --- |
| 决定用到的事实 | 全部在对象上：ModelServing、ServingGroup、Pod、ControllerRevision（02 的 3.2 的清单） |
| 进程里剩下的 | 四样。都不改变任何创建和删除；丢了只会让动作晚一点、钩子多调一次、一个 condition 晚报（6.3 的 J1、J2） |
| 相对上一版去掉的 | 组的在途登记、在途屏障、启动时的一致性 LIST、接管屏障、内存里的故障起点 |
| 靠什么去掉 | 名字确定（重复的创建撞名字）；有写的轮次按直读重算（按旧视图的删除不会发出去）；组的 `status` 带着它是按哪些 Pod 算的，不是按现在这些 Pod 算的不算数（停机期间变了而 `status` 没跟上的组不会按旧的 `status` 处理）；故障的起点写进账本 |

三处限定：

1. **不只是 CR。**输入还包括 Pod 和 ControllerRevision。它们也是对象。
2. **缓存不是对象。**每个 controller 都靠缓存触发，缓存可以旧。本设计的做法是：缓存只决定「这一轮有没有事要做」，要发的写按直读算。读和写之间的窗口、Pod 缓存自己的滞后仍然在（02 的 6.5）。
3. **原生 controller 也是这个范围。**见第 5 节：StatefulSet 和 Deployment 进程里没有影响动作的记忆；ReplicaSet、Job、DaemonSet 有 expectations；有的计时在内存里。本设计对齐的是前两个。

要批准的一处：故障的起点写进账本改变了 D9（N1）。不批准时它留在内存里，其余各条不受影响。

### 6.5 相对上一版得到的和失去的

| 得到的 | 失去的 |
| --- | --- |
| 名字不变 | 组一级的规则没有变少：surge 的标记、转正、回收，稀疏序号和补洞都在 |
| 两层同一套规则，`partition` 同一种含义 | 每次带 S 的滚动多建 S 个组、做完再删 S 个，与今天相同（上一版少建少删） |
| 与契约的差别从 14 条降到 6 条 | 同名重建：每一处要按 UID 分辨两代 |
| 创建幂等，进程里不记发过什么 | 序号被占着是一种新的停法；一个卡在终止中的组，上一版可以换个名字补建，这一版不能 |
| 升级前后名字、partition、surge 身份相同；没有回退窗口；灰度进行中可以回退 | SG-C16 的终态在上一版里是「少一个就绪」，这一版与契约相同；反过来上一版在 6 个场景里各少建一个组 |
