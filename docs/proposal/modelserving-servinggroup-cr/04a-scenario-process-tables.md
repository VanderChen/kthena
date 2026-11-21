# 行为预期表：过程表

本文件由任务目录的 `model/run_scenarios.py` 生成，不要手工修改。它是 [04](04-behavior-expectations.md) 的附表：把契约 2.12 里涉及组一级的场景，在本设计的规则上逐个重放一遍得到的过程表。

读法：

- 列是显示用的位置，不是序号：新建的组占最靠前的空列。
- `g<i>` 是场景开始时就有的组，i 是契约里同一个组的序号，方便两边对照；`n<k>` 是场景里新建的组，按创建的先后编号。真实的名字是 `<ModelServing 名>-<随机后缀>`。
- 一行是一个阶段。格子里用 `→` 连起这个阶段里先后出现的状态。「活动」是全部组对象的个数，包括删除中的和失败的。
- 「期望」一列是这个阶段的 N、U、S、P（百分比已经解析）、目标版本和基线。
- 每个场景后面的小表把本设计的可观察量和契约的过程表读出来的可观察量并排。结论由这张小表得出：全部相同是「一致」（只有名字不同），有一项不同是「有意变化」。
- 状态里的「未观测」指组的 `status` 还没有跟上刚下发的 `spec`；「空」指各 Role 的副本数都是 0，组名下没有 Pod。
- 小表只比五个量。一行里并排发出的几个删除不分先后，过程中的停点不在比较里：SG-C02、SG-C12 的行把「缩容完成」这个停点和后面的替换并在了一起，停点另见 [05](05-inference-verification.md) 第 2 节的重放。
- SG-C15 的最后三个旧组，契约的表是一次删一个，这里是先两个、再一个。U=2 时契约自己的预算允许一次两个；这个差别不是本设计带来的。
- 整轮滚动之后，创建的先后与契约的序号正好相反：契约里最先被换的是最高的序号，它在这里是最老的新组。下一轮滚动先换的、设了 P 之后留下的，个数相同，是哪一个不同（04 的 X2、X5）。

## 1. 契约 2.12 的场景

### SG-S01　单独缩容，再两次扩容

前提：g1、g2 的 deletion cost 低。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=4 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | 活动 4；Ready 4 |  |
| N 改成 2 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中 | g2 v1 Ready→g2 v1 删除中 | g3 v1 Ready | 活动 4；Ready 2～4，结束时 2 | 缩容选 deletion cost 低的；不占滚动预算 |
| 缩容完成 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 删除中→无 | g2 v1 删除中→无 | g3 v1 Ready | 活动 2～4；Ready 2 | 剩下的两个组没有「序号空洞」可言 |
| 建一个 v1 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | 无→n1 v1 NotReady→n1 v1 Ready | 无 | g3 v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 只多一个名额，建一个 |
| 建一个 v1 | N=4 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | n1 v1 Ready | 无→n2 v1 NotReady→n2 v1 Ready | g3 v1 Ready | 活动 3～4；Ready 3～4，结束时 4 | 再建一个；g3 没有动 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0、g3 | g0、g3 |
| 终态各版本的组数 | v1×4 | v1×4 |
| 终态就绪数 | 4 | 4 |

### SG-S02　两个旧版本的组滚到 v2

前提：契约里这是稀疏的 {sg-0, sg-3}。

| 阶段 | 期望 | 列 1 | 列 2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g3 v1 Ready | 活动 2；Ready 2 |  |
| 删 g3（替换）；建一个 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g3 v1 Ready→g3 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | 次序靠后的先 |
| 删 g0（替换）；建一个 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 活动 1～2；Ready 1～2，结束时 2 |  |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | n2 v2 Ready | n1 v2 Ready | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |

### SG-S03　健康的目标版本的组不动

| 阶段 | 期望 | 列 1 | 列 2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g3 v2 Ready | 活动 2；Ready 2 |  |
| 删 g0（替换）；建一个 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n1 v2 NotReady→n1 v2 Ready | g3 v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | 只替换 g0 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | n1 v2 Ready | g3 v2 Ready | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g3 | g3 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |

### SG-S04　滚动中扩容，已有一个多出来的 v2

前提：契约里 g4 是临时 surge，g3 是缩容留下的高序号保留组。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g3 v2 Ready | g4 v2 Ready | 无 | 活动 4；Ready 4 |  |
| 建一个 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g3 v2 Ready | g4 v2 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g1（替换）；建一个 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | g3 v2 Ready | g4 v2 Ready | n1 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g0（替换）；基线推进到 v2 | N=4 U=0 S=1 P=0 目标 v2 基线 v2 | g0 v1 Ready→g0 v1 删除中→无 | n2 v2 Ready | g3 v2 Ready | g4 v2 Ready | n1 v2 Ready | 活动 4～5；Ready 4～5，结束时 4 |  |

结论：**有意变化**。契约把 g4 当临时 surge：先补建 sg-2，最后删掉 g4。这里 g4 就是一个目标版本的组，留下；少建一个、少删一个。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 2　**不同** |
| 删除的组 | 3 | 2　**不同** |
| 保住身份的初始组 | g3 | g3、g4　**不同** |
| 终态各版本的组数 | v2×4 | v2×4 |
| 终态就绪数 | 4 | 4 |

### SG-S05　缩容、扩容并提交坏的 v2，再用 v3 修复

前提：g1 的 deletion cost 低；v2 不会就绪。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 3 |  |
| N 改成 2 | N=2 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→无 | g2 v1 Ready | 活动 2～3；Ready 2～3，结束时 2 | 按 deletion cost 删 g1 |
| 建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | 无→n1 v2 NotReady | g2 v1 Ready | 活动 2～3；Ready 2 | 新增的名额用 v2；它不会就绪，占住唯一的不可用额度 |
| 删 n1（替换未就绪的）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | n1 v2 NotReady→n1 v2 删除中→n2 v3 NotReady→n2 v3 Ready | g2 v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 坏的 v2 现在是过期的、未就绪的，先处理它 |
| 删 g2（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | n2 v3 Ready | g2 v1 Ready→g2 v1 删除中→n3 v3 NotReady→n3 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 健康的旧组按次序从后往前 |
| 删 g0（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready→g0 v1 删除中→n4 v3 NotReady→n4 v3 Ready | n2 v3 Ready | n3 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v3 | n4 v3 Ready | n2 v3 Ready | n3 v3 Ready | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |

