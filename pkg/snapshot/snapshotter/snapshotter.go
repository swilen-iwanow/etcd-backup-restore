// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapshotter

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/gardener/etcd-backup-restore/pkg/errors"
	"github.com/gardener/etcd-backup-restore/pkg/retry"
	"github.com/gardener/etcd-backup-restore/pkg/snapstore"
	"github.com/robfig/cron"
	"github.com/sirupsen/logrus"
)

// NewSnapshotter returns the snapshotter object.
func NewSnapshotter(schedule string, store snapstore.SnapStore, logger *logrus.Logger, maxBackups, deltaSnapshotIntervalSeconds int, etcdConnectionTimeout time.Duration, tlsConfig *TLSConfig) (*Snapshotter, error) {
	logger.Printf("Validating schedule...")
	sdl, err := cron.ParseStandard(schedule)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule provied %s : %v", schedule, err)
	}

	return &Snapshotter{
		logger:                       logger,
		schedule:                     sdl,
		store:                        store,
		maxBackups:                   maxBackups,
		etcdConnectionTimeout:        etcdConnectionTimeout,
		tlsConfig:                    tlsConfig,
		deltaSnapshotIntervalSeconds: deltaSnapshotIntervalSeconds,
		//currentDeltaSnapshotCount:    0,
	}, nil
}

// NewTLSConfig returns the TLSConfig object.
func NewTLSConfig(cert, key, caCert string, insecureTr, skipVerify bool, endpoints []string) *TLSConfig {
	return &TLSConfig{
		cert:       cert,
		key:        key,
		caCert:     caCert,
		insecureTr: insecureTr,
		skipVerify: skipVerify,
		endpoints:  endpoints,
	}
}

// Run process loop for scheduled backup
func (ssr *Snapshotter) Run(stopCh <-chan struct{}) error {
	var (
		wg                = &sync.WaitGroup{}
		now               = time.Now()
		effective         = ssr.schedule.Next(now)
		fullSnapshotTimer = time.NewTimer(effective.Sub(now))
		fullSnapshotCh    = make(chan time.Time)
		deltaStopCh       = make(chan bool)
		config            = &retry.Config{
			Attempts: 6,
			Delay:    1,
			Units:    time.Second,
			Logger:   ssr.logger,
		}
	)

	if effective.IsZero() {
		ssr.logger.Infoln("There are no backup scheduled for future. Stopping now.")
		close(fullSnapshotCh)
		return nil
	}

	// Add event based on full snapshot period
	go func(stopCh <-chan struct{}) {
		for {
			select {
			case tempTime := <-fullSnapshotTimer.C:
				fullSnapshotCh <- tempTime
			case <-stopCh:
				fullSnapshotTimer.Stop()
				return
			}
		}
	}(stopCh)

	if err := ssr.TakeFullSnapshot(); err != nil {
		return err
	}
	if err := ssr.applyWatch(wg, fullSnapshotCh, deltaStopCh); err != nil {
		return err
	}

	ssr.logger.Infof("Will take next full snapshot at time: %s", effective)
	for {
		select {
		case <-stopCh:
			ssr.logger.Infof("Stop signal received. Terminating scheduled snapshot.")
			close(fullSnapshotCh)
			close(deltaStopCh)
			return nil

		case <-fullSnapshotCh:
			deltaStopCh <- true
			wg.Wait()
			if err := retry.Do(func() error {
				ssr.logger.Infof("Taking scheduled snapshot for time: %s", time.Now().Local())
				err := ssr.TakeFullSnapshot()
				if err != nil {
					ssr.logger.Infof("Taking scheduled snapshot failed: %v", err)
				}
				return err
			}, config); err != nil {
				// As per design principle, in business critical service if backup is not working,
				// it's better to fail the process. So, we are quiting here.
				return err
			}

			now = time.Now()
			effective = ssr.schedule.Next(now)

			// TODO: move this garbage collector to paraller thread
			ssr.garbageCollector()
			if effective.IsZero() {
				ssr.logger.Infoln("There are no backup scheduled for future. Stopping now.")
				return nil
			}
			fullSnapshotTimer.Stop()
			fullSnapshotTimer.Reset(effective.Sub(now))
			ssr.logger.Infof("Will take next full snapshot at time: %s", effective)
			if err := ssr.applyWatch(wg, fullSnapshotCh, deltaStopCh); err != nil {
				return err
			}
		}

	}
}

