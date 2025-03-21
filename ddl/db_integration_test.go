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

package ddl_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/charset"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	tmysql "github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/store/mockstore/mocktikv"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/israce"
	"github.com/pingcap/tidb/util/mock"
	"github.com/pingcap/tidb/util/testkit"
	"github.com/pingcap/tidb/util/testutil"
)

var _ = Suite(&testIntegrationSuite1{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite2{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite3{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite4{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite5{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite6{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite7{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite8{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite9{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite10{&testIntegrationSuite{}})
var _ = Suite(&testIntegrationSuite11{&testIntegrationSuite{}})

type testIntegrationSuite struct {
	lease     time.Duration
	cluster   *mocktikv.Cluster
	mvccStore mocktikv.MVCCStore
	store     kv.Storage
	dom       *domain.Domain
	ctx       sessionctx.Context
	tk        *testkit.TestKit
}

func setupIntegrationSuite(s *testIntegrationSuite, c *C) {
	var err error
	s.lease = 50 * time.Millisecond
	ddl.WaitTimeWhenErrorOccured = 0

	s.cluster = mocktikv.NewCluster()
	mocktikv.BootstrapWithSingleStore(s.cluster)
	s.mvccStore = mocktikv.MustNewMVCCStore()
	s.store, err = mockstore.NewMockTikvStore(
		mockstore.WithCluster(s.cluster),
		mockstore.WithMVCCStore(s.mvccStore),
	)
	c.Assert(err, IsNil)
	session.SetSchemaLease(s.lease)
	session.SetStatsLease(0)
	s.dom, err = session.BootstrapSession(s.store)
	c.Assert(err, IsNil)

	se, err := session.CreateSession4Test(s.store)
	c.Assert(err, IsNil)
	s.ctx = se.(sessionctx.Context)
	_, err = se.Execute(context.Background(), "create database test_db")
	c.Assert(err, IsNil)
	s.tk = testkit.NewTestKit(c, s.store)
}

func tearDownIntegrationSuiteTest(s *testIntegrationSuite, c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	r := tk.MustQuery("show tables")
	for _, tb := range r.Rows() {
		tableName := tb[0]
		tk.MustExec(fmt.Sprintf("drop table %v", tableName))
	}
}

func tearDownIntegrationSuite(s *testIntegrationSuite, c *C) {
	s.dom.Close()
	s.store.Close()
}

func (s *testIntegrationSuite) SetUpSuite(c *C) {
	setupIntegrationSuite(s, c)
}

func (s *testIntegrationSuite) TearDownSuite(c *C) {
	tearDownIntegrationSuite(s, c)
}

type testIntegrationSuite1 struct{ *testIntegrationSuite }
type testIntegrationSuite2 struct{ *testIntegrationSuite }

func (s *testIntegrationSuite2) TearDownTest(c *C) {
	tearDownIntegrationSuiteTest(s.testIntegrationSuite, c)
}

type testIntegrationSuite3 struct{ *testIntegrationSuite }
type testIntegrationSuite4 struct{ *testIntegrationSuite }
type testIntegrationSuite5 struct{ *testIntegrationSuite }
type testIntegrationSuite6 struct{ *testIntegrationSuite }
type testIntegrationSuite7 struct{ *testIntegrationSuite }
type testIntegrationSuite8 struct{ *testIntegrationSuite }
type testIntegrationSuite9 struct{ *testIntegrationSuite }
type testIntegrationSuite10 struct{ *testIntegrationSuite }
type testIntegrationSuite11 struct{ *testIntegrationSuite }

func (s *testIntegrationSuite6) TestNoZeroDateMode(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	defer tk.MustExec("set session sql_mode='ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_CREATE_USER,NO_ENGINE_SUBSTITUTION';")

	tk.MustExec("use test;")
	tk.MustExec("set session sql_mode='STRICT_TRANS_TABLES,NO_ZERO_DATE,NO_ENGINE_SUBSTITUTION';")
	assertErrorCode(c, tk, "create table test_zero_date(agent_start_time date NOT NULL DEFAULT '0000-00-00')", mysql.ErrInvalidDefault)
	assertErrorCode(c, tk, "create table test_zero_date(agent_start_time datetime NOT NULL DEFAULT '0000-00-00 00:00:00')", mysql.ErrInvalidDefault)
	assertErrorCode(c, tk, "create table test_zero_date(agent_start_time timestamp NOT NULL DEFAULT '0000-00-00 00:00:00')", mysql.ErrInvalidDefault)
	assertErrorCode(c, tk, "create table test_zero_date(a timestamp default '0000-00-00 00');", mysql.ErrInvalidDefault)
	assertErrorCode(c, tk, "create table test_zero_date(a timestamp default 0);", mysql.ErrInvalidDefault)
}

func (s *testIntegrationSuite7) TestInvalidDefault(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("USE test;")

	_, err := tk.Exec("create table t(c1 decimal default 1.7976931348623157E308)")
	c.Assert(err, NotNil)
	c.Assert(terror.ErrorEqual(err, types.ErrInvalidDefault), IsTrue, Commentf("err %v", err))

	_, err = tk.Exec("create table t( c1 varchar(2) default 'TiDB');")
	c.Assert(err, NotNil)
	c.Assert(terror.ErrorEqual(err, types.ErrInvalidDefault), IsTrue, Commentf("err %v", err))
}

// TestInvalidNameWhenCreateTable for issue #3848
func (s *testIntegrationSuite8) TestInvalidNameWhenCreateTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("USE test;")

	_, err := tk.Exec("create table t(xxx.t.a bigint)")
	c.Assert(err, NotNil)
	c.Assert(terror.ErrorEqual(err, ddl.ErrWrongDBName), IsTrue, Commentf("err %v", err))

	_, err = tk.Exec("create table t(test.tttt.a bigint)")
	c.Assert(err, NotNil)
	c.Assert(terror.ErrorEqual(err, ddl.ErrWrongTableName), IsTrue, Commentf("err %v", err))

	_, err = tk.Exec("create table t(t.tttt.a bigint)")
	c.Assert(err, NotNil)
	c.Assert(terror.ErrorEqual(err, ddl.ErrWrongDBName), IsTrue, Commentf("err %v", err))
}

// TestCreateTableIfNotExists for issue #6879
func (s *testIntegrationSuite3) TestCreateTableIfNotExists(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("USE test;")

	tk.MustExec("create table t1(a bigint)")
	tk.MustExec("create table t(a bigint)")

	// Test duplicate create-table with `LIKE` clause
	tk.MustExec("create table if not exists t like t1;")
	warnings := tk.Se.GetSessionVars().StmtCtx.GetWarnings()
	c.Assert(len(warnings), GreaterEqual, 1)
	lastWarn := warnings[len(warnings)-1]
	c.Assert(terror.ErrorEqual(infoschema.ErrTableExists, lastWarn.Err), IsTrue, Commentf("err %v", lastWarn.Err))
	c.Assert(lastWarn.Level, Equals, stmtctx.WarnLevelNote)

	// Test duplicate create-table without `LIKE` clause
	tk.MustExec("create table if not exists t(b bigint, c varchar(60));")
	warnings = tk.Se.GetSessionVars().StmtCtx.GetWarnings()
	c.Assert(len(warnings), GreaterEqual, 1)
	lastWarn = warnings[len(warnings)-1]
	c.Assert(terror.ErrorEqual(infoschema.ErrTableExists, lastWarn.Err), IsTrue)
}

// for issue #9910
func (s *testIntegrationSuite2) TestCreateTableWithKeyWord(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("USE test;")

	_, err := tk.Exec("create table t1(pump varchar(20), drainer varchar(20), node_id varchar(20), node_state varchar(20));")
	c.Assert(err, IsNil)
}

func (s *testIntegrationSuite1) TestUniqueKeyNullValue(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("USE test")
	tk.MustExec("create table t(a int primary key, b varchar(255))")

	tk.MustExec("insert into t values(1, NULL)")
	tk.MustExec("insert into t values(2, NULL)")
	tk.MustExec("alter table t add unique index b(b);")
	res := tk.MustQuery("select count(*) from t use index(b);")
	res.Check(testkit.Rows("2"))
	tk.MustExec("admin check table t")
	tk.MustExec("admin check index t b")
}

func (s *testIntegrationSuite4) TestEndIncluded(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("USE test")
	tk.MustExec("create table t(a int, b int)")
	for i := 0; i < ddl.DefaultTaskHandleCnt+1; i++ {
		tk.MustExec("insert into t values(1, 1)")
	}
	tk.MustExec("alter table t add index b(b);")
	tk.MustExec("admin check index t b")
	tk.MustExec("admin check table t")
}

// TestModifyColumnAfterAddIndex Issue 5134
func (s *testIntegrationSuite3) TestModifyColumnAfterAddIndex(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table city (city VARCHAR(2) KEY);")
	tk.MustExec("alter table city change column city city varchar(50);")
	tk.MustExec(`insert into city values ("abc"), ("abd");`)
}

func (s *testIntegrationSuite9) TestIssue2293(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table t_issue_2293 (a int)")
	sql := "alter table t_issue_2293 add b int not null default 'a'"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidDefault)
	tk.MustExec("insert into t_issue_2293 value(1)")
	tk.MustQuery("select * from t_issue_2293").Check(testkit.Rows("1"))
}

func (s *testIntegrationSuite2) TestIssue6101(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table t1 (quantity decimal(2) unsigned);")
	_, err := tk.Exec("insert into t1 values (500), (-500), (~0), (-1);")
	terr := errors.Cause(err).(*terror.Error)
	c.Assert(terr.Code(), Equals, terror.ErrCode(tmysql.ErrWarnDataOutOfRange))
	tk.MustExec("drop table t1")

	tk.MustExec("set sql_mode=''")
	tk.MustExec("create table t1 (quantity decimal(2) unsigned);")
	tk.MustExec("insert into t1 values (500), (-500), (~0), (-1);")
	tk.MustQuery("select * from t1").Check(testkit.Rows("99", "0", "99", "0"))
	tk.MustExec("drop table t1")
}

func (s *testIntegrationSuite1) TestIndexLength(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table idx_len(a int(0), b timestamp(0), c datetime(0), d time(0), f float(0), g decimal(0))")
	tk.MustExec("create index idx on idx_len(a)")
	tk.MustExec("alter table idx_len add index idxa(a)")
	tk.MustExec("create index idx1 on idx_len(b)")
	tk.MustExec("alter table idx_len add index idxb(b)")
	tk.MustExec("create index idx2 on idx_len(c)")
	tk.MustExec("alter table idx_len add index idxc(c)")
	tk.MustExec("create index idx3 on idx_len(d)")
	tk.MustExec("alter table idx_len add index idxd(d)")
	tk.MustExec("create index idx4 on idx_len(f)")
	tk.MustExec("alter table idx_len add index idxf(f)")
	tk.MustExec("create index idx5 on idx_len(g)")
	tk.MustExec("alter table idx_len add index idxg(g)")
	tk.MustExec("create table idx_len1(a int(0), b timestamp(0), c datetime(0), d time(0), f float(0), g decimal(0), index(a), index(b), index(c), index(d), index(f), index(g))")
}

func (s *testIntegrationSuite4) TestIssue3833(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table issue3833 (b char(0)), c binary(0), d varchar(0)")
	assertErrorCode(c, tk, "create index idx on issue3833 (b)", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "alter table issue3833 add index idx (b)", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "create table issue3833_2 (b char(0), c binary(0), d varchar(0), index(b))", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "create index idx on issue3833 (c)", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "alter table issue3833 add index idx (c)", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "create table issue3833_2 (b char(0), c binary(0), d varchar(0), index(c))", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "create index idx on issue3833 (d)", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "alter table issue3833 add index idx (d)", tmysql.ErrWrongKeyColumn)
	assertErrorCode(c, tk, "create table issue3833_2 (b char(0), c binary(0), d varchar(0), index(d))", tmysql.ErrWrongKeyColumn)
}

func (s *testIntegrationSuite10) TestIssue2858And2717(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")

	tk.MustExec("create table t_issue_2858_bit (a bit(64) default b'0')")
	tk.MustExec("insert into t_issue_2858_bit value ()")
	tk.MustExec(`insert into t_issue_2858_bit values (100), ('10'), ('\0')`)
	tk.MustQuery("select a+0 from t_issue_2858_bit").Check(testkit.Rows("0", "100", "12592", "0"))
	tk.MustExec(`alter table t_issue_2858_bit alter column a set default '\0'`)

	tk.MustExec("create table t_issue_2858_hex (a int default 0x123)")
	tk.MustExec("insert into t_issue_2858_hex value ()")
	tk.MustExec("insert into t_issue_2858_hex values (123), (0x321)")
	tk.MustQuery("select a from t_issue_2858_hex").Check(testkit.Rows("291", "123", "801"))
	tk.MustExec(`alter table t_issue_2858_hex alter column a set default 0x321`)
}

func (s *testIntegrationSuite1) TestIssue4432(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")

	tk.MustExec("create table tx (col bit(10) default 'a')")
	tk.MustExec("insert into tx value ()")
	tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	tk.MustExec("drop table tx")

	tk.MustExec("create table tx (col bit(10) default 0x61)")
	tk.MustExec("insert into tx value ()")
	tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	tk.MustExec("drop table tx")

	tk.MustExec("create table tx (col bit(10) default 97)")
	tk.MustExec("insert into tx value ()")
	tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	tk.MustExec("drop table tx")

	tk.MustExec("create table tx (col bit(10) default 0b1100001)")
	tk.MustExec("insert into tx value ()")
	tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	tk.MustExec("drop table tx")
}

func (s *testIntegrationSuite5) TestMySQLErrorCode(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test_db")

	// create database
	sql := "create database aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	assertErrorCode(c, tk, sql, tmysql.ErrTooLongIdent)
	sql = "create database test"
	assertErrorCode(c, tk, sql, tmysql.ErrDBCreateExists)
	sql = "create database test1 character set uft8;"
	assertErrorCode(c, tk, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create database test2 character set gkb;"
	assertErrorCode(c, tk, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create database test3 character set laitn1;"
	assertErrorCode(c, tk, sql, tmysql.ErrUnknownCharacterSet)
	// drop database
	sql = "drop database db_not_exist"
	assertErrorCode(c, tk, sql, tmysql.ErrDBDropExists)
	// create table
	tk.MustExec("create table test_error_code_succ (c1 int, c2 int, c3 int, primary key(c3))")
	sql = "create table test_error_code_succ (c1 int, c2 int, c3 int)"
	assertErrorCode(c, tk, sql, tmysql.ErrTableExists)
	sql = "create table test_error_code1 (c1 int, c2 int, c2 int)"
	assertErrorCode(c, tk, sql, tmysql.ErrDupFieldName)
	sql = "create table test_error_code1 (c1 int, aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa int)"
	assertErrorCode(c, tk, sql, tmysql.ErrTooLongIdent)
	sql = "create table aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa(a int)"
	assertErrorCode(c, tk, sql, tmysql.ErrTooLongIdent)
	sql = "create table test_error_code1 (c1 int, c2 int, key aa (c1, c2), key aa (c1))"
	assertErrorCode(c, tk, sql, tmysql.ErrDupKeyName)
	sql = "create table test_error_code1 (c1 int, c2 int, c3 int, key(c_not_exist))"
	assertErrorCode(c, tk, sql, tmysql.ErrKeyColumnDoesNotExits)
	sql = "create table test_error_code1 (c1 int, c2 int, c3 int, primary key(c_not_exist))"
	assertErrorCode(c, tk, sql, tmysql.ErrKeyColumnDoesNotExits)
	sql = "create table test_error_code1 (c1 int not null default '')"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidDefault)
	sql = "CREATE TABLE `t` (`a` double DEFAULT 1.0 DEFAULT 2.0 DEFAULT now());"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidDefault)
	sql = "CREATE TABLE `t` (`a` double DEFAULT now());"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidDefault)
	sql = "create table t1(a int) character set uft8;"
	assertErrorCode(c, tk, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create table t1(a int) character set gkb;"
	assertErrorCode(c, tk, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create table t1(a int) character set laitn1;"
	assertErrorCode(c, tk, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create table test_error_code (a int not null ,b int not null,c int not null, d int not null, foreign key (b, c) references product(id));"
	assertErrorCode(c, tk, sql, tmysql.ErrWrongFkDef)
	sql = "create table test_error_code_2;"
	assertErrorCode(c, tk, sql, tmysql.ErrTableMustHaveColumns)
	sql = "create table test_error_code_2 (unique(c1));"
	assertErrorCode(c, tk, sql, tmysql.ErrTableMustHaveColumns)
	sql = "create table test_error_code_2(c1 int, c2 int, c3 int, primary key(c1), primary key(c2));"
	assertErrorCode(c, tk, sql, tmysql.ErrMultiplePriKey)
	sql = "create table test_error_code_3(pt blob ,primary key (pt));"
	assertErrorCode(c, tk, sql, tmysql.ErrBlobKeyWithoutLength)
	sql = "create table test_error_code_3(a text, unique (a(3073)));"
	assertErrorCode(c, tk, sql, tmysql.ErrTooLongKey)
	sql = "create table test_error_code_3(`id` int, key `primary`(`id`));"
	assertErrorCode(c, tk, sql, tmysql.ErrWrongNameForIndex)
	sql = "create table t2(c1.c2 blob default null);"
	assertErrorCode(c, tk, sql, tmysql.ErrWrongTableName)
	sql = "create table t2 (id int default null primary key , age int);"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidDefault)
	sql = "create table t2 (id int null primary key , age int);"
	assertErrorCode(c, tk, sql, tmysql.ErrPrimaryCantHaveNull)
	sql = "create table t2 (id int default null, age int, primary key(id));"
	assertErrorCode(c, tk, sql, tmysql.ErrPrimaryCantHaveNull)
	sql = "create table t2 (id int null, age int, primary key(id));"
	assertErrorCode(c, tk, sql, tmysql.ErrPrimaryCantHaveNull)

	sql = "create table t2 (id int primary key , age int);"
	tk.MustExec(sql)

	// add column
	sql = "alter table test_error_code_succ add column c1 int"
	assertErrorCode(c, tk, sql, tmysql.ErrDupFieldName)
	sql = "alter table test_error_code_succ add column aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa int"
	assertErrorCode(c, tk, sql, tmysql.ErrTooLongIdent)
	sql = "alter table test_comment comment 'test comment'"
	assertErrorCode(c, tk, sql, tmysql.ErrNoSuchTable)
	sql = "alter table test_error_code_succ add column `a ` int ;"
	assertErrorCode(c, tk, sql, tmysql.ErrWrongColumnName)
	tk.MustExec("create table test_on_update (c1 int, c2 int);")
	sql = "alter table test_on_update add column c3 int on update current_timestamp;"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidOnUpdate)
	sql = "create table test_on_update_2(c int on update current_timestamp);"
	assertErrorCode(c, tk, sql, tmysql.ErrInvalidOnUpdate)

	// drop column
	sql = "alter table test_error_code_succ drop c_not_exist"
	assertErrorCode(c, tk, sql, tmysql.ErrCantDropFieldOrKey)
	tk.MustExec("create table test_drop_column (c1 int );")
	sql = "alter table test_drop_column drop column c1;"
	assertErrorCode(c, tk, sql, tmysql.ErrCantRemoveAllFields)
	// add index
	sql = "alter table test_error_code_succ add index idx (c_not_exist)"
	assertErrorCode(c, tk, sql, tmysql.ErrKeyColumnDoesNotExits)
	tk.MustExec("alter table test_error_code_succ add index idx (c1)")
	sql = "alter table test_error_code_succ add index idx (c1)"
	assertErrorCode(c, tk, sql, tmysql.ErrDupKeyName)
	// drop index
	sql = "alter table test_error_code_succ drop index idx_not_exist"
	assertErrorCode(c, tk, sql, tmysql.ErrCantDropFieldOrKey)
	sql = "alter table test_error_code_succ drop column c3"
	assertErrorCode(c, tk, sql, int(tmysql.ErrUnknown))
	// modify column
	sql = "alter table test_error_code_succ modify testx.test_error_code_succ.c1 bigint"
	assertErrorCode(c, tk, sql, tmysql.ErrWrongDBName)
	sql = "alter table test_error_code_succ modify t.c1 bigint"
	assertErrorCode(c, tk, sql, tmysql.ErrWrongTableName)
	// insert value
	tk.MustExec("create table test_error_code_null(c1 char(100) not null);")
	sql = "insert into test_error_code_null (c1) values(null);"
	assertErrorCode(c, tk, sql, tmysql.ErrBadNull)
}

func (s *testIntegrationSuite9) TestTableDDLWithFloatType(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists t")
	assertErrorCode(c, s.tk, "create table t (a decimal(1, 2))", tmysql.ErrMBiggerThanD)
	assertErrorCode(c, s.tk, "create table t (a float(1, 2))", tmysql.ErrMBiggerThanD)
	assertErrorCode(c, s.tk, "create table t (a double(1, 2))", tmysql.ErrMBiggerThanD)
	s.tk.MustExec("create table t (a double(1, 1))")
	assertErrorCode(c, s.tk, "alter table t add column b decimal(1, 2)", tmysql.ErrMBiggerThanD)
	// add multi columns now not support, so no case.
	assertErrorCode(c, s.tk, "alter table t modify column a float(1, 4)", tmysql.ErrMBiggerThanD)
	assertErrorCode(c, s.tk, "alter table t change column a aa float(1, 4)", tmysql.ErrMBiggerThanD)
	s.tk.MustExec("drop table t")
}

func (s *testIntegrationSuite10) TestTableDDLWithTimeType(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists t")
	assertErrorCode(c, s.tk, "create table t (a time(7))", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "create table t (a datetime(7))", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "create table t (a timestamp(7))", tmysql.ErrTooBigPrecision)
	_, err := s.tk.Exec("create table t (a time(-1))")
	c.Assert(err, NotNil)
	s.tk.MustExec("create table t (a datetime)")
	assertErrorCode(c, s.tk, "alter table t add column b time(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t add column b datetime(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t add column b timestamp(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t modify column a time(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t modify column a datetime(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t modify column a timestamp(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t change column a aa time(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t change column a aa datetime(7)", tmysql.ErrTooBigPrecision)
	assertErrorCode(c, s.tk, "alter table t change column a aa timestamp(7)", tmysql.ErrTooBigPrecision)
	s.tk.MustExec("alter table t change column a aa datetime(0)")
	s.tk.MustExec("drop table t")
}

func (s *testIntegrationSuite2) TestUpdateMultipleTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("create database umt_db")
	tk.MustExec("use umt_db")
	tk.MustExec("create table t1 (c1 int, c2 int)")
	tk.MustExec("insert t1 values (1, 1), (2, 2)")
	tk.MustExec("create table t2 (c1 int, c2 int)")
	tk.MustExec("insert t2 values (1, 3), (2, 5)")
	ctx := tk.Se.(sessionctx.Context)
	dom := domain.GetDomain(ctx)
	is := dom.InfoSchema()
	db, ok := is.SchemaByName(model.NewCIStr("umt_db"))
	c.Assert(ok, IsTrue)
	t1Tbl, err := is.TableByName(model.NewCIStr("umt_db"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	t1Info := t1Tbl.Meta()

	// Add a new column in write only state.
	newColumn := &model.ColumnInfo{
		ID:                 100,
		Name:               model.NewCIStr("c3"),
		Offset:             2,
		DefaultValue:       9,
		OriginDefaultValue: 9,
		FieldType:          *types.NewFieldType(tmysql.TypeLonglong),
		State:              model.StateWriteOnly,
	}
	t1Info.Columns = append(t1Info.Columns, newColumn)

	kv.RunInNewTxn(s.store, false, func(txn kv.Transaction) error {
		m := meta.NewMeta(txn)
		_, err = m.GenSchemaVersion()
		c.Assert(err, IsNil)
		c.Assert(m.UpdateTable(db.ID, t1Info), IsNil)
		return nil
	})
	err = dom.Reload()
	c.Assert(err, IsNil)

	tk.MustExec("update t1, t2 set t1.c1 = 8, t2.c2 = 10 where t1.c2 = t2.c1")
	tk.MustQuery("select * from t1").Check(testkit.Rows("8 1", "8 2"))
	tk.MustQuery("select * from t2").Check(testkit.Rows("1 10", "2 10"))

	newColumn.State = model.StatePublic

	kv.RunInNewTxn(s.store, false, func(txn kv.Transaction) error {
		m := meta.NewMeta(txn)
		_, err = m.GenSchemaVersion()
		c.Assert(err, IsNil)
		c.Assert(m.UpdateTable(db.ID, t1Info), IsNil)
		return nil
	})
	err = dom.Reload()
	c.Assert(err, IsNil)

	tk.MustQuery("select * from t1").Check(testkit.Rows("8 1 9", "8 2 9"))
	tk.MustExec("drop database umt_db")
}

func (s *testIntegrationSuite7) TestNullGeneratedColumn(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("CREATE TABLE `t` (" +
		"`a` int(11) DEFAULT NULL," +
		"`b` int(11) DEFAULT NULL," +
		"`c` int(11) GENERATED ALWAYS AS (`a` + `b`) VIRTUAL," +
		"`h` varchar(10) DEFAULT NULL," +
		"`m` int(11) DEFAULT NULL" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")

	tk.MustExec("insert into t values()")
	tk.MustExec("alter table t add index idx_c(c)")
	tk.MustExec("drop table t")
}

func (s *testIntegrationSuite9) TestChangingCharsetToUtf8(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("use test")
	tk.MustExec("create table t1(a varchar(20) charset utf8)")
	tk.MustExec("insert into t1 values (?)", "t1_value")

	tk.MustExec("alter table t1 modify column a varchar(20) charset utf8mb4")
	tk.MustQuery("select * from t1;").Check(testkit.Rows("t1_value"))

	tk.MustExec("create table t(a varchar(20) charset latin1)")
	tk.MustExec("insert into t values (?)", "t_value")

	tk.MustExec("alter table t modify column a varchar(20) charset latin1")
	tk.MustQuery("select * from t;").Check(testkit.Rows("t_value"))

	rs, err := tk.Exec("alter table t modify column a varchar(20) charset utf8")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:210]unsupported modify charset from latin1 to utf8")
	rs, err = tk.Exec("alter table t modify column a varchar(20) charset utf8mb4")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:210]unsupported modify charset from latin1 to utf8mb4")

	rs, err = tk.Exec("alter table t modify column a varchar(20) charset utf8mb4 collate utf8bin")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
	rs, err = tk.Exec("alter table t modify column a varchar(20) charset utf8 collate utf8_bin")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
	rs, err = tk.Exec("alter table t modify column a varchar(20) charset utf8mb4 collate utf8mb4_general_ci")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
}

func (s *testIntegrationSuite10) TestChangingTableCharset(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("USE test")
	tk.MustExec("create table t(a char(10)) charset latin1 collate latin1_bin")
	rs, err := tk.Exec("alter table t charset gbk")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[parser:1115]Unknown character set: 'gbk'")
	rs, err = tk.Exec("alter table t charset utf8")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err.Error(), Equals, "[ddl:210]unsupported modify charset from latin1 to utf8")

	rs, err = tk.Exec("alter table t charset utf8 collate latin1_bin")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err, NotNil)
	rs, err = tk.Exec("alter table t charset utf8mb4")
	if rs != nil {
		rs.Close()
	}
	c.Assert(err.Error(), Equals, "[ddl:210]unsupported modify charset from latin1 to utf8mb4")

	rs, err = tk.Exec("alter table t charset utf8mb4 collate utf8mb4_bin")
	c.Assert(err, NotNil)

	rs, err = tk.Exec("alter table t charset ''")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[parser:1115]Unknown character set: ''")

	rs, err = tk.Exec("alter table t collate ''")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:1273]Unknown collation: ''")

	rs, err = tk.Exec("alter table t charset utf8mb4 collate '' collate utf8mb4_bin;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:1273]Unknown collation: ''")

	rs, err = tk.Exec("alter table t charset latin1 charset utf8 charset utf8mb4 collate utf8_bin;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:1302]Conflicting declarations: 'CHARACTER SET latin1' and 'CHARACTER SET utf8'")

	rs, err = tk.Exec("alter table t charset utf8 collate utf8mb4_bin;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:1253]COLLATION 'utf8mb4_bin' is not valid for CHARACTER SET 'utf8'")

	rs, err = tk.Exec("alter table t charset utf8 collate utf8_bin collate utf8mb4_bin collate utf8_bin;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:1253]COLLATION 'utf8mb4_bin' is not valid for CHARACTER SET 'utf8'")

	// Test change column charset when changing table charset.
	tk.MustExec("drop table t;")
	tk.MustExec("create table t(a varchar(10)) charset utf8")
	tk.MustExec("alter table t convert to charset utf8mb4;")
	checkCharset := func(chs, coll string) {
		tbl := testGetTableByName(c, s.ctx, "test", "t")
		c.Assert(tbl, NotNil)
		c.Assert(tbl.Meta().Charset, Equals, chs)
		c.Assert(tbl.Meta().Collate, Equals, coll)
		for _, col := range tbl.Meta().Columns {
			c.Assert(col.Charset, Equals, chs)
			c.Assert(col.Collate, Equals, coll)
		}
	}
	checkCharset(charset.CharsetUTF8MB4, charset.CollationUTF8MB4)

	// Test when column charset can not convert to the target charset.
	tk.MustExec("drop table t;")
	tk.MustExec("create table t(a varchar(10) character set ascii) charset utf8mb4")
	_, err = tk.Exec("alter table t convert to charset utf8mb4;")
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:210]unsupported modify charset from ascii to utf8mb4")

	tk.MustExec("drop table t;")
	tk.MustExec("create table t(a varchar(10) character set utf8) charset utf8")
	tk.MustExec("alter table t convert to charset utf8 collate utf8_general_ci;")
	checkCharset(charset.CharsetUTF8, "utf8_general_ci")

	// Test when table charset is equal to target charset but column charset is not equal.
	tk.MustExec("drop table t;")
	tk.MustExec("create table t(a varchar(10) character set utf8) charset utf8mb4")
	tk.MustExec("alter table t convert to charset utf8mb4 collate utf8mb4_general_ci;")
	checkCharset(charset.CharsetUTF8MB4, "utf8mb4_general_ci")

	// Mock table info with charset is "". Old TiDB maybe create table with charset is "".
	db, ok := domain.GetDomain(s.ctx).InfoSchema().SchemaByName(model.NewCIStr("test"))
	c.Assert(ok, IsTrue)
	tbl := testGetTableByName(c, s.ctx, "test", "t")
	tblInfo := tbl.Meta().Clone()
	tblInfo.Charset = ""
	tblInfo.Collate = ""
	updateTableInfo := func(tblInfo *model.TableInfo) {
		mockCtx := mock.NewContext()
		mockCtx.Store = s.store
		err = mockCtx.NewTxn(context.Background())
		c.Assert(err, IsNil)
		txn, err := mockCtx.Txn(true)
		c.Assert(err, IsNil)
		mt := meta.NewMeta(txn)

		err = mt.UpdateTable(db.ID, tblInfo)
		c.Assert(err, IsNil)
		err = txn.Commit(context.Background())
		c.Assert(err, IsNil)
	}
	updateTableInfo(tblInfo)

	// check table charset is ""
	tk.MustExec("alter table t add column b varchar(10);") //  load latest schema.
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	c.Assert(tbl, NotNil)
	c.Assert(tbl.Meta().Charset, Equals, "")
	c.Assert(tbl.Meta().Collate, Equals, "")
	// Test when table charset is "", this for compatibility.
	tk.MustExec("alter table t convert to charset utf8mb4;")
	checkCharset(charset.CharsetUTF8MB4, charset.CollationUTF8MB4)

	// Test when column charset is "".
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	tblInfo = tbl.Meta().Clone()
	tblInfo.Columns[0].Charset = ""
	tblInfo.Columns[0].Collate = ""
	updateTableInfo(tblInfo)
	// check table charset is ""
	tk.MustExec("alter table t drop column b;") //  load latest schema.
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	c.Assert(tbl, NotNil)
	c.Assert(tbl.Meta().Columns[0].Charset, Equals, "")
	c.Assert(tbl.Meta().Columns[0].Collate, Equals, "")
	tk.MustExec("alter table t convert to charset utf8mb4;")
	checkCharset(charset.CharsetUTF8MB4, charset.CollationUTF8MB4)

	tk.MustExec("drop table t")
	tk.MustExec("create table t (a blob) character set utf8;")
	tk.MustExec("alter table t charset=utf8mb4 collate=utf8mb4_bin;")
	tk.MustQuery("show create table t").Check(testutil.RowsWithSep("|",
		"t CREATE TABLE `t` (\n"+
			"  `a` blob DEFAULT NULL\n"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	))

}

func (s *testIntegrationSuite5) TestModifyingColumnOption(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("create database if not exists test")
	tk.MustExec("use test")

	errMsg := "[ddl:203]" // unsupported modify column with references
	assertErrCode := func(sql string, errCodeStr string) {
		_, err := tk.Exec(sql)
		c.Assert(err, NotNil)
		c.Assert(err.Error()[:len(errCodeStr)], Equals, errCodeStr)
	}

	tk.MustExec("drop table if exists t1")
	tk.MustExec("create table t1 (b char(1) default null) engine=InnoDB default charset=utf8mb4 collate=utf8mb4_general_ci")
	tk.MustExec("alter table t1 modify column b char(1) character set utf8mb4 collate utf8mb4_general_ci")

	tk.MustExec("drop table t1")
	tk.MustExec("create table t1 (b char(1) collate utf8mb4_general_ci)")
	tk.MustExec("alter table t1 modify b char(1) character set utf8mb4 collate utf8mb4_general_ci")

	tk.MustExec("drop table t1")
	tk.MustExec("drop table if exists t2")
	tk.MustExec("create table t1 (a int)")
	tk.MustExec("create table t2 (b int, c int)")
	assertErrCode("alter table t2 modify column c int references t1(a)", errMsg)
}

func (s *testIntegrationSuite7) TestCaseInsensitiveCharsetAndCollate(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("create database if not exists test_charset_collate")
	defer tk.MustExec("drop database test_charset_collate")
	tk.MustExec("use test_charset_collate")
	tk.MustExec("create table t(id int) ENGINE=InnoDB DEFAULT CHARSET=UTF8 COLLATE=UTF8_BIN;")
	tk.MustExec("create table t1(id int) ENGINE=InnoDB DEFAULT CHARSET=UTF8 COLLATE=uTF8_BIN;")
	tk.MustExec("create table t2(id int) ENGINE=InnoDB DEFAULT CHARSET=Utf8 COLLATE=utf8_BIN;")
	tk.MustExec("create table t3(id int) ENGINE=InnoDB DEFAULT CHARSET=Utf8mb4 COLLATE=utf8MB4_BIN;")
	tk.MustExec("create table t4(id int) ENGINE=InnoDB DEFAULT CHARSET=Utf8mb4 COLLATE=utf8MB4_general_ci;")

	tk.MustExec("create table t5(a varchar(20)) ENGINE=InnoDB DEFAULT CHARSET=UTF8MB4 COLLATE=UTF8MB4_GENERAL_CI;")
	tk.MustExec("insert into t5 values ('特克斯和凯科斯群岛')")

	db, ok := domain.GetDomain(s.ctx).InfoSchema().SchemaByName(model.NewCIStr("test_charset_collate"))
	c.Assert(ok, IsTrue)
	tbl := testGetTableByName(c, s.ctx, "test_charset_collate", "t5")
	tblInfo := tbl.Meta().Clone()
	c.Assert(tblInfo.Charset, Equals, "utf8mb4")
	c.Assert(tblInfo.Columns[0].Charset, Equals, "utf8mb4")

	tblInfo.Version = model.TableInfoVersion2
	tblInfo.Charset = "UTF8MB4"

	updateTableInfo := func(tblInfo *model.TableInfo) {
		mockCtx := mock.NewContext()
		mockCtx.Store = s.store
		err := mockCtx.NewTxn(context.Background())
		c.Assert(err, IsNil)
		txn, err := mockCtx.Txn(true)
		c.Assert(err, IsNil)
		mt := meta.NewMeta(txn)
		c.Assert(ok, IsTrue)
		err = mt.UpdateTable(db.ID, tblInfo)
		c.Assert(err, IsNil)
		err = txn.Commit(context.Background())
		c.Assert(err, IsNil)
	}
	updateTableInfo(tblInfo)
	tk.MustExec("alter table t5 add column b varchar(10);") //  load latest schema.

	tblInfo = testGetTableByName(c, s.ctx, "test_charset_collate", "t5").Meta()
	c.Assert(tblInfo.Charset, Equals, "utf8mb4")
	c.Assert(tblInfo.Columns[0].Charset, Equals, "utf8mb4")

	// For model.TableInfoVersion3, it is believed that all charsets / collations are lower-cased, do not do case-convert
	tblInfo = tblInfo.Clone()
	tblInfo.Version = model.TableInfoVersion3
	tblInfo.Charset = "UTF8MB4"
	updateTableInfo(tblInfo)
	tk.MustExec("alter table t5 add column c varchar(10);") //  load latest schema.

	tblInfo = testGetTableByName(c, s.ctx, "test_charset_collate", "t5").Meta()
	c.Assert(tblInfo.Charset, Equals, "UTF8MB4")
	c.Assert(tblInfo.Columns[0].Charset, Equals, "utf8mb4")
}

func (s *testIntegrationSuite3) TestZeroFillCreateTable(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists abc;")
	s.tk.MustExec("create table abc(y year, z tinyint(10) zerofill, primary key(y));")
	is := s.dom.InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("abc"))
	c.Assert(err, IsNil)
	var yearCol, zCol *model.ColumnInfo
	for _, col := range tbl.Meta().Columns {
		if col.Name.String() == "y" {
			yearCol = col
		}
		if col.Name.String() == "z" {
			zCol = col
		}
	}
	c.Assert(yearCol, NotNil)
	c.Assert(yearCol.Tp, Equals, mysql.TypeYear)
	c.Assert(mysql.HasUnsignedFlag(yearCol.Flag), IsTrue)

	c.Assert(zCol, NotNil)
	c.Assert(mysql.HasUnsignedFlag(zCol.Flag), IsTrue)
}

func (s *testIntegrationSuite6) TestBitDefaultValue(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table t_bit (c1 bit(10) default 250, c2 int);")
	tk.MustExec("insert into t_bit set c2=1;")
	tk.MustQuery("select bin(c1),c2 from t_bit").Check(testkit.Rows("11111010 1"))
	tk.MustExec("drop table t_bit")
	tk.MustExec(`create table testalltypes1 (
    field_1 bit default 1,
    field_2 tinyint null default null
	);`)
	tk.MustExec(`create table testalltypes2 (
    field_1 bit null default null,
    field_2 tinyint null default null,
    field_3 tinyint unsigned null default null,
    field_4 bigint null default null,
    field_5 bigint unsigned null default null,
    field_6 mediumblob null default null,
    field_7 longblob null default null,
    field_8 blob null default null,
    field_9 tinyblob null default null,
    field_10 varbinary(255) null default null,
    field_11 binary(255) null default null,
    field_12 mediumtext null default null,
    field_13 longtext null default null,
    field_14 text null default null,
    field_15 tinytext null default null,
    field_16 char(255) null default null,
    field_17 numeric null default null,
    field_18 decimal null default null,
    field_19 integer null default null,
    field_20 integer unsigned null default null,
    field_21 int null default null,
    field_22 int unsigned null default null,
    field_23 mediumint null default null,
    field_24 mediumint unsigned null default null,
    field_25 smallint null default null,
    field_26 smallint unsigned null default null,
    field_27 float null default null,
    field_28 double null default null,
    field_29 double precision null default null,
    field_30 real null default null,
    field_31 varchar(255) null default null,
    field_32 date null default null,
    field_33 time null default null,
    field_34 datetime null default null,
    field_35 timestamp null default null
	);`)
}

func (s *testIntegrationSuite5) TestBackwardCompatibility(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("create database if not exists test_backward_compatibility")
	defer tk.MustExec("drop database test_backward_compatibility")
	tk.MustExec("use test_backward_compatibility")
	tk.MustExec("create table t(a int primary key, b int)")
	for i := 0; i < 200; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%v, %v)", i, i))
	}

	// alter table t add index idx_b(b);
	is := s.dom.InfoSchema()
	schemaName := model.NewCIStr("test_backward_compatibility")
	tableName := model.NewCIStr("t")
	schema, ok := is.SchemaByName(schemaName)
	c.Assert(ok, IsTrue)
	tbl, err := is.TableByName(schemaName, tableName)
	c.Assert(err, IsNil)

	// Split the table.
	s.cluster.SplitTable(s.mvccStore, tbl.Meta().ID, 100)

	unique := false
	indexName := model.NewCIStr("idx_b")
	idxColName := &ast.IndexColName{
		Column: &ast.ColumnName{
			Schema: schemaName,
			Table:  tableName,
			Name:   model.NewCIStr("b"),
		},
		Length: types.UnspecifiedLength,
	}
	idxColNames := []*ast.IndexColName{idxColName}
	var indexOption *ast.IndexOption
	job := &model.Job{
		SchemaID:   schema.ID,
		TableID:    tbl.Meta().ID,
		Type:       model.ActionAddIndex,
		BinlogInfo: &model.HistoryInfo{},
		Args:       []interface{}{unique, indexName, idxColNames, indexOption},
	}
	txn, err := s.store.Begin()
	c.Assert(err, IsNil)
	t := meta.NewMeta(txn)
	job.ID, err = t.GenGlobalID()
	c.Assert(err, IsNil)
	job.Version = 1
	job.StartTS = txn.StartTS()

	// Simulate old TiDB init the add index job, old TiDB will not init the model.Job.ReorgMeta field,
	// if we set job.SnapshotVer here, can simulate the behavior.
	job.SnapshotVer = txn.StartTS()
	err = t.EnQueueDDLJob(job)
	c.Assert(err, IsNil)
	err = txn.Commit(context.Background())
	c.Assert(err, IsNil)
	ticker := time.NewTicker(s.lease)
	for range ticker.C {
		historyJob, err := s.getHistoryDDLJob(job.ID)
		c.Assert(err, IsNil)
		if historyJob == nil {

			continue
		}
		c.Assert(historyJob.Error, IsNil)

		if historyJob.IsSynced() {
			break
		}
	}

	// finished add index
	tk.MustExec("admin check index t idx_b")
}

