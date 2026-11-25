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
	"testing"
)

func TestSplitOrdinal(t *testing.T) {
	tests := []struct {
		name        string
		wantPrefix  string
		wantOrdinal int
		wantOK      bool
	}{
		{"llama-0", "llama", 0, true},
		{"llama-7b-12", "llama-7b", 12, true},
		{"llama", "", 0, false},
		{"llama-", "", 0, false},
		{"llama-01", "", 0, false},
		{"llama-+1", "", 0, false},
		{"-1", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, ordinal, ok := splitOrdinal(tt.name)
			if prefix != tt.wantPrefix || ordinal != tt.wantOrdinal || ok != tt.wantOK {
				t.Errorf("splitOrdinal(%q) = %q, %d, %t, want %q, %d, %t", tt.name, prefix, ordinal, ok, tt.wantPrefix, tt.wantOrdinal, tt.wantOK)
			}
		})
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		width int
		want  string
	}{
		{"short text stays on one line", "one two", 20, "  one two"},
		{"breaks between words", "one two three four", 11, "  one two\n  three\n  four"},
		{"a word longer than the width is not cut", "abcdefghijklmnop q", 8, "  abcdefghijklmnop\n  q"},
		{"empty text", "", 20, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrap(tt.text, "  ", tt.width); got != tt.want {
				t.Errorf("wrap() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOrdinalRanges(t *testing.T) {
	tests := []struct {
		ordinals []int
		want     string
	}{
		{nil, ""},
		{[]int{0}, "0"},
		{[]int{0, 1, 2}, "0-2"},
		{[]int{0, 1, 3}, "0-1,3"},
		{[]int{0, 2, 3, 4, 7}, "0,2-4,7"},
	}
	for _, tt := range tests {
		if got := ordinalRanges(tt.ordinals); got != tt.want {
			t.Errorf("ordinalRanges(%v) = %q, want %q", tt.ordinals, got, tt.want)
		}
	}
}