// TakeFullSnapshot will store full snapshot of etcd to snapstore.
// It basically will connect to etcd. Then ask for snapshot. And finally
// store it to underlying snapstore on the fly.
func (ssr *Snapshotter) TakeFullSnapshot() error {
	client, err := GetTLSClientForEtcd(ssr.tlsConfig)
	if err != nil {
		return &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd client: %v", err),
		}
	}

	ctx, cancel := context.WithTimeout(context.TODO(), ssr.etcdConnectionTimeout*time.Second)
	defer cancel()
	resp, err := client.Get(ctx, "", clientv3.WithLastRev()...)
	if err != nil {
		return &errors.EtcdError{
			Message: fmt.Sprintf("failed to get etcd latest revision: %v", err),
		}
	}
	lastRevision := resp.Header.Revision
	rc, err := client.Snapshot(ctx)
	if err != nil {
		return &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd snapshot: %v", err),
		}
	}
	ssr.logger.Infof("Successfully opened snapshot reader on etcd")
	s := snapstore.Snapshot{
		Kind:          snapstore.SnapshotKindFull,
		CreatedOn:     time.Now(),
		StartRevision: 0,
		LastRevision:  lastRevision,
	}
	s.GenerateSnapshotDirectory()
	s.GenerateSnapshotName()

	if err := ssr.store.Save(s, rc); err != nil {
		return &errors.SnapstoreError{
			Message: fmt.Sprintf("failed to save snapshot: %v", err),
		}
	}
	ssr.prevSnapshot = s
	ssr.logger.Infof("Successfully saved full snapshot at: %s", path.Join(s.SnapDir, s.SnapName))
	return nil
}

// garbageCollector basically consider the older backups as garbage and deletes it
func (ssr *Snapshotter) garbageCollector() {
	ssr.logger.Infoln("Executing garbage collection...")
	snapList, err := ssr.store.List()
	if err != nil {
		ssr.logger.Warnf("Failed to list snapshots: %v", err)
		return
	}
	snapLen := len(snapList)
	for i := 0; i < (snapLen - ssr.maxBackups); i++ {
		ssr.logger.Infof("Deleting old snapshot: %s", path.Join(snapList[i].SnapDir, snapList[i].SnapName))
		if err := ssr.store.Delete(*snapList[i]); err != nil {
			ssr.logger.Warnf("Failed to delete snapshot: %s", path.Join(snapList[i].SnapDir, snapList[i].SnapName))
		}
	}
}

// GetTLSClientForEtcd creates an etcd client using the TLS config params.
func GetTLSClientForEtcd(tlsConfig *TLSConfig) (*clientv3.Client, error) {
	// set tls if any one tls option set
	var cfgtls *transport.TLSInfo
	tlsinfo := transport.TLSInfo{}
	if tlsConfig.cert != "" {
		tlsinfo.CertFile = tlsConfig.cert
		cfgtls = &tlsinfo
	}

	if tlsConfig.key != "" {
		tlsinfo.KeyFile = tlsConfig.key
		cfgtls = &tlsinfo
	}

	if tlsConfig.caCert != "" {
		tlsinfo.CAFile = tlsConfig.caCert
		cfgtls = &tlsinfo
	}

	cfg := &clientv3.Config{
		Endpoints: tlsConfig.endpoints,
	}

	if cfgtls != nil {
		clientTLS, err := cfgtls.ClientConfig()
		if err != nil {
			return nil, err
		}
		cfg.TLS = clientTLS
	}

	// if key/cert is not given but user wants secure connection, we
	// should still setup an empty tls configuration for gRPC to setup
	// secure connection.
	if cfg.TLS == nil && !tlsConfig.insecureTr {
		cfg.TLS = &tls.Config{}
	}

	// If the user wants to skip TLS verification then we should set
	// the InsecureSkipVerify flag in tls configuration.
	if tlsConfig.skipVerify && cfg.TLS != nil {
		cfg.TLS.InsecureSkipVerify = true
	}

	return clientv3.New(*cfg)
}

