# 行为预期表：过程表

本文件由任务目录的 `model/run_scenarios.py` 生成，不要手工修改。它是 [04](04-behavior-expectations.md) 的附表：把契约 2.12 里涉及组一级的场景，在本设计的规则上逐个重放一遍得到的过程表。

读法：

- 列是序号，和契约的过程表是同一种列：`sg-0`、`sg-1`……组的名字是 `<ModelServing 名>-<序号>`。「无」表示这个序号上没有组对象。
- 同一个序号上先后可以是两个对象（删掉、等它消失、再建）：名字相同，UID 不同。「保住身份的初始组」只算对象没有换过的，记作 `g<i>`，i 是序号。
- 格子里的「（临时 surge）」表示这个组带 surge 标记；「（受保护）」表示它的序号小于 partition，不因为目标版本变了而替换。
- 状态：「未观测」指组的 `status` 还没有跟上刚下发的 `spec`，或者对象刚找回来、名下的 Pod 还没有被收养；「空 Ready」指各 Role 的副本数都是 0 的组，它没有 Pod，Ready 为真，计入就绪数（「空 NotReady」是刚建出来、`status` 还没写的空组）；「没有 Pod」指对象建了而 Pod 还没建出来；「还没写过 `status`，没有 Pod」指对象刚建出来、ServingGroup controller 还没有处理它（未观测，`status.pods` 是 0），它同样是没有成员的组；「Ready（Pod 已坏，status 没更新）」「NotReady（Pod 已就绪，status 没更新）」指 `status` 落后于 Pod（不新鲜），这样的组按未观测算；「无（留有无主的 Pod，Pod 缓存里还没有）」指对象被 orphan 方式删了，而 ModelServing controller 的 Pod 缓存还没看到这些 Pod 没有 owner；「（名下有收养不了的无主 Pod）」「无（留有收养不了的无主 Pod）」指这个序号上有带标签、没有 owner、名字对不上任何期望成员的 Pod（例如 `kubectl debug --copy-to --keep-labels` 的副本），它不算无主的成员 Pod。
- 一行是一个阶段。格子里用 `→` 连起这个阶段里先后出现的状态。「活动」是全部组对象的个数，包括删除中的和失败的；外来的同名对象不算。
- 「期望」一列是这个阶段的 N、U、S、P（百分比已经解析）、目标版本和基线。
- 每个场景后面的小表把本设计的可观察量和契约的过程表读出来的可观察量并排。结论由前五行得出：全部相同是「一致」，有一项不同是「不一致」。第六行「终态逐序号的版本」对照的是契约过程表的最后一行。
- 小表只比计数和终态，不比过程中的停点。停点见 [05](05-inference-verification.md) 里 runner 用例的重放：35 个用例的每个检查点（逐序号的版本、就绪、UID 是否保住）。
- 一行里并排发出的几个删除不分先后。SG-C15 的最后三个旧组，契约的表是一次删一个，这里是先两个、再一个：U=2 时契约自己的预算允许一次两个。
- 第 2 节里标了「不是定稿的读法」的场景是同一句规则被否掉的那种读法，并排给出来对照；标了「反例」的场景（现在没有）是定下来的规则在这个输入上的实际结果，不是期望的行为。

## 1. 契约 2.12 的场景

### SG-S01　单独缩容，再两次扩容

前提：sg-1、sg-2 的 deletion cost 低。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=4 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 4；Ready 4 |  |
| N 改成 2 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready→v1 删除中 | v1 Ready→v1 删除中 | v1 Ready | 活动 4；Ready 2～4，结束时 2 | 缩容选 deletion cost 低的；不占滚动预算 |
| 缩容完成 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 删除中→无 | v1 删除中→无 | v1 Ready | 活动 2～4；Ready 2 | 留下稀疏的 {0, 3}；不为排齐序号动 sg-3 |
| 建 sg-1 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 无→v1 NotReady→v1 Ready | 无 | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 只多一个名额：补最低的空洞 sg-1，空洞 2 留着 |
| 建 sg-2 v1 | N=4 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无→v1 NotReady→v1 Ready | v1 Ready | 活动 3～4；Ready 3～4，结束时 4 | 再补 sg-2；sg-3 原地进入正式范围，对象没有动 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0、g3 | g0、g3 |
| 终态各版本的组数 | v1×4 | v1×4 |
| 终态就绪数 | 4 | 4 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1 |

### SG-S02　两个旧版本的组滚到 v2

前提：之前的缩容留下稀疏的 {sg-0, sg-3}。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | 无 | v1 Ready | 活动 2；Ready 2 |  |
| 删 sg-3（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | 无→v2 NotReady→v2 Ready | v1 Ready→v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | 序号高的 sg-3 先；它在正式范围之外，替换的容量落到最低的空洞 sg-1 |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无 | 活动 1～2；Ready 1～2，结束时 2 | sg-0 原位重建 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 无 | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2 | sg-0 v2、sg-1 v2 |

### SG-S03　健康的目标版本的组不动

| 阶段 | 期望 | sg-0 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | 活动 2；Ready 2 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | 只替换 sg-0，原位重建；空洞 1 留着，不动健康的 sg-3 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g3 | g3 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |
| 终态逐序号的版本 | sg-0 v2、sg-3 v2 | sg-0 v2、sg-3 v2 |

### SG-S04　滚动中扩容，surge 的序号仍在正式范围之外

前提：sg-3 是缩容留下的高序号正式组，sg-4 是临时 surge。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | 无 | v2 Ready | v2 Ready（临时 surge） | 活动 4；Ready 4 |  |
| 建 sg-2 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | 无→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 | sg-4 的序号不小于 N，不转正；正式的组只有 3 个，在最低的空洞补 sg-2 |
| 删 sg-1（替换）；建 sg-1 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 | 借 sg-4 的容量滚 sg-1 |
| 删 sg-0（替换）；建 sg-0 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 | 再滚 sg-0 |
| 删 sg-4（回收 surge）；基线推进到 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 4～5；Ready 4～5，结束时 4 | 没有要滚的了：回收 sg-4 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | g3 | g3 |
| 终态各版本的组数 | v2×4 | v2×4 |
| 终态就绪数 | 4 | 4 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2 |

### SG-S05　缩容、扩容并提交坏的 v2，再用 v3 修复

前提：sg-1 的 deletion cost 低；v2 不会就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| N 改成 2 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready→v1 删除中→无 | v1 Ready | 活动 2～3；Ready 2～3，结束时 2 | 按 deletion cost 删 sg-1，留下 {0, 2} |
| 建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | 无→v2 NotReady | v1 Ready | 活动 2～3；Ready 2 | 新增的名额补最低的空洞 sg-1，用 v2；它不会就绪，占住唯一的不可用额度 |
| 删 sg-1（替换未就绪的）；建 sg-1 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v2 NotReady→v2 删除中→v3 NotReady→v3 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 坏的 v2 现在是可替换的、未就绪的，先处理它，原位补 v3 |
| 删 sg-2（替换）；建 sg-2 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v3 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 健康的旧组按序号从高到低 |
| 删 sg-0（替换）；建 sg-0 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v3 | v3 Ready | v3 Ready | v3 Ready | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v3、sg-1 v3、sg-2 v3 | sg-0 v3、sg-1 v3、sg-2 v3 |

### SG-S06　两个组滚到坏的 v2，再用 v3 修复

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=2 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 5 |  |
| 删 sg-4（替换）；删 sg-3（替换）；建 sg-3 v2；建 sg-4 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady | v1 Ready→v1 删除中→v2 NotReady | 活动 3～5；Ready 3～5，结束时 3 | 一批两个：sg-4、sg-3；两个 v2 都不就绪，maxScaleDown=0 |
| 阻塞 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 NotReady | v2 NotReady | 活动 5；Ready 3 | 不删健康的旧组，也不重建同版本的 v2 |
| 删 sg-4（替换未就绪的）；删 sg-3（替换未就绪的）；建 sg-3 v3；建 sg-4 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 NotReady→v2 删除中→v3 NotReady→v3 Ready | v2 NotReady→v2 删除中→v3 NotReady→v3 Ready | 活动 3～5；Ready 3～5，结束时 5 | 坏的 v2 变成可替换的，先清掉 |
| 删 sg-2（替换）；删 sg-1（替换）；建 sg-1 v3；建 sg-2 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | 活动 3～5；Ready 3～5，结束时 5 |  |
| 删 sg-0（替换）；建 sg-0 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v1 | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | v3 Ready | v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v3 | v3 Ready | v3 Ready | v3 Ready | v3 Ready | v3 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 7 | 7 |
| 删除的组 | 7 | 7 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×5 | v3×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v3、sg-1 v3、sg-2 v3、sg-3 v3、sg-4 v3 | sg-0 v3、sg-1 v3、sg-2 v3、sg-3 v3、sg-4 v3 |

### SG-S07　五个组全部未就绪，提交修正版本

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | v1 NotReady | v1 NotReady | v1 NotReady | v1 NotReady | v1 NotReady | 活动 5；Ready 0 | 故障早于滚动 |
| 删 sg-4（替换未就绪的）；删 sg-3（替换未就绪的）；建 sg-3 v2；建 sg-4 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | v1 NotReady | v1 NotReady | v1 NotReady | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | 活动 3～5；Ready 0～2，结束时 2 | maxScaleDown=2：一次只替换两个 |
| 删 sg-2（替换未就绪的）；删 sg-1（替换未就绪的）；建 sg-1 v2；建 sg-2 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | v1 NotReady | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 3～5；Ready 2～4，结束时 4 |  |
| 删 sg-0（替换未就绪的）；建 sg-0 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-C01　滚动中扩容，S=0

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 开始滚动 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | 无 | 无 | 活动 3；Ready 2～3，结束时 2 | 序号最高的 sg-2 的删除已经发出 |
| 建 sg-3 v2；建 sg-4 v2；建 sg-2 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 删除中→v2 NotReady→v2 Ready | 无→v2 NotReady→v2 Ready | 无→v2 NotReady→v2 Ready | 活动 3～5；Ready 2～5，结束时 5 | 已发的删除不撤销；扩容的 sg-3、sg-4 直接用 v2，sg-2 消失之后原位补 v2 |
| 删 sg-1（替换）；建 sg-1 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 | 新组全部就绪之后才有健康删除的额度 |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-C02　滚动中缩容

前提：sg-2 是在建的 v2，不会就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady | 活动 3；Ready 2 |  |
| N 改成 2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady→v2 删除中 | 活动 3；Ready 2 | 缩容先选未就绪的 sg-2；它消失之前不滚健康的组 |
| 缩容完成 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 删除中→无 | 活动 2～3；Ready 2 | 两个 v1 都在 |
| 删 sg-1（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | 活动 1～2；Ready 1～2，结束时 2 | 序号最高的 sg-1 先 |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无 | 活动 1～2；Ready 1～2，结束时 2 |  |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 无 | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2 | sg-0 v2、sg-1 v2 |

### SG-C03　滚动中扩容，surge 被扩容吸收

前提：sg-3 是临时 surge。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | sg-5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge） | 无 | 无 | 活动 4；Ready 4 |  |
| sg-3 转正；建 sg-4 v2；建 sg-5 v2（临时 surge） | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge）→v2 Ready | 无→v2 NotReady→v2 Ready | 无→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 4～6；Ready 4～6，结束时 6 | sg-3 的序号进了正式范围：转正，对象不变；sg-4 是新的正式组，sg-5 才占新的 surge 名额 |
| 删 sg-2（替换）；建 sg-2 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-5（回收 surge）；基线推进到 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 5～6；Ready 5～6，结束时 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | g3 | g3 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-C12　滚动中缩容，surge 的序号超出新范围

前提：sg-4 是临时 surge。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge） | 活动 5；Ready 5 |  |
| N 改成 3 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | v2 Ready（临时 surge） | 活动 5；Ready 4～5，结束时 4 | 缩容只在正式的组里选：删 sg-3，不是序号更高的 surge |
| 缩容完成 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 删除中→无 | v2 Ready（临时 surge） | 活动 4～5；Ready 4 | 活动数回到上限之内；sg-4 仍是临时 surge，对象没有动 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 | 借 sg-4 的容量滚 sg-2 |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无 | v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 无 | v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-4（回收 surge）；基线推进到 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 无 | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3～4，结束时 3 | 没有要滚的了：回收 sg-4 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2 | sg-0 v2、sg-1 v2、sg-2 v2 |

