# 设计说明

## 0. 文档信息

| 项 | 内容 |
| --- | --- |
| 读者 | 评审这份设计的人。想知道为什么这样做、好处是什么、代价是什么 |
| 内容 | 要解决的问题；为什么是 CR；优势，每条配一个当前实现的例子；代价；社区的先例 |
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

把 ServingGroup 做成对象，范围一级的事实就有了一个可以原子写入的地方。

| 事实放在哪 | 试过或评估过的 | 问题 |
| --- | --- | --- |
| 内存 | 当前实现；在网版本 | 重启就丢。回调和 worker 都要读写它，于是要锁 |
| 拆开写在范围里的每个 Pod 上 | v2 的恢复标记；在网版本的 `deletion-scope` 注解 | 不能原子完成：写到一半停下，范围里一部分有、一部分没有。要一组规则处理「写了一半」 |
| 一个私有的 ConfigMap | 在网版本（已撤回） | 是一张和对象并行的表，要自己保证它和对象一致 |
| 被删的 Pod 自己（finalizer） | 007 的 v3 | 不加对象就能确定。代价是 controller 不在时被删的 Pod 卡在终止中，卸载、回退都要按步骤来 |
| **范围自己的对象** | 本设计 | 一次写就是提交。代价在第 4 节 |

ServingGroup 做成 CR 之后，组一级的事实直接在对象上；Role 实例一级的事实写在组的 `status` 里的一张小表（账本）上，每个实例三个值。007 比较过三种粒度，结论是只把 ServingGroup 做成 CR：Role 实例的序号有实在的外部用途（HPA 的 selector、Volcano 的 task 名、Role 一级的 partition），而没有 worker 的实例只有一个 Pod，给它一个对象不划算。

用户还指出组的序号没有意义、副本本来是等价的。组成了对象之后名字不必带序号，组一级那些只因为序号才存在的东西（序号空洞、补洞、surge 的转正和回收）也就没有了。

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
| 本设计 | 状态不存储，每一轮从对象判定。新进程读到的状态属于判定表里的某一格，那一格的动作就是原进程下一步要做的。把每一次写当作一个可能停下的点，逐行列出来了（03 的 10.2），列出的写里没有「取决于原进程记得什么」的格子 |
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
| 本设计 | 回调只入队。状态在对象上，每个写带前置条件。内存里只有三样东西，都只会让动作晚一点。唯一共享的是「按 key 找到这份内存」的那张表 |
| 证据 | 设计上的性质（[02](02-design.md) 第 6 节）。没有解决的并发问题也列在那里 |

### 3.6 组一级少了一整套东西

| | |
| --- | --- |
| 今天 | 组的名字带序号，所以有：临时 surge 的标记；surge 被副本数覆盖时转正；滚动结束时回收；缩容留下的序号空洞；之后扩容和替换时「补最低的洞」；「身份债」。契约第 1 节的场景专门规定稀疏序号下的行为，另有 8 个场景涉及 surge 的身份（SG-S04、C03、C12、C13、C14、C16、P08、P10） |
| 问题 | 这些规则互相牵扯（例如：扩容之后总数正好等于 N，旧的 surge 算不算转正）。每次带 S 的滚动要多建 S 个组、最后再删掉 S 个组 |
| 本设计 | 多出来的组和别的组没有区别。还有过期的组时上限是 N + S，没有了就是 N。最后一个过期的组删掉时数量正好是 N |
| 证据 | 契约的 35 个组合场景重放：6 个场景各少建一个、少删一个组（SG-S04、C03、C12、C13、C14、P10）；稀疏序号的场景里 SG-S01、S02、S03、S05 的可观察量与契约相同，SG-P11 少一次白建白删（04 第 3 节）。对要拉模型的负载，每次滚动少启动 S 个组。有一个场景的终态变差：SG-C16 |

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

### 3.9 没有新的卡住方式

| | |
| --- | --- |
| 对照 | 007 的 v3 用 Pod finalizer 得到同样确定的状态机。代价是 controller 不在时受保护的 Pod 被删之后卡在终止中，连带 ModelServing、namespace 删不掉 |
| 本设计 | 没有 Pod finalizer，没有组的 finalizer。组的删除是前台级联，由 kube-controller-manager 推进，kthena 不在也照样完成 |

### 3.10 状态机读得出来

| | |
| --- | --- |
| 今天 | 状态分散在内存的几张表里，转换写在回调和 worker 的分支里 |
| 本设计 | 四层状态，每个状态有只用对象字段写的判定条件；每条转换有谁推动、条件、写和前置条件；两个 controller 各一条完整的 reconcile 线（03） |

## 4. 代价