### SG-S06　两个组滚到坏的 v2，再用 v3 修复

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=2 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v1 Ready | 活动 5；Ready 5 |  |
| 删 g4（替换）；删 g3（替换）；建一个 v2；建一个 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready→g3 v1 删除中→n1 v2 NotReady | g4 v1 Ready→g4 v1 删除中→n2 v2 NotReady | 活动 3～5；Ready 3～5，结束时 3 | 一批两个；两个 v2 都不就绪，maxScaleDown=0 |
| 阻塞 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v2 NotReady | n2 v2 NotReady | 活动 5；Ready 3 | 不删健康的旧组，也不重建同版本的 v2 |
| 删 n2（替换未就绪的）；删 n1（替换未就绪的）；建一个 v3；建一个 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v2 NotReady→n1 v2 删除中→n3 v3 NotReady→n3 v3 Ready | n2 v2 NotReady→n2 v2 删除中→n4 v3 NotReady→n4 v3 Ready | 活动 3～5；Ready 3～5，结束时 5 | 坏的 v2 变成过期的，先清掉 |
| 删 g2（替换）；删 g1（替换）；建一个 v3；建一个 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n5 v3 NotReady→n5 v3 Ready | g2 v1 Ready→g2 v1 删除中→n6 v3 NotReady→n6 v3 Ready | n3 v3 Ready | n4 v3 Ready | 活动 3～5；Ready 3～5，结束时 5 |  |
| 删 g0（替换）；建一个 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready→g0 v1 删除中→n7 v3 NotReady→n7 v3 Ready | n5 v3 Ready | n6 v3 Ready | n3 v3 Ready | n4 v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v3 | N=5 U=2 S=0 P=0 目标 v3 基线 v3 | n7 v3 Ready | n5 v3 Ready | n6 v3 Ready | n3 v3 Ready | n4 v3 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 7 | 7 |
| 删除的组 | 7 | 7 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×5 | v3×5 |
| 终态就绪数 | 5 | 5 |

### SG-S07　五个组全部未就绪，提交修正版本

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 NotReady | g1 v1 NotReady | g2 v1 NotReady | g3 v1 NotReady | g4 v1 NotReady | 活动 5；Ready 0 | 故障早于滚动 |
| 删 g4（替换未就绪的）；删 g3（替换未就绪的）；建一个 v2；建一个 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 NotReady | g1 v1 NotReady | g2 v1 NotReady | g3 v1 NotReady→g3 v1 删除中→n1 v2 NotReady→n1 v2 Ready | g4 v1 NotReady→g4 v1 删除中→n2 v2 NotReady→n2 v2 Ready | 活动 3～5；Ready 0～2，结束时 2 | maxScaleDown=2：一次只替换两个 |
| 删 g2（替换未就绪的）；删 g1（替换未就绪的）；建一个 v2；建一个 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 NotReady | g1 v1 NotReady→g1 v1 删除中→n3 v2 NotReady→n3 v2 Ready | g2 v1 NotReady→g2 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 3～5；Ready 2～4，结束时 4 |  |
| 删 g0（替换未就绪的）；建一个 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 NotReady→g0 v1 删除中→n5 v2 NotReady→n5 v2 Ready | n3 v2 Ready | n4 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=2 S=0 P=0 目标 v2 基线 v2 | n5 v2 Ready | n3 v2 Ready | n4 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |

### SG-C01　滚动中扩容，S=0

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 开始滚动 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中 | 无 | 无 | 活动 3；Ready 2～3，结束时 2 | 次序最靠后的 g2 的删除已经发出 |
| 建一个 v2；建一个 v2；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 删除中→n3 v2 NotReady→n3 v2 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 无→n2 v2 NotReady→n2 v2 Ready | 活动 3～5；Ready 2～5，结束时 5 | 已发的删除不撤销；能建几个由 cap − active 决定 |
| 删 g1（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 | 新组全部就绪之后才有健康删除的额度 |
| 删 g0（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n5 v2 NotReady→n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |

### SG-C02　滚动中缩容

前提：g2 是在建的 v2，不会就绪。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v2 NotReady | 活动 3；Ready 2 |  |
| 删 g2（缩容）；删 g1（替换） | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中 | g2 v2 NotReady→g2 v2 删除中→无 | 活动 2～3；Ready 1～2，结束时 1 | 缩容先选未就绪的 g2 |
| 建一个 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 无 | 活动 1～2；Ready 1～2，结束时 2 |  |
| 删 g0（替换）；建一个 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 无 | 活动 1～2；Ready 1～2，结束时 2 |  |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | n2 v2 Ready | n1 v2 Ready | 无 | 活动 2；Ready 2 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |

### SG-C03　滚动中扩容，已有一个多出来的 v2

前提：契约里 g3 是临时 surge。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 列 6 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v2 Ready | 无 | 无 | 活动 4；Ready 4 |  |
| 建一个 v2；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v2 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 无→n2 v2 NotReady→n2 v2 Ready | 活动 4～6；Ready 4～6，结束时 6 |  |
| 删 g2（替换）；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n3 v2 NotReady→n3 v2 Ready | g3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 g1（替换）；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n3 v2 Ready | g3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 g0（替换）；基线推进到 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v2 | g0 v1 Ready→g0 v1 删除中→无 | n4 v2 Ready | n3 v2 Ready | g3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 5 |  |

结论：**有意变化**。契约在滚动结束时还要回收一个临时 surge（sg-5）；这里最后一个旧组删掉时数量正好是 N，少建一个、少删一个。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 4　**不同** |
| 删除的组 | 4 | 3　**不同** |
| 保住身份的初始组 | g3 | g3 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |

### SG-C12　滚动中缩容，已有一个多出来的 v2

前提：契约里 g4 是临时 surge。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=4 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 Ready | 活动 5；Ready 5 |  |
| 删 g3（缩容）；删 g2（替换） | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中 | g3 v1 Ready→g3 v1 删除中→无 | g4 v2 Ready | 活动 4～5；Ready 3～5，结束时 3 | 缩容时过期的先：删的是一个 v1，不是最新的 v2 |
| 建一个 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 无 | g4 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 无 | g4 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 g0（替换）；基线推进到 v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v2 | g0 v1 Ready→g0 v1 删除中→无 | n2 v2 Ready | n1 v2 Ready | 无 | g4 v2 Ready | 活动 3～4；Ready 3～4，结束时 3 |  |

结论：**有意变化**。契约最后删掉 sg-4 并把三个正式序号都重建一遍；这里 g4 留下，少建一个、少删一个。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 2　**不同** |
| 删除的组 | 5 | 4　**不同** |
| 保住身份的初始组 | 无 | g4　**不同** |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |

