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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/btree"
	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb/store/mockstore/mocktikv"
)

type testRegionCacheSuite struct {
	OneByOneSuite
	cluster *mocktikv.Cluster
	store1  uint64
	store2  uint64
	peer1   uint64
	peer2   uint64
	region1 uint64
	cache   *RegionCache
	bo      *Backoffer
}

var _ = Suite(&testRegionCacheSuite{})

func (s *testRegionCacheSuite) SetUpTest(c *C) {
	s.cluster = mocktikv.NewCluster()
	storeIDs, peerIDs, regionID, _ := mocktikv.BootstrapWithMultiStores(s.cluster, 2)
	s.region1 = regionID
	s.store1 = storeIDs[0]
	s.store2 = storeIDs[1]
	s.peer1 = peerIDs[0]
	s.peer2 = peerIDs[1]
	pdCli := &codecPDClient{mocktikv.NewPDClient(s.cluster)}
	s.cache = NewRegionCache(pdCli)
	s.bo = NewBackoffer(context.Background(), 5000)
}

func (s *testRegionCacheSuite) TearDownTest(c *C) {
	s.cache.Close()
}

func (s *testRegionCacheSuite) storeAddr(id uint64) string {
	return fmt.Sprintf("store%d", id)
}

func (s *testRegionCacheSuite) checkCache(c *C, len int) {
	ts := time.Now().Unix()
	c.Assert(validRegions(s.cache.mu.regions, ts), Equals, len)
	c.Assert(validRegionsInBtree(s.cache.mu.sorted, ts), Equals, len)
}

func validRegions(regions map[RegionVerID]*Region, ts int64) (len int) {
	for _, region := range regions {
		if !region.checkRegionCacheTTL(ts) {
			continue
		}
		len++
	}
	return
}

func validRegionsInBtree(t *btree.BTree, ts int64) (len int) {
	t.Descend(func(item btree.Item) bool {
		r := item.(*btreeItem).cachedRegion
		if !r.checkRegionCacheTTL(ts) {
			return true
		}
		len++
		return true
	})
	return
}

func (s *testRegionCacheSuite) getRegion(c *C, key []byte) *Region {
	_, err := s.cache.LocateKey(s.bo, key)
	c.Assert(err, IsNil)
	r := s.cache.searchCachedRegion(key, false)
	c.Assert(r, NotNil)
	return r
}

func (s *testRegionCacheSuite) getRegionWithEndKey(c *C, key []byte) *Region {
	_, err := s.cache.LocateEndKey(s.bo, key)
	c.Assert(err, IsNil)
	r := s.cache.searchCachedRegion(key, true)
	c.Assert(r, NotNil)
	return r
}

func (s *testRegionCacheSuite) getAddr(c *C, key []byte) string {
	loc, err := s.cache.LocateKey(s.bo, key)
	c.Assert(err, IsNil)
	ctx, err := s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	if ctx == nil {
		return ""
	}
	return ctx.Addr
}

func (s *testRegionCacheSuite) TestSimple(c *C) {
	r := s.getRegion(c, []byte("a"))
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("a")), Equals, s.storeAddr(s.store1))
	s.checkCache(c, 1)
	c.Assert(r.GetMeta(), DeepEquals, r.meta)
	c.Assert(r.GetLeaderID(), Equals, r.meta.Peers[r.getStore().workStoreIdx].Id)
	s.cache.mu.regions[r.VerID()].lastAccess = 0
	r = s.cache.searchCachedRegion([]byte("a"), true)
	c.Assert(r, IsNil)
}

func (s *testRegionCacheSuite) TestDropStore(c *C) {
	bo := NewBackoffer(context.Background(), 100)
	s.cluster.RemoveStore(s.store1)
	loc, err := s.cache.LocateKey(bo, []byte("a"))
	c.Assert(err, IsNil)
	ctx, err := s.cache.GetRPCContext(bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx, IsNil)
	s.checkCache(c, 0)
}