### SG-C15　百分比 U 随扩容重算

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | sg-5 | sg-6 | sg-7 | sg-8 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 Ready | 无 | 无 | 无 | 无 | 活动 5；Ready 5 | U=1，至少 4 个就绪 |
| N 改成 9 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 Ready | 无→v2 NotReady | 无→v2 NotReady | 无→v2 NotReady | 无→v2 NotReady | 活动 5～9；Ready 5 | U=2，至少 7 个就绪；新组用 v2 |
| 两个新组就绪 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 Ready | v2 NotReady→v2 Ready | v2 NotReady→v2 Ready | v2 NotReady | v2 NotReady | 活动 9；Ready 5～7，结束时 7 | 就绪 7，正好在底线：不删健康的旧组 |
| 第三个就绪 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 Ready | v2 Ready | v2 Ready | v2 NotReady→v2 Ready | v2 NotReady | 活动 9；Ready 7～8，结束时 8 | 就绪 8：有一个健康删除的额度 |
| 删 sg-3（替换）；建 sg-3 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 NotReady→v2 Ready | 活动 8～9；Ready 7～9，结束时 9 | sg-3 和还没就绪的 sg-8 共用 U=2 |
| 删 sg-2（替换）；删 sg-1（替换）；建 sg-1 v2；建 sg-2 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 7～9；Ready 7～9，结束时 9 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 8～9；Ready 8～9，结束时 9 |  |
| 基线推进到 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 9；Ready 9 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 8 | 8 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | g4 | g4 |
| 终态各版本的组数 | v2×9 | v2×9 |
| 终态就绪数 | 9 | 9 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2、sg-5 v2、sg-6 v2、sg-7 v2、sg-8 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2、sg-5 v2、sg-6 v2、sg-7 v2、sg-8 v2 |

### SG-C15（坏 v2 分支）　v2 永不就绪

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | sg-5 | sg-6 | sg-7 | sg-8 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 NotReady | 无 | 无 | 无 | 无 | 活动 5；Ready 4 |  |
| 建 sg-5 v2；建 sg-6 v2；建 sg-7 v2；建 sg-8 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 NotReady | 无→v2 NotReady | 无→v2 NotReady | 无→v2 NotReady | 无→v2 NotReady | 活动 5～9；Ready 4 | 新组也不就绪 |
| 阻塞 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 NotReady | v2 NotReady | v2 NotReady | v2 NotReady | v2 NotReady | 活动 9；Ready 4 | 就绪 4 低于底线 7：不再删健康的 v1 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2、g3、g4 | g0、g1、g2、g3、g4 |
| 终态各版本的组数 | v1×4、v2×5 | v1×4、v2×5 |
| 终态就绪数 | 4 | 4 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v2、sg-5 v2、sg-6 v2、sg-7 v2、sg-8 v2 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v2、sg-5 v2、sg-6 v2、sg-7 v2、sg-8 v2 |

### SG-C16　surge 和未就绪的新组同时入账

前提：sg-2 本来就未就绪；建出来的临时 surge sg-3 一直不就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 NotReady | 无 | 活动 3；Ready 2 | maxScaleDown=1 |
| 建 sg-3 v2（临时 surge）；删 sg-2（替换未就绪的）；建 sg-2 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | 无→v2 NotReady（临时 surge） | 活动 3～4；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 NotReady（临时 surge） | 活动 3～4；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2；删 sg-3（回收 surge） | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 NotReady（临时 surge）→v2 删除中（临时 surge） | 活动 3～4；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2 | sg-0 v2、sg-1 v2、sg-2 v2 |

### SG-C04　扩容途中开始 v2

前提：扩容建出的 sg-3（v1）一直不就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| N 改成 5 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无→v1 NotReady | 无 | 活动 3～4；Ready 3 | 只发出了一个创建 |
| v2 到来 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 NotReady | 无→v2 NotReady | 活动 4～5；Ready 3 | 还缺的 sg-4 直接用 v2；在建的 sg-3 不改 |
| 就绪 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 NotReady | v2 NotReady→v2 Ready | 活动 5；Ready 3～4，结束时 4 | sg-4 就绪之后，未就绪的旧组 sg-3 先替换，不降低就绪数 |
| 删 sg-3（替换未就绪的）；建 sg-3 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 6 | 6 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-C05　缩容途中开始 v2

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 5 |  |
| N 改成 3 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | 活动 5；Ready 4～5，结束时 4 | 只发出了一个删除 |
| v2 到来 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | v1 删除中 | 活动 5；Ready 3～4，结束时 3 | 缩容先做完：再删 sg-3。被缩掉的组消失之前不开始替换 |
| 规模完成 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 删除中→无 | v1 删除中→无 | 活动 3～5；Ready 3 | 三个 v1 都在 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 无 | 无 | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2 | sg-0 v2、sg-1 v2、sg-2 v2 |

### SG-C06　同一请求扩容并更新

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 建 sg-3 v2；建 sg-4 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无→v2 NotReady→v2 Ready | 无→v2 NotReady→v2 Ready | 活动 3～5；Ready 3～5，结束时 5 | 先扩容，sg-3、sg-4 直接用 v2 |
| 删 sg-2（替换）；建 sg-2 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-C07　同一请求缩容并更新

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 5 |  |
| 同一请求 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | v1 Ready→v1 删除中 | 活动 5；Ready 3～5，结束时 3 | 先缩容；被缩掉的组消失之前不开始替换 |
| 规模完成 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 删除中→无 | v1 删除中→无 | 活动 3～5；Ready 3 | 三个 v1 都在 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 无 | 无 | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2 | sg-0 v2、sg-1 v2、sg-2 v2 |

### SG-C13　扩容中开始滚动，带 S

前提：扩容还差一个。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | sg-5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=0 S=1 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 4；Ready 4 |  |
| 建 sg-4 v2；建 sg-5 v2（临时 surge） | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 无→v2 NotReady→v2 Ready | 无→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 4～6；Ready 4～6，结束时 6 | sg-4 是正式的扩容位置，sg-5 才是按新的 N 申请的 surge |
| 删 sg-3（替换）；建 sg-3 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-5（回收 surge）；基线推进到 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 5～6；Ready 5～6，结束时 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 6 | 6 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-C08　v2 没滚完就提交 v3

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| v2 的下一步 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中 | v2 Ready | 活动 3；Ready 2～3，结束时 2 | sg-1 的删除已经发出 |
| 建 sg-1 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 删除中→v3 NotReady→v3 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 已发的删除不撤销；sg-1 原位直接用 v3 |
| 删 sg-2（替换）；建 sg-2 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v3 Ready | v2 Ready→v2 删除中→v3 NotReady→v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 健康的可替换组里序号最高的是 v2 的 sg-2 |
| 删 sg-0（替换）；建 sg-0 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v3 | v3 Ready | v3 Ready | v3 Ready | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v3、sg-1 v3、sg-2 v3 | sg-0 v3、sg-1 v3、sg-2 v3 |

### SG-C09　扩容中 v1 → v2 → v3

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| N 改成 5 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无→v1 NotReady→v1 Ready | 无 | 活动 3～4；Ready 3～4，结束时 4 | 只发出了一个创建 |
| v2 到来 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 无→v2 NotReady→v2 Ready | 活动 4～5；Ready 4～5，结束时 5 | 最后一个扩容位置 sg-4 用 v2 |
| 删 sg-4（替换）；建 sg-4 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 Ready→v2 删除中→v3 NotReady→v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-3（替换）；建 sg-3 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-2（替换）；建 sg-2 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-1（替换）；建 sg-1 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-0（替换）；建 sg-0 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | v3 Ready | v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v3 | v3 Ready | v3 Ready | v3 Ready | v3 Ready | v3 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 7 | 7 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×5 | v3×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v3、sg-1 v3、sg-2 v3、sg-3 v3、sg-4 v3 | sg-0 v3、sg-1 v3、sg-2 v3、sg-3 v3、sg-4 v3 |

### SG-C10　缩容中 v1 → v2 → v3

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 5 |  |
| N 改成 3 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | 活动 5；Ready 4～5，结束时 4 | 只发出了一个删除 |
| v2 到来 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→无 | v1 删除中→无 | 活动 3～5；Ready 3～4，结束时 3 | 先把缩容做完 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-2（替换）；建 sg-2 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v2 Ready→v2 删除中→v3 NotReady→v3 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v3 | v3 Ready | v3 Ready | v3 Ready | 无 | 无 | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 6 | 6 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v3、sg-1 v3、sg-2 v3 | sg-0 v3、sg-1 v3、sg-2 v3 |

### SG-C11　扩容中 v2 不就绪，回滚到 v1

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| N 改成 5 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无→v1 NotReady→v1 Ready | 无 | 活动 3～4；Ready 3～4，结束时 4 | 只发出了一个创建 |
| 坏的 v2 到来 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 无→v2 NotReady | 活动 4～5；Ready 4 | 最后一个位置 sg-4 用 v2，不就绪；不碰健康的 v1 |
| 删 sg-4（替换未就绪的）；建 sg-4 v1 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 NotReady→v2 删除中→v1 NotReady→v1 Ready | 活动 4～5；Ready 4～5，结束时 5 | sg-4 可替换并且未就绪，自动清掉，原位用 v1 重建 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×5 | v1×5 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v1 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v1 |

### SG-C14　v1 → v2 → v3，旧的 v2 surge 还在承担容量

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 活动 3；Ready 3 |  |
| v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 | 先建临时 surge sg-3 |
| 替换开始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中 | v2 Ready（临时 surge） | 活动 4；Ready 3～4，结束时 3 | sg-2 的删除已经发出 |
| 建 sg-2 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v1 删除中→v3 NotReady→v3 Ready | v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 | sg-2 原位直接用 v3；健康的 v2 surge 留到 v3 就绪 |
| 删 sg-3（替换）；建 sg-3 v3（临时 surge） | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready | v3 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→v3 NotReady（临时 surge）→v3 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 | v2 的 surge 是序号最高的健康候选：先换它，同一个序号上先删后建，仍是 surge |
| 删 sg-1（替换）；建 sg-1 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-0（替换）；建 sg-0 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | v3 Ready | v3 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-3（回收 surge）；基线推进到 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v3 | v3 Ready | v3 Ready | v3 Ready | v3 Ready（临时 surge）→v3 删除中（临时 surge）→无 | 活动 3～4；Ready 3～4，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v3、sg-1 v3、sg-2 v3 | sg-0 v3、sg-1 v3、sg-2 v3 |

### 基础过程　P=1，从序号最高的开始

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v1 Ready | 活动 3；Ready 3 | 受保护的是序号 0 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v2×2 | v1×1、v2×2 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v2、sg-2 v2 | sg-0 v1、sg-1 v2、sg-2 v2 |

### SG-P01　P=1 灰度后扩容

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 建 sg-3 v2；建 sg-4 v2 | N=5 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 无→v2 NotReady→v2 Ready | 无→v2 NotReady→v2 Ready | 活动 3～5；Ready 3～5，结束时 5 | 序号 3、4 不小于 P，用 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×1、v2×4 | v1×1、v2×4 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v1、sg-1 v2、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-P02　扩容并同时调整 P

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | sg-5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready（受保护） | 无 | 无 | 无 | 活动 3；Ready 3 |  |
| 建 sg-3 v1；建 sg-4 v1；建 sg-5 v2 | N=6 U=1 S=0 P=5 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | 无→v2 NotReady→v2 Ready | 活动 3～6；Ready 3～6，结束时 6 | 序号 3、4 小于 P，用基线 v1；序号 5 用 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×5、v2×1 | v1×5、v2×1 |
| 终态就绪数 | 6 | 6 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v1、sg-5 v2 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v1、sg-4 v1、sg-5 v2 |

### SG-P03（上半）　缩容穿过保护区

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 5；Ready 5 |  |
| 删 sg-4（缩容）；删 sg-3（缩容）；删 sg-2（缩容） | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready→v1 删除中→无 | v2 Ready→v2 删除中→无 | v2 Ready→v2 删除中→无 | 活动 2～5；Ready 2～5，结束时 2 | 同样健康、同样的 deletion cost：序号高的先删。partition 不改变缩容的次序 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 0 | 0 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | g0、g1 | g0、g1 |
| 终态各版本的组数 | v1×2 | v1×2 |
| 终态就绪数 | 2 | 2 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1 | sg-0 v1、sg-1 v1 |

### SG-P03（下半）　缩容先删未就绪的受保护组，再扩回

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 NotReady（受保护） | v2 Ready | 活动 3；Ready 2 |  |
| 删 sg-1（缩容） | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 NotReady（受保护）→v1 删除中（受保护）→无 | v2 Ready | 活动 2～3；Ready 2 | 未就绪的先，留下稀疏的 {0, 2} |
| 建 sg-1 v1 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 最低的空洞 sg-1 的序号小于 P：用基线 v1 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g2 | g0、g2 |
| 终态各版本的组数 | v1×2、v2×1 | v1×2、v2×1 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v2 | sg-0 v1、sg-1 v1、sg-2 v2 |

