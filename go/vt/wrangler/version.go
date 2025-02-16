/*
Copyright 2019 The Vitess Authors.

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

package wrangler

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"sync"

	"context"

	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/topo/topoproto"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

var getVersionFromTabletDebugVars = func(tabletAddr string) (string, error) {
	resp, err := http.Get("http://" + tabletAddr + "/debug/vars")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var vars struct {
		BuildHost      string
		BuildUser      string
		BuildTimestamp int64
		BuildGitRev    string
	}
	err = json.Unmarshal(body, &vars)
	if err != nil {
		return "", err
	}

	version := fmt.Sprintf("%v", vars)
	return version, nil
}

var getVersionFromTablet = getVersionFromTabletDebugVars

// ResetDebugVarsGetVersion is used by tests to reset the
// getVersionFromTablet variable to the default one. That way we can
// run the unit tests in testlib/ even when another implementation of
// getVersionFromTablet is used.
func ResetDebugVarsGetVersion() {
	getVersionFromTablet = getVersionFromTabletDebugVars
}

// GetVersion returns the version string from a tablet
func (wr *Wrangler) GetVersion(ctx context.Context, tabletAlias *topodatapb.TabletAlias) (string, error) {
	tablet, err := wr.ts.GetTablet(ctx, tabletAlias)
	if err != nil {
		return "", err
	}

	version, err := getVersionFromTablet(tablet.Addr())
	if err != nil {
		return "", err
	}
	log.Infof("Tablet %v is running version '%v'", topoproto.TabletAliasString(tabletAlias), version)
	return version, err
}

// helper method to asynchronously get and diff a version
func (wr *Wrangler) diffVersion(ctx context.Context, primaryVersion string, primaryAlias *topodatapb.TabletAlias, alias *topodatapb.TabletAlias, wg *sync.WaitGroup, er concurrency.ErrorRecorder) {
	defer wg.Done()
	log.Infof("Gathering version for %v", topoproto.TabletAliasString(alias))
	replicaVersion, err := wr.GetVersion(ctx, alias)
	if err != nil {
		er.RecordError(err)
		return
	}

	if primaryVersion != replicaVersion {
		er.RecordError(fmt.Errorf("primary %v version %v is different than replica %v version %v", topoproto.TabletAliasString(primaryAlias), primaryVersion, topoproto.TabletAliasString(alias), replicaVersion))
	}
}

// ValidateVersionShard validates all versions are the same in all
// tablets in a shard
func (wr *Wrangler) ValidateVersionShard(ctx context.Context, keyspace, shard string) error {
	si, err := wr.ts.GetShard(ctx, keyspace, shard)
	if err != nil {
		return err
	}

	// get version from the primary, or error
	if !si.HasPrimary() {
		return fmt.Errorf("no primary in shard %v/%v", keyspace, shard)
	}
	log.Infof("Gathering version for primary %v", topoproto.TabletAliasString(si.PrimaryAlias))
	primaryVersion, err := wr.GetVersion(ctx, si.PrimaryAlias)
	if err != nil {
		return err
	}

	// read all the aliases in the shard, that is all tablets that are
	// replicating from the primary
	aliases, err := wr.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
	if err != nil {
		return err
	}

	// then diff with all replicas
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, alias := range aliases {
		if topoproto.TabletAliasEqual(alias, si.PrimaryAlias) {
			continue
		}

		wg.Add(1)
		go wr.diffVersion(ctx, primaryVersion, si.PrimaryAlias, alias, &wg, &er)
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("version diffs: %v", er.Error().Error())
	}
	return nil
}

// ValidateVersionKeyspace validates all versions are the same in all
// tablets in a keyspace
func (wr *Wrangler) ValidateVersionKeyspace(ctx context.Context, keyspace string) error {
	// find all the shards
	shards, err := wr.ts.GetShardNames(ctx, keyspace)
	if err != nil {
		return err
	}

	// corner cases
	if len(shards) == 0 {
		return fmt.Errorf("no shards in keyspace %v", keyspace)
	}
	sort.Strings(shards)
	if len(shards) == 1 {
		return wr.ValidateVersionShard(ctx, keyspace, shards[0])
	}

	// find the reference version using the first shard's primary
	si, err := wr.ts.GetShard(ctx, keyspace, shards[0])
	if err != nil {
		return err
	}
	if !si.HasPrimary() {
		return fmt.Errorf("no primary in shard %v/%v", keyspace, shards[0])
	}
	referenceAlias := si.PrimaryAlias
	log.Infof("Gathering version for reference primary %v", topoproto.TabletAliasString(referenceAlias))
	referenceVersion, err := wr.GetVersion(ctx, referenceAlias)
	if err != nil {
		return err
	}

	// then diff with all tablets but primary 0
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, shard := range shards {
		aliases, err := wr.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
		if err != nil {
			er.RecordError(err)
			continue
		}

		for _, alias := range aliases {
			if topoproto.TabletAliasEqual(alias, si.PrimaryAlias) {
				continue
			}

			wg.Add(1)
			go wr.diffVersion(ctx, referenceVersion, referenceAlias, alias, &wg, &er)
		}
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("version diffs: %v", er.Error().Error())
	}
	return nil
}
