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
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/ghodss/yaml"
	"github.com/technosophos/moniker"
	ctx "golang.org/x/net/context"
	"k8s.io/kubernetes/pkg/api/unversioned"

	"k8s.io/helm/cmd/tiller/environment"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/proto/hapi/services"
	"k8s.io/helm/pkg/storage/driver"
	"k8s.io/helm/pkg/timeconv"
	"k8s.io/helm/pkg/version"
)

var srv *releaseServer

// releaseNameMaxLen is the maximum length of a release name.
//
// This is designed to accommodate the usage of release name in the 'name:'
// field of Kubernetes resources. Many of those fields are limited to 24
// characters in length. See https://github.com/kubernetes/helm/issues/1071
const releaseNameMaxLen = 14

// NOTESFILE_SUFFIX that we want to treat special. It goes through the templating engine
// but it's not a yaml file (resource) hence can't have hooks, etc. And the user actually
// wants to see this file after rendering in the status command. However, it must be a suffix
// since there can be filepath in front of it.
const notesFileSuffix = "NOTES.txt"

func init() {
	srv = &releaseServer{
		env: env,
	}
	services.RegisterReleaseServiceServer(rootServer, srv)
}

var (
	// errMissingChart indicates that a chart was not provided.
	errMissingChart = errors.New("no chart provided")
	// errMissingRelease indicates that a release (name) was not provided.
	errMissingRelease = errors.New("no release provided")
	// errIncompatibleVersion indicates incompatible client/server versions.
	errIncompatibleVersion = errors.New("client version is incompatible")
)

// ListDefaultLimit is the default limit for number of items returned in a list.
var ListDefaultLimit int64 = 512

type releaseServer struct {
	env *environment.Environment
}

func getVersion(c ctx.Context) string {
	if md, ok := metadata.FromContext(c); ok {
		if v, ok := md["x-helm-api-client"]; ok {
			return v[0]
		}
	}
	return ""
}

func (s *releaseServer) ListReleases(req *services.ListReleasesRequest, stream services.ReleaseService_ListReleasesServer) error {
	if !checkClientVersion(stream.Context()) {
		return errIncompatibleVersion
	}

	if len(req.StatusCodes) == 0 {
		req.StatusCodes = []release.Status_Code{release.Status_DEPLOYED}
	}

	//rels, err := s.env.Releases.ListDeployed()
	rels, err := s.env.Releases.ListFilterAll(func(r *release.Release) bool {
		for _, sc := range req.StatusCodes {
			if sc == r.Info.Status.Code {
				return true
			}
		}
		return false
	})
	if err != nil {
		return err
	}

	if len(req.Filter) != 0 {
		rels, err = filterReleases(req.Filter, rels)
		if err != nil {
			return err
		}
	}

	total := int64(len(rels))

	switch req.SortBy {
	case services.ListSort_NAME:
		sort.Sort(byName(rels))
	case services.ListSort_LAST_RELEASED:
		sort.Sort(byDate(rels))
	}

	if req.SortOrder == services.ListSort_DESC {
		ll := len(rels)
		rr := make([]*release.Release, ll)
		for i, item := range rels {
			rr[ll-i-1] = item
		}
		rels = rr
	}

	l := int64(len(rels))
	if req.Offset != "" {

		i := -1
		for ii, cur := range rels {
			if cur.Name == req.Offset {
				i = ii
			}
		}
		if i == -1 {
			return fmt.Errorf("offset %q not found", req.Offset)
		}

		if len(rels) < i {
			return fmt.Errorf("no items after %q", req.Offset)
		}

		rels = rels[i:]
		l = int64(len(rels))
	}

	if req.Limit == 0 {
		req.Limit = ListDefaultLimit
	}

	next := ""
	if l > req.Limit {
		next = rels[req.Limit].Name
		rels = rels[0:req.Limit]
		l = int64(len(rels))
	}

	res := &services.ListReleasesResponse{
		Next:     next,
		Count:    l,
		Total:    total,
		Releases: rels,
	}
	return stream.Send(res)
}