### SG-P04　百分比 P 因扩容变大

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v2 Ready | 无 | 无 | 活动 3；Ready 3 | P=2 |
| 建 sg-3 v2；建 sg-4 v2 | N=5 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v2 Ready（受保护） | 无→v2 NotReady→v2 Ready | 无→v2 NotReady→v2 Ready | 活动 3～5；Ready 3～5，结束时 5 | P=3：现存的 sg-2 留在 v2；新的序号 3、4 不小于 P，用 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×2、v2×3 | v1×2、v2×3 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v2、sg-3 v2、sg-4 v2 | sg-0 v1、sg-1 v1、sg-2 v2、sg-3 v2、sg-4 v2 |

### SG-P05　百分比 P 因缩容变小

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 5；Ready 5 | P=3 |
| N 改成 3 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready | v2 Ready→v2 删除中 | v2 Ready→v2 删除中 | 活动 5；Ready 3～5，结束时 3 | P=2。缩容先删序号高的 sg-4、sg-3；它们消失之前不滚 |
| 缩容完成 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready | v2 删除中→无 | v2 删除中→无 | 活动 3～5；Ready 3 | sg-2 的序号不小于新的 P，解除保护 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 | 只滚新解除保护的 sg-2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | g0、g1 | g0、g1 |
| 终态各版本的组数 | v1×2、v2×1 | v1×2、v2×1 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v2 | sg-0 v1、sg-1 v1、sg-2 v2 |

### SG-P06　在途时调大 P

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 在途 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 删除中 | v2 Ready | 活动 3；Ready 2 | sg-1 的删除已经发出 |
| 建 sg-1 v1 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 删除中（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v2 Ready（受保护） | 活动 2～3；Ready 2～3，结束时 3 | sg-1 的序号现在小于 P：原位用基线 v1 重建；现存的 sg-2 不回退 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g2 | g0、g2 |
| 终态各版本的组数 | v1×2、v2×1 | v1×2、v2×1 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v2 | sg-0 v1、sg-1 v1、sg-2 v2 |

### SG-P07　调小 P 的同时提交 v3

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v2 Ready | 活动 3；Ready 3 |  |
| 删 sg-2（替换）；建 sg-2 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | v1 Ready（受保护） | v1 Ready | v2 Ready→v2 删除中→v3 NotReady→v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号最高的可替换组是 v2 的 sg-2 |
| 删 sg-1（替换）；建 sg-1 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v3 NotReady→v3 Ready | v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | sg-1 解除保护，直接到 v3 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v3×2 | v1×1、v3×2 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v3、sg-2 v3 | sg-0 v1、sg-1 v3、sg-2 v3 |

### SG-P08　百分比 P、S 和扩容

前提：sg-3 是临时 surge，还没就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready | v2 NotReady（临时 surge） | 无 | 活动 4；Ready 3 | P=2；U=0，不删 sg-2 |
| sg-3 转正；建 sg-4 v2 | N=5 U=0 S=1 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready（受保护） | v2 NotReady（临时 surge）→v2 NotReady→v2 Ready | 无→v2 NotReady→v2 Ready | 活动 4～5；Ready 3～5，结束时 5 | P=3：三个 v1 都受保护，没有要滚的；sg-3 转正，sg-4 补正式的位置，不再申请 surge |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2、g3 | g0、g1、g2、g3 |
| 终态各版本的组数 | v1×3、v2×2 | v1×3、v2×2 |
| 终态就绪数 | 5 | 5 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v2、sg-4 v2 | sg-0 v1、sg-1 v1、sg-2 v1、sg-3 v2、sg-4 v2 |

### SG-P09　受保护的组故障也占预算

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 故障 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 NotReady（受保护） | v1 Ready | v1 Ready | 活动 3；Ready 2 | 受保护的 sg-0 未就绪 |
| 阻塞 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 NotReady（受保护） | v1 Ready | v1 Ready | 活动 3；Ready 2 | 就绪 2 正好在底线；sg-0 受保护，不是替换候选 |
| sg-0 恢复 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 NotReady（受保护）→v1 Ready（受保护） | v1 Ready | v1 Ready | 活动 3；Ready 2～3，结束时 3 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v2×2 | v1×1、v2×2 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v2、sg-2 v2 | sg-0 v1、sg-1 v2、sg-2 v2 |

### SG-P10　surge 被扩容吸收后继续灰度

前提：sg-3 是临时 surge。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v1 Ready | v2 Ready（临时 surge） | 无 | 活动 4；Ready 4 |  |
| sg-3 转正；建 sg-4 v2（临时 surge） | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v1 Ready | v2 Ready（临时 surge）→v2 Ready | 无→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 | sg-3 转正，对象不变；sg-4 占新的 surge 名额 |
| 删 sg-2（替换）；建 sg-2 v2 | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-4（回收 surge） | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 4～5；Ready 4～5，结束时 4 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | g0、g3 | g0、g3 |
| 终态各版本的组数 | v1×1、v2×3 | v1×1、v2×3 |
| 终态就绪数 | 4 | 4 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v2、sg-2 v2、sg-3 v2 | sg-0 v1、sg-1 v2、sg-2 v2、sg-3 v2 |

### SG-P11　P 等于 N 时提交 v2，再调小 P

前提：之前的缩容留下稀疏的 {sg-0, sg-3}。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | 无 | v1 Ready | 活动 2；Ready 2 |  |
| 删 sg-3（替换）；建 sg-1 v1 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | v1 Ready→v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | sg-3 的序号不小于 P，要替换；替换的容量落到最低的空洞 sg-1，它的序号小于 P，用基线 v1 |
| 灰度停点 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | 无 | 活动 2；Ready 2 | 两个 v1，都在 P 之内，没有要替换的 |
| 删 sg-1（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无 | 活动 1～2；Ready 1～2，结束时 2 | sg-1 解除保护 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v2×1 | v1×1、v2×1 |
| 终态就绪数 | 2 | 2 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v2 | sg-0 v1、sg-1 v2 |

### SG-R01　灰度中只改 Role 的副本数

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |
| Role 副本数 1 → 2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护）→v1 NotReady（受保护） | v2 Ready→v2 NotReady | v2 Ready→v2 NotReady | 活动 3；Ready 0～3，结束时 0 | `spec` 下发到三个组，各组自己建新成员；三个组可以同时未就绪。组一级没有动作 |
| 各组就绪 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 NotReady（受保护）→v1 Ready（受保护） | v2 NotReady→v2 Ready | v2 NotReady→v2 Ready | 活动 3；Ready 0～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 0 | 0 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×1、v2×2 | v1×1、v2×2 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v2、sg-2 v2 | sg-0 v1、sg-1 v2、sg-2 v2 |

### RC-01　整组恢复中断后重启

前提：SG 模式，`ServingGroupRecreate`。先提交 v2 并且 P=2（没有可更新的组）。sg-0 的一个成员 Failed。

| 阶段 | 期望 | sg-0 | sg-1 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | 活动 2；Ready 2 | 两个组都在 P 之内 |
| sg-0 的成员故障 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护）→v1 失败（受保护） | v1 Ready（受保护） | 活动 2；Ready 1～2，结束时 1 | ServingGroup controller 写上 Failed，删故障的成员 |
| controller 停着，P 改成 1 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | v1 失败（受保护） | v1 Ready | 活动 2；Ready 1 | 对象上留着的是：sg-0 Failed、还没清空 |
| 新进程 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | v1 失败（受保护）→v1 删除中（受保护） | v1 Ready | 活动 2；Ready 1 | 读到 Failed 和 Drained：删 sg-0。不需要知道是谁开始的。就绪 1 正好在底线，不动 sg-1 |
| sg-0 消失 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | v1 删除中（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 Ready | 活动 1～2；Ready 1～2，结束时 2 | 原序号重建：序号 0 小于 P=1，用基线 v1 |
| 删 sg-1（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | sg-0 就绪之后才滚序号不小于 P 的 sg-1 |
| 停点 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | 活动 2；Ready 2 | 1 个 v1、1 个 v2 |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | sg-0 解除保护，再滚一次 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2 | sg-0 v2、sg-1 v2 |

### EX-1　受保护的组是中间版本

前提：上一轮 v2 因为 sg-2 没就绪而没有完成，基线还是 v1；现在提交 v3。契约 0.5、A.3 第 2 条：现存的受保护组不论版本都不动。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | v2 Ready（受保护） | v2 Ready（受保护） | v2 NotReady | 活动 3；Ready 2 |  |
| 删 sg-2（替换未就绪的）；建 sg-2 v3 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | v2 Ready（受保护） | v2 Ready（受保护） | v2 NotReady→v2 删除中→v3 NotReady→v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号小于 P 的两个组不动，不问它是不是基线版本；只换 sg-2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g1 | g0、g1 |
| 终态各版本的组数 | v2×2、v3×1 | v2×2、v3×1 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v3 | sg-0 v2、sg-1 v2、sg-2 v3 |

### EX-2　有 surge 时调大 P，没有可更新的组了

前提：sg-3 是临时 surge；sg-0 的 deletion cost 最低。契约 A.1：灰度停点上没有可更新的组时，清理不再需要的临时 surge。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge） | 活动 4；Ready 4 |  |
| 删 sg-3（回收 surge） | N=3 U=0 S=1 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | v1 Ready（受保护） | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3～4，结束时 3 | 三个 v1 都受保护，没有要滚的：回收 sg-3。回收 surge 不看 deletion cost，不碰正式的组 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 0 | 0 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×3 | v1×3 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v1、sg-2 v1 | sg-0 v1、sg-1 v1、sg-2 v1 |

### EX-3　`spec` 刚下发、组还没有观测到时提交新版本

前提：sg-2 真的未就绪。用户先改 `recoveryPolicy`（`spec` 下发，四个组短暂未观测），紧接着提交 v2。契约 0.4：先处理合法的旧 NotReady。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=4 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 NotReady | v1 Ready | 活动 4；Ready 3 |  |
| 四个组都未观测 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | v1 未观测 | v1 未观测 | v1 未观测 | v1 未观测 | 活动 4；Ready 0 | 未观测的组占位置、不算就绪，但也不是替换候选：这一轮不动 |
| 删 sg-2（替换未就绪的）；建 sg-2 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v1 Ready | 活动 3～4；Ready 3～4，结束时 4 | 观测回来：先替换真的未就绪的 sg-2 |
| 删 sg-3（替换）；建 sg-3 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 基线推进到 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4；Ready 4 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×4 | v2×4 |
| 终态就绪数 | 4 | 4 |
| 终态逐序号的版本 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2 | sg-0 v2、sg-1 v2、sg-2 v2、sg-3 v2 |

### EX-4　灰度停点上提交第三个版本

前提：灰度停点 {sg-0 v1 受保护, sg-1 v2, sg-2 v2}。sg-0 之前被整组恢复过：名字没变，UID 是新的，规则不看它是什么时候建的。现在提交 v3，P 仍是 1。契约 0.5：中间版本不会成为受保护的基线。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |
| 删 sg-2（替换）；建 sg-2 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready→v2 删除中→v3 NotReady→v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 受保护的是序号 0，留在 v1；两个 v2 按序号从高到低换成 v3 |
| 删 sg-1（替换）；建 sg-1 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | v1 Ready（受保护） | v2 Ready→v2 删除中→v3 NotReady→v3 Ready | v3 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v3×2 | v1×1、v3×2 |
| 终态就绪数 | 3 | 3 |
| 终态逐序号的版本 | sg-0 v1、sg-1 v3、sg-2 v3 | sg-0 v1、sg-1 v3、sg-2 v3 |

## 2. 契约里没有过程表的场景

### NEW-1　整组恢复：失败的组在原序号上重建

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| sg-1 丢了成员 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready→v1 失败 | v1 Ready | 活动 3；Ready 2～3，结束时 2 | ServingGroup controller 写上失败；它先只删故障的成员。失败的组仍占它的序号和一个位置 |
| 故障成员消失 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 失败→v1 删除中 | v1 Ready | 活动 3；Ready 2 | ModelServing controller 这时才删 sg-1（前台级联） |
| sg-1 的 Pod 全部消失 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 删除中→v1 NotReady | v1 Ready | 活动 2～3；Ready 2 | 这时才在原序号上重建：名字不变，对象是新的（UID 不同） |
| 就绪 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 NotReady→v1 Ready | v1 Ready | 活动 3；Ready 2～3，结束时 3 |  |


### NEW-2　灰度停点上一个 v2 的组失败

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |
| sg-2 失败并重建 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready→v2 失败→v2 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号 2 不小于 P：重建的对象用 v2 |


### NEW-3　灰度停点上受保护的组失败

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |
| sg-0 失败并重建 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护）→v1 失败（受保护）→v1 删除中（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号 0 小于 P：重建的对象用基线 v1。保护的是序号 |