### SG-C15　百分比 U 随扩容重算

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 列 6 | 列 7 | 列 8 | 列 9 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 Ready | 无 | 无 | 无 | 无 | 活动 5；Ready 5 | U=1，至少 4 个就绪 |
| N 改成 9 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 Ready | 无→n1 v2 NotReady | 无→n2 v2 NotReady | 无→n3 v2 NotReady | 无→n4 v2 NotReady | 活动 5～9；Ready 5 | U=2，至少 7 个就绪；新组用 v2 |
| 两个新组就绪 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 Ready | n1 v2 NotReady→n1 v2 Ready | n2 v2 NotReady→n2 v2 Ready | n3 v2 NotReady | n4 v2 NotReady | 活动 9；Ready 5～7，结束时 7 | 就绪 7，正好在底线：不删健康的旧组 |
| 第三个就绪 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 Ready | n1 v2 Ready | n2 v2 Ready | n3 v2 NotReady→n3 v2 Ready | n4 v2 NotReady | 活动 9；Ready 7～8，结束时 8 | 就绪 8：有一个健康删除的额度 |
| 删 g3（替换）；建一个 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready→g3 v1 删除中→n5 v2 NotReady→n5 v2 Ready | g4 v2 Ready | n1 v2 Ready | n2 v2 Ready | n3 v2 Ready | n4 v2 NotReady→n4 v2 Ready | 活动 8～9；Ready 7～9，结束时 9 | g3 和还没就绪的 n4 共用 U=2 |
| 删 g2（替换）；删 g1（替换）；建一个 v2；建一个 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n6 v2 NotReady→n6 v2 Ready | g2 v1 Ready→g2 v1 删除中→n7 v2 NotReady→n7 v2 Ready | n5 v2 Ready | g4 v2 Ready | n1 v2 Ready | n2 v2 Ready | n3 v2 Ready | n4 v2 Ready | 活动 7～9；Ready 7～9，结束时 9 |  |
| 删 g0（替换）；建一个 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n8 v2 NotReady→n8 v2 Ready | n6 v2 Ready | n7 v2 Ready | n5 v2 Ready | g4 v2 Ready | n1 v2 Ready | n2 v2 Ready | n3 v2 Ready | n4 v2 Ready | 活动 8～9；Ready 8～9，结束时 9 |  |
| 基线推进到 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v2 | n8 v2 Ready | n6 v2 Ready | n7 v2 Ready | n5 v2 Ready | g4 v2 Ready | n1 v2 Ready | n2 v2 Ready | n3 v2 Ready | n4 v2 Ready | 活动 9；Ready 9 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 8 | 8 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | g4 | g4 |
| 终态各版本的组数 | v2×9 | v2×9 |
| 终态就绪数 | 9 | 9 |

### SG-C15（坏 v2 分支）　v2 永不就绪

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 列 6 | 列 7 | 列 8 | 列 9 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 NotReady | 无 | 无 | 无 | 无 | 活动 5；Ready 4 |  |
| 建一个 v2；建一个 v2；建一个 v2；建一个 v2 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 NotReady | 无→n1 v2 NotReady | 无→n2 v2 NotReady | 无→n3 v2 NotReady | 无→n4 v2 NotReady | 活动 5～9；Ready 4 | 新组也不就绪 |
| 阻塞 | N=9 U=2 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v2 NotReady | n1 v2 NotReady | n2 v2 NotReady | n3 v2 NotReady | n4 v2 NotReady | 活动 9；Ready 4 | 就绪 4 低于底线 7：不再删健康的 v1 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2、g3、g4 | g0、g1、g2、g3、g4 |
| 终态各版本的组数 | v1×4、v2×5 | v1×4、v2×5 |
| 终态就绪数 | 4 | 4 |

### SG-C16　多出来的组和未就绪的新组同时入账

前提：g2 本来就未就绪；第一个建出来的 v2（契约里的临时 surge）一直不就绪。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 NotReady | 无 | 活动 3；Ready 2 | maxScaleDown=1 |
| 删 g2（替换未就绪的）；建一个 v2；建一个 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 NotReady→g2 v1 删除中→n2 v2 NotReady→n2 v2 Ready | 无→n1 v2 NotReady | 活动 3～4；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n2 v2 Ready | n1 v2 NotReady | 活动 3～4；Ready 2～3，结束时 3 |  |
| 删 g0（替换） | N=3 U=1 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→无 | n3 v2 Ready | n2 v2 Ready | n1 v2 NotReady | 活动 3～4；Ready 2～3，结束时 2 |  |

结论：**有意变化**。账目逐步相同。差别在最后一步：契约把一直不就绪的 sg-3 当临时 surge 回收，正式序号上再建一个；这里它是 N 个目标版本的组之一，留下，终态就绪 2。与 Deployment 相同。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 3　**不同** |
| 删除的组 | 4 | 3　**不同** |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 2　**不同** |

### SG-C04　扩容途中开始 v2

前提：扩容建出的第一个 v1 一直不就绪。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| N 改成 5 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无→n1 v1 NotReady | 无 | 活动 3～4；Ready 3 | 只发出了一个创建 |
| v2 到来 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 NotReady | 无→n2 v2 NotReady | 活动 4～5；Ready 3 | 还缺的一个直接用 v2；在建的 v1 不改 |
| 就绪 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 NotReady | n2 v2 NotReady→n2 v2 Ready | 活动 5；Ready 3～4，结束时 4 | v2 就绪之后，未就绪的旧组先替换，不降低就绪数 |
| 删 n1（替换未就绪的）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 NotReady→n1 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g2（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n3 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g1（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n5 v2 NotReady→n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g0（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n6 v2 NotReady→n6 v2 Ready | n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | n6 v2 Ready | n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n2 v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 6 | 6 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |

### SG-C05　缩容途中开始 v2

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v1 Ready | 活动 5；Ready 5 |  |
| N 改成 3 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v1 Ready→g4 v1 删除中 | 活动 5；Ready 4～5，结束时 4 | 只发出了一个删除 |
| v2 到来 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready→g3 v1 删除中 | g4 v1 删除中 | 活动 5；Ready 3～4，结束时 3 | 缩容先做完：再删一个，过期的先。被缩掉的组消失之前不开始替换 |
| 规模完成 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 删除中→无 | g4 v1 删除中→无 | 活动 3～5；Ready 3 | 三个 v1 都在 |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g0（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n2 v2 Ready | n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | n3 v2 Ready | n2 v2 Ready | n1 v2 Ready | 无 | 无 | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |

### SG-C06　同一请求扩容并更新

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 建一个 v2；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 无→n2 v2 NotReady→n2 v2 Ready | 活动 3～5；Ready 3～5，结束时 5 | 先扩容，新组直接用 v2 |
| 删 g2（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g1（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g0（替换）；建一个 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n5 v2 NotReady→n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v2 | N=5 U=1 S=0 P=0 目标 v2 基线 v2 | n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 5 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |

