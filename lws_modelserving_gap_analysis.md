# LWS与ModelServing表意Annotation/Label Gap分析报告

## 1. 概述

本报告分析LeaderWorkerSet (LWS)与Kthena ModelServing在表意annotation和label上的差异。LWS相当于ModelServing中的role层级，两者都是推理服务的部署workload。

**重要发现**:
- ⚠️ **常量定义位置分散**: ModelServing的表意常量分散在多个文件中，而非集中在types定义文件：
  - Labels定义在: `pkg/apis/workload/v1alpha1/labels.go`
  - 环境变量定义在: `pkg/apis/workload/v1alpha1/model_serving_types.go`
  - Annotations定义在: `pkg/model-serving-controller/controller/binpack_scaledown.go`
  - Volcano集成在: `pkg/model-serving-controller/gangscheduling/manager.go`
- ✅ **核心功能已覆盖**: 通过环境变量和labels实现了主要功能
- ❌ **部分便捷性标识缺失**: group-index、group-hash等便于查询和操作的labels未实现

## 2. 核心概念映射

| LWS概念 | ModelServing概念 | 说明 |
|---------|-----------------|------|
| LeaderWorkerSet | ModelServing | 顶层资源对象 |
| Group | ServingGroup | 工作组 |
| Leader Pod | Entry Pod | 主节点/入口节点 |
| Worker Pod | Worker Pod | 工作节点 |
| Group Index | Group Name | 组标识 |
| Worker Index | Worker Index | Worker索引 |

## 3. Labels对比分析

**重要说明**: ModelServing的Labels主要定义在`pkg/apis/workload/v1alpha1/labels.go`中，而非types定义文件。

### 3.1 已实现的Labels

**ModelServing Labels完整列表**（定义位置：`pkg/apis/workload/v1alpha1/labels.go`）：
- `ModelServingNameLabelKey = "modelserving.volcano.sh/name"` - 记录ModelServing名称
- `GroupNameLabelKey = "modelserving.volcano.sh/group-name"` - 记录ServingGroup名称
- `RoleLabelKey = "modelserving.volcano.sh/role"` - 记录Role名称
- `RoleIDKey = "modelserving.volcano.sh/role-id"` - 记录Role实例ID
- `EntryLabelKey = "modelserving.volcano.sh/entry"` - 标识Entry Pod
- `RevisionLabelKey = "modelserving.volcano.sh/revision"` - 记录模板版本哈希

| LWS Label | LWS值 | ModelServing Label | ModelServing值 | 映射关系 | 备注 |
|-----------|------|-------------------|----------------|---------|------|
| SetNameLabelKey | `leaderworkerset.sigs.k8s.io/name` | ModelServingNameLabelKey | `modelserving.volcano.sh/name` | ✅ 等价 | 记录资源名称 |
| WorkerIndexLabelKey | `leaderworkerset.sigs.k8s.io/worker-index` | - | - | ✅ 通过环境变量实现 | ModelServing通过WorkerIndexEnv环境变量实现 |
| RevisionKey | `leaderworkerset.sigs.k8s.io/template-revision-hash` | RevisionLabelKey | `modelserving.volcano.sh/revision` | ✅ 等价 | 模板版本跟踪 |

### 3.2 ModelServing特有的Labels

| Label | 值 | 用途 | 与LWS对比 |
|-------|-----|------|----------|
| GroupNameLabelKey | `modelserving.volcano.sh/group-name` | 记录组名称 | LWS使用GroupIndexLabelKey记录数字索引，ModelServing使用完整名称 |
| RoleLabelKey | `modelserving.volcano.sh/role` | 记录角色名称 | LWS中leader/worker通过不同方式区分，ModelServing支持多角色 |
| RoleIDKey | `modelserving.volcano.sh/role-id` | 记录角色实例ID | LWS无此概念，ModelServing支持同角色多副本 |
| EntryLabelKey | `modelserving.volcano.sh/entry` | 标识entry pod | LWS通过命名规则区分leader，ModelServing显式标记 |

### 3.3 LWS特有但ModelServing未实现的Labels

| LWS Label | 值 | 用途 | ModelServing状态 |
|-----------|-----|------|-----------------|
| GroupIndexLabelKey | `leaderworkerset.sigs.k8s.io/group-index` | 记录组索引（数字） | ❌ 未实现 - 使用GroupNameLabelKey（字符串名称）替代 |
| GroupUniqueHashLabelKey | `leaderworkerset.sigs.k8s.io/group-key` | 同组pod的唯一哈希标识 | ❌ 未实现 |
| SubGroupIndexLabelKey | `leaderworkerset.sigs.k8s.io/subgroup-index` | 子组索引 | ❌ 未实现 - ModelServing无SubGroup概念 |
| SubGroupUniqueHashLabelKey | `leaderworkerset.sigs.k8s.io/subgroup-key` | 子组唯一哈希标识 | ❌ 未实现 - ModelServing无SubGroup概念 |