### NEW-5　滚动中一个旧组失败

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| sg-0 失败 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 失败→v1 删除中 | v1 Ready | v1 Ready | 活动 3；Ready 2～3，结束时 2 | 失败的组被删；同一轮里就绪 2 正好在底线，不再删健康的旧组 |
| 建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 删除中→v2 NotReady→v2 Ready | v1 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | sg-0 消失之后原位重建，序号 0 不小于 P=0：直接用 v2 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |


### NEW-6　空组：各 Role 的副本数都是 0 时提交新版本，再调回来

前提：SG 模式。用户把每个 Role 的副本数调成 0（组对象还在，名下没有 Pod），提交 v2，P=1，再把副本数调回来。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=1 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 活动 3；Ready 3 | 组对象留着，名下没有 Pod。空组的 Ready 为真，与今天相同（V-36）：就绪数仍是 3 |
| 提交 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 空 Ready（受保护） | v1 空 Ready→v2 空 Ready | v1 空 Ready→v2 空 Ready | 活动 3；Ready 3 | 没有成员的组不是「可替换」的：G2′(b) 把版本改成按序号现在新建会用的版本，序号 0 小于 P 留在基线，序号 1、2 改成 v2。不删对象，不占预算，不建 surge |
| 副本数调回来 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v2 空 Ready→v2 NotReady→v2 Ready | v2 空 Ready→v2 NotReady→v2 Ready | 活动 3；Ready 0～3，结束时 3 | 各组按自己的版本建 Pod：sg-0 是 v1，sg-1、sg-2 是 v2。没有哪个组先起旧版本再被替换 |


### NEW-9　空组：P 调大之后，目标版本的空组回到基线

前提：灰度停点。用户把每个 Role 的副本数调成 0，把 P 调到 2，再把副本数调回来。契约 0.6：扩回来视为新建，保护区按基线。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护）→v1 空 Ready（受保护） | v2 Ready→v2 空 Ready | v2 Ready→v2 空 Ready | 活动 3；Ready 3 |  |
| P 调到 2 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 空 Ready（受保护） | v2 空 Ready（受保护）→v1 空 Ready（受保护） | v2 空 Ready | 活动 3；Ready 3 | 序号 1 现在小于 P：把 sg-1 改回基线 v1。有 Pod 的目标组不会这样回退；空组没有 Pod，等于还没建 |
| 副本数调回来 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v2 空 Ready→v2 NotReady→v2 Ready | 活动 3；Ready 0～3，结束时 3 | sg-0、sg-1 是 v1，sg-2 是 v2 |


### NEW-10　空组：受保护的是中间版本时回到基线

前提：EX-1 的形状：基线还是 v1，三个组都是 v2，目标 v3，P=2。用户把每个 Role 的副本数调成 0，再调回来。契约 0.6：保护区按基线新建。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | v2 Ready（受保护） | v2 Ready（受保护） | v2 Ready | 活动 3；Ready 3 | 有 Pod 时，序号小于 P 的两个 v2 不动 |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | v2 Ready（受保护）→v2 空 Ready（受保护）→v1 空 Ready（受保护） | v2 Ready（受保护）→v2 空 Ready（受保护）→v1 空 Ready（受保护） | v2 Ready→v2 空 Ready→v3 空 Ready | 活动 3；Ready 3 | 空组按序号重新指派：sg-0、sg-1 回到基线 v1，sg-2 改成 v3 |
| 副本数调回来 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v3 空 Ready→v3 NotReady→v3 Ready | 活动 3；Ready 0～3，结束时 3 | 两个 v1，一个 v3 |


### NEW-24　空组不保存稀疏的序号：{0, 3}、P=N，全零时提交 v2

前提：提案分析 10.2 第 12 行的例子。之前的缩容留下 {sg-0, sg-3}，N=2，P=2。用户把每个 Role 的副本数调成 0，提交 v2，再调回来。契约 0.6：空组不保存旧的稀疏序号，扩回来视为新建：序号 0、1，都小于 P，两个 v1。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v1 基线 v1 | v1 Ready | 无 | v1 Ready | 活动 2；Ready 2 |  |
| Role 的副本数都调成 0 | N=2 U=1 S=0 P=2 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | 无 | v1 Ready→v1 空 Ready | 活动 2；Ready 2 |  |
| 提交 v2 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 空 Ready（受保护） | 无 | v1 空 Ready→v1 删除中 | 活动 2；Ready 1～2，结束时 1 | G2′(a)：sg-3 是空组、正式、序号不小于 N；没有别的删除在途，序号 1 空着、现在就按基线建得出来（V-62）：删掉，不占预算。sg-0 的序号小于 P，留在基线 |
| sg-3 消失 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 空 Ready（受保护） | 无→v1 空 NotReady（受保护）→v1 空 Ready（受保护） | v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | G4 在最低的空洞 sg-1 上补一个（也是空组）；序号 1 小于 P，用基线 v1 |
| 副本数调回来 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | 无 | 活动 2；Ready 0～2，结束时 2 | {0: v1, 1: v1}，与契约相同。不删的话终局也相同，只是晚一步、要动 Pod：留下的 sg-3 不改版本（V-55），副本数调回来之后是有成员的旧组，在预算之内被换到 sg-1 上 |


### NEW-47　空组不保存稀疏的序号：没有新版本时

前提：之前的缩容留下 {sg-0, sg-3}，N=2。用户把每个 Role 的副本数调成 0，再调回来，没有提交新版本。契约 0.6：空组不保存旧的稀疏序号。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 无 | v1 Ready | 活动 2；Ready 2 |  |
| Role 的副本数都调成 0 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | 无 | v1 Ready→v1 空 Ready→v1 删除中 | 活动 2；Ready 1～2，结束时 1 | G2′(a)：sg-3 是空组、没有成员、正式、序号不小于 N；序号 1 空着、建得出来：删掉，不占预算 |
| sg-3 消失 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 空 Ready | 无→v1 空 NotReady→v1 空 Ready | v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | G4 在最低的空洞 sg-1 上补一个空组 |
| 副本数调回来 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 空 Ready→v1 NotReady→v1 Ready | v1 空 Ready→v1 NotReady→v1 Ready | 无 | 活动 2；Ready 0～2，结束时 2 | {0, 1}。不删的话是 {0, 3}：没有新版本，留下的 sg-3 不会被替换，稀疏的序号一直留着 |


### NEW-35　范围外的空组只在低位现在建得出来时删：空洞被外来对象占着

前提：独立评审 IR4-13 的第一个反例。之前的缩容留下 {sg-0, sg-1, sg-3}，N=3。命名空间里有一个不属于这个 ModelServing 的 ServingGroup，名字正好是 `<ModelServing 名>-2`。用户把每个 Role 的副本数调成 0。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 外来的同名对象 | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 外来的同名对象 | v1 Ready→v1 空 Ready | 活动 3；Ready 3 | sg-3 是空组、正式、序号不小于 N。小于 N 的序号里只有 2 没有自己的组，而它被外来对象占着：没有空闲的低位，不删（V-45、V-62）。三个组都留着，报告序号被占。外来对象消失之后才删 sg-3、补 sg-2 |


### NEW-36　范围外的空组只在低位现在建得出来时删：要用的基线读不到。等着的时候不改版本

前提：独立评审 IR4-13 的第二个反例（IR4-27 的 A1）。之前的缩容留下 {sg-0, sg-1, sg-3}，N=3。用户把每个 Role 的副本数调成 0，提交 v2，P=3；这时基线版本读不到。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 活动 3；Ready 3 |  |
| 提交 v2，P=3，基线读不到 | N=3 U=1 S=0 P=3 目标 v2 基线 读不到 | v1 空 Ready（受保护） | v1 空 Ready（受保护） | v1 空 Ready | 活动 3；Ready 3 | 没有写。sg-3 是空组、正式、序号不小于 N。空着的序号 2 小于 P，要用基线建，基线读不到：现在建不出来，不删（V-45、V-62）；它在等，G2′(b) 不改它的版本（V-55）。sg-0、sg-1 的序号小于 P，基线读不到，也不改。基线读得到之后才删 sg-3、按基线补 sg-2（NEW-36c） |


### NEW-36b　接 NEW-36：基线读不到期间副本数调回来；基线读到之后，范围外的组在预算之内换到低位

前提：独立评审 IR4-27 的接续（A1 到 A3）。与 NEW-36 相同的开头：{sg-0, sg-1, sg-3}、N=3，副本数调成 0，提交 v2，P=3，基线读不到。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无 | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 无 | v1 Ready→v1 空 Ready | 活动 3；Ready 3 |  |
| 提交 v2，P=3，基线读不到 | N=3 U=1 S=0 P=3 目标 v2 基线 读不到 | v1 空 Ready（受保护） | v1 空 Ready（受保护） | 无 | v1 空 Ready | 活动 3；Ready 3 | A1：没有写（NEW-36）。sg-3 留下，版本不动 |
| 副本数调回来，基线仍然读不到 | N=3 U=1 S=0 P=3 目标 v2 基线 读不到 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | 无 | v1 空 Ready→v1 NotReady→v1 Ready | 活动 3；Ready 0～3，结束时 3 | A2：没有写。三个组各按自己的版本 v1 起 Pod。sg-3 是有成员的旧组，序号不小于 P，是可替换的；删掉它之后替代的容量要建在序号 2 上、要用基线，读不到：不替换（V-54），报告基线读不到 |
| 删 sg-3（替换）；建 sg-2 v1 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | v1 Ready→v1 删除中→无 | 活动 2～3；Ready 2～3，结束时 3 | A3：基线读到了。sg-3 在预算之内被替换；它消失之后 G4 在最低的空洞 sg-2 上补，序号 2 小于 P，按基线 v1 建。终局 {0, 1, 2} 全是 v1，与基线一直读得到时（NEW-36c）相同 |


### NEW-36c　对照 NEW-36b：基线一直读得到时，范围外的空组被删掉，在低位按基线补

前提：独立评审 IR4-27 的 A6。同样的 {sg-0, sg-1, sg-3}、N=3，副本数调成 0，提交 v2，P=3；基线读得到。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无 | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 无 | v1 Ready→v1 空 Ready | 活动 3；Ready 3 |  |
| 提交 v2，P=3 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 空 Ready（受保护） | v1 空 Ready（受保护） | 无 | v1 空 Ready→v1 删除中 | 活动 3；Ready 2～3，结束时 2 | G2′(a)：没有别的删除在途，序号 2 空着、现在就按基线建得出来：删 sg-3，不占预算。三个空组的版本都不用改 |
| sg-3 消失 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 空 Ready（受保护） | v1 空 Ready（受保护） | 无→v1 空 NotReady（受保护）→v1 空 Ready（受保护） | v1 删除中→无 | 活动 2～3；Ready 2～3，结束时 3 | G4 在 sg-2 上补一个空组；序号 2 小于 P，按基线 v1 |
| 副本数调回来 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | 无 | 活动 3；Ready 0～3，结束时 3 | 终局 {0, 1, 2} 全是 v1 |


### NEW-42　替换之前看补不补得上：替代的容量要用的基线读不到

前提：独立评审 IR4-27 的第一个反例。之前的缩容留下 {sg-0, sg-3}，N=2、U=1、P=2。用户提交 v2；这时基线版本读不到。

| 阶段 | 期望 | sg-0 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v1 基线 v1 | v1 Ready | v1 Ready | 活动 2；Ready 2 |  |
| 提交 v2，基线读不到 | N=2 U=1 S=0 P=2 目标 v2 基线 读不到 | v1 Ready（受保护） | v1 Ready | 活动 2；Ready 2 | sg-3 的序号不小于 P，是可替换的，额度也够（U=1）。删掉它之后小于 N 的最低空闲序号是 1，小于 P，要按基线建，基线读不到：补不上，不替换（V-54），报告基线读不到。sg-0 受保护。两个组都留着，等基线读得到；读到之后 sg-3 在预算之内被替换，替代的容量在 sg-1 上按基线建（同 NEW-36b 的 A3）。不看补不补得上的话 sg-3 现在就被删掉，基线读到之前只有一个组 |


### NEW-43　替换之前看补不补得上：小于 N 的序号被外来对象占着

前提：独立评审 IR4-27 的第二个反例。之前的缩容留下 {sg-0, sg-3}，N=2、U=1、P=0。命名空间里有一个不属于这个 ModelServing 的 ServingGroup，名字正好是 `<ModelServing 名>-1`。用户提交 v2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象 | v1 Ready | 活动 2；Ready 2 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 外来的同名对象 | v1 Ready | 活动 1～2；Ready 1～2，结束时 2 | 先看序号高的 sg-3：删掉它之后，小于 N 的序号里 0 是 sg-0 的、1 被外来对象占着，没有空闲的：不替换（V-54），报告序号被占。sg-0 照常替换：它自己的序号腾出来就是可建的，在原序号上重建成 v2 |
| 停住 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | v1 Ready | 活动 2；Ready 2 | {0: v2, 3: v1}，两个组都在；滚动停在 sg-3 上，等外来的对象消失。不看补不补得上的话先删的是 sg-3，补不上，只剩一个组 |
| 外来的对象被它的主人删掉 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象→无 | v1 Ready | 活动 2；Ready 2 |  |
| 删 sg-3（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 无→v2 NotReady→v2 Ready | v1 Ready→v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | 序号 1 空闲了：sg-3 被替换，替代的容量建在 sg-1 上 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 无 | 活动 2；Ready 2 | 全部是 v2，全部就绪：基线推进 |


