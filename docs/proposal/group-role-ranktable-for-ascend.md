---
title: Mount group/role level ranktable for Ascend resources
authors:
- "@VanderChen"
reviewers:
- "@robot"
- TBD
approvers:
- "@robot"
- TBD

creation-date: 2025-12-15

---

## Mount group/role level ranktable for Ascend resources

### Summary

### Motivation

For Ascend 910 reosurces, vLLM-Ascend or MindIE needs ranktable to support HCCL communication.
因此需要按照role粒度或者group粒度生成ranktable

#### Goals

前置条件：
使用ascend资源时，会有外部组件将该pod使用的ascend资源ranktable信息注入pod annotation，且pod下发完成后该annotation才会刷新

目标方案，
1. 用户定义需要生成role/group ranktable的模板及pod ranktable解析模板，其中role ranktable模板需要指定使用哪一个pod ranktable解析模板
2. 用户下发modelserving负载时使用annotation指定使用哪一个role/group ranktable模板
3. modelserving-controller下发pod，为每一个role/group创建对应的空白configmap，预挂载至role/group下的pod
4. pod下发成功后，controller读取每一个pod annotation，根据指定的role/group ranktable模板，生成role/group ranktable并刷新至对应的configmap
5. 这里需要注意一个点，由于ranktable需要晚于pod下发，因此需要给每一个pod注入一个init容器，用于等待对应ranktable configmap完成刷新
6. 注意rankid并非读取，而是根据device_id字典序0-n生成，在模板中注意区分

##### role ranktable生成模板的初步设想

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ascend-inference-config
  namespace: {{ .Release.Namespace }}
data:
  supported-inference-engine: "mindie" #vllm-ascend or mind-ie
  pod-ranktable-format: ""
  ranktable-level: "role" #role or group
  ranktable-template: |
    [
        "version": "1.0",
        "server_count": "1",
        "status": "{{ .Status }}",
        "device": [
            {
                "device_id": {{device_id}},
                "device_ip": {{device_ip}},
                "rank_id": {{rank_id}}
            },
        ]
    ],
    "status": "{{ .Status }}"
   
```

[MindIE多机推理](https://www.hiascend.com/document/detail/zh/mindie/22RC1/envdeployment/instg/mindie_instg_0027.html)
[MindIE部署多机PD分离服务](https://www.hiascend.com/document/detail/zh/mindie/22RC1/mindieservice/servicedev/mindie_service0060.html)

##### pod ranktable样例

```json
{
    "pod_name": "job-worker-0",
    "server_id": "192.168.105.82",
    "devices": [
        {
            "device_id": "0",
            "device_ip": "123.134.9.39"
        },
        {
            "device_id": "1",
            "device_ip": "123.134.9.40"
        },
        {
            "device_id": "2",
            "device_ip": "123.134.9.43"
        },
        {
            "device_id": "4",
            "device_ip": "123.134.9.59"
        }
    ]
}
```

##### role级别生成完成的ranktable实例

```json
{
    "version": "1.0",
    "server_count": "2",
    "server_list": [
        {
            "server_id": "Master节点IP地址",
            "device": [
                { "device_id": "0", "device_ip": "10.20.0.2", "rank_id": "0" }, 
                { "device_id": "1", "device_ip": "10.20.0.3", "rank_id": "1" },
                { "device_id": "2", "device_ip": "10.20.0.4", "rank_id": "2" },
                { "device_id": "3", "device_ip": "10.20.0.5", "rank_id": "3" },
                { "device_id": "4", "device_ip": "10.20.0.6", "rank_id": "4" },
                { "device_id": "5", "device_ip": "10.20.0.7", "rank_id": "5" },
                { "device_id": "6", "device_ip": "10.20.0.8", "rank_id": "6" },
                { "device_id": "7", "device_ip": "10.20.0.9", "rank_id": "7" }
            ]
        },
        {
            "server_id": "Slave节点IP地址",
            "device": [
                { "device_id": "0", "device_ip": "10.20.0.10", "rank_id": "8" },
                { "device_id": "1", "device_ip": "10.20.0.11", "rank_id": "9" },
                { "device_id": "2", "device_ip": "10.20.0.12", "rank_id": "10" },
                { "device_id": "3", "device_ip": "10.20.0.13", "rank_id": "11" },
                { "device_id": "4", "device_ip": "10.20.0.14", "rank_id": "12" },
                { "device_id": "5", "device_ip": "10.20.0.15", "rank_id": "13" },
                { "device_id": "6", "device_ip": "10.20.0.16", "rank_id": "14" },
                { "device_id": "7", "device_ip": "10.20.0.17", "rank_id": "15" }
            ]
        }
    ],
    "status": "{{ .Status }}"
}
```

#### Non-Goals

<!--
What is out of scope for this proposal? Listing non-goals helps to focus discussion
and make progress.
-->

### Proposal

<!--
This is where we get down to the specifics of what the proposal actually is.
This should have enough detail that reviewers can understand exactly what
you're proposing, but should not include things like API designs or
implementation. What is the desired outcome and how do we measure success?.
The "Design Details" section below is for the real
nitty-gritty.
-->

#### User Stories (Optional)

<!--
Detail the things that people will be able to do if this proposal is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

##### Story 1 



##### Story 2

#### Notes/Constraints/Caveats (Optional)

<!--
What are the caveats to the proposal?
What are some important details that didn't come across above?
Go in to as much detail as necessary here.
This might be a good place to talk about core concepts and how they relate.
-->

#### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate?

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->

### Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ascend-inference-config
  namespace: {{ .Release.Namespace }}
data:
  supported-inference-engine: "vllm"
  node-ranktable-format: ""
  ranktable-level: ""
  ranktable-template: |
    [
        "server_id": "{{device_id}}",
        "device": [
            {
                "device_id": {{device_id}},
                "device_ip": {{device_ip}},
                "rank_id": {{rank_id}}
            },
        ]
    ],
    "status": "{{ .Status }}"
   
```

[MindIE多机推理](https://www.hiascend.com/document/detail/zh/mindie/22RC1/envdeployment/instg/mindie_instg_0027.html)
[MindIE部署多机PD分离服务](https://www.hiascend.com/document/detail/zh/mindie/22RC1/mindieservice/servicedev/mindie_service0060.html)

```json
{
    "pod_name": "job-worker-0",
    "server_id": "192.168.105.82",
    "devices": [
        {
            "device_id": "0",
            "device_ip": "123.134.9.39"
        },
        {
            "device_id": "1",
            "device_ip": "123.134.9.39"
        },
        {
            "device_id": "2",
            "device_ip": "123.134.9.39"
        },
    ]
}
```

#### Test Plan

<!--
**Note:** *Not required until targeted at a release.*

Consider the following in developing a test plan for this enhancement:
- Will there be e2e and integration tests, in addition to unit tests?
- How will it be tested in isolation vs with other components?

No need to outline all test cases, just the general strategy. Anything
that would count as tricky in the implementation, and anything particularly
challenging to test, should be called out.

-->

### Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->

<!--
Note: This is a simplified version of kubernetes enhancement proposal template.
https://github.com/kubernetes/enhancements/tree/3317d4cb548c396a430d1c1ac6625226018adf6a/keps/NNNN-kep-template
-->