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
	"sort"

	. "github.com/pingcap/check"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/store/mockstore/mocktikv"
)

type testRangeTaskSuite struct {
	OneByOneSuite
	cluster *mocktikv.Cluster
	store   *tikvStore

	testRanges     []kv.KeyRange
	expectedRanges [][]kv.KeyRange
}

var _ = Suite(&testRangeTaskSuite{})

func makeRange(startKey string, endKey string) kv.KeyRange {
	return kv.KeyRange{
		StartKey: []byte(startKey),
		EndKey:   []byte(endKey),
	}
}

func (s *testRangeTaskSuite) SetUpTest(c *C) {
	// Split the store at "a" to "z"
	splitKeys := make([][]byte, 0)
	for k := byte('a'); k <= byte('z'); k++ {
		splitKeys = append(splitKeys, []byte{k})
	}

	// Calculate all region's ranges
	allRegionRanges := []kv.KeyRange{makeRange("", "a")}
	for i := 0; i < len(splitKeys)-1; i++ {
		allRegionRanges = append(allRegionRanges, kv.KeyRange{
			StartKey: splitKeys[i],
			EndKey:   splitKeys[i+1],
		})
	}
	allRegionRanges = append(allRegionRanges, makeRange("z", ""))

	s.cluster = mocktikv.NewCluster()
	mocktikv.BootstrapWithMultiRegions(s.cluster, splitKeys...)
	client, pdClient, err := mocktikv.NewTiKVAndPDClient(s.cluster, nil, "")
	c.Assert(err, IsNil)

	store, err := NewTestTiKVStore(client, pdClient, nil, nil, 0)
	c.Assert(err, IsNil)
	s.store = store.(*tikvStore)

	s.testRanges = []kv.KeyRange{
		makeRange("", ""),
		makeRange("", "b"),
		makeRange("b", ""),
		makeRange("b", "x"),
		makeRange("a", "d"),
		makeRange("a\x00", "d\x00"),
		makeRange("a\xff\xff\xff", "c\xff\xff\xff"),
		makeRange("a1", "a2"),
		makeRange("a", "a"),
		makeRange("a3", "a3"),
	}

	s.expectedRanges = [][]kv.KeyRange{
		allRegionRanges,
		allRegionRanges[:2],
		allRegionRanges[2:],
		allRegionRanges[2:24],
		{
			makeRange("a", "b"),
			makeRange("b", "c"),
			makeRange("c", "d"),
		},
		{
			makeRange("a\x00", "b"),
			makeRange("b", "c"),
			makeRange("c", "d"),
			makeRange("d", "d\x00"),
		},
		{
			makeRange("a\xff\xff\xff", "b"),
			makeRange("b", "c"),
			makeRange("c", "c\xff\xff\xff"),
		},
		{
			makeRange("a1", "a2"),
		},
		{},
		{},
	}
}

func (s *testRangeTaskSuite) TearDownTest(c *C) {
	err := s.store.Close()
	c.Assert(err, IsNil)
}

func collect(c chan *kv.KeyRange) []kv.KeyRange {
	c <- nil
	ranges := make([]kv.KeyRange, 0)

	for {
		r := <-c
		if r == nil {
			break
		}

		ranges = append(ranges, *r)
	}
	return ranges
}

func (s *testRangeTaskSuite) checkRanges(c *C, obtained []kv.KeyRange, expected []kv.KeyRange) {
	sort.Slice(obtained, func(i, j int) bool {
		return bytes.Compare(obtained[i].StartKey, obtained[j].StartKey) < 0
	})

	c.Assert(obtained, DeepEquals, expected)
}

func (s *testRangeTaskSuite) testRangeTaskImpl(c *C, concurrency int) {
	ranges := make(chan *kv.KeyRange, 100)

	handler := func(ctx context.Context, r kv.KeyRange) (int, error) {
		ranges <- &r
		return 1, nil
	}

	runner := NewRangeTaskRunner(
		"test-runner",
		s.store,
		concurrency,
		handler)

	for i, r := range s.testRanges {
		expectedRanges := s.expectedRanges[i]

		err := runner.RunOnRange(context.Background(), r.StartKey, r.EndKey)
		c.Assert(err, IsNil)
		s.checkRanges(c, collect(ranges), expectedRanges)
		c.Assert(int(runner.completedRegions), Equals, len(expectedRanges))
	}
}

func (s *testRangeTaskSuite) TestRangeTask(c *C) {
	for concurrency := 1; concurrency < 5; concurrency++ {
		s.testRangeTaskImpl(c, concurrency)
	}
}