### NEW-41　带标记的空组：不算有滚动要做的理由，过了下限才回收

前提：滚动刚开始：surge sg-3 已经就绪，还没有替换任何旧组（U=0，下限 3）。这时用户把每个 Role 的副本数调成 0。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge） | 活动 4；Ready 4 |  |
| Role 的副本数都调成 0 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 空 Ready→v2 空 Ready | v1 Ready→v1 空 Ready→v2 空 Ready | v1 Ready→v1 空 Ready→v2 空 Ready | v2 Ready（临时 surge）→v2 空 Ready（临时 surge）→v2 删除中（临时 surge） | 活动 4；Ready 3～4，结束时 3 | 四个空组都是就绪的（V-36）。G2′(b) 把三个正式的空组改成 v2；没有成员的组不可替换，没有滚动要做。sg-3 是就绪的 surge 组：删掉它确认就绪还剩 3，不低于下限，回收 |
| sg-3 消失 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v2 空 Ready | v2 空 Ready | v2 空 Ready | v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3 | 全部是目标版本、全部确认就绪、P=0，但都是空组：G6 不推进基线 |
| 副本数调回来 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 空 Ready→v2 NotReady→v2 Ready | v2 空 Ready→v2 NotReady→v2 Ready | v2 空 Ready→v2 NotReady→v2 Ready | 无 | 活动 3；Ready 0～3，结束时 3 | 三个组直接按 v2 起 Pod。就绪之后没有空组了，基线推进到 v2 |


### NEW-40　空组算就绪，但有空组时不推进基线

前提：契约 0.6：全零的计数不能让基线提前推进。P=0。用户把每个 Role 的副本数调成 0，提交 v2，再把 P 调到 2，再把副本数调回来。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 活动 3；Ready 3 | 三个空组都是就绪的（V-36） |
| 提交 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 空 Ready→v2 空 Ready | v1 空 Ready→v2 空 Ready | v1 空 Ready→v2 空 Ready | 活动 3；Ready 3 | G2′(b) 把三个空组都改成 v2。全部是目标版本、全部确认就绪、P=0，但它们是空组：G6 不推进基线，基线仍是 v1 |
| P 调到 2 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v2 空 Ready（受保护）→v1 空 Ready（受保护） | v2 空 Ready（受保护）→v1 空 Ready（受保护） | v2 空 Ready | 活动 3；Ready 3 | 序号 0、1 小于 P：改回基线 v1。基线要是已经推进到 v2，这里就没有 v1 可回了 |
| 副本数调回来 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 空 Ready（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v2 空 Ready→v2 NotReady→v2 Ready | 活动 3；Ready 0～3，结束时 3 | {0: v1, 1: v1, 2: v2} |


### NEW-7　灰度还没滚完时受保护的组失败

前提：sg-1 还没轮到。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready | v2 Ready | 活动 3；Ready 3 |  |
| sg-0 失败并重建 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护）→v1 失败（受保护）→v1 删除中（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v1 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号 0 小于 P：按基线 v1 重建。它没就绪之前就绪数在底线，sg-1 等着 |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | sg-0 就绪之后，序号不小于 P 的 sg-1 滚成 v2 |


### NEW-8　P=N 的停点上把模板改回基线

前提：P=0 时滚了两个组，再把 P 调到 3 冻结现状。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready（受保护） | v2 Ready（受保护） | 活动 3；Ready 3 | 三个序号都小于 P |
| 模板改回 v1 | N=3 U=1 S=0 P=3 目标 v1 基线 v1 | v1 Ready | v2 Ready（受保护） | v2 Ready（受保护） | 活动 3；Ready 3 | 三个序号都受保护：sg-1、sg-2 留在 v2，没有动作 |
| 删 sg-2（替换）；建 sg-2 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v2 Ready | v2 Ready→v2 删除中→v1 NotReady→v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | P 调小之后才按预算换回 v1 |
| 删 sg-1（替换）；建 sg-1 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v2 Ready→v2 删除中→v1 NotReady→v1 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |


### NEW-11　滚动中带 surge 标记的组因为 N 调大而转正

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 开始滚动 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 无 | 活动 3～4；Ready 3～4，结束时 4 | U=0：先建临时 surge sg-3 |
| N 改成 4 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge）→v2 Ready | 无→v2 NotReady（临时 surge） | 活动 4～5；Ready 4 | sg-3 的序号进了正式范围，正式的组少于 N：摘掉标记，对象和 UID 不变。还有要滚的，另建临时的 sg-4 |
| 就绪 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready | v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 5；Ready 4～5，结束时 5 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-4（回收 surge）；基线推进到 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 4～5；Ready 4～5，结束时 4 |  |


### NEW-12　稀疏的 {0, 3} 扩容：补最低的空洞

前提：之前按 deletion cost 的缩容留下 {sg-0, sg-3}。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 无 | 无 | v1 Ready | 无 | 活动 2；Ready 2 |  |
| 建 sg-1 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 无→v1 NotReady→v1 Ready | 无 | v1 Ready | 无 | 活动 2～3；Ready 2～3，结束时 3 | 只多一个名额：补 sg-1；sg-3 在范围之外，是正式的组，不动 |
| 建 sg-2 v1；建 sg-4 v1 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无→v1 NotReady→v1 Ready | v1 Ready | 无→v1 NotReady→v1 Ready | 活动 3～5；Ready 3～5，结束时 5 | 两个名额：补 sg-2 和 sg-4；sg-3 原地进入范围 |


### NEW-13　带标记的组序号在范围之内，但正式的组已经够 N 个

前提：规则自己到不了的状态（surge 总是建在不小于 N 的序号上）；接管来的、找回来的、两个进程并存时可以是这样。正式的组是稀疏的 {0, 1, 4}，sg-2 带标记。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready（临时 surge） | v2 Ready | 活动 4；Ready 4 |  |
| 删 sg-2（回收 surge） | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | v2 Ready | 活动 3～4；Ready 3～4，结束时 3 | 正式的组已经有 N 个：sg-2 不转正。没有要滚的：把它当 surge 回收。高序号的正式组 sg-4 不动 |


### NEW-27　带标记的组失败了：只由 G1 删

前提：滚动刚做完、还没回收的 surge 组丢了成员，ServingGroup controller 写上 Failed，故障的成员还没停下（没有 Drained）。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 失败（临时 surge） | 活动 4；Ready 3 |  |
| 一轮 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 失败（临时 surge） | 活动 4；Ready 3 | 没有要滚的，但 G3b 的候选只有运行中的带标记的组：失败的不由它删。G1 等 Drained |
| 故障成员停下 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 失败（临时 surge）→v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3 | G1 删。序号不小于 N：没有要滚的，不再建 |


### NEW-14　找回：用 `--cascade=orphan` 删了一个范围之内的组

前提：v2 已经提交、还没开始滚。用户用 `kubectl delete --cascade=orphan` 删了 sg-1：对象没了，它的 Pod 留着、没有 owner。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| sg-1 被 orphan 删除 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→无（留有无主的 Pod）→v1 未观测（Pod 待收养） | v1 Ready | 活动 2～3；Ready 2～3，结束时 2 | S1′：序号 1 上有无主的成员 Pod 而没有同名对象，先建对象，版本取 Pod 的 v1，不是按序号该用的 v2。Pod 被收养之前新对象未观测：不算就绪，也不是 G3c 的候选。就绪 2 在底线，这一轮不滚 |
| ServingGroup controller 收养 Pod | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 未观测（Pod 待收养）→v1 Ready | v1 Ready | 活动 3；Ready 2～3，结束时 3 | 成员是 v1，与对象的版本相同；组恢复就绪 |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 之后它和别的旧组一样，按序号轮到时替换 |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |


### NEW-23　找回：用 orphan 方式删了一个 surge 的组，找回来仍然是 surge

前提：行为分析 BA4-8 的例子。sg-2 已经换成 v2、还没就绪；sg-3 是就绪的 surge。用户用 orphan 方式删了 sg-3。它的 Pod 上带着组一级标记的副本（V-15）。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady | v2 Ready（临时 surge） | 活动 4；Ready 3 |  |
| sg-3 被 orphan 删除 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady | v2 Ready（临时 surge）→无（留有无主的 Pod）→v2 未观测（Pod 待收养）（临时 surge） | 活动 3～4；Ready 2～3，结束时 2 | S1′：序号不小于 N，Pod 带标记的副本：找回来的对象带标记。正式的组仍是 3 个，不进入缩容，sg-2 不会被缩掉 |
| ServingGroup controller 收养 Pod | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady | v2 未观测（Pod 待收养）（临时 surge）→v2 Ready（临时 surge） | 活动 4；Ready 2～3，结束时 3 | sg-3 恢复就绪，仍是临时 surge |
| 就绪 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady→v2 Ready | v2 Ready（临时 surge） | 活动 4；Ready 3～4，结束时 4 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 sg-3（回收 surge）；基线推进到 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3～4，结束时 3 |  |


### NEW-15　找回：无主的 Pod 在正式范围之外

前提：用户把 N 改成 2，同时用 `--cascade=orphan` 删了 sg-2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| N 改成 2，sg-2 被 orphan 删除 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无（留有无主的 Pod）→v1 未观测（Pod 待收养） | 活动 2～3；Ready 2 | S1′ 先把对象建回来（不受 N 限制）。名下的 Pod 收养完之前它占位置（计入 `active` 和 `regular`），不计入 `regularLive`，也不是缩容的候选（V-25）：缩容等着，不拿别的组顶替 |
| ServingGroup controller 收养 Pod | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 未观测（Pod 待收养）→v1 Ready→v1 删除中 | 活动 3；Ready 2～3，结束时 2 | 之后是一般的组。正式的运行的组 3 个，多于 N：按缩容的次序删序号最高的 sg-2，Pod 随对象级联删除 |
| 就绪 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 删除中→无 | 活动 2～3；Ready 2 |  |


### NEW-15b　同上，V-25 的另一种读法：待收养的组也计入缩容的个数（不是定稿的读法）

前提：同 NEW-15。待收养的组也计入 `regularLive`，只是不当候选：缩容照常进行，从其余的组里按次序选。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| N 改成 2，sg-2 被 orphan 删除 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready→v1 删除中 | 无（留有无主的 Pod）→v1 未观测（Pod 待收养） | 活动 2～3；Ready 1～2，结束时 1 | S1′ 建对象。正式的运行的组 3 个，多于 N；sg-2 不是候选：缩容在 sg-0、sg-1 里按次序选，选中 sg-1 |
| sg-1 消失；ServingGroup controller 收养 Pod | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 删除中→无 | v1 未观测（Pod 待收养）→v1 Ready | 活动 2～3；Ready 1～2，结束时 2 | 正式的组 2 个，等于 N：不再缩。留下的是 sg-0、sg-2：删掉的是范围之内健康的 sg-1，序号 1 是洞，sg-2 在范围之外 |


### NEW-37　收养不了的无主 Pod 不算无主的成员 Pod：组照常缩掉，不为它建组

前提：独立评审 IR4-1 的第一个反例。有人对 sg-2 的一个 Pod 做过 `kubectl debug --copy-to --keep-labels`，副本留着：带齐标签、没有 owner，名字不是任何期望成员的名字，没有哪个组收养得了它。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready（名下有收养不了的无主 Pod） | 活动 3；Ready 3 |  |
| N 改成 2 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready（名下有收养不了的无主 Pod）→v1 删除中（名下有收养不了的无主 Pod）→无（留有收养不了的无主 Pod） | 活动 2～3；Ready 2～3，结束时 2 | 副本不算无主的成员 Pod（V-44）：sg-2 不是待收养，照常按次序被缩掉。前台级联只带走它名下的 Pod，副本留下；序号 2 上只剩副本，不触发找回 |
| 删 sg-1（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无（留有收养不了的无主 Pod） | 活动 1～2；Ready 1～2，结束时 2 | 提交 v2：正式的组 2 个，不在缩容中，滚动照常做 |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无（留有收养不了的无主 Pod） | 活动 1～2；Ready 1～2，结束时 2 |  |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 无（留有收养不了的无主 Pod） | 活动 2；Ready 2 |  |


### NEW-39　序号上只剩一个收养不了的无主 Pod：不触发找回