func (s *testIntegrationSuite4) TestMultiRegionGetTableEndHandle(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("drop database if exists test_get_endhandle")
	tk.MustExec("create database test_get_endhandle")
	tk.MustExec("use test_get_endhandle")

	tk.MustExec("create table t(a bigint PRIMARY KEY, b int)")
	for i := 0; i < 1000; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%v, %v)", i, i))
	}

	// Get table ID for split.
	dom := domain.GetDomain(tk.Se)
	is := dom.InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test_get_endhandle"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tblID := tbl.Meta().ID

	d := s.dom.DDL()
	testCtx := newTestMaxTableRowIDContext(c, d, tbl)

	// Split the table.
	s.cluster.SplitTable(s.mvccStore, tblID, 100)

	maxID, emptyTable := s.getMaxTableRowID(testCtx)
	c.Assert(emptyTable, IsFalse)
	c.Assert(maxID, Equals, int64(999))

	tk.MustExec("insert into t values(10000, 1000)")
	maxID, emptyTable = s.getMaxTableRowID(testCtx)
	c.Assert(emptyTable, IsFalse)
	c.Assert(maxID, Equals, int64(10000))

	tk.MustExec("insert into t values(-1, 1000)")
	maxID, emptyTable = s.getMaxTableRowID(testCtx)
	c.Assert(emptyTable, IsFalse)
	c.Assert(maxID, Equals, int64(10000))
}