## 4. Annotations对比分析

**重要说明**: ModelServing中部分annotation定义在代码中（controller/utils），未体现在types定义文件中。

### 4.1 ModelServing已实现的Annotations（代码中定义）

| Annotation常量名 | 值/来源 | 定义位置 | 用途 | 对应LWS |
|-----------------|--------|---------|------|---------|
| PodDeletionCostAnnotation | `corev1.PodDeletionCost` ("controller.kubernetes.io/pod-deletion-cost") | pkg/model-serving-controller/controller/binpack_scaledown.go:28 | Binpack缩容策略，标记Pod删除成本 | ❌ LWS未实现 |
| schedulingv1beta1.KubeGroupNameAnnotationKey | `scheduling.volcano.sh/group-name` | 外部依赖(Volcano) | Gang scheduling的PodGroup名称 | ⚠️ LWS也使用相同Volcano annotation |
| batchv1alpha1.TaskSpecKey | `volcano.sh/task-spec` | 外部依赖(Volcano) | Gang scheduling的Task名称 | ⚠️ LWS也使用相同Volcano annotation |

**补充说明**:
- `PodDeletionCostAnnotation`: 在binpack缩容时用于计算ServingGroup和Role的删除优先级，优先删除成本低的pod
- Volcano相关annotations: 这些是Volcano调度器的标准annotations，用于gang scheduling功能，ModelServing和LWS都依赖Volcano时会使用相同的annotations

### 4.2 LWS特有的Annotations

| Annotation | 值 | 用途 | ModelServing状态 |
|------------|-----|------|-----------------|
| ExclusiveKeyAnnotationKey | `leaderworkerset.sigs.k8s.io/exclusive-topology` | 1:1独占调度拓扑 | ❌ 未实现 - 通过NetworkTopology实现类似功能 |
| SubGroupExclusiveKeyAnnotationKey | `leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology` | 子组独占调度拓扑 | ❌ 未实现 - ModelServing无SubGroup概念 |
| SizeAnnotationKey | `leaderworkerset.sigs.k8s.io/size` | 组大小 | ❌ 未实现 - 通过Spec字段和环境变量实现 |
| ReplicasAnnotationKey | `leaderworkerset.sigs.k8s.io/replicas` | 副本数 | ❌ 未实现 - 通过Spec.Replicas字段直接获取 |
| LeaderPodNameAnnotationKey | `leaderworkerset.sigs.k8s.io/leader-name` | Worker pod记录leader名称 | ❌ 未实现 - 通过环境变量ENTRY_ADDRESS实现 |
| SubGroupSizeAnnotationKey | `leaderworkerset.sigs.k8s.io/subgroup-size` | 子组大小 | ❌ 未实现 - ModelServing无SubGroup概念 |
| SubGroupPolicyTypeAnnotationKey | `leaderworkerset.sigs.k8s.io/subgroup-policy-type` | 子组策略类型 | ❌ 未实现 - ModelServing无SubGroup概念 |
| SubdomainPolicyAnnotationKey | `leaderworkerset.sigs.k8s.io/subdomainPolicy` | Subdomain策略 | ❌ 未实现 - ModelServing未实现网络子域策略 |

### 4.3 ModelServing的实现方式

ModelServing主要通过**Spec字段**和**环境变量**而非Annotation来传递信息：
- 使用Spec字段（如Replicas、Template等）直接定义
- 使用Labels进行索引和查询
- 使用环境变量传递运行时信息
- 在代码层面使用Kubernetes原生annotation（如PodDeletionCost）实现高级功能

## 5. 环境变量对比

| LWS环境变量 | 值 | ModelServing环境变量 | 值 | 映射关系 |
|------------|-----|---------------------|-----|---------|
| LwsLeaderAddress | `LWS_LEADER_ADDRESS` | EntryAddressEnv | `ENTRY_ADDRESS` | ✅ 等价 |
| LwsGroupSize | `LWS_GROUP_SIZE` | GroupSizeEnv | `GROUP_SIZE` | ✅ 等价 |
| LwsWorkerIndex | `LWS_WORKER_INDEX` | WorkerIndexEnv | `WORKER_INDEX` | ✅ 等价 |

## 6. 关键Gap总结

### 6.1 未实现的核心功能