### SG-C07　同一请求缩容并更新

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v1 Ready | 活动 5；Ready 5 |  |
| 同一请求 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready→g3 v1 删除中 | g4 v1 Ready→g4 v1 删除中 | 活动 5；Ready 3～5，结束时 3 | 先缩容；被缩掉的组消失之前不开始替换 |
| 规模完成 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 删除中→无 | g4 v1 删除中→无 | 活动 3～5；Ready 3 | 三个 v1 都在 |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g0（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n2 v2 Ready | n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | n3 v2 Ready | n2 v2 Ready | n1 v2 Ready | 无 | 无 | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×3 | v2×3 |
| 终态就绪数 | 3 | 3 |

### SG-C13　扩容中开始滚动，带 S

前提：扩容还差一个。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 列 6 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=0 S=1 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | 无 | 无 | 活动 4；Ready 4 |  |
| 建一个 v2；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 无→n2 v2 NotReady→n2 v2 Ready | 活动 4～6；Ready 4～6，结束时 6 |  |
| 删 g3（替换）；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready→g3 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 g2（替换）；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 g1（替换）；建一个 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n5 v2 NotReady→n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 6 |  |
| 删 g0（替换）；基线推进到 v2 | N=5 U=0 S=1 P=0 目标 v2 基线 v2 | g0 v1 Ready→g0 v1 删除中→无 | n5 v2 Ready | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 5～6；Ready 5～6，结束时 5 |  |

结论：**有意变化**。契约里正式的扩容槽和临时 surge 各建一个，surge 最后回收；这里两个都是目标版本的组，最后一个旧组删掉时数量正好是 N。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 6 | 5　**不同** |
| 删除的组 | 5 | 4　**不同** |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×5 | v2×5 |
| 终态就绪数 | 5 | 5 |

### SG-C08　v2 没滚完就提交 v3

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 3 |  |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| v2 的下一步 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中 | n1 v2 Ready | 活动 3；Ready 2～3，结束时 2 | g1 的删除已经发出 |
| 建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 删除中→n2 v3 NotReady→n2 v3 Ready | n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 已发的删除不撤销；空出来的名额直接用 v3 |
| 删 n1（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | n2 v3 Ready | n1 v2 Ready→n1 v2 删除中→n3 v3 NotReady→n3 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 健康的过期组里次序最靠后的是 v2 那个 |
| 删 g0（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready→g0 v1 删除中→n4 v3 NotReady→n4 v3 Ready | n2 v3 Ready | n3 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v3 | n4 v3 Ready | n2 v3 Ready | n3 v3 Ready | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |

### SG-C09　扩容中 v1 → v2 → v3

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| N 改成 5 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无→n1 v1 NotReady→n1 v1 Ready | 无 | 活动 3～4；Ready 3～4，结束时 4 | 只发出了一个创建 |
| v2 到来 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 Ready | 无→n2 v2 NotReady→n2 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 | 最后一个扩容名额用 v2 |
| 删 n2（替换）；建一个 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 Ready | n2 v2 Ready→n2 v2 删除中→n3 v3 NotReady→n3 v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 n1（替换）；建一个 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 Ready→n1 v1 删除中→n4 v3 NotReady→n4 v3 Ready | n3 v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g2（替换）；建一个 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n5 v3 NotReady→n5 v3 Ready | n4 v3 Ready | n3 v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g1（替换）；建一个 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n6 v3 NotReady→n6 v3 Ready | n5 v3 Ready | n4 v3 Ready | n3 v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g0（替换）；建一个 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready→g0 v1 删除中→n7 v3 NotReady→n7 v3 Ready | n6 v3 Ready | n5 v3 Ready | n4 v3 Ready | n3 v3 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 基线推进到 v3 | N=5 U=1 S=0 P=0 目标 v3 基线 v3 | n7 v3 Ready | n6 v3 Ready | n5 v3 Ready | n4 v3 Ready | n3 v3 Ready | 活动 5；Ready 5 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 7 | 7 |
| 删除的组 | 5 | 5 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×5 | v3×5 |
| 终态就绪数 | 5 | 5 |

### SG-C10　缩容中 v1 → v2 → v3

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v1 Ready | 活动 5；Ready 5 |  |
| N 改成 3 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready | g4 v1 Ready→g4 v1 删除中 | 活动 5；Ready 4～5，结束时 4 | 只发出了一个删除 |
| v2 到来 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 Ready→g3 v1 删除中→无 | g4 v1 删除中→无 | 活动 3～5；Ready 3～4，结束时 3 | 先把缩容做完 |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 n1（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | n1 v2 Ready→n1 v2 删除中→n2 v3 NotReady→n2 v3 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n3 v3 NotReady→n3 v3 Ready | n2 v3 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g0（替换）；建一个 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v1 | g0 v1 Ready→g0 v1 删除中→n4 v3 NotReady→n4 v3 Ready | n3 v3 Ready | n2 v3 Ready | 无 | 无 | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v3 | N=3 U=1 S=0 P=0 目标 v3 基线 v3 | n4 v3 Ready | n3 v3 Ready | n2 v3 Ready | 无 | 无 | 活动 3；Ready 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 6 | 6 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |

### SG-C11　扩容中 v2 不就绪，回滚到 v1

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| N 改成 5 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无→n1 v1 NotReady→n1 v1 Ready | 无 | 活动 3～4；Ready 3～4，结束时 4 | 只发出了一个创建 |
| 坏的 v2 到来 | N=5 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 Ready | 无→n2 v2 NotReady | 活动 4～5；Ready 4 | 最后一个名额用 v2，不就绪；不碰健康的 v1 |
| 删 n2（替换未就绪的）；建一个 v1 | N=5 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | n1 v1 Ready | n2 v2 NotReady→n2 v2 删除中→n3 v1 NotReady→n3 v1 Ready | 活动 4～5；Ready 4～5，结束时 5 | v2 过期并且未就绪，自动清掉，用 v1 重建 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×5 | v1×5 |
| 终态就绪数 | 5 | 5 |

### SG-C14　v1 → v2 → v3，多出来的 v2 还在承担容量

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无 | 活动 3；Ready 3 |  |
| v2 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 | 先建一个 v2 |
| 替换开始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中 | n1 v2 Ready | 活动 4；Ready 3～4，结束时 3 | 一个 v1 的删除已经发出 |
| 建一个 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 删除中→n2 v3 NotReady→n2 v3 Ready | n1 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 | 空出来的名额直接用 v3；健康的 v2 留到 v3 就绪 |
| 删 n1（替换）；建一个 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready | n2 v3 Ready | n1 v2 Ready→n1 v2 删除中→n3 v3 NotReady→n3 v3 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 g1（替换）；建一个 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n4 v3 NotReady→n4 v3 Ready | n2 v3 Ready | n3 v3 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 g0（替换）；基线推进到 v3 | N=3 U=0 S=1 P=0 目标 v3 基线 v3 | g0 v1 Ready→g0 v1 删除中→无 | n4 v3 Ready | n2 v3 Ready | n3 v3 Ready | 活动 3～4；Ready 3～4，结束时 3 |  |