| 代价 | 内容 | 怎样控制 |
| --- | --- | --- |
| 多一个 CRD | API 面、RBAC、生成的 client、文档、chart；一个 Go 类型要改名 | — |
| 多一个 controller 和两者之间的约定 | 组的就绪要经过组的 `status` 才到 ModelServing controller，晚一跳。这段时间里滚动可能多删健康的组，缩容可能选错对象（界见 04 第 8 节） | 约定写成六条（02 第 3 节）。晚一跳的窗口模型里可达，列为已知的局限 |
| `status` 当输入 | 约定里 `status` 应当能从观察重建，账本不能 | 零值是不会引起删除的状态；`status` 端点用 RBAC 保护 |
| 组的创建不幂等 | 随机名字没有「撞名字」这道挡板 | 在途登记、在途屏障、接管屏障。单进程下不多建；两个进程并存时会多建，事后缩回 |
| partition 的语义变了 | 组一级按数量，不能再指定哪几个组留在旧版本；和 Role 一级的按序号并存 | 一处对外的变化，要批准 |
| 名字变了 | 组名和 Pod 名的前缀不再是序号；每次替换、每次整组恢复都换名字 | 对外的变化（[07](07-user-and-ops-guide.md)） |
| 部署前提 | 选主打开，失主退出 | 今天默认不开，要改 |
| plugin | 整个组被删时 `OnRoleDelete` 不保证调到 | plugin 的资源挂在 ServingGroup 名下，靠垃圾回收 |
| 稳态下的写 | 每次就绪变化从 1 次 status 写变成 2 次；每个实例建成时多一次；对象多 N 个 | 没有测量 |
| 一次升级 | 存量负载要接管；新旧进程不能重叠；回退要走受控的步骤 | [06](06-live-upgrade.md) |
| 行为变化 | 契约 2.12 有 14 条在字面上被碰到 | 04 第 11 节，逐条等批准 |

Role 一级和原地更新没有从这个设计里受益：它们的规则原样搬进新的 controller。

## 5. 社区的先例

| 做法 | 先例 | 来源 |
| --- | --- | --- |
| 父对象管个数和版本，子对象管自己里面的事，通过 `status` 交接 | Deployment 和 ReplicaSet | [`deployment/sync.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controller/deployment/sync.go) |
| 两个额度的滚动算式：未就绪的旧副本先清，健康的受可用数限制 | Deployment 的 `reconcileOldReplicaSets`、`cleanupUnhealthyReplicas` | [`deployment/rolling.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controller/deployment/rolling.go) |
| 按数量的 partition：留在旧版本的副本的个数；扩容时旧版本不够的部分按旧版本建 | OpenKruise CloneSet；Kruise Rollout 把它和 Deployment 的算式放在一起 | [`cloneset_sync_utils.go`](https://github.com/openkruise/kruise/blob/859efa1c218f55bf511f919eb84f52bc43b89f42/pkg/controller/cloneset/sync/cloneset_sync_utils.go#L99-L282)、[`rollouts` 的 `rolling.go`](https://github.com/openkruise/rollouts/blob/5c9d391269d282ab82844ffcdcd6258614a75bc7/pkg/controller/deployment/rolling.go#L246-L279) |
| 去掉组的序号 | LeaderWorkerSet 的 `groupIdentity: Hash`。它去掉序号之后直接拒绝 `partition` | [KEP-898](https://github.com/kubernetes-sigs/lws/blob/afba24af4480bdabc2c96c449245e0aa7d152c4b/keps/898-group-identity/README.md) |
| 随机名字的子对象，重复了确定地选一个 | Cluster API 的 MachineDeployment | [`machinedeployment_sync.go`](https://github.com/kubernetes-sigs/cluster-api/blob/67ee2f32340593d8b0c70ddb216718cd6326564e/core/reconcilers/machinedeployment/machinedeployment_sync.go#L96-L112) |
| 做破坏性决定之前不走缓存读一次 | `ControllerRefManager` 收养之前的复核；Cluster API 的 `APIReader` | [`controller_ref_manager.go`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controller/controller_ref_manager.go)、[kubernetes#42639](https://github.com/kubernetes/kubernetes/issues/42639) |
| `status` 里放不能从观察重建的状态 | Job 的 `uncountedTerminatedPods`（KEP-2307） | 007 的调研报告 |
| 按 UID 原位替换 owner | Cluster API 的 `ReplaceOwnerRef`，用在 Secret 上 | [`util.go`](https://github.com/kubernetes-sigs/cluster-api/blob/67ee2f32340593d8b0c70ddb216718cd6326564e/util/util.go#L331-L348) |
| 存量对象保留旧名字和旧的版本身份，直到自然重建 | Strimzi 的节点池；LWS 的 KEP-238 | 调研报告第 2 节 |

和先例不同的地方：

- **滚动途中改副本数。**Deployment 在新旧两边按比例分摊。这里先把规模做完再继续滚动，与契约、CloneSet、Kruise Rollout 相同。
- **缩容的次序。**CloneSet 先按版本分桶，桶里才看就绪。这里未就绪的排在过期的前面，是契约的规定（SG-C02）。
- **deletion cost 汇总到组一级。**没有找到先例；LWS 明确不做。
- **给运行中的 Pod 换 controller owner。**上游的 `ControllerRefManager` 对已经有别的 controller owner 的对象一律不碰。读到的就地迁移（Kubernetes 文档、PingCAP、Strimzi）都是先让旧 owner 以 orphan 方式删除、再由新 owner 收养，中间有一段 Pod 没有 controller owner。这里一次 PATCH 原位替换，没有那一段，但「两个 controller 不打架」不再由对象上的状态保证，完全靠独占保证（06 第 4 节）。
- **同类项目在类似的变化上多数选择不迁移存量。**RBG 重建 Pod；LWS 要求删除重建；Job 的 KEP-2307 让存量的 Job 留在旧机制上。这里要求不断服，目标比多数先例高。
- **一个带不可逆失败状态的组 CR、由父对象换名替换**：007 和这一次都没有找到先例。