func (s *testRegionCacheSuite) TestDropStoreRetry(c *C) {
	s.cluster.RemoveStore(s.store1)
	done := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond * 10)
		s.cluster.AddStore(s.store1, s.storeAddr(s.store1))
		close(done)
	}()
	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	c.Assert(loc.Region.id, Equals, s.region1)
	<-done
}

func (s *testRegionCacheSuite) TestUpdateLeader(c *C) {
	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	// tikv-server reports `NotLeader`
	s.cache.UpdateLeader(loc.Region, s.store2, 0)

	r := s.getRegion(c, []byte("a"))
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("a")), Equals, s.storeAddr(s.store2))

	r = s.getRegionWithEndKey(c, []byte("z"))
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("z")), Equals, s.storeAddr(s.store2))
}

func (s *testRegionCacheSuite) TestUpdateLeader2(c *C) {
	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	// new store3 becomes leader
	store3 := s.cluster.AllocID()
	peer3 := s.cluster.AllocID()
	s.cluster.AddStore(store3, s.storeAddr(store3))
	s.cluster.AddPeer(s.region1, store3, peer3)
	// tikv-server reports `NotLeader`
	s.cache.UpdateLeader(loc.Region, store3, 0)

	// Store3 does not exist in cache, causes a reload from PD.
	r := s.getRegion(c, []byte("a"))
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("a")), Equals, s.storeAddr(s.store1))

	// tikv-server notifies new leader to pd-server.
	s.cluster.ChangeLeader(s.region1, peer3)
	// tikv-server reports `NotLeader` again.
	s.cache.UpdateLeader(r.VerID(), store3, 0)
	r = s.getRegion(c, []byte("a"))
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("a")), Equals, s.storeAddr(store3))
}

func (s *testRegionCacheSuite) TestUpdateLeader3(c *C) {
	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	// store2 becomes leader
	s.cluster.ChangeLeader(s.region1, s.peer2)
	// store2 gone, store3 becomes leader
	s.cluster.RemoveStore(s.store2)
	store3 := s.cluster.AllocID()
	peer3 := s.cluster.AllocID()
	s.cluster.AddStore(store3, s.storeAddr(store3))
	s.cluster.AddPeer(s.region1, store3, peer3)
	// tikv-server notifies new leader to pd-server.
	s.cluster.ChangeLeader(s.region1, peer3)
	// tikv-server reports `NotLeader`(store2 is the leader)
	s.cache.UpdateLeader(loc.Region, s.store2, 0)

	// Store2 does not exist any more, causes a reload from PD.
	r := s.getRegion(c, []byte("a"))
	c.Assert(err, IsNil)
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	addr := s.getAddr(c, []byte("a"))
	c.Assert(addr, Equals, "")
	s.getRegion(c, []byte("a"))
	// pd-server should return the new leader.
	c.Assert(s.getAddr(c, []byte("a")), Equals, s.storeAddr(store3))
}

func (s *testRegionCacheSuite) TestSendFailedButLeaderNotChange(c *C) {
	// 3 nodes and no.1 is leader.
	store3 := s.cluster.AllocID()
	peer3 := s.cluster.AllocID()
	s.cluster.AddStore(store3, s.storeAddr(store3))
	s.cluster.AddPeer(s.region1, store3, peer3)
	s.cluster.ChangeLeader(s.region1, s.peer1)

	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	ctx, err := s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer1)
	c.Assert(len(ctx.Meta.Peers), Equals, 3)

	// send fail leader switch to 2
	s.cache.OnSendFail(s.bo, ctx, false, nil)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer2)

	// access 1 it will return NotLeader, leader back to 2 again
	s.cache.UpdateLeader(loc.Region, s.store2, ctx.PeerIdx)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer2)
}

func (s *testRegionCacheSuite) TestSendFailedInHibernateRegion(c *C) {
	// 3 nodes and no.1 is leader.
	store3 := s.cluster.AllocID()
	peer3 := s.cluster.AllocID()
	s.cluster.AddStore(store3, s.storeAddr(store3))
	s.cluster.AddPeer(s.region1, store3, peer3)
	s.cluster.ChangeLeader(s.region1, s.peer1)

	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	ctx, err := s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer1)
	c.Assert(len(ctx.Meta.Peers), Equals, 3)

	// send fail leader switch to 2
	s.cache.OnSendFail(s.bo, ctx, false, nil)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer2)

	// access 2, it's in hibernate and return 0 leader, so switch to 3
	s.cache.UpdateLeader(loc.Region, 0, ctx.PeerIdx)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, peer3)

	// again peer back to 1
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	s.cache.UpdateLeader(loc.Region, 0, ctx.PeerIdx)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer1)
}

