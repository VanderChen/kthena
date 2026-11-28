{{/*
The names --controllers of kthena-controller-manager understands. Keep in step with
defaultControllers in cmd/kthena-controller-manager/main.go. The autoscaler is not a controller
of that binary any more, but its name still takes part in the selection.
*/}}
{{- define "kthena.workload.controllerNames" -}}
modelserving,modelbooster,autoscaler
{{- end -}}

{{/*
The names a comma-separated --controllers value selects, as a comma-separated list.
Mirrors isControllerEnabled in cmd/kthena-controller-manager/main.go: the first explicit token
for a name wins, and "*" covers names without one. Empty when the value selects none.
*/}}
{{- define "kthena.workload.selectedControllers" -}}
{{- $tokens := list -}}
{{- range splitList "," . -}}
{{- with trim . -}}{{- $tokens = append $tokens . -}}{{- end -}}
{{- end -}}
{{- $star := has "*" $tokens -}}
{{- $selected := list -}}
{{- range $name := splitList "," (include "kthena.workload.controllerNames" .) -}}
{{- $state := "" -}}
{{- range $tokens -}}
{{- if not $state -}}
{{- if or (eq . $name) (eq . (printf "+%s" $name)) -}}{{- $state = "on" -}}
{{- else if eq . (printf "-%s" $name) -}}{{- $state = "off" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if or (eq $state "on") (and (not $state) $star) -}}{{- $selected = append $selected $name -}}{{- end -}}
{{- end -}}
{{- join "," $selected -}}
{{- end -}}

{{/*
The value of a flag in controllerManager.image.args, as the binary sees it: "--name=value" or
"--name" followed by the value, the last occurrence winning. With "all" set, every occurrence
is returned, comma-separated, which is how a repeated --controllers adds up.
*/}}
{{- define "kthena.workload.controllerManager.argValue" -}}
{{- $flag := printf "--%s" .name -}}
{{- $values := list -}}
{{- $next := false -}}
{{- range (.root.Values.controllerManager.image.args | default list) -}}
{{- $arg := toString . -}}
{{- if $next -}}{{- $values = append $values $arg -}}{{- $next = false -}}
{{- else if eq $arg $flag -}}{{- $next = true -}}
{{- else if hasPrefix (printf "%s=" $flag) $arg -}}{{- $values = append $values (trimPrefix (printf "%s=" $flag) $arg) -}}
{{- end -}}
{{- end -}}
{{- if .all -}}{{- join "," $values -}}{{- else if $values -}}{{- last $values -}}{{- end -}}
{{- end -}}

{{/*
controllerManager.controllers as the user wrote it, joined when it is a list.
*/}}
{{- define "kthena.workload.controllerManager.rawControllers" -}}
{{- with .Values.controllerManager.controllers -}}
{{- if kindIs "string" . -}}{{- . -}}{{- else -}}{{- join "," . -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/*
A value that has to be true or false, as "true" or "false"; the default when it is not set.
Anything else fails, so that a value such as "True" or 0 is not silently read as the opposite.
*/}}
{{- define "kthena.workload.boolValue" -}}
{{- if kindIs "invalid" .value -}}
{{- .default -}}
{{- else if has (toString .value) (list "true" "false") -}}
{{- toString .value -}}
{{- else -}}
{{- fail (printf "%s must be true or false, got %q" .name (toString .value)) -}}
{{- end -}}
{{- end -}}