// applyWatch applies the new watch on etcd and start go routine to periodically take delta snapshots
func (ssr *Snapshotter) applyWatch(wg *sync.WaitGroup, fullSnapshotCh chan<- time.Time, stopCh <-chan bool) error {
	client, err := GetTLSClientForEtcd(ssr.tlsConfig)
	if err != nil {
		return &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd client: %v", err),
		}
	}
	go ssr.processWatch(wg, client, fullSnapshotCh, stopCh)
	wg.Add(1)
	return nil
}

// processWatch processess watch to take delta snapshot periodically by collecting set of events within period
func (ssr *Snapshotter) processWatch(wg *sync.WaitGroup, client *clientv3.Client, fullSnapshotCh chan<- time.Time, stopCh <-chan bool) {
	ctx := context.TODO()
	watchCh := client.Watch(ctx, "", clientv3.WithPrefix(), clientv3.WithRev(ssr.prevSnapshot.LastRevision+1))
	ssr.logger.Infof("Applied watch on etcd from revision: %8d", ssr.prevSnapshot.LastRevision+1)

	var (
		currentRev int64
		events     = []*event{}
		timer      = time.NewTimer(time.Second * time.Duration(ssr.deltaSnapshotIntervalSeconds))
	)
	for {
		select {
		case <-stopCh:
			ssr.logger.Infoln("Received stop signal. Terminating current watch...")
			wg.Done()
			return

		case wr, ok := <-watchCh:
			if !ok {
				ssr.logger.Warnln("watch channel closed")
				fullSnapshotCh <- time.Now()
				return
			}
			if wr.Canceled {
				ssr.logger.Warnln("watch canceled")
				fullSnapshotCh <- time.Now()
				return
			}
			if wr.CompactRevision != 0 {
				//TODO Handle this control flow
				ssr.logger.Warnln("failed to keep watch. etcd server has compacted required revision")
				fullSnapshotCh <- time.Now()
				return
			}

			//aggregate events
			for _, ev := range wr.Events {
				currentRev = ev.Kv.ModRevision
				events = append(events, newEvent(ev))
			}

		case <-timer.C:
			//persist aggregated events
			if len(events) != 0 {
				if err := ssr.saveDeltaSnapshot(events, currentRev); err != nil {
					ssr.logger.Errorf("failed to take new delta snapshot: %s", err)
					fullSnapshotCh <- time.Now()
					return
				}
				events = []*event{}
			} else {
				ssr.logger.Infof("No events received to save snapshot.")
			}
			timer.Reset(time.Second * time.Duration(ssr.deltaSnapshotIntervalSeconds))
		}
	}
}

// saveDeltaSnapshot save the delta events compared to previous snapshot in new snapshot of kind Incr.
func (ssr *Snapshotter) saveDeltaSnapshot(ops []*event, lastRevision int64) error {
	snap := snapstore.Snapshot{
		Kind:          snapstore.SnapshotKindDelta,
		CreatedOn:     time.Now(),
		StartRevision: ssr.prevSnapshot.LastRevision + 1,
		LastRevision:  lastRevision,
		SnapDir:       ssr.prevSnapshot.SnapDir,
	}
	snap.GenerateSnapshotName()

	//TODO Append hash
	data, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	dataReader := bytes.NewReader(data)

	if err := ssr.store.Save(snap, dataReader); err != nil {
		return &errors.SnapstoreError{
			Message: fmt.Sprintf("failed to save snapshot: %v", err),
		}
	}

	ssr.prevSnapshot = snap
	ssr.logger.Infof("Successfully saved delta snapshot at: %s", path.Join(snap.SnapDir, snap.SnapName))
	return nil
}

func newEvent(e *clientv3.Event) *event {
	return &event{
		EtcdEvent: e,
		Time:      time.Now(),
	}
}
