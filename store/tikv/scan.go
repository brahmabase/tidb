// Copyright 2025 Ekjot Singh
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"bytes"
	"context"

	"github.com/pingcap/errors"
	pb "github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

// Scanner support tikv scan
type Scanner struct {
	snapshot     *tikvSnapshot
	batchSize    int
	valid        bool
	cache        []*pb.KvPair
	idx          int
	nextStartKey []byte
	endKey       []byte
	eof          bool

	// Use for reverse scan.
	reverse    bool
	nextEndKey []byte
}

func newScanner(snapshot *tikvSnapshot, startKey []byte, endKey []byte, batchSize int, reverse bool) (*Scanner, error) {
	// It must be > 1. Otherwise scanner won't skipFirst.
	if batchSize <= 1 {
		batchSize = scanBatchSize
	}
	scanner := &Scanner{
		snapshot:     snapshot,
		batchSize:    batchSize,
		valid:        true,
		nextStartKey: startKey,
		endKey:       endKey,
		reverse:      reverse,
		nextEndKey:   endKey,
	}
	err := scanner.Next()
	if kv.IsErrNotFound(err) {
		return scanner, nil
	}
	return scanner, errors.Trace(err)
}

// Valid return valid.
func (s *Scanner) Valid() bool {
	return s.valid
}

// Key return key.
func (s *Scanner) Key() kv.Key {
	if s.valid {
		return s.cache[s.idx].Key
	}
	return nil
}

// Value return value.
func (s *Scanner) Value() []byte {
	if s.valid {
		return s.cache[s.idx].Value
	}
	return nil
}

// Next return next element.
func (s *Scanner) Next() error {
	bo := NewBackoffer(context.WithValue(context.Background(), txnStartKey, s.snapshot.version.Ver), scannerNextMaxBackoff)
	if !s.valid {
		return errors.New("scanner iterator is invalid")
	}
	var err error
	for {
		s.idx++
		if s.idx >= len(s.cache) {
			if s.eof {
				s.Close()
				return nil
			}
			err = s.getData(bo)
			if err != nil {
				s.Close()
				return errors.Trace(err)
			}
			if s.idx >= len(s.cache) {
				continue
			}
		}

		current := s.cache[s.idx]
		if (!s.reverse && (len(s.endKey) > 0 && kv.Key(current.Key).Cmp(kv.Key(s.endKey)) >= 0)) ||
			(s.reverse && len(s.nextStartKey) > 0 && kv.Key(current.Key).Cmp(kv.Key(s.nextStartKey)) < 0) {
			s.eof = true
			s.Close()
			return nil
		}
		// Try to resolve the lock
		if current.GetError() != nil {
			// 'current' would be modified if the lock being resolved
			if err := s.resolveCurrentLock(bo, current); err != nil {
				s.Close()
				return errors.Trace(err)
			}

			// The check here does not violate the KeyOnly semantic, because current's value
			// is filled by resolveCurrentLock which fetches the value by snapshot.get, so an empty
			// value stands for NotExist
			if len(current.Value) == 0 {
				continue
			}
		}
		return nil
	}
}

// Close close iterator.
func (s *Scanner) Close() {
	s.valid = false
}

func (s *Scanner) startTS() uint64 {
	return s.snapshot.version.Ver
}

func (s *Scanner) resolveCurrentLock(bo *Backoffer, current *pb.KvPair) error {
	val, err := s.snapshot.get(bo, kv.Key(current.Key))
	if err != nil {
		return errors.Trace(err)
	}
	current.Error = nil
	current.Value = val
	return nil
}

