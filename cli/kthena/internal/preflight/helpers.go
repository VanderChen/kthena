/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package preflight

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// splitOrdinal splits a name of the form <prefix>-<ordinal>. The ordinal is
// written without a sign and without leading zeros.
func splitOrdinal(name string) (prefix string, ordinal int, ok bool) {
	cut := strings.LastIndexByte(name, '-')
	if cut <= 0 {
		return "", 0, false
	}
	ordinal, err := strconv.Atoi(name[cut+1:])
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != name[cut+1:] {
		return "", 0, false
	}
	return name[:cut], ordinal, true
}

// counted returns one when n is 1, and many with n filled in otherwise.
func counted(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

// listed joins names for a message and keeps it readable when there are many.
func listed(names []string) string {
	const shown = 5
	switch {
	case len(names) == 0:
		return "none"
	case len(names) <= shown:
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}

func podNames(pods []*corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	sort.Strings(names)
	return names
}

func ordinalList(ordinals []int) string {
	parts := make([]string, 0, len(ordinals))
	for _, ordinal := range ordinals {
		parts = append(parts, strconv.Itoa(ordinal))
	}
	return listed(parts)
}

// controlledBy reports whether the controller owner of object has the UID.
func controlledBy(object metav1.Object, uid types.UID) bool {
	owner := metav1.GetControllerOfNoCopy(object)
	return owner != nil && owner.UID == uid
}

// WriteJSON writes a report or a comparison as indented JSON.
func WriteJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	// Messages name placeholders such as <group>; keep them readable.
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// textWidth is the width messages are wrapped to.
const textWidth = 100

// wrap breaks text into lines of at most width characters, each after indent.
func wrap(text, indent string, width int) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && len(indent)+len(line)+1+len(word) > width {
			lines = append(lines, indent+line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		lines = append(lines, indent+line)
	}
	return strings.Join(lines, "\n")
}

// ordinalRanges writes sorted ordinals compactly: 0-2,5.
func ordinalRanges(ordinals []int) string {
	var parts []string
	for i := 0; i < len(ordinals); {
		j := i
		for j+1 < len(ordinals) && ordinals[j+1] == ordinals[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(ordinals[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ordinals[i], ordinals[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