func (s *testRegionCacheSuite) TestSendFailedInMultipleNode(c *C) {
	// 3 nodes and no.1 is leader.
	store3 := s.cluster.AllocID()
	peer3 := s.cluster.AllocID()
	s.cluster.AddStore(store3, s.storeAddr(store3))
	s.cluster.AddPeer(s.region1, store3, peer3)
	s.cluster.ChangeLeader(s.region1, s.peer1)

	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)
	ctx, err := s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer1)
	c.Assert(len(ctx.Meta.Peers), Equals, 3)

	// send fail leader switch to 2
	s.cache.OnSendFail(s.bo, ctx, false, nil)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer2)

	// send 2 fail leader switch to 3
	s.cache.OnSendFail(s.bo, ctx, false, nil)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, peer3)

	// 3 can be access, so switch to 1
	s.cache.UpdateLeader(loc.Region, s.store1, ctx.PeerIdx)
	ctx, err = s.cache.GetRPCContext(s.bo, loc.Region)
	c.Assert(err, IsNil)
	c.Assert(ctx.Peer.Id, Equals, s.peer1)
}

func (s *testRegionCacheSuite) TestSplit(c *C) {
	r := s.getRegion(c, []byte("x"))
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("x")), Equals, s.storeAddr(s.store1))

	// split to ['' - 'm' - 'z']
	region2 := s.cluster.AllocID()
	newPeers := s.cluster.AllocIDs(2)
	s.cluster.Split(s.region1, region2, []byte("m"), newPeers, newPeers[0])

	// tikv-server reports `NotInRegion`
	s.cache.InvalidateCachedRegion(r.VerID())
	s.checkCache(c, 0)

	r = s.getRegion(c, []byte("x"))
	c.Assert(r.GetID(), Equals, region2)
	c.Assert(s.getAddr(c, []byte("x")), Equals, s.storeAddr(s.store1))
	s.checkCache(c, 1)

	r = s.getRegionWithEndKey(c, []byte("m"))
	c.Assert(r.GetID(), Equals, s.region1)
	s.checkCache(c, 2)
}

func (s *testRegionCacheSuite) TestMerge(c *C) {
	// key range: ['' - 'm' - 'z']
	region2 := s.cluster.AllocID()
	newPeers := s.cluster.AllocIDs(2)
	s.cluster.Split(s.region1, region2, []byte("m"), newPeers, newPeers[0])

	loc, err := s.cache.LocateKey(s.bo, []byte("x"))
	c.Assert(err, IsNil)
	c.Assert(loc.Region.id, Equals, region2)

	// merge to single region
	s.cluster.Merge(s.region1, region2)

	// tikv-server reports `NotInRegion`
	s.cache.InvalidateCachedRegion(loc.Region)
	s.checkCache(c, 0)

	loc, err = s.cache.LocateKey(s.bo, []byte("x"))
	c.Assert(err, IsNil)
	c.Assert(loc.Region.id, Equals, s.region1)
	s.checkCache(c, 1)
}

func (s *testRegionCacheSuite) TestReconnect(c *C) {
	loc, err := s.cache.LocateKey(s.bo, []byte("a"))
	c.Assert(err, IsNil)

	// connect tikv-server failed, cause drop cache
	s.cache.InvalidateCachedRegion(loc.Region)

	r := s.getRegion(c, []byte("a"))
	c.Assert(r, NotNil)
	c.Assert(r.GetID(), Equals, s.region1)
	c.Assert(s.getAddr(c, []byte("a")), Equals, s.storeAddr(s.store1))
	s.checkCache(c, 1)
}