#### 高优先级 (P0)
1. **GroupIndexLabelKey** - 组索引标签
   - **用途**: 数字索引方便排序和范围查询
   - **影响**: 用户从LWS迁移时，基于group-index的查询和操作无法直接使用
   - **建议**: 添加 `modelserving.volcano.sh/group-index` label

2. **GroupUniqueHashLabelKey** - 组唯一哈希标识
   - **用途**: 快速识别同一组的所有pod，用于批量操作
   - **影响**: 无法通过单一label选择器获取整个组的pod
   - **建议**: 添加 `modelserving.volcano.sh/group-hash` label

3. **LeaderPodNameAnnotationKey** - Worker记录Leader名称
   - **用途**: Worker pod直接引用leader pod名称
   - **影响**: 某些需要显式leader名称的场景需要修改代码
   - **建议**: 添加 `modelserving.volcano.sh/entry-name` annotation

#### 中优先级 (P1)
4. **SizeAnnotationKey** - 组大小注解
   - **用途**: 在pod级别记录组大小，方便查询
   - **影响**: 需要从ModelServing对象查询组大小
   - **建议**: 添加 `modelserving.volcano.sh/group-size` annotation

5. **ReplicasAnnotationKey** - 副本数注解
   - **用途**: 在pod级别记录副本总数
   - **影响**: 需要从ModelServing对象查询副本数
   - **建议**: 添加 `modelserving.volcano.sh/replicas` annotation

#### 低优先级 (P2)
6. **SubGroup相关** - 子组功能
   - SubGroupIndexLabelKey
   - SubGroupSizeAnnotationKey
   - SubGroupUniqueHashLabelKey
   - SubGroupPolicyTypeAnnotationKey
   - SubGroupExclusiveKeyAnnotationKey
   - **影响**: 不支持子组划分功能
   - **建议**: 如需支持复杂的组内子划分场景，需设计并实现SubGroup功能

7. **ExclusiveKeyAnnotationKey** - 独占拓扑调度
   - **用途**: 指定1:1独占调度的拓扑域
   - **影响**: ModelServing通过NetworkTopology实现，但无annotation暴露
   - **建议**: 考虑添加annotation或使用NetworkTopology的RolePolicy

8. **SubdomainPolicyAnnotationKey** - 子域策略
   - **用途**: 控制headless service的域名策略
   - **影响**: ModelServing当前未实现多种subdomain策略
   - **建议**: 如需支持不同域名策略，需实现该功能

### 6.2 架构差异

| 特性 | LWS | ModelServing | 影响 |
|------|-----|--------------|------|
| 角色抽象 | Leader/Worker二元结构 | 支持多角色(Role)，每个角色有Entry+Worker | ModelServing更灵活，但概念复杂 |
| 子组支持 | 支持SubGroup | 不支持 | LWS可以进行组内二级划分 |
| 索引方式 | 数字索引为主 | 名称为主，辅以索引 | ModelServing更语义化 |
| 网络配置 | Subdomain策略 | NetworkTopology策略 | ModelServing更强大但不兼容 |
| 调度策略 | 独占拓扑调度 | NetworkTopology + GangPolicy | ModelServing集成度更高 |

## 7. 迁移建议

### 7.1 立即实现（快速兼容）

为降低迁移成本，建议ModelServing添加以下annotations/labels:

```go
const (
    // 对应LWS的GroupIndexLabelKey
    GroupIndexLabelKey = "modelserving.volcano.sh/group-index"

    // 对应LWS的GroupUniqueHashLabelKey
    GroupHashLabelKey = "modelserving.volcano.sh/group-hash"

    // 对应LWS的LeaderPodNameAnnotationKey
    EntryPodNameAnnotationKey = "modelserving.volcano.sh/entry-name"

    // 对应LWS的SizeAnnotationKey
    GroupSizeAnnotationKey = "modelserving.volcano.sh/group-size"

    // 对应LWS的ReplicasAnnotationKey
    ReplicasAnnotationKey = "modelserving.volcano.sh/replicas"
)
```

### 7.2 中期规划（功能对齐）

1. **SubGroup功能评估**: 评估是否需要子组功能，若需要则设计实现方案
2. **网络策略增强**: 添加更多网络配置选项，如Subdomain策略
3. **调度策略注解**: 考虑将NetworkTopology配置暴露为annotation

### 7.3 长期规划（体验优化）

1. **迁移工具**: 提供LWS YAML到ModelServing YAML的转换工具
2. **兼容模式**: 提供LWS兼容模式，自动映射LWS的annotations到ModelServing
3. **文档完善**: 提供详细的迁移指南和对照表

## 8. 风险评估

