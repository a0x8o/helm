/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSearchCmd(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		flags  []string
		expect string
		regexp bool
		fail   bool
	}{
		{
			name:   "search for 'maria', expect one match",
			args:   []string{"maria"},
			expect: "testing/mariadb-0.3.0",
		},
		{
			name:   "search for 'alpine', expect two matches",
			args:   []string{"alpine"},
			expect: "testing/alpine-0.1.0\ntesting/alpine-0.2.0",
		},
		{
			name:   "search for 'syzygy', expect no matches",
			args:   []string{"syzygy"},
			expect: "",
		},
		{
			name:   "search for 'alp[a-z]+', expect two matches",
			args:   []string{"alp[a-z]+"},
			flags:  []string{"--regexp"},
			expect: "testing/alpine-0.1.0\ntesting/alpine-0.2.0",
			regexp: true,
		},
		{
			name:   "search for 'alp[', expect failure to compile regexp",
			args:   []string{"alp["},
			flags:  []string{"--regexp"},
			regexp: true,
			fail:   true,
		},
	}

	oldhome := helmHome
	helmHome = "testdata/helmhome"
	defer func() { helmHome = oldhome }()

	for _, tt := range tests {
		buf := bytes.NewBuffer(nil)
		cmd := newSearchCmd(buf)
		cmd.ParseFlags(tt.flags)
		if err := cmd.RunE(cmd, tt.args); err != nil {
			if tt.fail {
				continue
			}
			t.Fatalf("%s: unexpected error %s", tt.name, err)
		}
		got := strings.TrimSpace(buf.String())
		if got != tt.expect {
			t.Errorf("%s: expected %q, got %q", tt.name, tt.expect, got)
		}
	}
}
