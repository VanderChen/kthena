# 附录：六份独立分析报告

方案分两轮写成，每一轮有三份由不同分析者独立完成的报告。这里原样收录，供核对正文结论的来源。

**报告不是结论。** 它们写在对应那一轮的 Kind 刻画之前或同时，依据是源码、包级实测和上游资料；其中一部分判断后来被 Kind 改掉了。
以正文（[01](../01-design.md)、[02](../02-servinggroup-cr.md)、[04](../04-evidence.md)）为准，下面的表列出了差异。

| 文件 | 轮次 | 角色 | 回答的问题 |
| --- | --- | --- | --- |
| [round1-proposal-analysis.md](round1-proposal-analysis.md) | 1：使用方案 | 方案分析 | 解释器和重调度的各个选项、取舍、验证计划 |
| [round1-community-research.md](round1-community-research.md) | 1 | 社区调研 | Karmada 和可比项目怎么处理同样的问题，有没有先例 |
| [round1-behavior-regression.md](round1-behavior-regression.md) | 1 | 行为和回归 | 单集群和经 Karmada 管理相比，行为变了什么，哪些是回归 |
| [round2-proposal-analysis.md](round2-proposal-analysis.md) | 2：ServingGroup CR | 方案分析 | 按组分发有哪几种做法，现有 CR 原型能不能当单元 |
| [round2-community-research.md](round2-community-research.md) | 2 | 社区调研 | 上游对「一个对象一个集群」和子 CR 层级的态度和先例 |
| [round2-behavior-regression.md](round2-behavior-regression.md) | 2 | 行为和回归 | 两种方案在滚动、伸缩、恢复、规模上的行为差异 |

## 报告结论的去向

第一轮：

| 报告里的判断 | 去向 |
| --- | --- |
| `componentResource` 和 `replicaRevision` 同时实现并开门控时，成员副本数变成 0 | 采纳，Kind 证实 |
| 资源需求用整组求和、不带 `nodeClaim`，并写明适用条件 | 采纳，Kind 证实 |
| 默认资源模型下 GPU 请求估算为 0 | 采纳，Kind 证实 |
| 成员按自己的份额校验，拒绝拆分后的模板 | 采纳为文档约束，Kind 证实 |
| 放不全就一个不放 | 采纳，Kind 证实 |
| 过期状态会导致重复降副本 | 采纳为判定规则；现象本身没有在 Kind 上复现 |
| 降副本之后调度器会把副本还给同一个集群 | **被 Kind 改掉。** 卡住的 Pod 被计入已分配资源，原集群估算为 0，副本去了别的集群。真正的问题是没有去处时的循环 |
| 重调度先做独立控制器 | **没有采纳。** 它的主要理由是上一条；改 karmada-descheduler 更简单，独立控制器留作后备 |
| 没装解释器时写 `Divided`，装上之后能否自动收敛（报告列为未验证） | Kind 的结果：不会平滑收敛。所有集群的组先被删除，调度成功则重建，调度失败则停在 0 |
| 没有找到「多 Pod 组因调度不上而自动换集群」的先例 | 记录为空白。对其他项目的描述只依据官方文档，没有读源码 |

第二轮：

| 报告里的判断 | 去向 |
| --- | --- |
| 现有 CR 原型原样不能当分发单元 | 采纳 |
| 运行中的单元不留在原集群 | 采纳，Kind 证实，并且多发现一种触发方式（策略的集群名单变化） |
| 批量创建会扎堆，超卖保护能挡住 | 采纳，Kind 证实 |
| 应用故障转移在没有去处时会循环 | 采纳，Kind 证实；实际周期比报告估计的短，取决于宽限时间 |
| 节点级的组件集估算是 First Fit，可能低估 | 采纳，据此修正了 01 的 4.3 |
| 一个候选集群连不上 scheduler-estimator，这次调度的所有集群都退回汇总估算 | 采纳，据此修正了 01 的 5.3 |
| 暂缓把解释器提交为 Karmada 内置定制 | 采纳 |
| 用按组分发替换第 2、3 阶段 | 部分采纳：列为备选方向，「单元会被搬走」解决之前不替换 |
| 按组分发让滚动预算、`partition`、缩容选择重新变成全局的 | 记为收益，是推断，没有验证 |
| 自动扩缩容在按组分发下都不能直接用 | 采纳，列为待决策的事实问题 |
| 按组分发的规模风险（上游 #7062） | 记录，没有自己测 |

## 读报告时需要知道的

报告是工作过程中的文档，用了当时的名字：

| 报告里的写法 | 指的是 |
| --- | --- |
| `DESIGN.md` | [01-design.md](../01-design.md) |
| `PROPOSAL_COMMIT.md` | [03-decisions.md](../03-decisions.md) 和 [04-evidence.md](../04-evidence.md) |
| `evidence/` | [artifacts/](../artifacts/) |
| `<workspace>/karmada`、`<workspace>/kthena-014` | Karmada 和 kthena 在上述基线 commit 的检出目录 |
| `kthena-010`、`kthena-010:`、任务 010 的原型 | ServingGroup CR 原型：本 fork 的分支 `feat/010-modelserving-servinggroup-cr-impl` @ `98fcbf01`（正文记作 `sgcr:`） |
| `010-doc:` | 本 fork 的分支 `feat/008-modelserving-servinggroup-cr-design` 的 `docs/proposal/modelserving-servinggroup-cr/` |
| 任务 014、本任务 | 这份提案 |
| 协调者 | 汇总三份报告、做 Kind 刻画并写正文的人 |
| 用户 | 提出问题的人；讨论行为时也指 ModelServing 的使用者 |
| 草稿、协调者草稿 | 01 在第一轮报告之前的版本 |
| 选项 M / G、G-a / G-b / G-c | 同正文的方案 M / 方案 G 及其做法 |

发布时的改动：本机的绝对路径换成了 `<workspace>/`；去掉了三处对未公开材料的引用，以及和内容无关的过程记录。
除此之外没有改动报告的内容，所以其中被 Kind 改掉的判断仍然保留原样。