type testMaxTableRowIDContext struct {
	c   *C
	d   ddl.DDL
	tbl table.Table
}

func newTestMaxTableRowIDContext(c *C, d ddl.DDL, tbl table.Table) *testMaxTableRowIDContext {
	return &testMaxTableRowIDContext{
		c:   c,
		d:   d,
		tbl: tbl,
	}
}

func (s *testIntegrationSuite) getMaxTableRowID(ctx *testMaxTableRowIDContext) (int64, bool) {
	c := ctx.c
	d := ctx.d
	tbl := ctx.tbl
	curVer, err := s.store.CurrentVersion()
	c.Assert(err, IsNil)
	maxID, emptyTable, err := d.GetTableMaxRowID(curVer.Ver, tbl.(table.PhysicalTable))
	c.Assert(err, IsNil)
	return maxID, emptyTable
}

func (s *testIntegrationSuite) checkGetMaxTableRowID(ctx *testMaxTableRowIDContext, expectEmpty bool, expectMaxID int64) {
	c := ctx.c
	maxID, emptyTable := s.getMaxTableRowID(ctx)
	c.Assert(emptyTable, Equals, expectEmpty)
	c.Assert(maxID, Equals, expectMaxID)
}

func (s *testIntegrationSuite6) TestGetTableEndHandle(c *C) {
	// TestGetTableEndHandle test ddl.GetTableMaxRowID method, which will return the max row id of the table.
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("drop database if exists test_get_endhandle")
	tk.MustExec("create database test_get_endhandle")
	tk.MustExec("use test_get_endhandle")
	// Test PK is handle.
	tk.MustExec("create table t(a bigint PRIMARY KEY, b int)")

	is := s.dom.InfoSchema()
	d := s.dom.DDL()
	tbl, err := is.TableByName(model.NewCIStr("test_get_endhandle"), model.NewCIStr("t"))
	c.Assert(err, IsNil)

	testCtx := newTestMaxTableRowIDContext(c, d, tbl)
	// test empty table
	s.checkGetMaxTableRowID(testCtx, true, int64(math.MaxInt64))

	tk.MustExec("insert into t values(-1, 1)")
	s.checkGetMaxTableRowID(testCtx, false, int64(-1))

	tk.MustExec("insert into t values(9223372036854775806, 1)")
	s.checkGetMaxTableRowID(testCtx, false, int64(9223372036854775806))

	tk.MustExec("insert into t values(9223372036854775807, 1)")
	s.checkGetMaxTableRowID(testCtx, false, int64(9223372036854775807))

	tk.MustExec("insert into t values(10, 1)")
	tk.MustExec("insert into t values(102149142, 1)")
	s.checkGetMaxTableRowID(testCtx, false, int64(9223372036854775807))

	tk.MustExec("create table t1(a bigint PRIMARY KEY, b int)")

	for i := 0; i < 1000; i++ {
		tk.MustExec(fmt.Sprintf("insert into t1 values(%v, %v)", i, i))
	}
	is = s.dom.InfoSchema()
	testCtx.tbl, err = is.TableByName(model.NewCIStr("test_get_endhandle"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	s.checkGetMaxTableRowID(testCtx, false, int64(999))

	// Test PK is not handle
	tk.MustExec("create table t2(a varchar(255))")

	is = s.dom.InfoSchema()
	testCtx.tbl, err = is.TableByName(model.NewCIStr("test_get_endhandle"), model.NewCIStr("t2"))
	c.Assert(err, IsNil)
	s.checkGetMaxTableRowID(testCtx, true, int64(math.MaxInt64))

	for i := 0; i < 1000; i++ {
		tk.MustExec(fmt.Sprintf("insert into t2 values(%v)", i))
	}

	result := tk.MustQuery("select MAX(_tidb_rowid) from t2")
	maxID, emptyTable := s.getMaxTableRowID(testCtx)
	result.Check(testkit.Rows(fmt.Sprintf("%v", maxID)))
	c.Assert(emptyTable, IsFalse)

	tk.MustExec("insert into t2 values(100000)")
	result = tk.MustQuery("select MAX(_tidb_rowid) from t2")
	maxID, emptyTable = s.getMaxTableRowID(testCtx)
	result.Check(testkit.Rows(fmt.Sprintf("%v", maxID)))
	c.Assert(emptyTable, IsFalse)

	tk.MustExec(fmt.Sprintf("insert into t2 values(%v)", math.MaxInt64-1))
	result = tk.MustQuery("select MAX(_tidb_rowid) from t2")
	maxID, emptyTable = s.getMaxTableRowID(testCtx)
	result.Check(testkit.Rows(fmt.Sprintf("%v", maxID)))
	c.Assert(emptyTable, IsFalse)

	tk.MustExec(fmt.Sprintf("insert into t2 values(%v)", math.MaxInt64))
	result = tk.MustQuery("select MAX(_tidb_rowid) from t2")
	maxID, emptyTable = s.getMaxTableRowID(testCtx)
	result.Check(testkit.Rows(fmt.Sprintf("%v", maxID)))
	c.Assert(emptyTable, IsFalse)

	tk.MustExec("insert into t2 values(100)")
	result = tk.MustQuery("select MAX(_tidb_rowid) from t2")
	maxID, emptyTable = s.getMaxTableRowID(testCtx)
	result.Check(testkit.Rows(fmt.Sprintf("%v", maxID)))
	c.Assert(emptyTable, IsFalse)
}

func (s *testIntegrationSuite) getHistoryDDLJob(id int64) (*model.Job, error) {
	var job *model.Job

	err := kv.RunInNewTxn(s.store, false, func(txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		var err1 error
		job, err1 = t.GetHistoryDDLJob(id)
		return errors.Trace(err1)
	})

	return job, errors.Trace(err)
}

func (s *testIntegrationSuite1) TestCreateTableTooLarge(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")

	sql := "create table t_too_large ("
	cnt := 3000
	for i := 1; i <= cnt; i++ {
		sql += fmt.Sprintf("a%d double, b%d double, c%d double, d%d double", i, i, i, i)
		if i != cnt {
			sql += ","
		}
	}
	sql += ");"
	assertErrorCode(c, s.tk, sql, tmysql.ErrTooManyFields)

	originLimit := atomic.LoadUint32(&ddl.TableColumnCountLimit)
	atomic.StoreUint32(&ddl.TableColumnCountLimit, uint32(cnt*4))
	_, err := s.tk.Exec(sql)
	c.Assert(kv.ErrEntryTooLarge.Equal(err), IsTrue, Commentf("err:%v", err))
	atomic.StoreUint32(&ddl.TableColumnCountLimit, originLimit)
}

func (s *testIntegrationSuite8) TestChangeColumnPosition(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")

	s.tk.MustExec("create table position (a int default 1, b int default 2)")
	s.tk.MustExec("insert into position value ()")
	s.tk.MustExec("insert into position values (3,4)")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("1 2", "3 4"))
	s.tk.MustExec("alter table position modify column b int first")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("2 1", "4 3"))
	s.tk.MustExec("insert into position value ()")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("2 1", "4 3", "<nil> 1"))

	s.tk.MustExec("create table position1 (a int, b int, c double, d varchar(5))")
	s.tk.MustExec(`insert into position1 value (1, 2, 3.14, 'TiDB')`)
	s.tk.MustExec("alter table position1 modify column d varchar(5) after a")
	s.tk.MustQuery("select * from position1").Check(testkit.Rows("1 TiDB 2 3.14"))
	s.tk.MustExec("alter table position1 modify column a int after c")
	s.tk.MustQuery("select * from position1").Check(testkit.Rows("TiDB 2 3.14 1"))
	s.tk.MustExec("alter table position1 modify column c double first")
	s.tk.MustQuery("select * from position1").Check(testkit.Rows("3.14 TiDB 2 1"))
	assertErrorCode(c, s.tk, "alter table position1 modify column b int after b", tmysql.ErrBadField)

	s.tk.MustExec("create table position2 (a int, b int)")
	s.tk.MustExec("alter table position2 add index t(a, b)")
	s.tk.MustExec("alter table position2 modify column b int first")
	s.tk.MustExec("insert into position2 value (3, 5)")
	s.tk.MustQuery("select a from position2 where a = 3").Check(testkit.Rows())
	s.tk.MustExec("alter table position2 change column b c int first")
	s.tk.MustQuery("select * from position2 where c = 3").Check(testkit.Rows("3 5"))
	assertErrorCode(c, s.tk, "alter table position2 change column c b int after c", tmysql.ErrBadField)

	s.tk.MustExec("create table position3 (a int default 2)")
	s.tk.MustExec("alter table position3 modify column a int default 5 first")
	s.tk.MustExec("insert into position3 value ()")
	s.tk.MustQuery("select * from position3").Check(testkit.Rows("5"))

	s.tk.MustExec("create table position4 (a int, b int)")
	s.tk.MustExec("alter table position4 add index t(b)")
	s.tk.MustExec("alter table position4 change column b c int first")
	createSQL := s.tk.MustQuery("show create table position4").Rows()[0][1]
	exceptedSQL := []string{
		"CREATE TABLE `position4` (",
		"  `c` int(11) DEFAULT NULL,",
		"  `a` int(11) DEFAULT NULL,",
		"  KEY `t` (`c`)",
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	}
	c.Assert(createSQL, Equals, strings.Join(exceptedSQL, "\n"))
}