结论：**有意变化**。契约里临时 surge 要先换成 v3、最后再回收；这里过期的 v2 只是替换候选之一，少建一个、少删一个。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 5 | 4　**不同** |
| 删除的组 | 5 | 4　**不同** |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v3×3 | v3×3 |
| 终态就绪数 | 3 | 3 |

### 基础过程　P=1，从次序最靠后的开始

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 3 | 受保护的是次序最靠前的一个基线组 |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v2×2 | v1×1、v2×2 |
| 终态就绪数 | 3 | 3 |

### SG-P01　P=1 灰度后扩容

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 无 | 无 | 活动 3；Ready 3 |  |
| 建一个 v2；建一个 v2 | N=5 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 无→n2 v2 NotReady→n2 v2 Ready | 活动 3～5；Ready 3～5，结束时 5 | 基线组已经有 P 个，新组用 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×1、v2×4 | v1×1、v2×4 |
| 终态就绪数 | 5 | 5 |

### SG-P02　扩容并同时调整 P

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 列 6 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready（受保护） | 无 | 无 | 无 | 活动 3；Ready 3 |  |
| 建一个 v1；建一个 v1；建一个 v2 | N=6 U=1 S=0 P=5 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready（受保护） | 无→n1 v1 NotReady（受保护）→n1 v1 Ready（受保护） | 无→n2 v1 NotReady（受保护）→n2 v1 Ready（受保护） | 无→n3 v2 NotReady→n3 v2 Ready | 活动 3～6；Ready 3～6，结束时 6 | 基线组不足 P 的部分用 v1 补，其余用 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 3 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×5、v2×1 | v1×5、v2×1 |
| 终态就绪数 | 6 | 6 |

### SG-P03（上半）　缩容穿过保护区

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready（受保护） | g3 v2 Ready | g4 v2 Ready | 活动 5；Ready 5 |  |
| 删 g2（缩容）；删 g4（缩容）；删 g3（缩容） | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready→g2 v1 删除中→无 | g3 v2 Ready→g3 v2 删除中→无 | g4 v2 Ready→g4 v2 删除中→无 | 活动 2～5；Ready 2～5，结束时 2 | 多于 P 的那个基线组先删，再删 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 0 | 0 |
| 删除的组 | 3 | 3 |
| 保住身份的初始组 | g0、g1 | g0、g1 |
| 终态各版本的组数 | v1×2 | v1×2 |
| 终态就绪数 | 2 | 2 |

### SG-P03（下半）　缩容先删未就绪的受保护组，再扩回

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 NotReady（受保护） | g2 v2 Ready | 活动 3；Ready 2 |  |
| 删 g1（缩容） | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 NotReady（受保护）→g1 v1 删除中→无 | g2 v2 Ready | 活动 2～3；Ready 2 | 未就绪的先 |
| 建一个 v1 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | 无→n1 v1 NotReady（受保护）→n1 v1 Ready（受保护） | g2 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 基线组只剩 1 个，少于 P=2：新组用 v1 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g2 | g0、g2 |
| 终态各版本的组数 | v1×2、v2×1 | v1×2、v2×1 |
| 终态就绪数 | 3 | 3 |

### SG-P04　百分比 P 因扩容变大

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v2 Ready | 无 | 无 | 活动 3；Ready 3 | P=2 |
| 建一个 v1；建一个 v2 | N=5 U=1 S=0 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v2 Ready | 无→n1 v1 NotReady（受保护）→n1 v1 Ready（受保护） | 无→n2 v2 NotReady→n2 v2 Ready | 活动 3～5；Ready 3～5，结束时 5 | P=3：基线组少一个，第一个新组用 v1 |

结论：**有意变化**。契约按序号：新序号 3、4 都不小于 P，都用 v2，结果是 2 个 v1。按数量：P=3 表示保留 3 个基线组，扩容出来的第一个用 v1。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×2、v2×3 | v1×3、v2×2　**不同** |
| 终态就绪数 | 5 | 5 |

### SG-P05　百分比 P 因缩容变小

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=5 U=1 S=0 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready（受保护） | g3 v2 Ready | g4 v2 Ready | 活动 5；Ready 5 | P=3 |
| 删 g2（缩容）；删 g4（缩容） | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready→g2 v1 删除中→无 | g3 v2 Ready | g4 v2 Ready→g4 v2 删除中→无 | 活动 3～5；Ready 3～5，结束时 3 | P=2：多出来的一个 v1 反正要换，缩容先删它 |

结论：**有意变化**。终态相同（2 个 v1、1 个 v2）。契约先删两个 v2，再把解除保护的 v1 滚成 v2：删 3 建 1。这里缩容直接删掉那个多出来的 v1 和一个 v2：删 2 建 0。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 0　**不同** |
| 删除的组 | 3 | 2　**不同** |
| 保住身份的初始组 | g0、g1 | g0、g1、g3　**不同** |
| 终态各版本的组数 | v1×2、v2×1 | v1×2、v2×1 |
| 终态就绪数 | 3 | 3 |

### SG-P06　在途时调大 P

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 在途 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 删除中 | g2 v2 Ready | 活动 3；Ready 2 | g1 的删除已经发出 |
| 建一个 v1 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 删除中→n1 v1 NotReady（受保护）→n1 v1 Ready（受保护） | g2 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 基线组少于 P：空出来的名额用 v1；已有的 v2 不回退 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g2 | g0、g2 |
| 终态各版本的组数 | v1×2、v2×1 | v1×2、v2×1 |
| 终态就绪数 | 3 | 3 |

### SG-P07　调小 P 的同时提交 v3

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v2 Ready | 活动 3；Ready 3 |  |
| 删 g2（替换）；建一个 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v2 Ready→g2 v2 删除中→n1 v3 NotReady→n1 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | v2 既不是基线也不是目标，先换 |
| 删 g1（替换）；建一个 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready→g1 v1 删除中→n2 v3 NotReady→n2 v3 Ready | n1 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 多于 P 的那个 v1 直接到 v3 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v3×2 | v1×1、v3×2 |
| 终态就绪数 | 3 | 3 |

### SG-P08　百分比 P、S 和扩容