func (s *testRegionCacheSuite) TestRegionEpochAheadOfTiKV(c *C) {
	// Create a separated region cache to do this test.
	pdCli := &codecPDClient{mocktikv.NewPDClient(s.cluster)}
	cache := NewRegionCache(pdCli)
	defer cache.Close()

	region := createSampleRegion([]byte("k1"), []byte("k2"))
	region.meta.Id = 1
	region.meta.RegionEpoch = &metapb.RegionEpoch{Version: 10, ConfVer: 10}
	cache.insertRegionToCache(region)

	r1 := metapb.Region{Id: 1, RegionEpoch: &metapb.RegionEpoch{Version: 9, ConfVer: 10}}
	r2 := metapb.Region{Id: 1, RegionEpoch: &metapb.RegionEpoch{Version: 10, ConfVer: 9}}

	bo := NewBackoffer(context.Background(), 2000000)

	err := cache.OnRegionEpochNotMatch(bo, &RPCContext{Region: region.VerID()}, []*metapb.Region{&r1})
	c.Assert(err, IsNil)
	err = cache.OnRegionEpochNotMatch(bo, &RPCContext{Region: region.VerID()}, []*metapb.Region{&r2})
	c.Assert(err, IsNil)
	c.Assert(len(bo.errors), Equals, 2)
}

const regionSplitKeyFormat = "t%08d"

func createClusterWithStoresAndRegions(regionCnt, storeCount int) *mocktikv.Cluster {
	cluster := mocktikv.NewCluster()
	_, _, regionID, _ := mocktikv.BootstrapWithMultiStores(cluster, storeCount)
	for i := 0; i < regionCnt; i++ {
		rawKey := []byte(fmt.Sprintf(regionSplitKeyFormat, i))
		ids := cluster.AllocIDs(4)
		// Make leaders equally distributed on the 3 stores.
		storeID := ids[0]
		peerIDs := ids[1:]
		leaderPeerID := peerIDs[i%3]
		cluster.SplitRaw(regionID, storeID, rawKey, peerIDs, leaderPeerID)
		regionID = ids[0]
	}
	return cluster
}

func loadRegionsToCache(cache *RegionCache, regionCnt int) {
	for i := 0; i < regionCnt; i++ {
		rawKey := []byte(fmt.Sprintf(regionSplitKeyFormat, i))
		cache.LocateKey(NewBackoffer(context.Background(), 1), rawKey)
	}
}

func (s *testRegionCacheSuite) TestUpdateStoreAddr(c *C) {
	mvccStore := mocktikv.MustNewMVCCStore()
	defer mvccStore.Close()

	client := &RawKVClient{
		clusterID:   0,
		regionCache: NewRegionCache(mocktikv.NewPDClient(s.cluster)),
		rpcClient:   mocktikv.NewRPCClient(s.cluster, mvccStore),
	}
	defer client.Close()
	testKey := []byte("test_key")
	testValue := []byte("test_value")
	err := client.Put(testKey, testValue)
	c.Assert(err, IsNil)
	// tikv-server reports `StoreNotMatch` And retry
	store1Addr := s.storeAddr(s.store1)
	s.cluster.UpdateStoreAddr(s.store1, s.storeAddr(s.store2))
	s.cluster.UpdateStoreAddr(s.store2, store1Addr)

	getVal, err := client.Get(testKey)

	c.Assert(err, IsNil)
	c.Assert(getVal, BytesEquals, testValue)
}

func (s *testRegionCacheSuite) TestListRegionIDsInCache(c *C) {
	// ['' - 'm' - 'z']
	region2 := s.cluster.AllocID()
	newPeers := s.cluster.AllocIDs(2)
	s.cluster.Split(s.region1, region2, []byte("m"), newPeers, newPeers[0])

	regionIDs, err := s.cache.ListRegionIDsInKeyRange(s.bo, []byte("a"), []byte("z"))
	c.Assert(err, IsNil)
	c.Assert(regionIDs, DeepEquals, []uint64{s.region1, region2})
	regionIDs, err = s.cache.ListRegionIDsInKeyRange(s.bo, []byte("m"), []byte("z"))
	c.Assert(err, IsNil)
	c.Assert(regionIDs, DeepEquals, []uint64{region2})

	regionIDs, err = s.cache.ListRegionIDsInKeyRange(s.bo, []byte("a"), []byte("m"))
	c.Assert(err, IsNil)
	c.Assert(regionIDs, DeepEquals, []uint64{s.region1, region2})
}

