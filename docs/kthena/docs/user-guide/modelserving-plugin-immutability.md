# ModelServing Plugin 不可修改：规则与场景

**ModelServing 创建后，`spec.plugins` 的实质内容不可修改。** 新增、删除、修改配置或作用范围、调整执行顺序都会被 validating webhook 拒绝。需要另一套 plugin 配置时，应创建新的 ModelServing。

这项规则同样适用于 ServingGroup（SG）模式、Role 模式、0 副本，以及用户、自动化工具和上层控制器发起的更新。仅允许删除也不例外。规则不要求切换滚动模式，也不会为 plugin 变更触发滚动。

## 为什么删除也不允许

Plugin 可能在创建 Pod 时注入环境变量、注解、卷，也可能持续管理 Headless Service、ranktable ConfigMap 等附属资源。删除 plugin 声明并不会从已运行的 Pod 中撤销这些内容。

当前控制器实际使用的 revision 和历史恢复路径没有完整保存、恢复 plugin 配置。如果允许删除，可能出现：

| 场景 | 允许删除可能产生的结果 |
| --- | --- |
| 删除后扩容 | 新 Pod 不再执行该 plugin；原有 Pod 保留注入内容，实例配置不一致 |
| 删除后故障重建 | 替补 Pod 使用当前 plugin 配置，无法仅凭旧 revision 恢复原 plugin 行为 |
| 触发一次滚动 | 滚动期间仍有新旧实例并存；partition、预算、就绪失败可能让旧实例长期保留，单次触发不保证安全完成 |
| 删除管理附属资源的 plugin | 清理过早可能影响旧实例；清理不足可能遗留资源；仅重建 Pod 无法完整解决资源生命周期问题 |

因此，“只允许删除”和“顺便触发滚动”都不足以作为当前通用安全保证。本功能从 API 更新入口拒绝 plugin 变化，保留创建时的配置。

## 统一示例

下面的 ModelServing 在创建时配置两个 plugin，此时允许创建。可保存为 `plugin-fixed.yaml`：

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: plugin-fixed
spec:
  replicas: 1
  schedulerName: volcano
  recoveryPolicy: None
  plugins:
    - name: demo-pod-tweaks
      type: BuiltIn
      config:
        annotations:
          plugin-value: old
          owner: inference
    - name: headless-service
      type: BuiltIn
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 1
  template:
    roles:
      - name: predictor
        replicas: 1
        workerReplicas: 0
        entryTemplate:
          spec:
            containers:
              - name: main
                image: busybox:1.36
                command: [sh, -c, "sleep 3600"]
```

```bash
kubectl create -f plugin-fixed.yaml
```

下列拒绝示例都独立作用于这个原始对象；失败后对象的 plugin 配置保持不变。错误包含：

```text
spec.plugins: Forbidden: field is immutable after creation; create a new ModelServing to use a different plugin configuration
```

### 1. 删除单个 plugin：拒绝

```bash
kubectl patch modelserving plugin-fixed --type=json \
  -p='[{"op":"remove","path":"/spec/plugins/1"}]'
```

即使只删除 `headless-service`、保留其他 plugin，也会拒绝。

### 2. 删除全部 plugin：拒绝

```bash
kubectl patch modelserving plugin-fixed --type=merge \
  -p='{"spec":{"plugins":[]}}'
```

把 `plugins` 改为 `null`、通过 JSON Patch 移除整个字段，或在完整替换对象时省略原有 `plugins`，同样是删除，都会拒绝。`apply` 是否产生删除取决于字段管理关系；只要实际更新结果删除了已有配置，就会拒绝。

### 3. 新增 plugin，包括从无到有：拒绝

```bash
kubectl patch modelserving plugin-fixed --type=json \
  -p='[{"op":"add","path":"/spec/plugins/-","value":{"name":"lws-standard-labels","type":"BuiltIn"}}]'
```

如果另一个已有 ModelServing 创建时没有 plugin，之后给它添加第一个 plugin 也会拒绝。只有 CREATE 可以首次确定 plugin 列表。

### 4. 修改 plugin 配置：拒绝

```bash
kubectl patch modelserving plugin-fixed --type=json \
  -p='[{"op":"replace","path":"/spec/plugins/0/config/annotations/plugin-value","value":"new"}]'
```

同样拒绝增加或删除 config 字段、把 config 清空、改变其数组元素顺序。即使调用者认为某个配置对当前 Pod 没有影响，也没有特例。

### 5. 修改作用范围：拒绝

```bash
kubectl patch modelserving plugin-fixed --type=json \
  -p='[{"op":"add","path":"/spec/plugins/0/scope","value":{"roles":["predictor"],"target":"Entry"}}]'