前提：g3 还没就绪。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready | g3 v2 NotReady | 无 | 活动 4；Ready 3 | P=2；U=0，不删 g2 |
| 建一个 v2 | N=5 U=0 S=1 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready（受保护） | g3 v2 NotReady→g3 v2 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 活动 4～5；Ready 3～5，结束时 5 | P=3：三个 v1 都受保护，没有要替换的；补一个 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2、g3 | g0、g1、g2、g3 |
| 终态各版本的组数 | v1×3、v2×2 | v1×3、v2×2 |
| 终态就绪数 | 5 | 5 |

### SG-P09　受保护的组故障也占预算

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 故障 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 NotReady（受保护） | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 2 | 受保护的是次序最靠前的 g0，它未就绪 |
| 阻塞 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 NotReady（受保护） | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 2 | 就绪 2 正好在底线；g0 受保护，不是替换候选 |
| g0 恢复 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 NotReady（受保护）→g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 2～3，结束时 3 |  |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v2×2 | v1×1、v2×2 |
| 终态就绪数 | 3 | 3 |

### SG-P10　多出来的 v2 被扩容吸收后继续灰度

前提：契约里 g3 是临时 surge。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 列 5 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready | g3 v2 Ready | 无 | 活动 4；Ready 4 |  |
| 建一个 v2 | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready | g3 v2 Ready | 无→n1 v2 NotReady→n1 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g2（替换）；建一个 v2 | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n2 v2 NotReady→n2 v2 Ready | g3 v2 Ready | n1 v2 Ready | 活动 4～5；Ready 4～5，结束时 5 |  |
| 删 g1（替换） | N=4 U=0 S=1 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready→g1 v1 删除中→无 | n2 v2 Ready | g3 v2 Ready | n1 v2 Ready | 活动 4～5；Ready 4～5，结束时 4 |  |

结论：**有意变化**。契约在结束时回收临时 surge（sg-4）；这里少建一个、少删一个。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 2　**不同** |
| 删除的组 | 3 | 2　**不同** |
| 保住身份的初始组 | g0、g3 | g0、g3 |
| 终态各版本的组数 | v1×1、v2×3 | v1×1、v2×3 |
| 终态就绪数 | 4 | 4 |

### SG-P11　P 等于 N 时提交 v2，再调小 P

前提：契约里这是稀疏的 {sg-0, sg-3}。

| 阶段 | 期望 | 列 1 | 列 2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g3 v1 Ready（受保护） | 活动 2；Ready 2 |  |
| 灰度停点 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g3 v1 Ready（受保护） | 活动 2；Ready 2 | 两个基线组都在 P 之内，没有要替换的 |
| 删 g3（替换）；建一个 v2 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g3 v1 Ready→g3 v1 删除中→n1 v2 NotReady→n1 v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | 次序靠后的 g3 解除保护 |

结论：**有意变化**。契约里 sg-3 的序号不小于 P，要替换；替换出来的容量落到序号 1，又受保护，用 v1：白建白删一次。按数量没有这一次。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 1　**不同** |
| 删除的组 | 2 | 1　**不同** |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v2×1 | v1×1、v2×1 |
| 终态就绪数 | 2 | 2 |

### SG-R01　灰度中只改 Role 的副本数

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 3；Ready 3 |  |
| Role 副本数 1 → 2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护）→g0 v1 NotReady（受保护） | g1 v2 Ready→g1 v2 NotReady | g2 v2 Ready→g2 v2 NotReady | 活动 3；Ready 0～3，结束时 0 | `spec` 下发到三个组，各组自己建新成员；三个组可以同时未就绪。组一级没有动作 |
| 各组就绪 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 NotReady（受保护）→g0 v1 Ready（受保护） | g1 v2 NotReady→g1 v2 Ready | g2 v2 NotReady→g2 v2 Ready | 活动 3；Ready 0～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 0 | 0 |
| 删除的组 | 0 | 0 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×1、v2×2 | v1×1、v2×2 |
| 终态就绪数 | 3 | 3 |

### RC-01　整组恢复中断后重启

前提：SG 模式，`ServingGroupRecreate`。先提交 v2 并且 P=2（没有可更新的组）。g0 的一个成员 Failed。

| 阶段 | 期望 | 列 1 | 列 2 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- |
| 初始 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | 活动 2；Ready 2 | 两个组都在 P 之内 |
| g0 的成员故障 | N=2 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 Ready（受保护）→g0 v1 失败 | g1 v1 Ready（受保护） | 活动 2；Ready 1～2，结束时 1 | ServingGroup controller 写上 Failed，删故障的成员 |
| controller 停着，P 改成 1 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 失败 | g1 v1 Ready（受保护） | 活动 2；Ready 1 | 对象上留着的是：g0 Failed、还没清空 |
| 新进程 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 失败→g0 v1 删除中 | g1 v1 Ready（受保护） | 活动 2；Ready 1 | 读到 Failed 和 Drained：删 g0。不需要知道是谁开始的 |
| g0 消失 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 删除中→n1 v2 NotReady→n1 v2 Ready | g1 v1 Ready（受保护） | 活动 1～2；Ready 1～2，结束时 2 | 非目标的组还有 1 个，已经够 P=1：替换的组用 v2。g1 受保护，不动 |
| 停点 | N=2 U=1 S=0 P=1 目标 v2 基线 v1 | n1 v2 Ready | g1 v1 Ready（受保护） | 活动 2；Ready 2 | 1 个 v1、1 个 v2 |
| 删 g1（替换）；建一个 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v1 | n1 v2 Ready | g1 v1 Ready→g1 v1 删除中→n2 v2 NotReady→n2 v2 Ready | 活动 1～2；Ready 1～2，结束时 2 | g1 解除保护 |
| 基线推进到 v2 | N=2 U=1 S=0 P=0 目标 v2 基线 v2 | n1 v2 Ready | n2 v2 Ready | 活动 2；Ready 2 |  |

结论：**有意变化**。契约按序号：组 0 的序号小于 P，按历史 v1 重建；然后组 1（序号不小于 P）滚成 v2；P 到 0 时组 0 再滚一次。这里幸存的 g1 占了受保护的名额，替换的组直接用 v2：少建一个、少删一个，健康的 g1 在 P=1 的阶段不动。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 3 | 2　**不同** |
| 删除的组 | 3 | 2　**不同** |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×2 | v2×2 |
| 终态就绪数 | 2 | 2 |

### EX-1　受保护的组是第三个版本

前提：上一轮 v2 因为 g2 没就绪而没有完成，基线还是 v1；现在提交 v3。契约 0.5、A.3 第 2 条：现存的受保护组不论版本都不动。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | g0 v2 Ready（受保护） | g1 v2 Ready（受保护） | g2 v2 NotReady | 活动 3；Ready 2 |  |
| 删 g2（替换未就绪的）；建一个 v3 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | g0 v2 Ready（受保护） | g1 v2 Ready（受保护） | g2 v2 NotReady→g2 v2 删除中→n1 v3 NotReady→n1 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 受保护的是最靠前的两个非目标组，不问它是不是基线版本；只换 g2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 1 | 1 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g1 | g0、g1 |
| 终态各版本的组数 | v2×2、v3×1 | v2×2、v3×1 |
| 终态就绪数 | 3 | 3 |

