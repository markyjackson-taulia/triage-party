// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package updater handles background refreshes of GitHub data
package updater

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/google/triage-party/pkg/logu"
	"github.com/google/triage-party/pkg/triage"

	"k8s.io/klog/v2"
)

// Minimum age to flush to avoid bad behavior
const minFlushAge = 5 * time.Second

type PFunc = func() error

type Config struct {
	Party       *triage.Party
	MinRefresh  time.Duration
	MaxRefresh  time.Duration
	PersistFunc PFunc
}

func New(cfg Config) *Updater {
	return &Updater{
		party:             cfg.Party,
		maxRefresh:        cfg.MaxRefresh,
		minRefresh:        cfg.MinRefresh,
		idleDuration:      5 * time.Minute,
		cache:             map[string]*triage.CollectionResult{},
		lastRequest:       sync.Map{},
		secondLastRequest: sync.Map{},
		loopEvery:         250 * time.Millisecond,
		mutex:             &sync.Mutex{},
		persistFunc:       cfg.PersistFunc,
		startTime:         time.Time{},
	}
}

type Updater struct {
	party             *triage.Party
	maxRefresh        time.Duration
	minRefresh        time.Duration
	idleDuration      time.Duration
	cache             map[string]*triage.CollectionResult
	lastRequest       sync.Map
	secondLastRequest sync.Map
	lastPersist       time.Time
	lastRun           time.Time
	startTime         time.Time
	loopEvery         time.Duration
	mutex             *sync.Mutex
	persistFunc       PFunc
	persistStart      time.Time
	updateCycles      int

	state string
}

// recordAccess records stats on collection accesses
func (u *Updater) recordAccess(id string) {
	last := u.lastRequested(id)
	if !last.IsZero() {
		u.secondLastRequest.Store(id, last)
	}
	u.lastRequest.Store(id, time.Now())
}

// State returns a basic state
func (u *Updater) Status() string {
	if !u.persistStart.IsZero() {
		return fmt.Sprintf("%s - persisting since %s (%d cycles, %s uptime)", u.state, u.persistStart, u.updateCycles, time.Since(u.startTime))
	}
	return fmt.Sprintf("%s (%d cycles, %s uptime)", u.state, u.updateCycles, time.Since(u.startTime))
}

// Lookup results for a given metric
func (u *Updater) Lookup(ctx context.Context, id string, blocking bool) *triage.CollectionResult {
	defer u.recordAccess(id)
	r := u.cache[id]
	if r == nil {
		if blocking {
			klog.Warningf("%s is not available in the cache, blocking page load!", id)
			if _, err := u.RefreshCollection(ctx, id, time.Time{}, true); err != nil {
				klog.Errorf("unable to run %s: %v", id, err)
			}
		} else {
			klog.Warningf("%s unavailable, but not blocking: happily returning nil", id)
		}
	}
	r = u.cache[id]
	return r
}

func (u *Updater) ForceRefresh(ctx context.Context, id string) *triage.CollectionResult {
	defer u.recordAccess(id)

	_, ok := u.lastRequest.Load(id)
	if !ok {
		klog.Warningf("ignoring refresh request, %s has never been requested", id)
		return u.Lookup(ctx, id, true)
	}

	start := time.Now()

	// At the risk of ignoring the user, this seems like a reasonable delta
	newerThan := start.Add(-1 * time.Second)

	klog.Infof("Forcing %s to refresh with data from %s or newer", id, newerThan)
	if _, err := u.RefreshCollection(ctx, id, newerThan, true); err != nil {
		klog.Errorf("update failed: %v", err)
	}
	klog.Infof("refresh complete for %s after %s", id, time.Since(start))
	return u.cache[id]
}

// shouldUpdate returns an error if a collection needs an update
func (u *Updater) shouldUpdate(id string, usedForStats bool, force bool) error {
	// The first cycle is based on a pared down set of results for faster initial load
	if u.updateCycles < 2 {
		return fmt.Errorf("cycle count is only %d", u.updateCycles)
	}

	result, ok := u.cache[id]
	if !ok {
		return fmt.Errorf("results are not cached")
	}

	resultAge := time.Since(result.Created)
	maxRefresh := u.maxRefresh

	// stats-based metrics can wait longer to refresh
	if usedForStats {
		maxRefresh *= 3
	}

	if resultAge > maxRefresh {
		return fmt.Errorf("%s at %s is older than max refresh age (%s), should update", id, logu.STime(result.Created), resultAge)
	}

	if force {
		return fmt.Errorf("force-mode enabled")
	}

	// collection has never been requested.
	if u.lastRequested(id).IsZero() {
		klog.V(4).Infof("%q has never been requested", id)
		return nil
	}

	if resultAge < u.minRefresh {
		klog.V(4).Infof("too soon since %q was refreshed (%s)", id, resultAge)
		return nil
	}

	// Back-off based on average of time since last two requests
	requestAge := time.Since(u.lastRequested(id))
	secondRequestDiff := u.lastRequested(id).Sub(u.secondLastRequested(id))
	needAge := ((requestAge + secondRequestDiff) / 2) + u.minRefresh
	if resultAge > needAge && !usedForStats {
		return fmt.Errorf("result age (%s) too old based on popularity", resultAge)
	}

	klog.V(4).Infof("no need to refresh %q", id)
	return nil
}