func (s *testIntegrationSuite2) TestAddIndexAfterAddColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")

	s.tk.MustExec("create table test_add_index_after_add_col(a int, b int not null default '0')")
	s.tk.MustExec("insert into test_add_index_after_add_col values(1, 2),(2,2)")
	s.tk.MustExec("alter table test_add_index_after_add_col add column c int not null default '0'")
	sql := "alter table test_add_index_after_add_col add unique index cc(c) "
	assertErrorCode(c, s.tk, sql, tmysql.ErrDupEntry)
	sql = "alter table test_add_index_after_add_col add index idx_test(f1,f2,f3,f4,f5,f6,f7,f8,f9,f10,f11,f12,f13,f14,f15,f16,f17);"
	assertErrorCode(c, s.tk, sql, tmysql.ErrTooManyKeyParts)
}

func (s *testIntegrationSuite8) TestResolveCharset(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists resolve_charset")
	s.tk.MustExec(`CREATE TABLE resolve_charset (a varchar(255) DEFAULT NULL) DEFAULT CHARSET=latin1`)
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("resolve_charset"))
	c.Assert(err, IsNil)
	c.Assert(tbl.Cols()[0].Charset, Equals, "latin1")
	s.tk.MustExec("INSERT INTO resolve_charset VALUES('鰈')")

	s.tk.MustExec("create database resolve_charset charset binary")
	s.tk.MustExec("use resolve_charset")
	s.tk.MustExec(`CREATE TABLE resolve_charset (a varchar(255) DEFAULT NULL) DEFAULT CHARSET=latin1`)

	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("resolve_charset"), model.NewCIStr("resolve_charset"))
	c.Assert(err, IsNil)
	c.Assert(tbl.Cols()[0].Charset, Equals, "latin1")
	c.Assert(tbl.Meta().Charset, Equals, "latin1")

	s.tk.MustExec(`CREATE TABLE resolve_charset1 (a varchar(255) DEFAULT NULL)`)
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("resolve_charset"), model.NewCIStr("resolve_charset1"))
	c.Assert(err, IsNil)
	c.Assert(tbl.Cols()[0].Charset, Equals, "binary")
	c.Assert(tbl.Meta().Charset, Equals, "binary")
}

