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

package binloginfo_test

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/terror"
	pumpcli "github.com/pingcap/tidb-tools/tidb-binlog/pump_client"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/binloginfo"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/testkit"
	binlog "github.com/pingcap/tipb/go-binlog"
	"google.golang.org/grpc"
)

func TestT(t *testing.T) {
	CustomVerboseFlag = true
	logLevel := os.Getenv("log_level")
	logutil.InitLogger(logutil.NewLogConfig(logLevel, logutil.DefaultLogFormat, "", logutil.EmptyFileLogConfig, false))
	TestingT(t)
}

type mockBinlogPump struct {
	mu struct {
		sync.Mutex
		payloads [][]byte
		mockFail bool
	}
}

func (p *mockBinlogPump) WriteBinlog(ctx context.Context, req *binlog.WriteBinlogReq) (*binlog.WriteBinlogResp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mu.mockFail {
		return &binlog.WriteBinlogResp{}, errors.New("mock fail")
	}
	p.mu.payloads = append(p.mu.payloads, req.Payload)
	return &binlog.WriteBinlogResp{}, nil
}

// PullBinlogs implements PumpServer interface.
func (p *mockBinlogPump) PullBinlogs(req *binlog.PullBinlogReq, srv binlog.Pump_PullBinlogsServer) error {
	return nil
}

var _ = Suite(&testBinlogSuite{})

type testBinlogSuite struct {
	store    kv.Storage
	domain   *domain.Domain
	unixFile string
	serv     *grpc.Server
	pump     *mockBinlogPump
	client   *pumpcli.PumpsClient
	ddl      ddl.DDL
}

const maxRecvMsgSize = 64 * 1024

func (s *testBinlogSuite) SetUpSuite(c *C) {
	store, err := mockstore.NewMockTikvStore()
	c.Assert(err, IsNil)
	s.store = store
	session.SetSchemaLease(0)
	s.unixFile = "/tmp/mock-binlog-pump" + strconv.FormatInt(time.Now().UnixNano(), 10)
	l, err := net.Listen("unix", s.unixFile)
	c.Assert(err, IsNil)
	s.serv = grpc.NewServer(grpc.MaxRecvMsgSize(maxRecvMsgSize))
	s.pump = new(mockBinlogPump)
	binlog.RegisterPumpServer(s.serv, s.pump)
	go s.serv.Serve(l)
	opt := grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("unix", addr, timeout)
	})
	clientCon, err := grpc.Dial(s.unixFile, opt, grpc.WithInsecure())
	c.Assert(err, IsNil)
	c.Assert(clientCon, NotNil)
	tk := testkit.NewTestKit(c, s.store)
	s.domain, err = session.BootstrapSession(store)
	c.Assert(err, IsNil)
	tk.MustExec("use test")
	sessionDomain := domain.GetDomain(tk.Se.(sessionctx.Context))
	s.ddl = sessionDomain.DDL()

	s.client = binloginfo.MockPumpsClient(binlog.NewPumpClient(clientCon))
	s.ddl.SetBinlogClient(s.client)
}

func (s *testBinlogSuite) TearDownSuite(c *C) {
	s.ddl.Stop()
	s.serv.Stop()
	os.Remove(s.unixFile)
	s.domain.Close()
	s.store.Close()
}

