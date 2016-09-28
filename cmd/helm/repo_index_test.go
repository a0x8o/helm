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
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/helm/pkg/repo"
)

func TestRepoIndexCmd(t *testing.T) {

	dir, err := ioutil.TempDir("", "helm-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Link("testdata/testcharts/compressedchart-0.1.0.tgz", filepath.Join(dir, "compressedchart-0.1.0.tgz")); err != nil {
		t.Fatal(err)
	}

	buf := bytes.NewBuffer(nil)
	c := newRepoIndexCmd(buf)

	if err := c.RunE(c, []string{dir}); err != nil {
		t.Errorf("%q", err)
	}

	index, err := repo.LoadIndexFile(filepath.Join(dir, "index.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if len(index.Entries) != 1 {
		t.Errorf("expected 1 entry, got %v: %#v", len(index.Entries), index.Entries)
	}

}