func filterReleases(filter string, rels []*release.Release) ([]*release.Release, error) {
	preg, err := regexp.Compile(filter)
	if err != nil {
		return rels, err
	}
	matches := []*release.Release{}
	for _, r := range rels {
		if preg.MatchString(r.Name) {
			matches = append(matches, r)
		}
	}
	return matches, nil
}

func (s *releaseServer) GetVersion(c ctx.Context, req *services.GetVersionRequest) (*services.GetVersionResponse, error) {
	v := version.GetVersionProto()
	return &services.GetVersionResponse{Version: v}, nil
}

func checkClientVersion(c ctx.Context) bool {
	v := getVersion(c)
	return version.IsCompatible(v, version.Version)
}

func (s *releaseServer) GetReleaseStatus(c ctx.Context, req *services.GetReleaseStatusRequest) (*services.GetReleaseStatusResponse, error) {
	if !checkClientVersion(c) {
		return nil, errIncompatibleVersion
	}

	if req.Name == "" {
		return nil, errMissingRelease
	}

	var rel *release.Release
	var err error

	if req.Version <= 0 {
		if rel, err = s.env.Releases.Deployed(req.Name); err != nil {
			return nil, fmt.Errorf("getting deployed release '%s': %s", req.Name, err)
		}
	} else {
		if rel, err = s.env.Releases.Get(req.Name, req.Version); err != nil {
			return nil, fmt.Errorf("getting release '%s' (v%d): %s", req.Name, req.Version, err)
		}
	}

	if rel.Info == nil {
		return nil, errors.New("release info is missing")
	}
	if rel.Chart == nil {
		return nil, errors.New("release chart is missing")
	}

	sc := rel.Info.Status.Code
	statusResp := &services.GetReleaseStatusResponse{Info: rel.Info, Namespace: rel.Namespace}

	// Ok, we got the status of the release as we had jotted down, now we need to match the
	// manifest we stashed away with reality from the cluster.
	kubeCli := s.env.KubeClient
	resp, err := kubeCli.Get(rel.Namespace, bytes.NewBufferString(rel.Manifest))
	if sc == release.Status_DELETED || sc == release.Status_FAILED {
		// Skip errors if this is already deleted or failed.
		return statusResp, nil
	} else if err != nil {
		log.Printf("warning: Get for %s failed: %v", rel.Name, err)
		return nil, err
	}
	rel.Info.Status.Resources = resp
	return statusResp, nil
}

func (s *releaseServer) GetReleaseContent(c ctx.Context, req *services.GetReleaseContentRequest) (*services.GetReleaseContentResponse, error) {
	if !checkClientVersion(c) {
		return nil, errIncompatibleVersion
	}

	if req.Name == "" {
		return nil, errMissingRelease
	}
	if req.Version <= 0 {
		rel, err := s.env.Releases.Deployed(req.Name)
		return &services.GetReleaseContentResponse{Release: rel}, err
	}

	rel, err := s.env.Releases.Get(req.Name, req.Version)
	return &services.GetReleaseContentResponse{Release: rel}, err
}