func (s *testBinlogSuite) TestBinlog(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.Se.GetSessionVars().BinlogClient = s.client
	pump := s.pump
	tk.MustExec("drop table if exists local_binlog")
	ddlQuery := "create table local_binlog (id int unique key, name varchar(10)) shard_row_id_bits=1"
	binlogDDLQuery := "create table local_binlog (id int unique key, name varchar(10)) /*!90000 shard_row_id_bits=1 */"
	tk.MustExec(ddlQuery)
	var matched bool // got matched pre DDL and commit DDL
	for i := 0; i < 10; i++ {
		preDDL, commitDDL := getLatestDDLBinlog(c, pump, binlogDDLQuery)
		if preDDL != nil && commitDDL != nil {
			if preDDL.DdlJobId == commitDDL.DdlJobId {
				c.Assert(commitDDL.StartTs, Equals, preDDL.StartTs)
				c.Assert(commitDDL.CommitTs, Greater, commitDDL.StartTs)
				matched = true
				break
			}
		}
		time.Sleep(time.Millisecond * 10)
	}
	c.Assert(matched, IsTrue)

	tk.MustExec("insert local_binlog values (1, 'abc'), (2, 'cde')")
	prewriteVal := getLatestBinlogPrewriteValue(c, pump)
	c.Assert(prewriteVal.SchemaVersion, Greater, int64(0))
	c.Assert(prewriteVal.Mutations[0].TableId, Greater, int64(0))
	expected := [][]types.Datum{
		{types.NewIntDatum(1), types.NewStringDatum("abc")},
		{types.NewIntDatum(2), types.NewStringDatum("cde")},
	}
	gotRows := mutationRowsToRows(c, prewriteVal.Mutations[0].InsertedRows, 2, 4)
	c.Assert(gotRows, DeepEquals, expected)

	tk.MustExec("update local_binlog set name = 'xyz' where id = 2")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)
	oldRow := [][]types.Datum{
		{types.NewIntDatum(2), types.NewStringDatum("cde")},
	}
	newRow := [][]types.Datum{
		{types.NewIntDatum(2), types.NewStringDatum("xyz")},
	}
	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].UpdatedRows, 1, 3)
	c.Assert(gotRows, DeepEquals, oldRow)

	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].UpdatedRows, 7, 9)
	c.Assert(gotRows, DeepEquals, newRow)

	tk.MustExec("delete from local_binlog where id = 1")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)
	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].DeletedRows, 1, 3)
	expected = [][]types.Datum{
		{types.NewIntDatum(1), types.NewStringDatum("abc")},
	}
	c.Assert(gotRows, DeepEquals, expected)

	// Test table primary key is not integer.
	tk.MustExec("create table local_binlog2 (name varchar(64) primary key, age int)")
	tk.MustExec("insert local_binlog2 values ('abc', 16), ('def', 18)")
	tk.MustExec("delete from local_binlog2 where name = 'def'")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)
	c.Assert(prewriteVal.Mutations[0].Sequence[0], Equals, binlog.MutationType_DeleteRow)

	expected = [][]types.Datum{
		{types.NewStringDatum("def"), types.NewIntDatum(18), types.NewIntDatum(-1), types.NewIntDatum(2)},
	}
	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].DeletedRows, 1, 3, 4, 5)
	c.Assert(gotRows, DeepEquals, expected)

	// Test Table don't have primary key.
	tk.MustExec("create table local_binlog3 (c1 int, c2 int)")
	tk.MustExec("insert local_binlog3 values (1, 2), (1, 3), (2, 3)")
	tk.MustExec("update local_binlog3 set c1 = 3 where c1 = 2")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)

	// The encoded update row is [oldColID1, oldColVal1, oldColID2, oldColVal2, -1, handle,
	// 		newColID1, newColVal2, newColID2, newColVal2, -1, handle]
	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].UpdatedRows, 7, 9)
	expected = [][]types.Datum{
		{types.NewIntDatum(3), types.NewIntDatum(3)},
	}
	c.Assert(gotRows, DeepEquals, expected)
	expected = [][]types.Datum{
		{types.NewIntDatum(-1), types.NewIntDatum(3), types.NewIntDatum(-1), types.NewIntDatum(3)},
	}
	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].UpdatedRows, 4, 5, 10, 11)
	c.Assert(gotRows, DeepEquals, expected)

	tk.MustExec("delete from local_binlog3 where c1 = 3 and c2 = 3")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)
	c.Assert(prewriteVal.Mutations[0].Sequence[0], Equals, binlog.MutationType_DeleteRow)
	gotRows = mutationRowsToRows(c, prewriteVal.Mutations[0].DeletedRows, 1, 3, 4, 5)
	expected = [][]types.Datum{
		{types.NewIntDatum(3), types.NewIntDatum(3), types.NewIntDatum(-1), types.NewIntDatum(3)},
	}
	c.Assert(gotRows, DeepEquals, expected)

	// Test Mutation Sequence.
	tk.MustExec("create table local_binlog4 (c1 int primary key, c2 int)")
	tk.MustExec("insert local_binlog4 values (1, 1), (2, 2), (3, 2)")
	tk.MustExec("begin")
	tk.MustExec("delete from local_binlog4 where c1 = 1")
	tk.MustExec("insert local_binlog4 values (1, 1)")
	tk.MustExec("update local_binlog4 set c2 = 3 where c1 = 3")
	tk.MustExec("commit")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)
	c.Assert(prewriteVal.Mutations[0].Sequence, DeepEquals, []binlog.MutationType{
		binlog.MutationType_DeleteRow,
		binlog.MutationType_Insert,
		binlog.MutationType_Update,
	})

	// Test statement rollback.
	tk.MustExec("create table local_binlog5 (c1 int primary key)")
	tk.MustExec("begin")
	tk.MustExec("insert into local_binlog5 value (1)")
	// This statement execute fail and should not write binlog.
	_, err := tk.Exec("insert into local_binlog5 value (4),(3),(1),(2)")
	c.Assert(err, NotNil)
	tk.MustExec("commit")
	prewriteVal = getLatestBinlogPrewriteValue(c, pump)
	c.Assert(prewriteVal.Mutations[0].Sequence, DeepEquals, []binlog.MutationType{
		binlog.MutationType_Insert,
	})

	checkBinlogCount(c, pump)

	pump.mu.Lock()
	originBinlogLen := len(pump.mu.payloads)
	pump.mu.Unlock()
	tk.MustExec("set @@global.autocommit = 0")
	tk.MustExec("set @@global.autocommit = 1")
	pump.mu.Lock()
	newBinlogLen := len(pump.mu.payloads)
	pump.mu.Unlock()
	c.Assert(newBinlogLen, Equals, originBinlogLen)
}