前提：NEW-37 里 sg-2 被缩掉之后的状态，v2 已经提交。评审的反例脚本就是从这里开始的。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | 无（留有收养不了的无主 Pod） | 活动 2；Ready 2 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 无（留有收养不了的无主 Pod） | 活动 1～2；Ready 1～2，结束时 2 | 不为副本建组；滚动照常做 |
| 删 sg-0（替换）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 无（留有收养不了的无主 Pod） | 活动 1～2；Ready 1～2，结束时 2 |  |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 无（留有收养不了的无主 Pod） | 活动 2；Ready 2 |  |


### NEW-38　收养不了的无主 Pod 指着一个现存的组：这个组照常被替换

前提：独立评审 IR4-1 的第二个反例。副本的组名标签指着现存的 sg-1。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready（名下有收养不了的无主 Pod） | v1 Ready | 活动 3；Ready 3 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready（名下有收养不了的无主 Pod） | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | sg-1 不是待收养：和别的组一样是替换的候选 |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready（名下有收养不了的无主 Pod）→v1 删除中（名下有收养不了的无主 Pod）→无（留有收养不了的无主 Pod）→v2 NotReady（名下有收养不了的无主 Pod）→v2 Ready（名下有收养不了的无主 Pod） | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready（名下有收养不了的无主 Pod） | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready（名下有收养不了的无主 Pod） | v2 Ready | 活动 3；Ready 3 |  |


### NEW-32　找回的竞态：稀疏布局，Pod 缓存晚于组缓存；新建的组还没就绪

前提：V-14 第 5 条。正式的组是稀疏的 {0, 1, 3}，N=3。用户用 orphan 方式删了 sg-3。ModelServing controller 的组缓存已经看到 sg-3 没了，Pod 缓存里它的 Pod 还带着 owner。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无 | v1 Ready | 活动 3；Ready 3 |  |
| sg-3 被 orphan 删除，Pod 缓存还没跟上 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无→v1 NotReady | 无（留有无主的 Pod，Pod 缓存里还没有） | 活动 2～3；Ready 2 | 看上去只是少了一个正式的组：G4 在最低的空洞上补 sg-2 |
| Pod 缓存跟上 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 NotReady | 无（留有无主的 Pod，Pod 缓存里还没有）→无（留有无主的 Pod）→v1 未观测（Pod 待收养） | 活动 3～4；Ready 2 | S1′ 把 sg-3 的对象建回来。正式的组 4 个，多于 N；sg-3 待收养，不计入 `regularLive`：缩容等着 |
| ServingGroup controller 收养 Pod | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 NotReady→v1 删除中 | v1 未观测（Pod 待收养）→v1 Ready | 活动 4；Ready 2～3，结束时 3 | sg-3 就绪。正式的运行的组 4 个：缩容先选未就绪的，是刚建的 sg-2。回到 {0, 1, 3} |
| 就绪 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 删除中→无 | v1 Ready | 活动 3～4；Ready 3 |  |


### NEW-33　找回的竞态：同上，新建的组先就绪

前提：同 NEW-32，不同的是 sg-2 在 sg-3 的 Pod 被收养之前已经就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无 | v1 Ready | 活动 3；Ready 3 |  |
| sg-3 被 orphan 删除，Pod 缓存还没跟上 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 无→v1 NotReady→v1 Ready | 无（留有无主的 Pod，Pod 缓存里还没有） | 活动 2～3；Ready 2～3，结束时 3 | G4 在最低的空洞上补 sg-2，它很快就绪 |
| Pod 缓存跟上 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 无（留有无主的 Pod，Pod 缓存里还没有）→无（留有无主的 Pod）→v1 未观测（Pod 待收养） | 活动 3～4；Ready 3 | S1′ 把 sg-3 的对象建回来；待收养，缩容等着 |
| ServingGroup controller 收养 Pod | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 未观测（Pod 待收养）→v1 Ready→v1 删除中 | 活动 4；Ready 3～4，结束时 3 | 四个组都就绪：缩容按次序删序号最高的，是 sg-3，它运行着的 Pod 随对象级联删除。终态 {0, 1, 2}：原来的 sg-3 换成了新建的 sg-2 |
| 就绪 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 删除中→无 | 活动 3～4；Ready 3 |  |


### NEW-29　回收 surge：没有确认未就绪的 surge 组要过下限

前提：滚动的最后一步：sg-0 刚重建、还没就绪，就绪的 surge sg-3 还在顶着下限（U=0，下限 3）。这时用户改了一个要下发到各组的字段（例如 Role 的副本数），各组短暂未观测。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 NotReady | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4；Ready 3 | 没有要滚的；sg-3 确认就绪，删掉它 ready 会低于下限：留着 |
| 一轮 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 NotReady | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4；Ready 3 | 没有动作 |
| `spec` 下发，四个组未观测 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 未观测 | v2 未观测 | v2 未观测 | v2 未观测（临时 surge） | 活动 4；Ready 0 | sg-3 未观测：不是确认未就绪，不能直接回收；删掉它确认就绪的个数不变，所以要求现在的个数本身不低于下限（V-34）。现在是 0：留着。Pod 一级的就绪保持 3 |
| 各组的 `status` 跟上 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 NotReady | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 4；Ready 3 | sg-3 确认就绪：删掉它剩 2，低于下限，仍然留着 |
| sg-0 就绪 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge） | 活动 4；Ready 3～4，结束时 3 | 确认就绪 4 个，删掉 sg-3 剩 3：回收 |
| 就绪 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3 |  |


### NEW-30　`status` 落后在变好的方向：`status` 不新鲜的组按未观测算

前提：v2 已经提交。两个 controller 停着的时候（或者 ServingGroup controller 还没轮到这两个组）：sg-0 的 Pod 恢复了就绪，`status` 还写着未就绪；sg-1 的 Pod 坏了，`status` 还写着就绪。新进程里两个 controller 没有先后。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 NotReady（Pod 已就绪，status 没更新） | v1 Ready（Pod 已坏，status 没更新） | v1 Ready | 活动 3；Ready 1 | Pod 一级就绪的是 sg-0、sg-2，共 2 个，正好在下限（N − U = 2） |
| ModelServing controller 的一轮 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 NotReady（Pod 已就绪，status 没更新） | v1 Ready（Pod 已坏，status 没更新） | v1 Ready | 活动 3；Ready 1 | sg-0（`status` 写着未就绪，Pod 已经就绪）、sg-1（`status` 写着就绪，Pod 已坏）的 `status` 都不是按现在这些 Pod 算的，不新鲜（V-40）：按未观测算，不是替换的候选，不进 `ready`（V-34）。确认就绪只有 sg-2，健康额度 0：这一轮不动。Pod 一级的就绪保持 2 |
| ServingGroup controller 写出 `status` | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 NotReady（Pod 已就绪，status 没更新）→v1 Ready | v1 Ready（Pod 已坏，status 没更新）→v1 NotReady→v1 删除中 | v1 Ready | 活动 3；Ready 1～2，结束时 2 | sg-0 确认就绪，sg-1 确认未就绪。按未就绪的替换 sg-1：Pod 一级的就绪保持 2 |
| 建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 删除中→v2 NotReady→v2 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |


### NEW-31　`spec` 刚下发：未观测的组不是替换的候选

前提：Pod 的状态同 NEW-30（sg-0 的 Pod 已经恢复，`status` 还写着未就绪；sg-1 的 Pod 坏了，`status` 还写着就绪）。不同的是提交 v2 的同时各组的 `spec` 刚被改过：三个组都是未观测。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 未观测（Pod 已就绪，status 没更新） | v1 未观测（Pod 已坏，status 没更新） | v1 未观测 | 活动 3；Ready 0 |  |
| ModelServing controller 的一轮 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 未观测（Pod 已就绪，status 没更新） | v1 未观测（Pod 已坏，status 没更新） | v1 未观测 | 活动 3；Ready 0 | 未观测的组不是替换的候选，也不算确认就绪：这一轮不动 |
| ServingGroup controller 写出 `status` | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 未观测（Pod 已就绪，status 没更新）→v1 NotReady（Pod 已就绪，status 没更新）→v1 Ready | v1 未观测（Pod 已坏，status 没更新）→v1 Ready（Pod 已坏，status 没更新）→v1 NotReady→v1 删除中 | v1 未观测→v1 Ready | 活动 3；Ready 0～2，结束时 2 | sg-0 就绪，sg-1 未就绪。按未就绪的替换 sg-1：Pod 一级的就绪保持 2 |
| 建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 删除中→v2 NotReady→v2 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |


### NEW-34　缩容的次序按 Pod 一级的就绪：`status` 晚一跳

前提：没有滚动。sg-0 的 Pod 坏了，`status` 还写着就绪。这时用户把 N 改成 2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v1 基线 v1 | v1 Ready（Pod 已坏，status 没更新） | v1 Ready | v1 Ready | 活动 3；Ready 2 | Pod 一级就绪的是 sg-1、sg-2，共 2 个 |
| N 改成 2 | N=2 U=0 S=1 P=0 目标 v1 基线 v1 | v1 Ready（Pod 已坏，status 没更新）→v1 删除中 | v1 Ready | v1 Ready | 活动 3；Ready 2 | 缩容先选 Pod 一级不就绪的（V-35）：sg-0，不管它的 `status` 写着什么。Pod 一级的就绪保持 2。按报告的就绪排的话，三个组都报告就绪，删的是序号最高的 sg-2，就绪降到 1 |
| sg-0 消失 | N=2 U=0 S=1 P=0 目标 v1 基线 v1 | v1 删除中→无 | v1 Ready | v1 Ready | 活动 2～3；Ready 2 | 留下 sg-1、sg-2：序号 0 是洞，sg-2 在范围之外（「未就绪的先」本来就会留下稀疏的集合） |


### NEW-16　RecreateRequired：未就绪的目标版本的组在预算之内替换

前提：sg-1 收养来的成员版本和它的 `spec.revision` 不同，ServingGroup controller 写上 RecreateRequired；它一直不就绪。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 NotReady | v1 Ready | 活动 3；Ready 2 |  |
| 删 sg-1（替换未就绪的）；建 sg-1 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 NotReady（RecreateRequired）→v1 删除中（RecreateRequired）→v1 NotReady→v1 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | sg-1 可替换、未就绪：它不算进 notReadyTarget，maxScaleDown=1，删掉，原序号重建 |


### NEW-17　基线读不到：要用基线的位置等着，也不跳到更高的序号

前提：灰度停点上 sg-0 整组恢复，对象已经删掉；这时基线版本读不到。

| 阶段 | 期望 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 读不到 | v2 Ready | v2 Ready | 活动 2；Ready 2 |  |
| 一轮 | N=3 U=1 S=0 P=1 目标 v2 基线 读不到 | v2 Ready | v2 Ready | 活动 2；Ready 2 | sg-0 的序号小于 P，要用基线；基线读不到：不建，不拿目标版本顶替 |
| N 改成 4 | N=4 U=1 S=0 P=1 目标 v2 基线 读不到 | v2 Ready | v2 Ready | 活动 2；Ready 2 | G4 停在序号 0 上：不跳过去建 sg-3。基线读得到之后，sg-0 用 v1、sg-3 用 v2 一起建出来 |


### NEW-19　Pod 全没了的组：每组一个 Pod，滚动途中旧组的 Pod 被外部删除

前提：SG 模式，每组只有一个 Pod。sg-2 已经换成 v2。这时 sg-0 唯一的 Pod 被外部删除（节点排空、手工删除）。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 Ready | 活动 3；Ready 3 |  |
| sg-0 的 Pod 没了 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 失败 | v1 Ready | v2 Ready | 活动 3；Ready 2～3，结束时 2 | 组期望有成员、建成过、名下一个 Pod 都没有了：ServingGroup controller 写 Failed，Drained 同时为真，不再创建（V-13） |
| G1 删，G4 补 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 失败→v1 删除中→v2 NotReady | v1 Ready | v2 Ready | 活动 2～3；Ready 2 | 原序号重建，版本按序号和当时的 partition：序号 0 不小于 P=0，直接用 v2。没有先起一遍 v1 |
| 就绪 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v2 NotReady→v2 Ready | v1 Ready | v2 Ready | 活动 3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |


### NEW-20　Pod 全没了的组：序号在保护区里

前提：同上，灰度停点 P=1。sg-0 唯一的 Pod 被外部删除。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |
| sg-0 的 Pod 没了 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | v1 Ready（受保护）→v1 失败（受保护）→v1 删除中（受保护）→v1 NotReady（受保护）→v1 Ready（受保护） | v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | Failed、Drained，G1 删，G4 补：序号 0 小于 P，用基线 v1 |


### NEW-21　对象建了、Pod 建不出来时提交新模板