### EX-2　多出一个组时调大 P，受保护的组 deletion cost 最低

前提：g0 的 deletion cost 最低。契约里 g3 是临时 surge，P 调到 3 之后没有可更新的组，surge 被回收。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=0 S=1 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v2 Ready | 活动 4；Ready 4 |  |
| 删 g3（缩容） | N=3 U=0 S=1 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready（受保护） | g2 v1 Ready（受保护） | g3 v2 Ready→g3 v2 删除中→无 | 活动 3～4；Ready 3～4，结束时 3 | 三个 v1 都受保护，没有过期的组，cap=N：缩掉一个。受保护的排在 deletion cost 前面，删的是 v2 |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 0 | 0 |
| 删除的组 | 1 | 1 |
| 保住身份的初始组 | g0、g1、g2 | g0、g1、g2 |
| 终态各版本的组数 | v1×3 | v1×3 |
| 终态就绪数 | 3 | 3 |

### EX-3　`spec` 刚下发、组还没有观测到时提交新版本

前提：g2 真的未就绪。用户先改 `recoveryPolicy`（`spec` 下发，四个组短暂未观测），紧接着提交 v2。契约 0.4：先处理合法的旧 NotReady。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=4 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 NotReady | g3 v1 Ready | 活动 4；Ready 3 |  |
| 四个组都未观测 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 未观测 | g1 v1 未观测 | g2 v1 未观测 | g3 v1 未观测 | 活动 4；Ready 0 | 未观测的组占位置、不算就绪，但也不是替换候选：这一轮不动 |
| 删 g2（替换未就绪的）；建一个 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 NotReady→g2 v1 删除中→n1 v2 NotReady→n1 v2 Ready | g3 v1 Ready | 活动 3～4；Ready 3～4，结束时 4 | 观测回来：先替换真的未就绪的 g2 |
| 删 g3（替换）；建一个 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | n1 v2 Ready | g3 v1 Ready→g3 v1 删除中→n2 v2 NotReady→n2 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 g1（替换）；建一个 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 删 g0（替换）；建一个 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 删除中→n4 v2 NotReady→n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 3～4；Ready 3～4，结束时 4 |  |
| 基线推进到 v2 | N=4 U=1 S=0 P=0 目标 v2 基线 v2 | n4 v2 Ready | n3 v2 Ready | n1 v2 Ready | n2 v2 Ready | 活动 4；Ready 4 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 4 | 4 |
| 删除的组 | 4 | 4 |
| 保住身份的初始组 | 无 | 无 |
| 终态各版本的组数 | v2×4 | v2×4 |
| 终态就绪数 | 4 | 4 |

### EX-4　受保护的基线组被重建过，再提交第三个版本

前提：灰度停点 {g0 v1 受保护, g1 v2, g2 v2} 上 g0 被整组恢复过：它是按基线补出来的，创建得最晚。契约按序号，序号 0 上的 v1 受保护。现在提交 v3，P 仍是 1。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 3；Ready 3 | g0 创建得最晚 |
| 删 g2（替换）；建一个 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready→g2 v2 删除中→n1 v3 NotReady→n1 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 | 受保护的名额先给基线版本的组：留下的是 v1 的 g0，两个 v2 被替换 |
| 删 g1（替换）；建一个 v3 | N=3 U=1 S=0 P=1 目标 v3 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready→g1 v2 删除中→n2 v3 NotReady→n2 v3 Ready | n1 v3 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |

结论：**一致**。

| 可观察量 | 契约 2.12 | 本设计 |
| --- | --- | --- |
| 创建的组 | 2 | 2 |
| 删除的组 | 2 | 2 |
| 保住身份的初始组 | g0 | g0 |
| 终态各版本的组数 | v1×1、v3×2 | v1×1、v3×2 |
| 终态就绪数 | 3 | 3 |

## 2. 契约里没有的场景

### NEW-1　整组恢复：失败的组被替换

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 3 |  |
| g1 丢了成员 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready→g1 v1 失败 | g2 v1 Ready | 活动 3；Ready 2～3，结束时 2 | ServingGroup controller 写上失败；它先只删故障的成员。失败的组仍占一个位置 |
| 故障成员消失 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 失败→g1 v1 删除中 | g2 v1 Ready | 活动 3；Ready 2 | ModelServing controller 这时才删 g1（前台级联） |
| g1 的 Pod 全部消失 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 删除中→n1 v1 NotReady | g2 v1 Ready | 活动 2～3；Ready 2 | 这时才建替换的组，名字是新的 |
| 就绪 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | n1 v1 NotReady→n1 v1 Ready | g2 v1 Ready | 活动 3；Ready 2～3，结束时 3 |  |


### NEW-2　灰度停点上一个 v2 的组失败

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 3；Ready 3 |  |
| g2 失败并被替换 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready→g2 v2 失败→g2 v2 删除中→n1 v2 NotReady→n1 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 基线组已经有 P 个：替换的组用 v2 |


### NEW-3　灰度停点上受保护的组失败

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 3；Ready 3 |  |
| g0 失败并被替换 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护）→g0 v1 失败→g0 v1 删除中→n1 v1 NotReady（受保护）→n1 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 基线组少于 P：替换的组用 v1。保护的是数量，不是哪一个组 |


### NEW-4　接管之后多建了一个组

前提：g3 是新进程在缓存没跟上时多建的。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 列 4 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 多建之后 | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 NotReady | 活动 4；Ready 3 | active 4 超过 N |
| 删 g3（缩容） | N=3 U=1 S=0 P=0 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | g3 v1 NotReady→g3 v1 删除中→无 | 活动 3～4；Ready 3 | 没有过期的组，cap=N：缩回。未就绪的、最新的先删，健康的老组不动 |


### NEW-5　滚动中一个旧组失败

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 3 |  |
| g0 失败 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 Ready→g0 v1 失败→g0 v1 删除中 | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 2～3，结束时 2 | 失败的组被删；同一轮里就绪 2 正好在底线，不再删健康的旧组 |
| 建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | g0 v1 删除中→n1 v2 NotReady→n1 v2 Ready | g1 v1 Ready | g2 v1 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g2（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | n1 v2 Ready | g1 v1 Ready | g2 v1 Ready→g2 v1 删除中→n2 v2 NotReady→n2 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 删 g1（替换）；建一个 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v1 | n1 v2 Ready | g1 v1 Ready→g1 v1 删除中→n3 v2 NotReady→n3 v2 Ready | n2 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
| 基线推进到 v2 | N=3 U=1 S=0 P=0 目标 v2 基线 v2 | n1 v2 Ready | n3 v2 Ready | n2 v2 Ready | 活动 3；Ready 3 |  |