func (s *Scanner) getData(bo *Backoffer) error {
	logutil.Logger(context.Background()).Debug("txn getData",
		zap.Binary("nextStartKey", s.nextStartKey),
		zap.Binary("nextEndKey", s.nextEndKey),
		zap.Bool("reverse", s.reverse),
		zap.Uint64("txnStartTS", s.startTS()))
	sender := NewRegionRequestSender(s.snapshot.store.regionCache, s.snapshot.store.client)
	var reqEndKey, reqStartKey []byte
	var loc *KeyLocation
	var err error
	for {
		if !s.reverse {
			loc, err = s.snapshot.store.regionCache.LocateKey(bo, s.nextStartKey)
		} else {
			loc, err = s.snapshot.store.regionCache.LocateEndKey(bo, s.nextEndKey)
		}
		if err != nil {
			return errors.Trace(err)
		}

		if !s.reverse {
			reqEndKey = s.endKey
			if len(reqEndKey) > 0 && len(loc.EndKey) > 0 && bytes.Compare(loc.EndKey, reqEndKey) < 0 {
				reqEndKey = loc.EndKey
			}
		} else {
			reqStartKey = s.nextStartKey
			if len(reqStartKey) == 0 ||
				(len(loc.StartKey) > 0 && bytes.Compare(loc.StartKey, reqStartKey) > 0) {
				reqStartKey = loc.StartKey
			}
		}

		req := &tikvrpc.Request{
			Type: tikvrpc.CmdScan,
			Scan: &pb.ScanRequest{
				StartKey: s.nextStartKey,
				EndKey:   reqEndKey,
				Limit:    uint32(s.batchSize),
				Version:  s.startTS(),
				KeyOnly:  s.snapshot.keyOnly,
			},
			Context: pb.Context{
				Priority:     s.snapshot.priority,
				NotFillCache: s.snapshot.notFillCache,
			},
		}
		if s.reverse {
			req.Scan.StartKey = s.nextEndKey
			req.Scan.EndKey = reqStartKey
			req.Scan.Reverse = true
		}
		resp, err := sender.SendReq(bo, req, loc.Region, ReadTimeoutMedium)
		if err != nil {
			return errors.Trace(err)
		}
		regionErr, err := resp.GetRegionError()
		if err != nil {
			return errors.Trace(err)
		}
		if regionErr != nil {
			logutil.Logger(context.Background()).Debug("scanner getData failed",
				zap.Stringer("regionErr", regionErr))
			err = bo.Backoff(BoRegionMiss, errors.New(regionErr.String()))
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}
		cmdScanResp := resp.Scan
		if cmdScanResp == nil {
			return errors.Trace(ErrBodyMissing)
		}

		err = s.snapshot.store.CheckVisibility(s.startTS())
		if err != nil {
			return errors.Trace(err)
		}

		kvPairs := cmdScanResp.Pairs
		// Check if kvPair contains error, it should be a Lock.
		for _, pair := range kvPairs {
			if keyErr := pair.GetError(); keyErr != nil {
				lock, err := extractLockFromKeyErr(keyErr)
				if err != nil {
					return errors.Trace(err)
				}
				pair.Key = lock.Key
			}
		}

		s.cache, s.idx = kvPairs, 0
		if len(kvPairs) < s.batchSize {
			// No more data in current Region. Next getData() starts
			// from current Region's endKey.
			if !s.reverse {
				s.nextStartKey = loc.EndKey
			} else {
				s.nextEndKey = reqStartKey
			}
			if (!s.reverse && (len(loc.EndKey) == 0 || (len(s.endKey) > 0 && kv.Key(s.nextStartKey).Cmp(kv.Key(s.endKey)) >= 0))) ||
				(s.reverse && (len(loc.StartKey) == 0 || (len(s.nextStartKey) > 0 && kv.Key(s.nextStartKey).Cmp(kv.Key(s.nextEndKey)) >= 0))) {
				// Current Region is the last one.
				s.eof = true
			}
			return nil
		}
		// next getData() starts from the last key in kvPairs (but skip
		// it by appending a '\x00' to the key). Note that next getData()
		// may get an empty response if the Region in fact does not have
		// more data.
		lastKey := kvPairs[len(kvPairs)-1].GetKey()
		if !s.reverse {
			s.nextStartKey = kv.Key(lastKey).Next()
		} else {
			s.nextEndKey = kv.Key(lastKey)
		}
		return nil
	}
}