前提：行为分析 BA4-2、RUN-637 的夹具。扩容 3→5 做到一半：sg-3 已经就绪，sg-4 的 PodGroup 建不出来（配额、webhook、Volcano 不在）。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | sg-5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=0 S=1 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 无 | 无 | 活动 4；Ready 4 |  |
| 补 sg-4 | N=5 U=0 S=1 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 无→v1 没有 Pod | 无 | 活动 4～5；Ready 4 | 对象建出来了，版本是当时的 v1；名下没有 Pod：没有成员的组 |
| 提交 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 没有 Pod→v2 没有 Pod | 无→v2 NotReady（临时 surge） | 活动 5～6；Ready 4 | G2′(b)：sg-4 没有成员，版本改成按序号现在新建会用的 v2。它不是「可替换」的，不占预算。四个 v1 是要滚的：建临时 surge sg-5 |
| PodGroup 建得出来了 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 没有 Pod→v2 NotReady | v2 NotReady（临时 surge） | 活动 6；Ready 4 | sg-4 的 Pod 直接按 v2 起，没有先起一遍 v1 |
| 就绪 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v2 NotReady→v2 Ready | v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 6；Ready 4～6，结束时 6 |  |
| 删 sg-3（替换）；建 sg-3 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 sg-5（回收 surge）；基线推进到 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 5～6；Ready 5～6，结束时 5 |  |


### NEW-44　刚建出来、还没写过 `status` 的组遇到新模板

前提：独立评审 IR4-26。扩容 2→3 刚把 sg-2 的对象建出来，ServingGroup controller 还没有处理它：没有 `status`（未观测），`status.pods` 是 0，Pod 缓存里也没有它的 Pod。这时用户提交 v2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 还没写过 `status`，没有 Pod | 无 | 活动 3；Ready 2 |  |
| 提交 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 还没写过 `status`，没有 Pod→v2 还没写过 `status`，没有 Pod | 无→v2 NotReady（临时 surge） | 活动 3～4；Ready 2 | sg-2 没有成员（V-56：不要求已观测）。G2′(b) 直接把它的版本改成 v2，不占预算；它不是可替换的。两个 v1 是要滚的：建临时 surge sg-3 |
| ServingGroup controller 第一次处理 sg-2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 还没写过 `status`，没有 Pod→v2 NotReady | v2 NotReady（临时 surge） | 活动 4；Ready 2 | 先写 `status` 再建 Pod。Pod 直接按 v2 起，没有先起一遍 v1 |
| 就绪 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 NotReady→v2 Ready | v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 4；Ready 2～4，结束时 4 |  |
| 删 sg-1（替换）；删 sg-0（替换）；建 sg-0 v2；建 sg-1 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready（临时 surge） | 活动 2～4；Ready 2～4，结束时 4 |  |
| 删 sg-3（回收 surge）；基线推进到 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3～4，结束时 3 |  |


### NEW-22　序号被外来的同名对象占着

前提：命名空间里有一个不属于这个 ModelServing 的 ServingGroup，名字正好是 `<ModelServing 名>-1`。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象 | 无 | 活动 1；Ready 1 |  |
| 建 sg-2 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象 | 无→v1 NotReady→v1 Ready | 活动 1～2；Ready 1～2，结束时 2 | 序号 1 不空闲：补最低的空闲序号 sg-2。外来的对象不建、不删、不计入任何计数。正式的组 2 个，少一个，报告 `OrdinalOccupied`；不到更高的序号上去凑数 |
| 停住 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象 | v1 Ready | 活动 2；Ready 2 | 等外来的对象消失 |
| 外来的对象被它的主人删掉 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象→无 | v1 Ready | 活动 2；Ready 2 |  |
| 建 sg-1 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 无→v1 NotReady→v1 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号 1 空闲了：补上 |


### NEW-25　复核：停机期间组变坏，`status` 没有人写

前提：提案分析 5.1 的过程。N=5、U=1，下限 4。controller 停着的时候 sg-0、sg-1 的 Pod 不再就绪；两个组的 `status` 还是 Ready、已观测。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 5 |  |
| 停机期间 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 Ready（Pod 已坏，status 没更新） | v1 Ready→v1 Ready（Pod 已坏，status 没更新） | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 3～5，结束时 3 | Pod 一级只有 3 个组就绪；`status` 里是 5 个 |
| 新进程的第一轮 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready（Pod 已坏，status 没更新） | v1 Ready（Pod 已坏，status 没更新） | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 3 | 复核：sg-0、sg-1 的 `status` 是按停机之前的 Pod 算的，Pod 缓存里它们已经变了：`status` 不新鲜（V-40），按未观测算，不进 `ready`，也不是候选。ready=3，健康额度是 0：不删 sg-4 |
| ServingGroup controller 写出 `status` | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready（Pod 已坏，status 没更新）→v1 NotReady | v1 Ready（Pod 已坏，status 没更新）→v1 NotReady→v1 删除中 | v1 Ready | v1 Ready | v1 Ready | 活动 5；Ready 3 | sg-0、sg-1 现在是未就绪的旧组：maxScaleDown=1，先换序号高的 sg-1。就绪数不降 |
| 建 sg-1 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 NotReady→v1 Ready | v1 删除中→v2 NotReady→v2 Ready | v1 Ready | v1 Ready | v1 Ready | 活动 4～5；Ready 3～5，结束时 5 |  |
| 删 sg-4（替换）；建 sg-4 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | v1 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-3（替换）；建 sg-3 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 sg-0（替换）；建 sg-0 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 Ready | v2 Ready | 活动 5；Ready 5 |  |


### NEW-26　复核：新模板和「组 0 丢了一个成员」同时到

前提：行为分析 BA4-20 的夹具（`recovery` 套件 SG 模式、U ≥ 1 的 32 例）：暂停 controller，提交 v2，删 sg-0 的一个 Pod，放开。下限 2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| 提交 v2，sg-0 丢了一个 Pod | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 Ready（Pod 已坏，status 没更新） | v1 Ready | v1 Ready | 活动 3；Ready 2～3，结束时 2 | `status` 里三个组都还是就绪 |
| ModelServing controller 先到 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready（Pod 已坏，status 没更新） | v1 Ready | v1 Ready | 活动 3；Ready 2 | 复核：sg-0 丢了一个 Pod，它的 `status` 还是按原来那些 Pod 算的：不新鲜（V-40），按未观测算。ready=2，健康额度 0：不删 sg-2 |
| ServingGroup controller 写出 `status` | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 Ready（Pod 已坏，status 没更新）→v1 NotReady→v1 删除中 | v1 Ready | v1 Ready | 活动 3；Ready 2 | sg-0 是未就绪的旧组，在 maxScaleDown 之内先换它 |
| 建 sg-0 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v1 删除中→v2 NotReady→v2 Ready | v1 Ready | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-2（替换）；建 sg-2 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 sg-1（替换）；建 sg-1 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 活动 3；Ready 3 |  |


### NEW-45　两个范围外的组，只有一个补得上的低位：只替换一个

前提：之前的缩容留下 {sg-0, sg-3, sg-4}，N=3、U=2。序号 1 被外来的同名对象占着，小于 N 的空闲序号只有 2。用户提交 v2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=2 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象 | 无 | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| 提交 v2 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中 | 外来的同名对象 | 无 | v1 Ready | v1 Ready→v1 删除中 | 活动 3；Ready 1～3，结束时 1 | 先看 sg-4：拿掉它，G4 在序号 2 上补得回来，替换。再看 sg-3（U=2，额度还有一个）：这一轮选中的 sg-4 也拿掉之后，序号 2 只补得回一个，正式的组比不删 sg-3 时少一个：补不上，跳过（V-59）。接着看 sg-0：它自己的序号腾出来就能建，替换 |
| 建 sg-0 v2；建 sg-2 v2 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v1 删除中→v2 NotReady→v2 Ready | 外来的同名对象 | 无→v2 NotReady→v2 Ready | v1 Ready | v1 删除中→无 | 活动 1～3；Ready 1～3，结束时 3 | sg-4、sg-0 消失：G4 补 sg-0 和 sg-2。剩下的候选只有 sg-3，补不上：报告序号被占 |
| 停住 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | v2 Ready | v1 Ready | 无 | 活动 3；Ready 3 | {0: v2, 2: v2, 3: v1}：三个组都在。滚动停在 sg-3 上，等外来的对象消失。逐个组看的话 sg-4、sg-3 都被替换，只补得上一个 |
| 外来的对象被它的主人删掉 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象→无 | v2 Ready | v1 Ready | 无 | 活动 3；Ready 3 |  |
| 删 sg-3（替换）；建 sg-1 v2 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 无→v2 NotReady→v2 Ready | v2 Ready | v1 Ready→v1 删除中→无 | 无 | 活动 2～3；Ready 2～3，结束时 3 | 序号 1 空闲了：sg-3 被替换，替代的容量建在 sg-1 上 |
| 基线推进到 v2 | N=3 U=2 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | 无 | 无 | 活动 3；Ready 3 | 全部是 v2，全部就绪：基线推进 |


### NEW-46　两个范围外的空组，只有一个空着的低位：只删一个

前提：之前的缩容留下 {sg-0, sg-3, sg-4}，N=3。序号 1 被外来的同名对象占着，小于 N 的空闲序号只有 2。用户把每个 Role 的副本数调成 0。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | 外来的同名对象 | 无 | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | 外来的同名对象 | 无 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready→v1 删除中 | 活动 3；Ready 2～3，结束时 2 | sg-4、sg-3 都是空组、正式、序号不小于 N。先看 sg-4：没有别的删除在途，序号 2 空着、现在就建得出来，删。再看 sg-3：这一轮已经选中了 sg-4，一轮至多删一个（V-62） |
| sg-4 消失 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 空 Ready | 外来的同名对象 | 无→v1 空 NotReady→v1 空 Ready | v1 空 Ready | v1 删除中→无 | 活动 2～3；Ready 2～3，结束时 3 | sg-4 腾出来的位置还没补上，那次删除还算在途：sg-3 不删。G4 在 sg-2 上补一个空组 |
| 停住 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 空 Ready | 外来的同名对象 | v1 空 Ready | v1 空 Ready | 无 | 活动 3；Ready 3 | {0, 2, 3}：三个组都在。之后 sg-3 也不删：小于 N 的序号没有空闲的了，报告序号被占。「在途」只看终止中的对象的话，sg-4 一消失 sg-3 就被删掉（那一轮 G2′ 排在 G4 前面，序号 2 还空着），只补得上一个 |


### NEW-48　范围之内有带标记的组、正式的组已有 N 个：转正也是一种着落

前提：接管或者找回留下的状态：sg-2 带着 surge 标记而序号在范围之内，正式的组 {sg-0, sg-1, sg-3} 已经有 3 个，所以 G0 不转正它。目标 v2，U=1、S=1。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v2 Ready（临时 surge） | v1 Ready | 活动 4；Ready 4 |  |
| 删 sg-3（替换）；删 sg-1（替换）；sg-2 转正；建 sg-1 v2；建 sg-3 v2（临时 surge） | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready（临时 surge）→v2 Ready | v1 Ready→v1 删除中→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | 活动 2～4；Ready 2～4，结束时 4 | 先看 sg-3：小于 N 的序号没有空闲的，但拿掉它之后正式的组少于 N，sg-2 会被 G0 转正，个数不变：补得上（V-59），替换。额度还够一个（4 个就绪，下限 2）：sg-1 在原序号上替换。两个组消失之后 sg-2 转正，G4 补 sg-1；还有 sg-0 要滚，建临时 surge sg-3 |
| 删 sg-0（替换）；建 sg-0 v2；删 sg-3（回收 surge） | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready→v1 删除中→v2 NotReady→v2 Ready | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge） | 活动 3～4；Ready 2～4，结束时 3 | sg-0 照常替换。没有要滚的了：回收 surge sg-3 |
| 基线推进到 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready | v2 删除中（临时 surge）→无 | 活动 3～4；Ready 3 | 全部是 v2，全部就绪：基线推进。终局 {0, 1, 2}，没有带标记的组 |


### NEW-48b　同上，只剩范围外的那个组要滚

前提：NEW-48 的布局，sg-0、sg-1 已经是 v2：要滚的只剩范围外的 sg-3，小于 N 的序号都被自己的组占着（sg-2 带着标记）。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v2 Ready | v2 Ready | v2 Ready（临时 surge） | v1 Ready | 活动 4；Ready 4 |  |
| 删 sg-3（替换）；sg-2 转正；基线推进到 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 Ready | v1 Ready→v1 删除中→无 | 活动 3～4；Ready 3～4，结束时 3 | sg-3 补得上（它消失之后 sg-2 转正），替换。它消失之后 sg-2 转正；全部是 v2、没有带标记的组、`active` = N：基线推进。逐个组看的话 sg-3 不替换，也没有可报的原因，一直停在初始状态 |