{{/*
"true" when the release runs the autoscaler: workload.autoscaler.enabled is set, and the
controllers selected for kthena-controller-manager include it, as they had to when the
autoscaler was one of its controllers. Mirrors parseControllers: --controllers in
controllerManager.image.args comes first, and a selection of nothing means everything.
The binary reads the value as CSV, and the Deployment template puts it in a double-quoted
YAML string; a value with quotes or escapes is refused rather than misread.
*/}}
{{- define "kthena.workload.autoscaler.enabled" -}}
{{- $as := .Values.autoscaler | default dict -}}
{{- $enabled := eq (include "kthena.workload.boolValue" (dict "value" $as.enabled "name" "workload.autoscaler.enabled" "default" "true")) "true" -}}
{{- $tokens := printf "%s,%s" (include "kthena.workload.controllerManager.argValue" (dict "root" . "name" "controllers" "all" true)) (include "kthena.workload.controllerManager.rawControllers" .) -}}
{{- if and $enabled (or (contains "\"" $tokens) (contains "\\" $tokens)) -}}
{{- fail (printf "the controllers selected for kthena-controller-manager (%s) contain a double quote or a backslash, which YAML and the binary's CSV parsing may read differently from the chart: write the names without them, so that the chart can tell whether to deploy the autoscaler" $tokens) -}}
{{- end -}}
{{- $selected := include "kthena.workload.selectedControllers" $tokens | default (include "kthena.workload.controllerNames" .) -}}
{{- if and $enabled (has "autoscaler" (splitList "," $selected)) -}}true{{- end -}}
{{- end -}}

{{/*
"true" when the autoscaler elects a leader: workload.autoscaler.leaderElect when it is set,
otherwise whether controllerManager.image.args turn leader election on.
*/}}
{{- define "kthena.workload.autoscaler.leaderElect" -}}
{{- $as := .Values.autoscaler | default dict -}}
{{- $explicit := include "kthena.workload.boolValue" (dict "value" $as.leaderElect "name" "workload.autoscaler.leaderElect" "default" "") -}}
{{- if $explicit -}}
{{- if eq $explicit "true" -}}true{{- end -}}
{{- else -}}
{{- $on := false -}}
{{- range (.Values.controllerManager.image.args | default list) -}}
{{- $arg := toString . -}}
{{- if eq $arg "--leader-elect" -}}{{- $on = true -}}{{- end -}}
{{- if hasPrefix "--leader-elect=" $arg -}}
{{- $on = not (has (trimPrefix "--leader-elect=" $arg) (list "false" "False" "FALSE" "f" "F" "0")) -}}
{{- end -}}
{{- end -}}
{{- if $on -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The autoscaler image: workload.autoscaler.image, or the image of kthena-controller-manager
with the image name replaced, so that a mirror and a version configured for one are used for
the other. A digest is not carried over: it names the other image.
*/}}
{{- define "kthena.workload.autoscaler.image" -}}
{{- $image := (.Values.autoscaler | default dict).image | default dict -}}
{{- $manager := .Values.controllerManager.image.repository | toString -}}
{{- if and (contains "@" $manager) (not (and $image.repository $image.tag)) -}}
{{- fail (printf "workload.controllerManager.image.repository %q pins the image by digest, which the autoscaler image cannot take over: set workload.autoscaler.image.repository and workload.autoscaler.image.tag" $manager) -}}
{{- end -}}
{{- $repository := $image.repository -}}
{{- if not $repository -}}
{{- if not (hasSuffix "kthena-controller-manager" $manager) -}}
{{- fail (printf "the autoscaler image cannot be derived from workload.controllerManager.image.repository %q, which does not end in kthena-controller-manager: set workload.autoscaler.image.repository" $manager) -}}
{{- end -}}
{{- $repository = printf "%skthena-autoscaler" (trimSuffix "kthena-controller-manager" $manager) -}}
{{- end -}}
{{- $managerTag := "" -}}
{{- if not (kindIs "invalid" .Values.controllerManager.image.tag) -}}{{- $managerTag = toString .Values.controllerManager.image.tag -}}{{- end -}}
{{- $tag := $image.tag | default (regexReplaceAll "@.*$" $managerTag "") -}}
{{- if not $tag -}}
{{- fail "the autoscaler image tag cannot be derived from workload.controllerManager.image.tag: set workload.autoscaler.image.tag" -}}
{{- end -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