func (s *releaseServer) UpdateRelease(c ctx.Context, req *services.UpdateReleaseRequest) (*services.UpdateReleaseResponse, error) {
	if !checkClientVersion(c) {
		return nil, errIncompatibleVersion
	}

	currentRelease, updatedRelease, err := s.prepareUpdate(req)
	if err != nil {
		return nil, err
	}

	res, err := s.performUpdate(currentRelease, updatedRelease, req)
	if err != nil {
		return nil, err
	}

	if !req.DryRun {
		if err := s.env.Releases.Create(updatedRelease); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (s *releaseServer) performUpdate(originalRelease, updatedRelease *release.Release, req *services.UpdateReleaseRequest) (*services.UpdateReleaseResponse, error) {
	res := &services.UpdateReleaseResponse{Release: updatedRelease}

	if req.DryRun {
		log.Printf("Dry run for %s", updatedRelease.Name)
		return res, nil
	}

	// pre-ugrade hooks
	if !req.DisableHooks {
		if err := s.execHook(updatedRelease.Hooks, updatedRelease.Name, updatedRelease.Namespace, preUpgrade); err != nil {
			return res, err
		}
	}

	if err := s.performKubeUpdate(originalRelease, updatedRelease); err != nil {
		return nil, err
	}

	// post-upgrade hooks
	if !req.DisableHooks {
		if err := s.execHook(updatedRelease.Hooks, updatedRelease.Name, updatedRelease.Namespace, postUpgrade); err != nil {
			return res, err
		}
	}

	originalRelease.Info.Status.Code = release.Status_SUPERSEDED
	if err := s.env.Releases.Update(originalRelease); err != nil {
		return nil, fmt.Errorf("Update of %s failed: %s", originalRelease.Name, err)
	}

	updatedRelease.Info.Status.Code = release.Status_DEPLOYED

	return res, nil
}

// prepareUpdate builds an updated release for an update operation.
func (s *releaseServer) prepareUpdate(req *services.UpdateReleaseRequest) (*release.Release, *release.Release, error) {
	if req.Name == "" {
		return nil, nil, errMissingRelease
	}

	if req.Chart == nil {
		return nil, nil, errMissingChart
	}

	// finds the non-deleted release with the given name
	currentRelease, err := s.env.Releases.Deployed(req.Name)
	if err != nil {
		return nil, nil, err
	}

	ts := timeconv.Now()
	options := chartutil.ReleaseOptions{
		Name:      req.Name,
		Time:      ts,
		Namespace: currentRelease.Namespace,
	}

	valuesToRender, err := chartutil.ToRenderValues(req.Chart, req.Values, options)
	if err != nil {
		return nil, nil, err
	}

	hooks, manifestDoc, notesTxt, err := s.renderResources(req.Chart, valuesToRender)
	if err != nil {
		return nil, nil, err
	}

	// Store an updated release.
	updatedRelease := &release.Release{
		Name:      req.Name,
		Namespace: currentRelease.Namespace,
		Chart:     req.Chart,
		Config:    req.Values,
		Info: &release.Info{
			FirstDeployed: currentRelease.Info.FirstDeployed,
			LastDeployed:  ts,
			Status:        &release.Status{Code: release.Status_UNKNOWN},
		},
		Version:  currentRelease.Version + 1,
		Manifest: manifestDoc.String(),
		Hooks:    hooks,
	}

	if len(notesTxt) > 0 {
		updatedRelease.Info.Status.Notes = notesTxt
	}
	return currentRelease, updatedRelease, nil
}

func (s *releaseServer) RollbackRelease(c ctx.Context, req *services.RollbackReleaseRequest) (*services.RollbackReleaseResponse, error) {
	if !checkClientVersion(c) {
		return nil, errIncompatibleVersion
	}

	currentRelease, targetRelease, err := s.prepareRollback(req)
	if err != nil {
		return nil, err
	}

	rel, err := s.performRollback(currentRelease, targetRelease, req)
	if err != nil {
		return nil, err
	}

	if !req.DryRun {
		if err := s.env.Releases.Create(targetRelease); err != nil {
			return nil, err
		}
	}

	return rel, nil
}

func (s *releaseServer) performRollback(currentRelease, targetRelease *release.Release, req *services.RollbackReleaseRequest) (*services.RollbackReleaseResponse, error) {
	res := &services.RollbackReleaseResponse{Release: targetRelease}

	if req.DryRun {
		log.Printf("Dry run for %s", targetRelease.Name)
		return res, nil
	}

	// pre-rollback hooks
	if !req.DisableHooks {
		if err := s.execHook(targetRelease.Hooks, targetRelease.Name, targetRelease.Namespace, preRollback); err != nil {
			return res, err
		}
	}

	if err := s.performKubeUpdate(currentRelease, targetRelease); err != nil {
		return nil, err
	}

	// post-rollback hooks
	if !req.DisableHooks {
		if err := s.execHook(targetRelease.Hooks, targetRelease.Name, targetRelease.Namespace, postRollback); err != nil {
			return res, err
		}
	}

	currentRelease.Info.Status.Code = release.Status_SUPERSEDED
	if err := s.env.Releases.Update(currentRelease); err != nil {
		return nil, fmt.Errorf("Update of %s failed: %s", currentRelease.Name, err)
	}

	targetRelease.Info.Status.Code = release.Status_DEPLOYED

	return res, nil
}

func (s *releaseServer) performKubeUpdate(currentRelease, targetRelease *release.Release) error {
	kubeCli := s.env.KubeClient
	current := bytes.NewBufferString(currentRelease.Manifest)
	target := bytes.NewBufferString(targetRelease.Manifest)
	return kubeCli.Update(targetRelease.Namespace, current, target)
}

// prepareRollback finds the previous release and prepares a new release object with
//  the previous release's configuration
func (s *releaseServer) prepareRollback(req *services.RollbackReleaseRequest) (*release.Release, *release.Release, error) {

	if req.Name == "" {
		return nil, nil, errMissingRelease
	}

	// finds the non-deleted release with the given name
	currentRelease, err := s.env.Releases.Deployed(req.Name)
	if err != nil {
		return nil, nil, err
	}

	previousRelease, err := s.env.Releases.Get(req.Name, currentRelease.Version-1)
	if err != nil {
		return nil, nil, err
	}

	ts := timeconv.Now()

	// Store a new release object with previous release's configuration
	targetRelease := &release.Release{
		Name:      req.Name,
		Namespace: currentRelease.Namespace,
		Chart:     previousRelease.Chart,
		Config:    previousRelease.Config,
		Info: &release.Info{
			FirstDeployed: currentRelease.Info.FirstDeployed,
			LastDeployed:  ts,
			Status: &release.Status{
				Code:  release.Status_UNKNOWN,
				Notes: previousRelease.Info.Status.Notes,
			},
		},
		Version:  currentRelease.Version + 1,
		Manifest: previousRelease.Manifest,
		Hooks:    previousRelease.Hooks,
	}

	return currentRelease, targetRelease, nil
}

func (s *releaseServer) uniqName(start string, reuse bool) (string, error) {

	// If a name is supplied, we check to see if that name is taken. If not, it
	// is granted. If reuse is true and a deleted release with that name exists,
	// we re-grant it. Otherwise, an error is returned.
	if start != "" {

		if len(start) > releaseNameMaxLen {
			return "", fmt.Errorf("release name %q exceeds max length of %d", start, releaseNameMaxLen)
		}

		if rel, err := s.env.Releases.Get(start, 1); err == driver.ErrReleaseNotFound {
			return start, nil
		} else if st := rel.Info.Status.Code; reuse && (st == release.Status_DELETED || st == release.Status_FAILED) {
			// Allowe re-use of names if the previous release is marked deleted.
			log.Printf("reusing name %q", start)
			return start, nil
		} else if reuse {
			return "", errors.New("cannot re-use a name that is still in use")
		}

		return "", fmt.Errorf("a release named %q already exists", start)
	}

	maxTries := 5
	for i := 0; i < maxTries; i++ {
		namer := moniker.New()
		name := namer.NameSep("-")
		if len(name) > releaseNameMaxLen {
			name = name[:releaseNameMaxLen]
		}
		if _, err := s.env.Releases.Get(name, 1); err == driver.ErrReleaseNotFound {
			return name, nil
		}
		log.Printf("info: Name %q is taken. Searching again.", name)
	}
	log.Printf("warning: No available release names found after %d tries", maxTries)
	return "ERROR", errors.New("no available release name found")
}

func (s *releaseServer) engine(ch *chart.Chart) environment.Engine {
	renderer := s.env.EngineYard.Default()
	if ch.Metadata.Engine != "" {
		if r, ok := s.env.EngineYard.Get(ch.Metadata.Engine); ok {
			renderer = r
		} else {
			log.Printf("warning: %s requested non-existent template engine %s", ch.Metadata.Name, ch.Metadata.Engine)
		}
	}
	return renderer
}

func (s *releaseServer) InstallRelease(c ctx.Context, req *services.InstallReleaseRequest) (*services.InstallReleaseResponse, error) {
	if !checkClientVersion(c) {
		return nil, errIncompatibleVersion
	}

	rel, err := s.prepareRelease(req)
	if err != nil {
		log.Printf("Failed install prepare step: %s", err)
		return nil, err
	}

	res, err := s.performRelease(rel, req)
	if err != nil {
		log.Printf("Failed install perform step: %s", err)
	}
	return res, err
}

// prepareRelease builds a release for an install operation.
func (s *releaseServer) prepareRelease(req *services.InstallReleaseRequest) (*release.Release, error) {
	if req.Chart == nil {
		return nil, errMissingChart
	}

	name, err := s.uniqName(req.Name, req.ReuseName)
	if err != nil {
		return nil, err
	}

	ts := timeconv.Now()
	options := chartutil.ReleaseOptions{Name: name, Time: ts, Namespace: req.Namespace}
	valuesToRender, err := chartutil.ToRenderValues(req.Chart, req.Values, options)
	if err != nil {
		return nil, err
	}

	hooks, manifestDoc, notesTxt, err := s.renderResources(req.Chart, valuesToRender)
	if err != nil {
		return nil, err
	}

	// Store a release.
	rel := &release.Release{
		Name:      name,
		Namespace: req.Namespace,
		Chart:     req.Chart,
		Config:    req.Values,
		Info: &release.Info{
			FirstDeployed: ts,
			LastDeployed:  ts,
			Status:        &release.Status{Code: release.Status_UNKNOWN},
		},
		Manifest: manifestDoc.String(),
		Hooks:    hooks,
		Version:  1,
	}
	if len(notesTxt) > 0 {
		rel.Info.Status.Notes = notesTxt
	}
	return rel, nil
}

func (s *releaseServer) getVersionSet() (versionSet, error) {
	defVersions := newVersionSet("v1")
	cli, err := s.env.KubeClient.APIClient()
	if err != nil {
		log.Printf("API Client for Kubernetes is missing: %s.", err)
		return defVersions, err
	}

	groups, err := cli.Discovery().ServerGroups()
	if err != nil {
		return defVersions, err
	}

	// FIXME: The Kubernetes test fixture for cli appears to always return nil
	// for calls to Discovery().ServerGroups(). So in this case, we return
	// the default API list. This is also a safe value to return in any other
	// odd-ball case.
	if groups == nil {
		return defVersions, nil
	}

	versions := unversioned.ExtractGroupVersions(groups)
	return newVersionSet(versions...), nil
}

func (s *releaseServer) renderResources(ch *chart.Chart, values chartutil.Values) ([]*release.Hook, *bytes.Buffer, string, error) {
	renderer := s.engine(ch)
	files, err := renderer.Render(ch, values)
	if err != nil {
		return nil, nil, "", err
	}

	// NOTES.txt gets rendered like all the other files, but because it's not a hook nor a resource,
	// pull it out of here into a separate file so that we can actually use the output of the rendered
	// text file. We have to spin through this map because the file contains path information, so we
	// look for terminating NOTES.txt. We also remove it from the files so that we don't have to skip
	// it in the sortHooks.
	notes := ""
	for k, v := range files {
		if strings.HasSuffix(k, notesFileSuffix) {
			notes = v
			delete(files, k)
		}
	}

	// Sort hooks, manifests, and partials. Only hooks and manifests are returned,
	// as partials are not used after renderer.Render. Empty manifests are also
	// removed here.
	vs, err := s.getVersionSet()
	if err != nil {
		return nil, nil, "", fmt.Errorf("Could not get apiVersions from Kubernetes: %s", err)
	}
	hooks, manifests, err := sortManifests(files, vs, InstallOrder)
	if err != nil {
		// By catching parse errors here, we can prevent bogus releases from going
		// to Kubernetes.
		return nil, nil, "", err
	}

	// Aggregate all valid manifests into one big doc.
	b := bytes.NewBuffer(nil)
	for _, m := range manifests {
		b.WriteString("\n---\n# Source: " + m.name + "\n")
		b.WriteString(m.content)
	}

	return hooks, b, notes, nil
}

// validateYAML checks to see if YAML is well-formed.
func validateYAML(data string) error {
	b := map[string]interface{}{}
	return yaml.Unmarshal([]byte(data), b)
}

func (s *releaseServer) recordRelease(r *release.Release, reuse bool) {
	if reuse {
		if err := s.env.Releases.Update(r); err != nil {
			log.Printf("warning: Failed to update release %q: %s", r.Name, err)
		}
	} else if err := s.env.Releases.Create(r); err != nil {
		log.Printf("warning: Failed to record release %q: %s", r.Name, err)
	}
}

// performRelease runs a release.
func (s *releaseServer) performRelease(r *release.Release, req *services.InstallReleaseRequest) (*services.InstallReleaseResponse, error) {
	res := &services.InstallReleaseResponse{Release: r}

	if req.DryRun {
		log.Printf("Dry run for %s", r.Name)
		return res, nil
	}

	// pre-install hooks
	if !req.DisableHooks {
		if err := s.execHook(r.Hooks, r.Name, r.Namespace, preInstall); err != nil {
			return res, err
		}
	}

	// regular manifests
	kubeCli := s.env.KubeClient
	b := bytes.NewBufferString(r.Manifest)
	if err := kubeCli.Create(r.Namespace, b); err != nil {
		log.Printf("warning: Release %q failed: %s", r.Name, err)
		r.Info.Status.Code = release.Status_FAILED
		s.recordRelease(r, req.ReuseName)
		return res, fmt.Errorf("release %s failed: %s", r.Name, err)
	}

	// post-install hooks
	if !req.DisableHooks {
		if err := s.execHook(r.Hooks, r.Name, r.Namespace, postInstall); err != nil {
			log.Printf("warning: Release %q failed post-install: %s", r.Name, err)
			r.Info.Status.Code = release.Status_FAILED
			s.recordRelease(r, req.ReuseName)
			return res, err
		}
	}

	// This is a tricky case. The release has been created, but the result
	// cannot be recorded. The truest thing to tell the user is that the
	// release was created. However, the user will not be able to do anything
	// further with this release.
	//
	// One possible strategy would be to do a timed retry to see if we can get
	// this stored in the future.
	r.Info.Status.Code = release.Status_DEPLOYED
	s.recordRelease(r, req.ReuseName)
	return res, nil
}

func (s *releaseServer) execHook(hs []*release.Hook, name, namespace, hook string) error {
	kubeCli := s.env.KubeClient
	code, ok := events[hook]
	if !ok {
		return fmt.Errorf("unknown hook %q", hook)
	}

	log.Printf("Executing %s hooks for %s", hook, name)
	for _, h := range hs {
		found := false
		for _, e := range h.Events {
			if e == code {
				found = true
			}
		}
		// If this doesn't implement the hook, skip it.
		if !found {
			continue
		}

		b := bytes.NewBufferString(h.Manifest)
		if err := kubeCli.Create(namespace, b); err != nil {
			log.Printf("warning: Release %q pre-install %s failed: %s", name, h.Path, err)
			return err
		}
		// No way to rewind a bytes.Buffer()?
		b.Reset()
		b.WriteString(h.Manifest)
		if err := kubeCli.WatchUntilReady(namespace, b); err != nil {
			log.Printf("warning: Release %q pre-install %s could not complete: %s", name, h.Path, err)
			return err
		}
		h.LastRun = timeconv.Now()
	}
	log.Printf("Hooks complete for %s %s", hook, name)
	return nil
}

func (s *releaseServer) UninstallRelease(c ctx.Context, req *services.UninstallReleaseRequest) (*services.UninstallReleaseResponse, error) {
	if !checkClientVersion(c) {
		return nil, errIncompatibleVersion
	}

	if req.Name == "" {
		log.Printf("uninstall: Release not found: %s", req.Name)
		return nil, errMissingRelease
	}

	rel, err := s.env.Releases.Deployed(req.Name)
	if err != nil {
		log.Printf("uninstall: Release not loaded: %s", req.Name)
		return nil, err
	}

	// TODO: Are there any cases where we want to force a delete even if it's
	// already marked deleted?
	if rel.Info.Status.Code == release.Status_DELETED {
		if req.Purge {
			if _, err := s.env.Releases.Delete(rel.Name, rel.Version); err != nil {
				log.Printf("uninstall: Failed to purge the release: %s", err)
				return nil, err
			}
			return &services.UninstallReleaseResponse{Release: rel}, nil
		}
		return nil, fmt.Errorf("the release named %q is already deleted", req.Name)
	}

	log.Printf("uninstall: Deleting %s", req.Name)
	rel.Info.Status.Code = release.Status_DELETED
	rel.Info.Deleted = timeconv.Now()
	res := &services.UninstallReleaseResponse{Release: rel}

	if !req.DisableHooks {
		if err := s.execHook(rel.Hooks, rel.Name, rel.Namespace, preDelete); err != nil {
			return res, err
		}
	}

	vs, err := s.getVersionSet()
	if err != nil {
		return nil, fmt.Errorf("Could not get apiVersions from Kubernetes: %s", err)
	}

	manifests := splitManifests(rel.Manifest)
	_, files, err := sortManifests(manifests, vs, UninstallOrder)
	if err != nil {
		// We could instead just delete everything in no particular order.
		return nil, err
	}
	// Note: We could re-join these into one file and delete just that one. Or
	// we could collect errors (instead of bailing on the first error) and try
	// to delete as much as possible instead of failing at the first error.
	for _, file := range files {
		b := bytes.NewBufferString(file.content)
		if err := s.env.KubeClient.Delete(rel.Namespace, b); err != nil {
			log.Printf("uninstall: Failed deletion of %q: %s", req.Name, err)
			return nil, err
		}
	}

	if !req.DisableHooks {
		if err := s.execHook(rel.Hooks, rel.Name, rel.Namespace, postDelete); err != nil {
			return res, err
		}
	}

	if !req.Purge {
		if err := s.env.Releases.Update(rel); err != nil {
			log.Printf("uninstall: Failed to store updated release: %s", err)
		}
	} else {
		if _, err := s.env.Releases.Delete(rel.Name, rel.Version); err != nil {
			log.Printf("uninstall: Failed to purge the release: %s", err)
		}
	}

	return res, nil
}

// byName implements the sort.Interface for []*release.Release.
type byName []*release.Release

func (r byName) Len() int {
	return len(r)
}
func (r byName) Swap(p, q int) {
	r[p], r[q] = r[q], r[p]
}
func (r byName) Less(i, j int) bool {
	return r[i].Name < r[j].Name
}

type byDate []*release.Release

func (r byDate) Len() int { return len(r) }
func (r byDate) Swap(p, q int) {
	r[p], r[q] = r[q], r[p]
}
func (r byDate) Less(p, q int) bool {
	return r[p].Info.LastDeployed.Seconds < r[q].Info.LastDeployed.Seconds
}

func splitManifests(bigfile string) map[string]string {
	// This is not the best way of doing things, but it's how k8s itself does it.
	// Basically, we're quickly splitting a stream of YAML documents into an
	// array of YAML docs. In the current implementation, the file name is just
	// a place holder, and doesn't have any further meaning.
	sep := "\n---\n"
	tpl := "manifest-%d"
	res := map[string]string{}
	tmp := strings.Split(bigfile, sep)
	for i, d := range tmp {
		res[fmt.Sprintf(tpl, i)] = d
	}
	return res
}