### NEW-49　只有一个补得上的低位，上一轮替换的组还在终止中

前提：与 NEW-45 相同的布局，滚动做了一部分：sg-0 已经是 v2。N=3、U=2，序号 1 被外来的同名对象占着。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | sg-4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | 无 | v1 Ready | v1 Ready | 活动 3；Ready 3 |  |
| 一轮 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | 无 | v1 Ready | v1 Ready→v1 删除中 | 活动 3；Ready 2～3，结束时 2 | sg-4 补得上（序号 2），替换。sg-3 补不上，跳过 |
| 再一轮，sg-4 还在终止中 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | 无 | v1 Ready | v1 删除中 | 活动 3；Ready 2 | 额度还有一个（U=2）。sg-4 还占着名字，序号 2 在工作副本里仍然是空闲的；但已经在终止中的组也要拿掉再算（V-59）：序号 2 是给 sg-4 的替代用的，sg-3 仍然补不上。没有写 |
| 建 sg-2 v2 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | 无→v2 NotReady→v2 Ready | v1 Ready | v1 删除中→无 | 活动 2～3；Ready 2～3，结束时 3 | sg-4 消失：G4 补 sg-2 |
| 停住 | N=3 U=2 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | v2 Ready | v1 Ready | 无 | 活动 3；Ready 3 | {0: v2, 2: v2, 3: v1}，三个组都在，报告序号被占 |


### NEW-50　唯一空着的低位本来就缺着：范围外的空组不删

前提：NEW-35 的状态（{sg-0, sg-1, sg-3}，N=3，序号 2 被外来的同名对象占着，副本数都是 0）。这时有人把 sg-1 的对象删掉了。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready | v1 Ready | 外来的同名对象 | v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 Ready→v1 空 Ready | v1 Ready→v1 空 Ready | 外来的同名对象 | v1 Ready→v1 空 Ready | 活动 3；Ready 3 | 同 NEW-35：没有空闲的低位，sg-3 不删 |
| sg-1 的对象被删掉 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 空 Ready | v1 空 Ready→无 | 外来的同名对象 | v1 空 Ready | 活动 2～3；Ready 2～3，结束时 2 | 正式的组剩 2 个，序号 1 空着 |
| 一轮 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | v1 空 Ready | 无→v1 空 NotReady→v1 空 Ready | 外来的同名对象 | v1 空 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号 1 空闲、建得出来，但它是缺着的那个正式位置，还没补上：算有删除在途，sg-3 不删（V-62）。G4 补 sg-1。之后正式的组够 N 个了，低位没有空闲的，sg-3 仍然不删。把那个空位算给 sg-3 的话它被删掉，只剩 {0, 1} |


### NEW-51　滚动中把 maxSurge 调成 0，而就绪的只有 surge 的组：N + S 的上限不算补不上

前提：N=2、U=1、S=1 的滚动刚建出 surge sg-2（就绪），两个旧组 sg-0、sg-1 都坏了。这时用户把 maxSurge 调成 0。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=1 P=0 目标 v2 基线 v1 | v1 NotReady | v1 NotReady | v2 Ready（临时 surge） | 活动 3；Ready 1 |  |
| 删 sg-1（替换未就绪的）；删 sg-0（替换未就绪的）；建 sg-0 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v1 NotReady→v1 删除中→v2 NotReady→v2 Ready | v1 NotReady→v1 删除中→无 | v2 Ready（临时 surge） | 活动 1～3；Ready 1～2，结束时 2 | 活动 3 个，超过 N + S。sg-2 是唯一就绪的，回收它会压到下限以下：不回收。两个旧组是确认未就绪的，`maxScaleDown` 是 2：都替换。「补得上」看的是序号和基线，不看 N + S 这个上限：上限是暂时的，surge 回收之后位置就腾得出来。它们消失之后 G4 先补 sg-0（`active` 到 N + S 为止） |
| 删 sg-2（回收 surge）；建 sg-1 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | v2 Ready | 无→v2 NotReady→v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 活动 1～2；Ready 1～2，结束时 2 | sg-0 就绪之后，回收 sg-2 不再越过下限；它消失之后补 sg-1 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | 无 | 活动 2；Ready 2 | 全部是 v2，全部就绪：基线推进。把上限也算进「补得上」的话，两个旧组都补不上，一直停在初始状态，也没有可报的原因 |


### NEW-52　有滚动要做要看补不补得上：基线读不到时不建 surge

前提：独立评审 IR4-32 的第一个例子。之前的缩容留下 {sg-0, sg-3}，N=2、U=0、S=1、P=2。用户提交 v2；这时基线版本读不到。

| 阶段 | 期望 | sg-0 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=0 S=1 P=2 目标 v1 基线 v1 | v1 Ready | v1 Ready | 活动 2；Ready 2 |  |
| 提交 v2，基线读不到 | N=2 U=0 S=1 P=2 目标 v2 基线 读不到 | v1 Ready（受保护） | v1 Ready | 活动 2；Ready 2 | sg-3 是可替换的，但补不上（替代的容量要在序号 1 上按基线建）。可替换的正式的组没有一个补得上：没有滚动要做（V-60），不建 surge。计划里只有等待的原因：基线读不到。两个组留着。基线读到之后才有滚动要做：建 surge、替换 sg-3、在 sg-1 上按基线补、回收 surge（走法同 NEW-53 的后半段）。不看补不补得上的话现在就建 surge：P=N 的停点上多一个目标版本的组在服务，直到基线读得到 |


### NEW-53　有滚动要做要看补不补得上：剩下的旧组补不上时回收 surge

前提：独立评审 IR4-32 的第二个例子。N=2、U=0、S=1，序号 1 被外来的同名对象占着。sg-0 已经换成 v2，要滚的只剩范围外的 sg-3，临时 surge sg-2 还在。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=0 S=1 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | v2 Ready（临时 surge） | v1 Ready | 活动 3；Ready 3 |  |
| 删 sg-2（回收 surge） | N=2 U=0 S=1 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | v1 Ready | 活动 2～3；Ready 2～3，结束时 2 | sg-3 补不上（小于 N 的序号没有空闲的）：没有滚动要做（V-60）。回收 surge sg-2：删掉之后确认就绪的还有 2 个，不低于下限。报告序号被占 |
| 停住 | N=2 U=0 S=1 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象 | 无 | v1 Ready | 活动 2；Ready 2 | {0: v2, 3: v1}，两个组，等外来的对象消失。不看补不补得上的话 sg-2 一直留着：三个组长期运行 |
| 外来的对象被它的主人删掉 | N=2 U=0 S=1 P=0 目标 v2 基线 v1 | v2 Ready | 外来的同名对象→无 | 无 | v1 Ready | 活动 2；Ready 2 |  |
| 建 sg-2 v2（临时 surge） | N=2 U=0 S=1 P=0 目标 v2 基线 v1 | v2 Ready | 无 | 无→v2 NotReady（临时 surge）→v2 Ready（临时 surge） | v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 序号 1 空闲了：sg-3 补得上，有滚动要做。U=0，先建临时 surge sg-2 |
| 删 sg-3（替换）；建 sg-1 v2 | N=2 U=0 S=1 P=0 目标 v2 基线 v1 | v2 Ready | 无→v2 NotReady→v2 Ready | v2 Ready（临时 surge） | v1 Ready→v1 删除中→无 | 活动 2～3；Ready 2～3，结束时 3 | sg-2 就绪之后替换 sg-3；替代的容量建在 sg-1 上 |
| 删 sg-2（回收 surge）；基线推进到 v2 | N=2 U=0 S=1 P=0 目标 v2 基线 v2 | v2 Ready | v2 Ready | v2 Ready（临时 surge）→v2 删除中（临时 surge）→无 | 无 | 活动 2～3；Ready 2～3，结束时 2 | 全部是 v2：回收 surge，基线推进 |


### NEW-54　范围外的没有成员的组（不是空组）：低位现在建得出来就删，不改版本

前提：独立评审 IR4-33 的例子。N=2、P=2，基线 v1 读得到。范围外的 sg-3 的对象在，PodGroup 建不出来，名下没有 Pod：没有成员，不是空组。用户提交 v2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v1 基线 v1 | v1 Ready | 无 | v1 没有 Pod | 活动 2；Ready 1 |  |
| 提交 v2 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | 无 | v1 没有 Pod→v1 删除中 | 活动 2；Ready 1 | G2′(a)：sg-3 没有成员、正式、序号不小于 N；没有别的删除在途，序号 1 空着、现在就按基线建得出来：删掉，不占预算（V-61、V-62）。G2′(b) 不改它的版本 |
| sg-3 消失 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | G4 在 sg-1 上补；序号 1 小于 P，按基线 v1。终局 {0: v1, 1: v1}，与 sg-3 有 Pod 时（NEW-54b）相同。只管空组的话 sg-3 被改成 v2 留下，终局 {0: v1, 3: v2} |


### NEW-54b　对照 NEW-54：范围外的组有 Pod 时走替换，终局相同

前提：与 NEW-54 相同，只是 sg-3 有 Pod、就绪。用户提交 v2。

| 阶段 | 期望 | sg-0 | sg-1 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v1 基线 v1 | v1 Ready | 无 | v1 Ready | 活动 2；Ready 2 |  |
| 删 sg-3（替换）；建 sg-1 v1 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | v1 Ready（受保护） | 无→v1 NotReady（受保护）→v1 Ready（受保护） | v1 Ready→v1 删除中→无 | 活动 1～2；Ready 1～2，结束时 2 | sg-3 有成员，序号不小于 P，是可替换的，补得上：在预算之内替换；替代的容量在 sg-1 上按基线 v1 建。终局 {0: v1, 1: v1} |


### NEW-55　带标记的组比 S 多、又回收不了时：范围外的没有成员的组留着

前提：N=2、U=1，maxSurge 在滚动中从 1 调成了 0，所以带标记的 sg-2 比 S 多一个。sg-0 起不来；范围外的正式的 sg-3 的 PodGroup 建不出来，名下没有 Pod。sg-2 是唯一就绪的组：回收它会越过下限，回收不了。没有版本要滚。

| 阶段 | 期望 | sg-0 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 NotReady | v2 Ready（临时 surge） | v2 没有 Pod | 活动 3；Ready 1 |  |
| 一轮 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 NotReady | v2 Ready（临时 surge） | v2 没有 Pod | 活动 3；Ready 1 | G2′(a)：sg-3 没有成员、正式、序号不小于 N，序号 1 也空着。但把它拿掉之后 `active` 是 2，不小于 N + S：G4 现在建不出替代的，不删（V-62）。它留着，版本不动。sg-2 回收不了（下限）。没有写 |
| PodGroup 建得出来了 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 NotReady | v2 Ready（临时 surge）→v2 删除中（临时 surge） | v2 没有 Pod→v2 NotReady→v2 Ready | 活动 3；Ready 1～2，结束时 1 | sg-3 起 Pod、就绪。就绪的有 2 个了：回收 sg-2 不再越过下限 |
| sg-2 消失 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | v2 NotReady | v2 删除中（临时 surge）→无 | v2 Ready | 活动 2～3；Ready 1 | 终局两个正式的组 {0, 3}（sg-0 仍然起不来）。按 V-59 的「补得上」（不看上限）的话 sg-3 第一轮就被删掉，替代的组一直建不出来，PodGroup 修好之后也不恢复 |


### NEW-18　RoleRollingUpdate：组一级只有找回、恢复和扩缩；带标记的组摘掉标记

前提：Role 模式。格子里的版本是组的 `status.revision`；组里面的滚动是 ServingGroup controller 的事，这里没有建模。sg-3 是从 SG 模式切过来时留下的带标记的组；sg-2 的 deletion cost 低。

| 阶段 | 期望 | sg-0 | sg-1 | sg-2 | sg-3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready | v2 Ready（临时 surge） | 活动 4；Ready 4 | 三个组还在 v1：组一级不替换任何组 |
| sg-3 转正；删 sg-2（缩容） | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready | v1 Ready→v1 删除中→无 | v2 Ready（临时 surge）→v2 Ready | 活动 3～4；Ready 3～4，结束时 3 | G0：这个模式下带标记的组一律摘掉标记。正式的组 4 个，多于 N：G2 按缩容的次序删 deletion cost 低的 sg-2，留下的 sg-3 是正式的组 |
| sg-1 失败并重建 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v1 Ready→v1 失败→v1 删除中→v2 NotReady→v2 Ready | 无 | v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 原序号重建，用目标版本 |
| 建 sg-2 v2 | N=4 U=1 S=1 P=0 目标 v2 基线 v1 | v1 Ready | v2 Ready | 无→v2 NotReady→v2 Ready | v2 Ready | 活动 3～4；Ready 3～4，结束时 4 | 扩容补最低的空洞 sg-2。sg-3 一直是正式的组 |