func (s *testBinlogSuite) TestMaxRecvSize(c *C) {
	info := &binloginfo.BinlogInfo{
		Data: &binlog.Binlog{
			Tp:            binlog.BinlogType_Prewrite,
			PrewriteValue: make([]byte, maxRecvMsgSize+1),
		},
		Client: s.client,
	}
	err := info.WriteBinlog(1)
	c.Assert(err, NotNil)
	c.Assert(terror.ErrCritical.Equal(err), IsFalse, Commentf("%v", err))
}

func getLatestBinlogPrewriteValue(c *C, pump *mockBinlogPump) *binlog.PrewriteValue {
	var bin *binlog.Binlog
	pump.mu.Lock()
	for i := len(pump.mu.payloads) - 1; i >= 0; i-- {
		payload := pump.mu.payloads[i]
		bin = new(binlog.Binlog)
		bin.Unmarshal(payload)
		if bin.Tp == binlog.BinlogType_Prewrite {
			break
		}
	}
	pump.mu.Unlock()
	c.Assert(bin, NotNil)
	preVal := new(binlog.PrewriteValue)
	preVal.Unmarshal(bin.PrewriteValue)
	return preVal
}

func getLatestDDLBinlog(c *C, pump *mockBinlogPump, ddlQuery string) (preDDL, commitDDL *binlog.Binlog) {
	pump.mu.Lock()
	for i := len(pump.mu.payloads) - 1; i >= 0; i-- {
		payload := pump.mu.payloads[i]
		bin := new(binlog.Binlog)
		bin.Unmarshal(payload)
		if bin.Tp == binlog.BinlogType_Commit && bin.DdlJobId > 0 {
			commitDDL = bin
		}
		if bin.Tp == binlog.BinlogType_Prewrite && bin.DdlJobId != 0 {
			preDDL = bin
		}
		if preDDL != nil && commitDDL != nil {
			break
		}
	}
	pump.mu.Unlock()
	c.Assert(preDDL.DdlJobId, Greater, int64(0))
	c.Assert(preDDL.StartTs, Greater, int64(0))
	c.Assert(preDDL.CommitTs, Equals, int64(0))
	c.Assert(string(preDDL.DdlQuery), Equals, ddlQuery)
	return
}