```

`scope.roles` 的成员、`scope.target` 从 All 改为 Entry/Worker、删除原有非默认 scope 都属于修改。即使当前只有 predictor，一个明确的角色白名单也不等于“所有角色”：将来增加角色时含义不同。

### 6. 更换 plugin 或调整顺序：拒绝

```bash
kubectl patch modelserving plugin-fixed --type=json \
  -p='[{"op":"move","from":"/spec/plugins/1","path":"/spec/plugins/0"}]'
```

Plugin 按列表顺序执行，交换顺序可能改变最终 Pod。替换 `name` 或实质改变 `type` 同样禁止；当前 schema 也仅支持 `BuiltIn` 类型。

### 7. 修改 plugin 的同时扩容或更新镜像：整次拒绝

```bash
kubectl patch modelserving plugin-fixed --type=merge \
  -p='{"spec":{"replicas":2,"plugins":[]}}'
```

整次 API 更新失败，`replicas` 仍为 1，不会出现副本数已更新而 plugin 校验失败的部分写入。Role 模式下修改 Role 副本数时也遵守相同规则。携带镜像变更、手动滚动操作或修改预算都不会豁免 plugin 校验。

### 8. 先缩容到 0 再删除：仍然拒绝

```bash
kubectl patch modelserving plugin-fixed --type=merge -p='{"spec":{"replicas":0}}'
kubectl patch modelserving plugin-fixed --type=merge -p='{"spec":{"plugins":[]}}'
```

第一步单独缩容允许；第二步删除 plugin 拒绝。0 副本不改变该对象的创建配置约束，也不能被用作修改 plugin 的中间步骤。

### 9. Plugin 不变的扩容或模板更新：允许通过这项校验

```bash
kubectl patch modelserving plugin-fixed --type=merge -p='{"spec":{"replicas":2}}'
```

仅修改副本数、metadata 或 Pod 模板，且 plugin 配置不变时，本规则允许更新；仍需通过其他既有校验。新增或故障重建的 Pod 继续使用原 plugin 声明。Pod 模板变化是否触发滚动由现有滚动逻辑决定。

### 10. 表达等价的调整：允许

以下调整不视为 plugin 实质修改：

| 原表达 | 等价表达 |
| --- | --- |
| config JSON 对象的 key 顺序、空白不同 | key 和值相同，保持所有数组顺序 |
| 没有 plugin | 空 plugin 列表（`[]`） |
| type 缺省 | `BuiltIn` |
| scope 缺省 | 空 scope，或无 roles 限制且 target 为 All |
| scope.roles 为 `[a, b]` | `[b, a]` 或 `[a, b, a]`，同一角色集合 |
| config 缺省 | config 为 null |

这里的规范化只覆盖上述结构规则，不解释各 plugin 的私有配置。例如 `config: {}` 与省略 config 不承诺等价；仅数值表示不同也不承诺放行。

### 11. 已创建但 Pod 未就绪，想修正 plugin：拒绝

即使尚无可用 Pod，或 plugin 配错导致实例无法启动，只要 ModelServing 对象已经创建成功，就不能修改其 plugin。需要用正确配置新建对象。若 CREATE 请求本身失败、对象并未创建，则可以修正后重新提交 CREATE。

## 需要不同 plugin 时如何迁移

1. 使用不同名称创建新的 ModelServing，在 CREATE 时配置期望的 plugin；需要删除全部 plugin 时，新对象不配置 plugins。
2. 等待新实例就绪，检查实际 Pod 和它所依赖的附属资源，再验证推理请求。
3. 按业务现有流量管理方式把请求切到新 ModelServing；确认旧实例不再承担请求后，再回收旧对象及其资源。

这需要评估额外资源容量和切流方式，不保证天然无中断。直接删除并同名重建也是新对象，会失去原对象的运行连续性，不应作为无感更新方式。

## 部署与兼容边界

- 校验由本功能版本的 ModelServing validating webhook 执行。部署必须启用 `workload.controllerManager.webhook.enabled=true`；默认 Helm 配置启用它，ModelServing 验证规则覆盖 CREATE/UPDATE，`failurePolicy: Fail`。关闭、移除或排除该 webhook 会失去这项保护。
- 已有 ModelServing 无需重建即可保留现有 plugin 并继续进行其他合法更新；从升级后的第一次更新起，plugin 实质变更即被拒绝。
- 本功能不会修复升级前因修改 plugin 产生的 Pod 差异，也不冻结控制器版本中的 plugin 实现或其引用的外部资源。存在历史差异时需要单独排查和迁移。
- 上层控制器也没有豁免。若上层资源的一次更新会给已有 ModelServing 增删 plugin，该更新写入会被拒绝，上层需采用新建并迁移的流程。例如当前 LWS 转换器在存在 worker 时添加 `headless-service`；LWS 调整 size 导致 worker 从无到有或从有到无时，会改变生成的 plugin 列表，写入已有 ModelServing 将被拒绝。