func (s *testIntegrationSuite2) TestAddAnonymousIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("create table t_anonymous_index (c1 int, c2 int, C3 int)")
	s.tk.MustExec("alter table t_anonymous_index add index (c1, c2)")
	// for dropping empty index
	_, err := s.tk.Exec("alter table t_anonymous_index drop index")
	c.Assert(err, NotNil)
	// The index name is c1 when adding index (c1, c2).
	s.tk.MustExec("alter table t_anonymous_index drop index c1")
	t := testGetTableByName(c, s.ctx, "test", "t_anonymous_index")
	c.Assert(t.Indices(), HasLen, 0)
	// for adding some indices that the first column name is c1
	s.tk.MustExec("alter table t_anonymous_index add index (c1)")
	_, err = s.tk.Exec("alter table t_anonymous_index add index c1 (c2)")
	c.Assert(err, NotNil)
	t = testGetTableByName(c, s.ctx, "test", "t_anonymous_index")
	c.Assert(t.Indices(), HasLen, 1)
	idx := t.Indices()[0].Meta().Name.L
	c.Assert(idx, Equals, "c1")
	// The MySQL will be a warning.
	s.tk.MustExec("alter table t_anonymous_index add index c1_3 (c1)")
	s.tk.MustExec("alter table t_anonymous_index add index (c1, c2, C3)")
	// The MySQL will be a warning.
	s.tk.MustExec("alter table t_anonymous_index add index (c1)")
	t = testGetTableByName(c, s.ctx, "test", "t_anonymous_index")
	c.Assert(t.Indices(), HasLen, 4)
	s.tk.MustExec("alter table t_anonymous_index drop index c1")
	s.tk.MustExec("alter table t_anonymous_index drop index c1_2")
	s.tk.MustExec("alter table t_anonymous_index drop index c1_3")
	s.tk.MustExec("alter table t_anonymous_index drop index c1_4")
	// for case insensitive
	s.tk.MustExec("alter table t_anonymous_index add index (C3)")
	s.tk.MustExec("alter table t_anonymous_index drop index c3")
	s.tk.MustExec("alter table t_anonymous_index add index c3 (C3)")
	s.tk.MustExec("alter table t_anonymous_index drop index C3")
	// for anonymous index with column name `primary`
	s.tk.MustExec("create table t_primary (`primary` int, key (`primary`))")
	t = testGetTableByName(c, s.ctx, "test", "t_primary")
	c.Assert(t.Indices()[0].Meta().Name.String(), Equals, "primary_2")
	s.tk.MustExec("create table t_primary_2 (`primary` int, key primary_2 (`primary`), key (`primary`))")
	t = testGetTableByName(c, s.ctx, "test", "t_primary_2")
	c.Assert(t.Indices()[0].Meta().Name.String(), Equals, "primary_2")
	c.Assert(t.Indices()[1].Meta().Name.String(), Equals, "primary_3")
	s.tk.MustExec("create table t_primary_3 (`primary_2` int, key(`primary_2`), `primary` int, key(`primary`));")
	t = testGetTableByName(c, s.ctx, "test", "t_primary_3")
	c.Assert(t.Indices()[0].Meta().Name.String(), Equals, "primary_2")
	c.Assert(t.Indices()[1].Meta().Name.String(), Equals, "primary_3")
}