// lastRequested is the last time someone requested to view a collection
func (u *Updater) lastRequested(id string) time.Time {
	x, ok := u.lastRequest.Load(id)
	if !ok {
		return time.Time{}
	}

	lr, ok := x.(time.Time)
	if !ok {
		return time.Time{}
	}

	return lr
}

// secondLastRequested is the second last time someone requested to view a collection
func (u *Updater) secondLastRequested(id string) time.Time {
	x, ok := u.secondLastRequest.Load(id)
	if !ok {
		return u.startTime
	}

	lr, ok := x.(time.Time)
	if !ok {
		return u.startTime
	}

	return lr
}

func (u *Updater) update(ctx context.Context, s triage.Collection, newerThan time.Time) error {
	start := time.Now()
	u.state = fmt.Sprintf("updating %s to %s", s.ID, logu.STime(newerThan))

	klog.Infof(">>> updating %q with data newer than %s >>>", s.ID, logu.STime(newerThan))
	r, err := u.party.ExecuteCollection(ctx, s, newerThan)
	if err != nil {
		return err
	}
	u.cache[s.ID] = r
	klog.Infof("<<< updated %q to %s (oldest input: %s, duration: %s) <<<", s.ID, logu.STime(r.Created), logu.STime(r.OldestInput), time.Since(start))
	return nil
}

// Run a single collection, optionally forcing an update
func (u *Updater) RefreshCollection(ctx context.Context, id string, newerThan time.Time, force bool) (bool, error) {
	klog.V(5).Infof("RefreshCollection: %s newer than %s, force=%v (locking mutex)", id, newerThan, force)
	u.mutex.Lock()
	defer u.mutex.Unlock()

	s, err := u.party.LookupCollection(id)
	if err != nil {
		return false, err
	}

	err = u.shouldUpdate(s.ID, s.UsedForStats, force)
	if err == nil {
		return false, nil
	}

	klog.Infof("reason for updating %q: %v", s.ID, err)
	err = u.update(ctx, s, newerThan)
	return true, err
}

// Persist saves results to the persistence layer
func (u *Updater) Persist() error {
	if !u.persistStart.IsZero() {
		return errors.New("already persisting")
	}

	// advisory lock
	u.persistStart = time.Now()
	klog.Infof("*** Started to persist ...")

	defer func() {
		klog.Infof("*** Persist complete! Took %s", time.Since(u.persistStart))
		u.persistStart = time.Time{}
		u.lastPersist = time.Now()
	}()

	if err := u.persistFunc(); err != nil {
		return err
	}

	return nil
}

func (u *Updater) shouldPersist(updated bool) bool {
	// Already running
	if !u.persistStart.IsZero() {
		return false
	}

	// No new data to persist
	if !updated {
		return false
	}

	// Avoid write contention by fuzzing
	fuzz := time.Duration(rand.Intn(int(u.maxRefresh.Seconds()))) * time.Second
	cutoff := u.maxRefresh + fuzz

	sinceSave := time.Since(u.lastPersist)
	if sinceSave > cutoff {
		klog.Infof("Should persist: we have new data, and it's been %s since the last run", sinceSave)
		return true
	}

	return false
}

// Run once, optionally forcing an update
func (u *Updater) RunOnce(ctx context.Context, force bool) (bool, error) {
	updated := false
	start := time.Now()

	defer func() {
		if updated {
			klog.Infof("update cycle #%d took %s", u.updateCycles, time.Since(start))
			u.updateCycles++
		}
	}()

	if force {
		klog.Warning(">>> RunOnce has force enabled")
	} else {
		klog.V(5).Infof("RunOnce: force=%v", force)
	}

	sts, err := u.party.ListCollections()
	if err != nil {
		updated = false
		return updated, err
	}

	if u.lastRun.IsZero() {
		u.startTime = time.Now()
		force = true
	}

	newerThan := start.Add(-2 * minFlushAge)
	if u.updateCycles == 0 {
		klog.Info("have not yet completed a cycle - will accept stale results")
		newerThan = time.Time{}
	}

	var failed []string
	for _, s := range sts {
		// Run all collections with the same timestamp for maximum cache sharing
		runUpdated, err := u.RefreshCollection(ctx, s.ID, newerThan, force)
		if err != nil {
			klog.Errorf("%s failed to update: %v", s.ID, err)
			failed = append(failed, s.ID)
			continue
		}
		if runUpdated {
			updated = true
		}
	}

	if len(failed) > 0 {
		return updated, fmt.Errorf("collections failed: %v", failed)
	}

	return updated, nil
}

// Update loop
func (u *Updater) Loop(ctx context.Context) error {
	u.state = "starting loop"

	// Loop if everything goes to plan
	klog.Infof("Looping: data will be updated between %s and %s (loop every %s)", u.minRefresh, u.maxRefresh, u.loopEvery)
	ticker := time.NewTicker(u.loopEvery)
	defer ticker.Stop()
	for range ticker.C {
		updated, err := u.RunOnce(ctx, false)
		if err != nil {
			klog.Errorf("err: %v", err)
		}

		u.state = fmt.Sprintf("idle, waiting %s", u.loopEvery)
		u.lastRun = time.Now()

		if u.shouldPersist(updated) {
			go func() {
				if err := u.Persist(); err != nil {
					klog.Errorf("persist failed: %v", err)
				}
			}()
		}
	}
	return nil
}