### NEW-6　各 Role 的副本数都是 0 时提交新版本，再调回来

前提：SG 模式。用户把每个 Role 的副本数调成 0（组还在，名下没有 Pod），提交 v2，P=1，再把副本数调回来。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v1 基线 v1 | g0 v1 Ready | g1 v1 Ready | g2 v1 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=1 目标 v1 基线 v1 | g0 v1 Ready→g0 v1 空 | g1 v1 Ready→g1 v1 空 | g2 v1 Ready→g2 v1 空 | 活动 3；Ready 0～3，结束时 0 | 组对象留着，名下没有 Pod，不算就绪 |
| 提交 v2 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 空（受保护） | g1 v1 空→g1 v2 空 | g2 v1 空→g2 v2 空 | 活动 3；Ready 0 | 空组没有 Pod 可扰动：版本按新建的规则重新指派，P 个名额给基线，其余给 v2。不删对象，不占预算 |
| 副本数调回来 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 空（受保护）→g0 v1 NotReady（受保护）→g0 v1 Ready（受保护） | g1 v2 空→g1 v2 NotReady→g1 v2 Ready | g2 v2 空→g2 v2 NotReady→g2 v2 Ready | 活动 3；Ready 0～3，结束时 3 | 各组按自己的版本建 Pod：一个 v1，两个 v2。没有哪个组先起旧版本再被替换。副本数和模板在同一次请求里改也是这个过程：空组按组自己的 `spec` 判定，改版本（G3）先于下发副本数（G5） |


### NEW-9　空组：P 调大之后，目标版本的空组回到基线

前提：灰度停点。用户把每个 Role 的副本数调成 0，把 P 调到 2，再把副本数调回来。契约 0.6：扩回来视为新建，保护区按基线。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 3；Ready 3 |  |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护）→g0 v1 空（受保护） | g1 v2 Ready→g1 v2 空 | g2 v2 Ready→g2 v2 空 | 活动 3；Ready 0～3，结束时 0 |  |
| P 调到 2 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 空（受保护） | g1 v2 空→g1 v1 空（受保护） | g2 v2 空 | 活动 3；Ready 0 | 两个名额给基线：g0 已经是 v1，再把 g1 改回 v1。有 Pod 的目标组不会这样回退；空组没有 Pod，等于还没建 |
| 副本数调回来 | N=3 U=1 S=0 P=2 目标 v2 基线 v1 | g0 v1 空（受保护）→g0 v1 NotReady（受保护）→g0 v1 Ready（受保护） | g1 v1 空（受保护）→g1 v1 NotReady（受保护）→g1 v1 Ready（受保护） | g2 v2 空→g2 v2 NotReady→g2 v2 Ready | 活动 3；Ready 0～3，结束时 3 | 两个 v1，一个 v2，与契约按序号的结果相同 |


### NEW-10　空组：受保护的是中间版本时回到基线

前提：EX-1 的形状：基线还是 v1，三个组都是 v2，目标 v3，P=2。用户把每个 Role 的副本数调成 0，再调回来。契约 0.6：保护区按基线新建。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | g0 v2 Ready（受保护） | g1 v2 Ready（受保护） | g2 v2 Ready | 活动 3；Ready 3 | 有 Pod 时，受保护的两个 v2 不动 |
| Role 的副本数都调成 0 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | g0 v2 Ready（受保护）→g0 v2 空（受保护）→g0 v1 空（受保护） | g1 v2 Ready（受保护）→g1 v2 空（受保护）→g1 v1 空（受保护） | g2 v2 Ready→g2 v2 空→g2 v3 空 | 活动 3；Ready 0～3，结束时 0 | 空组按新建的规则指派：两个名额给基线 v1，其余给 v3 |
| 副本数调回来 | N=3 U=1 S=0 P=2 目标 v3 基线 v1 | g0 v1 空（受保护）→g0 v1 NotReady（受保护）→g0 v1 Ready（受保护） | g1 v1 空（受保护）→g1 v1 NotReady（受保护）→g1 v1 Ready（受保护） | g2 v3 空→g2 v3 NotReady→g2 v3 Ready | 活动 3；Ready 0～3，结束时 3 | 两个 v1，一个 v3 |


### NEW-7　灰度还没滚完时受保护的组失败

前提：g1 还没轮到。契约按序号：序号 0 按 v1 重建，g1 随后滚成 v2。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v1 Ready | g2 v2 Ready | 活动 3；Ready 3 |  |
| g0 失败并被替换 | N=3 U=1 S=0 P=1 目标 v2 基线 v1 | g0 v1 Ready（受保护）→g0 v1 失败→g0 v1 删除中→n1 v2 NotReady→n1 v2 Ready | g1 v1 Ready→g1 v1 Ready（受保护） | g2 v2 Ready | 活动 2～3；Ready 2～3，结束时 3 | 非目标的组只剩 g1，正好 P 个：g1 成为受保护的那一个，替换的组用 v2。留在旧版本的个数相同，是哪一个不同；比契约少建一个 |


### NEW-8　P=N 的停点上把模板改回基线

前提：P=0 时滚了两个组，再把 P 调到 3 冻结现状。契约按序号：三个序号都受保护，改回 v1 之后 g1、g2 留在 v2，直到 P 调小。

| 阶段 | 期望 | 列 1 | 列 2 | 列 3 | 组数 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| 初始 | N=3 U=1 S=0 P=3 目标 v2 基线 v1 | g0 v1 Ready（受保护） | g1 v2 Ready | g2 v2 Ready | 活动 3；Ready 3 | 三个非目标的组都受保护 |
| 删 g2（替换）；建一个 v1 | N=3 U=1 S=0 P=3 目标 v1 基线 v1 | g0 v1 Ready | g1 v2 Ready | g2 v2 Ready→g2 v2 删除中→n1 v1 NotReady→n1 v1 Ready | 活动 2～3；Ready 2～3，结束时 3 | 目标等于基线：没有『留在旧版本』这回事，g1、g2 是过期的，按预算换回 v1 |
| 删 g1（替换）；建一个 v1 | N=3 U=1 S=0 P=3 目标 v1 基线 v1 | g0 v1 Ready | g1 v2 Ready→g1 v2 删除中→n2 v1 NotReady→n2 v1 Ready | n1 v1 Ready | 活动 2～3；Ready 2～3，结束时 3 |  |