func checkBinlogCount(c *C, pump *mockBinlogPump) {
	var bin *binlog.Binlog
	prewriteCount := 0
	ddlCount := 0
	pump.mu.Lock()
	length := len(pump.mu.payloads)
	for i := length - 1; i >= 0; i-- {
		payload := pump.mu.payloads[i]
		bin = new(binlog.Binlog)
		bin.Unmarshal(payload)
		if bin.Tp == binlog.BinlogType_Prewrite {
			if bin.DdlJobId != 0 {
				ddlCount++
			} else {
				prewriteCount++
			}
		}
	}
	pump.mu.Unlock()
	c.Assert(ddlCount, Greater, 0)
	match := false
	for i := 0; i < 10; i++ {
		pump.mu.Lock()
		length = len(pump.mu.payloads)
		pump.mu.Unlock()
		if (prewriteCount+ddlCount)*2 == length {
			match = true
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	c.Assert(match, IsTrue)
}

func mutationRowsToRows(c *C, mutationRows [][]byte, columnValueOffsets ...int) [][]types.Datum {
	var rows = make([][]types.Datum, 0)
	for _, mutationRow := range mutationRows {
		datums, err := codec.Decode(mutationRow, 5)
		c.Assert(err, IsNil)
		for i := range datums {
			if datums[i].Kind() == types.KindBytes {
				datums[i].SetBytesAsString(datums[i].GetBytes())
			}
		}
		row := make([]types.Datum, 0, len(columnValueOffsets))
		for _, colOff := range columnValueOffsets {
			row = append(row, datums[colOff])
		}
		rows = append(rows, row)
	}
	return rows
}

// Sometimes this test doesn't clean up fail, let the function name begin with 'Z'
// so it runs last and would not disrupt other tests.
func (s *testBinlogSuite) TestZIgnoreError(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.Se.GetSessionVars().BinlogClient = s.client
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (id int)")

	binloginfo.SetIgnoreError(true)
	s.pump.mu.Lock()
	s.pump.mu.mockFail = true
	s.pump.mu.Unlock()

	tk.MustExec("insert into t values (1)")
	tk.MustExec("insert into t values (1)")

	// Clean up.
	s.pump.mu.Lock()
	s.pump.mu.mockFail = false
	s.pump.mu.Unlock()
	binloginfo.DisableSkipBinlogFlag()
	binloginfo.SetIgnoreError(false)
}

func (s *testBinlogSuite) TestPartitionedTable(c *C) {
	// This test checks partitioned table write binlog with table ID, rather than partition ID.
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.Se.GetSessionVars().BinlogClient = s.client
	tk.MustExec("drop table if exists t")
	tk.MustExec(`create table t (id int) partition by range (id) (
			partition p0 values less than (1),
			partition p1 values less than (4),
			partition p2 values less than (7),
			partition p3 values less than (10))`)
	tids := make([]int64, 0, 10)
	for i := 0; i < 10; i++ {
		tk.MustExec("insert into t values (?)", i)
		prewriteVal := getLatestBinlogPrewriteValue(c, s.pump)
		tids = append(tids, prewriteVal.Mutations[0].TableId)
	}
	c.Assert(len(tids), Equals, 10)
	for i := 1; i < 10; i++ {
		c.Assert(tids[i], Equals, tids[0])
	}
}

func (s *testBinlogSuite) TestDeleteSchema(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("CREATE TABLE `b1` (`id` int(11) NOT NULL AUTO_INCREMENT, `job_id` varchar(50) NOT NULL, `split_job_id` varchar(30) DEFAULT NULL, PRIMARY KEY (`id`), KEY `b1` (`job_id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;")
	tk.MustExec("CREATE TABLE `b2` (`id` int(11) NOT NULL AUTO_INCREMENT, `job_id` varchar(50) NOT NULL, `batch_class` varchar(20) DEFAULT NULL, PRIMARY KEY (`id`), UNIQUE KEY `bu` (`job_id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	tk.MustExec("insert into b2 (job_id, batch_class) values (2, 'TEST');")
	tk.MustExec("insert into b1 (job_id) values (2);")

	// This test cover a bug that the final schema and the binlog row inconsistent.
	// The final schema of this SQL should be the schema of table b1, rather than the schema of join result.
	tk.MustExec("delete from b1 where job_id in (select job_id from b2 where batch_class = 'TEST') or split_job_id in (select job_id from b2 where batch_class = 'TEST');")
	tk.MustExec("delete b1 from b2 right join b1 on b1.job_id = b2.job_id and batch_class = 'TEST';")
}