func createSampleRegion(startKey, endKey []byte) *Region {
	return &Region{
		meta: &metapb.Region{
			StartKey: startKey,
			EndKey:   endKey,
		},
	}
}

func (s *testRegionCacheSuite) TestContains(c *C) {
	c.Assert(createSampleRegion(nil, nil).Contains([]byte{}), IsTrue)
	c.Assert(createSampleRegion(nil, nil).Contains([]byte{10}), IsTrue)
	c.Assert(createSampleRegion([]byte{10}, nil).Contains([]byte{}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, nil).Contains([]byte{9}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, nil).Contains([]byte{10}), IsTrue)
	c.Assert(createSampleRegion(nil, []byte{10}).Contains([]byte{}), IsTrue)
	c.Assert(createSampleRegion(nil, []byte{10}).Contains([]byte{9}), IsTrue)
	c.Assert(createSampleRegion(nil, []byte{10}).Contains([]byte{10}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, []byte{20}).Contains([]byte{}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, []byte{20}).Contains([]byte{15}), IsTrue)
	c.Assert(createSampleRegion([]byte{10}, []byte{20}).Contains([]byte{30}), IsFalse)
}

func (s *testRegionCacheSuite) TestContainsByEnd(c *C) {
	c.Assert(createSampleRegion(nil, nil).ContainsByEnd([]byte{}), IsFalse)
	c.Assert(createSampleRegion(nil, nil).ContainsByEnd([]byte{10}), IsTrue)
	c.Assert(createSampleRegion([]byte{10}, nil).ContainsByEnd([]byte{}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, nil).ContainsByEnd([]byte{10}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, nil).ContainsByEnd([]byte{11}), IsTrue)
	c.Assert(createSampleRegion(nil, []byte{10}).ContainsByEnd([]byte{}), IsFalse)
	c.Assert(createSampleRegion(nil, []byte{10}).ContainsByEnd([]byte{10}), IsTrue)
	c.Assert(createSampleRegion(nil, []byte{10}).ContainsByEnd([]byte{11}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, []byte{20}).ContainsByEnd([]byte{}), IsFalse)
	c.Assert(createSampleRegion([]byte{10}, []byte{20}).ContainsByEnd([]byte{15}), IsTrue)
	c.Assert(createSampleRegion([]byte{10}, []byte{20}).ContainsByEnd([]byte{30}), IsFalse)
}

func BenchmarkOnRequestFail(b *testing.B) {
	/*
			This benchmark simulate many concurrent requests call OnSendRequestFail method
			after failed on a store, validate that on this scene, requests don't get blocked on the
		    RegionCache lock.
	*/
	regionCnt, storeCount := 998, 3
	cluster := createClusterWithStoresAndRegions(regionCnt, storeCount)
	cache := NewRegionCache(mocktikv.NewPDClient(cluster))
	defer cache.Close()
	loadRegionsToCache(cache, regionCnt)
	bo := NewBackoffer(context.Background(), 1)
	loc, err := cache.LocateKey(bo, []byte{})
	if err != nil {
		b.Fatal(err)
	}
	region := cache.getRegionByIDFromCache(loc.Region.id)
	b.ResetTimer()
	regionStore := region.getStore()
	store, peer, idx := region.WorkStorePeer(regionStore)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rpcCtx := &RPCContext{
				Region:  loc.Region,
				Meta:    region.meta,
				PeerIdx: idx,
				Peer:    peer,
				Store:   store,
			}
			r := cache.getCachedRegionWithRLock(rpcCtx.Region)
			if r == nil {
				cache.switchNextPeer(r, rpcCtx.PeerIdx)
			}
		}
	})
	if len(cache.mu.regions) != regionCnt*2/3 {
		b.Fatal(len(cache.mu.regions))
	}
}