func (s *testIntegrationSuite1) TestAddColumnTooMany(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	count := int(atomic.LoadUint32(&ddl.TableColumnCountLimit) - 1)
	var cols []string
	for i := 0; i < count; i++ {
		cols = append(cols, fmt.Sprintf("a%d int", i))
	}
	createSQL := fmt.Sprintf("create table t_column_too_many (%s)", strings.Join(cols, ","))
	s.tk.MustExec(createSQL)
	s.tk.MustExec("alter table t_column_too_many add column a_512 int")
	alterSQL := "alter table t_column_too_many add column a_513 int"
	assertErrorCode(c, s.tk, alterSQL, tmysql.ErrTooManyFields)
}

func (s *testIntegrationSuite4) TestAlterColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test_db")

	s.tk.MustExec("create table test_alter_column (a int default 111, b varchar(8), c varchar(8) not null, d timestamp on update current_timestamp)")
	s.tk.MustExec("insert into test_alter_column set b = 'a', c = 'aa'")
	s.tk.MustQuery("select a from test_alter_column").Check(testkit.Rows("111"))
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo := tbl.Meta()
	colA := tblInfo.Columns[0]
	hasNoDefault := tmysql.HasNoDefaultValueFlag(colA.Flag)
	c.Assert(hasNoDefault, IsFalse)
	s.tk.MustExec("alter table test_alter_column alter column a set default 222")
	s.tk.MustExec("insert into test_alter_column set b = 'b', c = 'bb'")
	s.tk.MustQuery("select a from test_alter_column").Check(testkit.Rows("111", "222"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colA = tblInfo.Columns[0]
	hasNoDefault = tmysql.HasNoDefaultValueFlag(colA.Flag)
	c.Assert(hasNoDefault, IsFalse)
	s.tk.MustExec("alter table test_alter_column alter column b set default null")
	s.tk.MustExec("insert into test_alter_column set c = 'cc'")
	s.tk.MustQuery("select b from test_alter_column").Check(testkit.Rows("a", "b", "<nil>"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colC := tblInfo.Columns[2]
	hasNoDefault = tmysql.HasNoDefaultValueFlag(colC.Flag)
	c.Assert(hasNoDefault, IsTrue)
	s.tk.MustExec("alter table test_alter_column alter column c set default 'xx'")
	s.tk.MustExec("insert into test_alter_column set a = 123")
	s.tk.MustQuery("select c from test_alter_column").Check(testkit.Rows("aa", "bb", "cc", "xx"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colC = tblInfo.Columns[2]
	hasNoDefault = tmysql.HasNoDefaultValueFlag(colC.Flag)
	c.Assert(hasNoDefault, IsFalse)
	// TODO: After fix issue 2606.
	// s.tk.MustExec( "alter table test_alter_column alter column d set default null")
	s.tk.MustExec("alter table test_alter_column alter column a drop default")
	s.tk.MustExec("insert into test_alter_column set b = 'd', c = 'dd'")
	s.tk.MustQuery("select a from test_alter_column").Check(testkit.Rows("111", "222", "222", "123", "<nil>"))

	// for failing tests
	sql := "alter table db_not_exist.test_alter_column alter column b set default 'c'"
	assertErrorCode(c, s.tk, sql, tmysql.ErrNoSuchTable)
	sql = "alter table test_not_exist alter column b set default 'c'"
	assertErrorCode(c, s.tk, sql, tmysql.ErrNoSuchTable)
	sql = "alter table test_alter_column alter column col_not_exist set default 'c'"
	assertErrorCode(c, s.tk, sql, tmysql.ErrBadField)
	sql = "alter table test_alter_column alter column c set default null"
	assertErrorCode(c, s.tk, sql, tmysql.ErrInvalidDefault)

	// The followings tests whether adding constraints via change / modify column
	// is forbidden as expected.
	s.tk.MustExec("drop table if exists mc")
	s.tk.MustExec("create table mc(a int key, b int, c int)")
	_, err = s.tk.Exec("alter table mc modify column a int key") // Adds a new primary key
	c.Assert(err, NotNil)
	_, err = s.tk.Exec("alter table mc modify column c int unique") // Adds a new unique key
	c.Assert(err, NotNil)
	result := s.tk.MustQuery("show create table mc")
	createSQL := result.Rows()[0][1]
	expected := "CREATE TABLE `mc` (\n  `a` int(11) NOT NULL,\n  `b` int(11) DEFAULT NULL,\n  `c` int(11) DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	c.Assert(createSQL, Equals, expected)

	// Change / modify column should preserve index options.
	s.tk.MustExec("drop table if exists mc")
	s.tk.MustExec("create table mc(a int key, b int, c int unique)")
	s.tk.MustExec("alter table mc modify column a bigint") // NOT NULL & PRIMARY KEY should be preserved
	s.tk.MustExec("alter table mc modify column b bigint")
	s.tk.MustExec("alter table mc modify column c bigint") // Unique should be preserved
	result = s.tk.MustQuery("show create table mc")
	createSQL = result.Rows()[0][1]
	expected = "CREATE TABLE `mc` (\n  `a` bigint(20) NOT NULL,\n  `b` bigint(20) DEFAULT NULL,\n  `c` bigint(20) DEFAULT NULL,\n  PRIMARY KEY (`a`),\n  UNIQUE KEY `c` (`c`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	c.Assert(createSQL, Equals, expected)

	// Dropping or keeping auto_increment is allowed, however adding is not allowed.
	s.tk.MustExec("drop table if exists mc")
	s.tk.MustExec("create table mc(a int key auto_increment, b int)")
	s.tk.MustExec("alter table mc modify column a bigint auto_increment") // Keeps auto_increment
	result = s.tk.MustQuery("show create table mc")
	createSQL = result.Rows()[0][1]
	expected = "CREATE TABLE `mc` (\n  `a` bigint(20) NOT NULL AUTO_INCREMENT,\n  `b` int(11) DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	c.Assert(createSQL, Equals, expected)
	s.tk.MustExec("alter table mc modify column a bigint") // Drops auto_increment
	result = s.tk.MustQuery("show create table mc")
	createSQL = result.Rows()[0][1]
	expected = "CREATE TABLE `mc` (\n  `a` bigint(20) NOT NULL,\n  `b` int(11) DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	c.Assert(createSQL, Equals, expected)
	_, err = s.tk.Exec("alter table mc modify column a bigint auto_increment") // Adds auto_increment should throw error
	c.Assert(err, NotNil)
}

func (s *testIntegrationSuite) assertWarningExec(c *C, sql string, expectedWarn *terror.Error) {
	_, err := s.tk.Exec(sql)
	c.Assert(err, IsNil)
	st := s.tk.Se.GetSessionVars().StmtCtx
	c.Assert(st.WarningCount(), Equals, uint16(1))
	c.Assert(expectedWarn.Equal(st.GetWarnings()[0].Err), IsTrue, Commentf("error:%v", err))
}

func (s *testIntegrationSuite) assertAlterWarnExec(c *C, sql string) {
	s.assertWarningExec(c, sql, ddl.ErrAlterOperationNotSupported)
}

func (s *testIntegrationSuite) assertAlterErrorExec(c *C, sql string) {
	assertErrorCode(c, s.tk, sql, mysql.ErrAlterOperationNotSupportedReason)
}

func (s *testIntegrationSuite3) TestAlterAlgorithm(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists t, t1")
	defer s.tk.MustExec("drop table if exists t")

	s.tk.MustExec(`create table t(
	a int,
	b varchar(100),
	c int,
	INDEX idx_c(c)) PARTITION BY RANGE ( a ) (
	PARTITION p0 VALUES LESS THAN (6),
		PARTITION p1 VALUES LESS THAN (11),
		PARTITION p2 VALUES LESS THAN (16),
		PARTITION p3 VALUES LESS THAN (21)
	)`)
	s.assertAlterErrorExec(c, "alter table t modify column a bigint, ALGORITHM=INPLACE;")
	s.tk.MustExec("alter table t modify column a bigint, ALGORITHM=INPLACE, ALGORITHM=INSTANT;")
	s.tk.MustExec("alter table t modify column a bigint, ALGORITHM=DEFAULT;")

	// Test add/drop index
	s.assertAlterErrorExec(c, "alter table t add index idx_b(b), ALGORITHM=INSTANT")
	s.assertAlterWarnExec(c, "alter table t add index idx_b1(b), ALGORITHM=COPY")
	s.tk.MustExec("alter table t add index idx_b2(b), ALGORITHM=INPLACE")
	s.assertAlterErrorExec(c, "alter table t drop index idx_b, ALGORITHM=INPLACE")
	s.assertAlterWarnExec(c, "alter table t drop index idx_b1, ALGORITHM=COPY")
	s.tk.MustExec("alter table t drop index idx_b2, ALGORITHM=INSTANT")

	// Test rename
	s.assertAlterWarnExec(c, "alter table t rename to t1, ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t1 rename to t, ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t1 rename to t, ALGORITHM=INSTANT")
	s.tk.MustExec("alter table t rename to t1, ALGORITHM=DEFAULT")
	s.tk.MustExec("alter table t1 rename to t")

	// Test rename index
	s.assertAlterWarnExec(c, "alter table t rename index idx_c to idx_c1, ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t rename index idx_c1 to idx_c, ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t rename index idx_c1 to idx_c, ALGORITHM=INSTANT")
	s.tk.MustExec("alter table t rename index idx_c to idx_c1, ALGORITHM=DEFAULT")

	// partition.
	s.assertAlterWarnExec(c, "alter table t truncate partition p1, ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t truncate partition p2, ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t truncate partition p3, ALGORITHM=INSTANT")

	s.assertAlterWarnExec(c, "alter table t add partition (partition p4 values less than (2002)), ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t add partition (partition p5 values less than (3002)), ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t add partition (partition p6 values less than (4002)), ALGORITHM=INSTANT")

	s.assertAlterWarnExec(c, "alter table t drop partition p4, ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t drop partition p5, ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t drop partition p6, ALGORITHM=INSTANT")

	// Table options
	s.assertAlterWarnExec(c, "alter table t comment = 'test', ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t comment = 'test', ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t comment = 'test', ALGORITHM=INSTANT")

	s.assertAlterWarnExec(c, "alter table t default charset = utf8mb4, ALGORITHM=COPY")
	s.assertAlterErrorExec(c, "alter table t default charset = utf8mb4, ALGORITHM=INPLACE")
	s.tk.MustExec("alter table t default charset = utf8mb4, ALGORITHM=INSTANT")
}

func (s *testIntegrationSuite5) TestFulltextIndexIgnore(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists t_ft")
	defer s.tk.MustExec("drop table if exists t_ft")
	// Make sure that creating and altering to add a fulltext key gives the correct warning
	s.assertWarningExec(c, "create table t_ft (a text, fulltext key (a))", ddl.ErrTableCantHandleFt)
	s.assertWarningExec(c, "alter table t_ft add fulltext key (a)", ddl.ErrTableCantHandleFt)

	// Make sure table t_ft still has no indexes even after it was created and altered
	r := s.tk.MustQuery("show index from t_ft")
	c.Assert(r.Rows(), HasLen, 0)
	r = s.tk.MustQuery("select * from information_schema.statistics where table_schema='test' and table_name='t_ft'")
	c.Assert(r.Rows(), HasLen, 0)
}

func (s *testIntegrationSuite1) TestTreatOldVersionUTF8AsUTF8MB4(c *C) {
	if israce.RaceEnabled {
		c.Skip("skip race test")
	}
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists t")
	defer s.tk.MustExec("drop table if exists t")

	s.tk.MustExec("create table t (a varchar(10) character set utf8, b varchar(10) character set ascii) charset=utf8mb4;")
	assertErrorCode(c, s.tk, "insert into t set a= x'f09f8c80';", mysql.ErrTruncatedWrongValueForField)
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(10) CHARACTER SET utf8 COLLATE utf8_bin DEFAULT NULL,\n" +
		"  `b` varchar(10) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))

	// Mock old version table info with column charset is utf8.
	db, ok := domain.GetDomain(s.ctx).InfoSchema().SchemaByName(model.NewCIStr("test"))
	tbl := testGetTableByName(c, s.ctx, "test", "t")
	tblInfo := tbl.Meta().Clone()
	tblInfo.Version = model.TableInfoVersion0
	tblInfo.Columns[0].Version = model.ColumnInfoVersion0
	updateTableInfo := func(tblInfo *model.TableInfo) {
		mockCtx := mock.NewContext()
		mockCtx.Store = s.store
		err := mockCtx.NewTxn(context.Background())
		c.Assert(err, IsNil)
		txn, err := mockCtx.Txn(true)
		c.Assert(err, IsNil)
		mt := meta.NewMeta(txn)
		c.Assert(ok, IsTrue)
		err = mt.UpdateTable(db.ID, tblInfo)
		c.Assert(err, IsNil)
		err = txn.Commit(context.Background())
		c.Assert(err, IsNil)
	}
	updateTableInfo(tblInfo)
	s.tk.MustExec("alter table t add column c varchar(10) character set utf8;") // load latest schema.
	c.Assert(config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4, IsTrue)
	s.tk.MustExec("insert into t set a= x'f09f8c80'")
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(10) DEFAULT NULL,\n" +
		"  `b` varchar(10) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,\n" +
		"  `c` varchar(10) CHARACTER SET utf8 COLLATE utf8_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))

	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = false
	s.tk.MustExec("alter table t drop column c;") //  reload schema.
	assertErrorCode(c, s.tk, "insert into t set a= x'f09f8c80'", mysql.ErrTruncatedWrongValueForField)
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(10) CHARACTER SET utf8 COLLATE utf8_bin DEFAULT NULL,\n" +
		"  `b` varchar(10) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))

	// Mock old version table info with table and column charset is utf8.
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	tblInfo = tbl.Meta().Clone()
	tblInfo.Charset = charset.CharsetUTF8
	tblInfo.Collate = charset.CollationUTF8
	tblInfo.Version = model.TableInfoVersion0
	tblInfo.Columns[0].Version = model.ColumnInfoVersion0
	updateTableInfo(tblInfo)

	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = true
	s.tk.MustExec("alter table t add column c varchar(10);") //  load latest schema.
	s.tk.MustExec("insert into t set a= x'f09f8c80'")
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(10) DEFAULT NULL,\n" +
		"  `b` varchar(10) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,\n" +
		"  `c` varchar(10) DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))

	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = false
	s.tk.MustExec("alter table t drop column c;") //  reload schema.
	assertErrorCode(c, s.tk, "insert into t set a= x'f09f8c80'", mysql.ErrTruncatedWrongValueForField)
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(10) DEFAULT NULL,\n" +
		"  `b` varchar(10) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"))

	// Test modify column charset.
	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = true
	s.tk.MustExec("alter table t modify column a varchar(10) character set utf8mb4") //  change column charset.
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	c.Assert(tbl.Meta().Columns[0].Charset, Equals, charset.CharsetUTF8MB4)
	c.Assert(tbl.Meta().Columns[0].Collate, Equals, charset.CollationUTF8MB4)
	c.Assert(tbl.Meta().Columns[0].Version, Equals, model.ColumnInfoVersion0)
	s.tk.MustExec("insert into t set a= x'f09f8c80'")
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(10) DEFAULT NULL,\n" +
		"  `b` varchar(10) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))
	// Test for change column should not modify the column version.
	s.tk.MustExec("alter table t change column a a varchar(20)") //  change column.
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	c.Assert(tbl.Meta().Columns[0].Charset, Equals, charset.CharsetUTF8MB4)
	c.Assert(tbl.Meta().Columns[0].Collate, Equals, charset.CollationUTF8MB4)
	c.Assert(tbl.Meta().Columns[0].Version, Equals, model.ColumnInfoVersion0)

	// Test for v2.1.5 and v2.1.6 that table version is 1 but column version is 0.
	tbl = testGetTableByName(c, s.ctx, "test", "t")
	tblInfo = tbl.Meta().Clone()
	tblInfo.Charset = charset.CharsetUTF8
	tblInfo.Collate = charset.CollationUTF8
	tblInfo.Version = model.TableInfoVersion1
	tblInfo.Columns[0].Version = model.ColumnInfoVersion0
	tblInfo.Columns[0].Charset = charset.CharsetUTF8
	tblInfo.Columns[0].Collate = charset.CollationUTF8
	updateTableInfo(tblInfo)
	c.Assert(config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4, IsTrue)
	s.tk.MustExec("alter table t change column b b varchar(20) character set ascii") // reload schema.
	s.tk.MustExec("insert into t set a= x'f09f8c80'")
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(20) DEFAULT NULL,\n" +
		"  `b` varchar(20) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))

	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = false
	s.tk.MustExec("alter table t change column b b varchar(30) character set ascii") // reload schema.
	assertErrorCode(c, s.tk, "insert into t set a= x'f09f8c80'", mysql.ErrTruncatedWrongValueForField)
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(20) DEFAULT NULL,\n" +
		"  `b` varchar(30) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"))

	// Test for alter table convert charset
	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = true
	s.tk.MustExec("alter table t drop column b") // reload schema.
	s.tk.MustExec("alter table t convert to charset utf8mb4;")

	config.GetGlobalConfig().TreatOldVersionUTF8AsUTF8MB4 = false
	s.tk.MustExec("alter table t add column b varchar(50);") // reload schema.
	s.tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `a` varchar(20) DEFAULT NULL,\n" +
		"  `b` varchar(50) DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))
}

func (s *testIntegrationSuite3) TestDefaultValueIsString(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("drop table if exists t")
	defer s.tk.MustExec("drop table if exists t")
	s.tk.MustExec("create table t (a int default b'1');")
	tbl := testGetTableByName(c, s.ctx, "test", "t")
	c.Assert(tbl.Meta().Columns[0].DefaultValue, Equals, "1")
}

func (s *testIntegrationSuite11) TestChangingDBCharset(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("DROP DATABASE IF EXISTS alterdb1")
	tk.MustExec("CREATE DATABASE alterdb1 CHARSET=utf8 COLLATE=utf8_unicode_ci")

	// No default DB errors.
	noDBFailedCases := []struct {
		stmt   string
		errMsg string
	}{
		{
			"ALTER DATABASE CHARACTER SET = 'utf8'",
			"[planner:1046]No database selected",
		},
		{
			"ALTER SCHEMA `` CHARACTER SET = 'utf8'",
			"[ddl:1102]Incorrect database name ''",
		},
	}
	for _, fc := range noDBFailedCases {
		c.Assert(tk.ExecToErr(fc.stmt).Error(), Equals, fc.errMsg, Commentf("%v", fc.stmt))
	}

	verifyDBCharsetAndCollate := func(dbName, chs string, coll string) {
		// check `SHOW CREATE SCHEMA`.
		r := tk.MustQuery("SHOW CREATE SCHEMA " + dbName).Rows()[0][1].(string)
		c.Assert(strings.Contains(r, "CHARACTER SET "+chs), IsTrue)

		template := `SELECT
					DEFAULT_CHARACTER_SET_NAME,
					DEFAULT_COLLATION_NAME
				FROM INFORMATION_SCHEMA.SCHEMATA
				WHERE SCHEMA_NAME = '%s'`
		sql := fmt.Sprintf(template, dbName)
		tk.MustQuery(sql).Check(testkit.Rows(fmt.Sprintf("%s %s", chs, coll)))

		dom := domain.GetDomain(s.ctx)
		// Make sure the table schema is the new schema.
		err := dom.Reload()
		c.Assert(err, IsNil)
		dbInfo, ok := dom.InfoSchema().SchemaByName(model.NewCIStr(dbName))
		c.Assert(ok, Equals, true)
		c.Assert(dbInfo.Charset, Equals, chs)
		c.Assert(dbInfo.Collate, Equals, coll)
	}

	tk.MustExec("ALTER SCHEMA alterdb1 COLLATE = utf8mb4_general_ci")
	verifyDBCharsetAndCollate("alterdb1", "utf8mb4", "utf8mb4_general_ci")

	tk.MustExec("DROP DATABASE IF EXISTS alterdb2")
	tk.MustExec("CREATE DATABASE alterdb2 CHARSET=utf8 COLLATE=utf8_unicode_ci")
	tk.MustExec("USE alterdb2")

	failedCases := []struct {
		stmt   string
		errMsg string
	}{
		{
			"ALTER SCHEMA `` CHARACTER SET = 'utf8'",
			"[ddl:1102]Incorrect database name ''",
		},
		{
			"ALTER DATABASE CHARACTER SET = ''",
			"[parser:1115]Unknown character set: ''",
		},
		{
			"ALTER DATABASE CHARACTER SET = 'INVALID_CHARSET'",
			"[parser:1115]Unknown character set: 'INVALID_CHARSET'",
		},
		{
			"ALTER SCHEMA COLLATE = ''",
			"[ddl:1273]Unknown collation: ''",
		},
		{
			"ALTER DATABASE COLLATE = 'INVALID_COLLATION'",
			"[ddl:1273]Unknown collation: 'INVALID_COLLATION'",
		},
		{
			"ALTER DATABASE CHARACTER SET = 'utf8' DEFAULT CHARSET = 'utf8mb4'",
			"[ddl:1302]Conflicting declarations: 'CHARACTER SET utf8' and 'CHARACTER SET utf8mb4'",
		},
		{
			"ALTER SCHEMA CHARACTER SET = 'utf8' COLLATE = 'utf8mb4_bin'",
			"[ddl:1302]Conflicting declarations: 'CHARACTER SET utf8' and 'CHARACTER SET utf8mb4'",
		},
		{
			"ALTER DATABASE COLLATE = 'utf8mb4_bin' COLLATE = 'utf8_bin'",
			"[ddl:1302]Conflicting declarations: 'CHARACTER SET utf8mb4' and 'CHARACTER SET utf8'",
		},
	}

	for _, fc := range failedCases {
		c.Assert(tk.ExecToErr(fc.stmt).Error(), Equals, fc.errMsg, Commentf("%v", fc.stmt))
	}
	tk.MustExec("ALTER SCHEMA CHARACTER SET = 'utf8' COLLATE = 'utf8_unicode_ci'")
	verifyDBCharsetAndCollate("alterdb2", "utf8", "utf8_unicode_ci")

	tk.MustExec("ALTER SCHEMA CHARACTER SET = 'utf8mb4'")
	verifyDBCharsetAndCollate("alterdb2", "utf8mb4", "utf8mb4_bin")

	tk.MustExec("ALTER SCHEMA CHARACTER SET = 'utf8mb4' COLLATE = 'utf8mb4_general_ci'")
	verifyDBCharsetAndCollate("alterdb2", "utf8mb4", "utf8mb4_general_ci")
}