| 风险项 | 严重程度 | 说明 | 缓解措施 |
|--------|---------|------|---------|
| Label选择器失效 | 高 | 基于group-index的选择器无法使用 | 添加group-index label |
| 批量操作困难 | 高 | 缺少group-hash导致无法批量选择组内pod | 添加group-hash label |
| 子组功能缺失 | 中 | 依赖SubGroup的场景无法迁移 | 评估SubGroup需求，设计实现方案 |
| 网络配置不兼容 | 中 | Subdomain策略差异 | 文档说明差异，提供配置指导 |
| 学习成本 | 中 | Role概念比Leader/Worker复杂 | 提供详细文档和示例 |

## 9. ModelServing常量定义位置总结

与LWS不同，ModelServing的表意常量**分散在多个文件中**，未全部集中在types定义文件：

### 9.1 Labels定义
**位置**: `pkg/apis/workload/v1alpha1/labels.go`
```go
const (
    ModelServingNameLabelKey = "modelserving.volcano.sh/name"
    GroupNameLabelKey = "modelserving.volcano.sh/group-name"
    RoleLabelKey = "modelserving.volcano.sh/role"
    RoleIDKey = "modelserving.volcano.sh/role-id"
    EntryLabelKey = "modelserving.volcano.sh/entry"
    RevisionLabelKey = "modelserving.volcano.sh/revision"
)
```

### 9.2 环境变量定义
**位置**: `pkg/apis/workload/v1alpha1/model_serving_types.go:24-32`
```go
const (
    EntryAddressEnv = "ENTRY_ADDRESS"
    WorkerIndexEnv = "WORKER_INDEX"
    GroupSizeEnv = "GROUP_SIZE"
)
```

### 9.3 Annotation定义
**位置**: `pkg/model-serving-controller/controller/binpack_scaledown.go:27-29`
```go
const (
    PodDeletionCostAnnotation = corev1.PodDeletionCost
)
```

**外部依赖** (Volcano调度器):
- `schedulingv1beta1.KubeGroupNameAnnotationKey` - 使用位置: `pkg/model-serving-controller/gangscheduling/manager.go:101,295`
- `batchv1alpha1.TaskSpecKey` - 使用位置: `pkg/model-serving-controller/gangscheduling/manager.go:296`

### 9.4 Indexer Key定义
**位置**: `pkg/model-serving-controller/controller/model_serving_controller.go:27-30`
```go
const (
    GroupNameKey = "GroupName"  // Informer索引键，非Pod Label
    RoleIDKey    = "RoleID"     // Informer索引键，非Pod Label
)
```
**注意**: 这些是Informer的索引键名，**不是**Pod的Label键名。对应的IndexFunc分别是`utils.GroupNameIndexFunc`和`utils.RoleIDIndexFunc`。

### 9.5 与LWS的对比

| 项目 | LWS | ModelServing |
|------|-----|--------------|
| 常量集中度 | ✅ 高度集中在types文件 | ❌ 分散在多个文件 |
| Labels定义位置 | types文件 | 独立的labels.go |
| Annotations定义 | types文件 | controller代码中 |
| 环境变量定义 | types文件 | types文件 |
| 可发现性 | ✅ 高 | ⚠️ 中等（需查看多个文件） |

**建议**: 为提升可维护性和可发现性，建议将所有表意常量集中定义在一个文件中（如`constants.go`），或在types文件中通过注释说明常量定义位置。

## 10. 结论

ModelServing与LWS在核心功能上基本等价，主要通过**环境变量**实现了关键信息传递。但在**表意annotations和labels**方面存在显著gap:

**已实现**:
- ✅ 核心环境变量完全对应
- ✅ 基本labels (name, revision) - 定义在独立的`labels.go`文件中
- ✅ 更强大的角色抽象和调度能力
- ✅ PodDeletionCost annotation支持（用于binpack缩容）

**缺失**:
- ❌ 组索引和组哈希labels（影响查询和批量操作）
- ❌ Entry名称和组大小annotations（影响便捷性）
- ❌ SubGroup完整功能集（影响复杂场景）
- ❌ Subdomain策略（影响网络配置灵活性）

**架构差异**:
- ⚠️ **常量定义分散**: ModelServing的表意常量分散在多个文件中（labels.go、types.go、controller代码），而LWS集中在types文件中，ModelServing的可发现性较低
- ⚠️ **依赖外部调度器**: ModelServing使用Volcano的annotations进行gang scheduling，与LWS相同

**建议**:
1. **立即实施**: 优先添加P0级别的labels/annotations（group-index, group-hash, entry-name），实现基本迁移兼容性
2. **代码组织**: 将所有表意常量集中定义或在types文件中添加常量位置索引，提升可维护性
3. **中长期**: 评估SubGroup等高级功能的必要性
