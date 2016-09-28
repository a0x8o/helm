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
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"regexp"
	"testing"

	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/spf13/cobra"

	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	rls "k8s.io/helm/pkg/proto/hapi/services"
	"k8s.io/helm/pkg/proto/hapi/version"
)

var mockHookTemplate = `apiVersion: v1
kind: Job
metadata:
  annotations:
    "helm.sh/hooks": pre-install
`

var mockManifest = `apiVersion: v1
kind: Secret
metadata:
  name: fixture
`

type releaseOptions struct {
	name       string
	version    int32
	chart      *chart.Chart
	statusCode release.Status_Code
}

func releaseMock(opts *releaseOptions) *release.Release {
	date := timestamp.Timestamp{Seconds: 242085845, Nanos: 0}

	name := opts.name
	if name == "" {
		name = "testrelease-" + string(rand.Intn(100))
	}

	var version int32 = 1
	if opts.version != 0 {
		version = opts.version
	}

	ch := opts.chart
	if opts.chart == nil {
		ch = &chart.Chart{
			Metadata: &chart.Metadata{
				Name:    "foo",
				Version: "0.1.0-beta.1",
			},
			Templates: []*chart.Template{
				{Name: "foo.tpl", Data: []byte(mockManifest)},
			},
		}
	}

	scode := release.Status_DEPLOYED
	if opts.statusCode > 0 {
		scode = opts.statusCode
	}

	return &release.Release{
		Name: name,
		Info: &release.Info{
			FirstDeployed: &date,
			LastDeployed:  &date,
			Status:        &release.Status{Code: scode},
		},
		Chart:   ch,
		Config:  &chart.Config{Raw: `name: "value"`},
		Version: version,
		Hooks: []*release.Hook{
			{
				Name:     "pre-install-hook",
				Kind:     "Job",
				Path:     "pre-install-hook.yaml",
				Manifest: mockHookTemplate,
				LastRun:  &date,
				Events:   []release.Hook_Event{release.Hook_PRE_INSTALL},
			},
		},
		Manifest: mockManifest,
	}
}

type fakeReleaseClient struct {
	rels []*release.Release
	err  error
}

var _ helm.Interface = &fakeReleaseClient{}
var _ helm.Interface = &helm.Client{}

func (c *fakeReleaseClient) ListReleases(opts ...helm.ReleaseListOption) (*rls.ListReleasesResponse, error) {
	resp := &rls.ListReleasesResponse{
		Count:    int64(len(c.rels)),
		Releases: c.rels,
	}
	return resp, c.err
}

func (c *fakeReleaseClient) InstallRelease(chStr, ns string, opts ...helm.InstallOption) (*rls.InstallReleaseResponse, error) {
	return &rls.InstallReleaseResponse{
		Release: c.rels[0],
	}, nil
}

func (c *fakeReleaseClient) DeleteRelease(rlsName string, opts ...helm.DeleteOption) (*rls.UninstallReleaseResponse, error) {
	return nil, nil
}

func (c *fakeReleaseClient) ReleaseStatus(rlsName string, opts ...helm.StatusOption) (*rls.GetReleaseStatusResponse, error) {
	if c.rels[0] != nil {
		return &rls.GetReleaseStatusResponse{
			Name:      c.rels[0].Name,
			Info:      c.rels[0].Info,
			Namespace: c.rels[0].Namespace,
		}, nil
	}
	return nil, fmt.Errorf("No such release: %s", rlsName)
}

func (c *fakeReleaseClient) GetVersion(opts ...helm.VersionOption) (*rls.GetVersionResponse, error) {
	return &rls.GetVersionResponse{
		Version: &version.Version{
			SemVer: "1.2.3-fakeclient+testonly",
		},
	}, nil
}

func (c *fakeReleaseClient) UpdateRelease(rlsName string, chStr string, opts ...helm.UpdateOption) (*rls.UpdateReleaseResponse, error) {
	return nil, nil
}

func (c *fakeReleaseClient) RollbackRelease(rlsName string, opts ...helm.RollbackOption) (*rls.RollbackReleaseResponse, error) {
	return nil, nil
}

func (c *fakeReleaseClient) ReleaseContent(rlsName string, opts ...helm.ContentOption) (resp *rls.GetReleaseContentResponse, err error) {
	if len(c.rels) > 0 {
		resp = &rls.GetReleaseContentResponse{
			Release: c.rels[0],
		}
	}
	return resp, c.err
}

func (c *fakeReleaseClient) Option(opt ...helm.Option) helm.Interface {
	return c
}

// releaseCmd is a command that works with a fakeReleaseClient
type releaseCmd func(c *fakeReleaseClient, out io.Writer) *cobra.Command

// runReleaseCases runs a set of release cases through the given releaseCmd.
func runReleaseCases(t *testing.T, tests []releaseCase, rcmd releaseCmd) {
	var buf bytes.Buffer
	for _, tt := range tests {
		c := &fakeReleaseClient{
			rels: []*release.Release{tt.resp},
		}
		cmd := rcmd(c, &buf)
		cmd.ParseFlags(tt.flags)
		err := cmd.RunE(cmd, tt.args)
		if (err != nil) != tt.err {
			t.Errorf("%q. expected error, got '%v'", tt.name, err)
		}
		re := regexp.MustCompile(tt.expected)
		if !re.Match(buf.Bytes()) {
			t.Errorf("%q. expected\n%q\ngot\n%q", tt.name, tt.expected, buf.String())
		}
		buf.Reset()
	}
}

// releaseCase describes a test case that works with releases.
type releaseCase struct {
	name  string
	args  []string
	flags []string
	// expected is the string to be matched. This supports regular expressions.
	expected string
	err      bool
	resp     *release.Release
}

// tmpHelmHome sets up a Helm Home in a temp dir.
//
// This does not clean up the directory. You must do that yourself.
// You  must also set helmHome yourself.
func tempHelmHome() (string, error) {
	oldhome := helmHome
	dir, err := ioutil.TempDir("", "helm_home-")
	if err != nil {
		return "n/", err
	}

	helmHome = dir
	if err := ensureHome(); err != nil {
		return "n/", err
	}
	helmHome = oldhome
	return dir, nil
}
