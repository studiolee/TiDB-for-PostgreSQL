// Copyright 2018 PingCAP, Inc.
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

package executor_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/DigitalChinaOpenSource/DCParser/terror"
	. "github.com/pingcap/check"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/testkit"
)

type testPointGetSuite struct {
	store kv.Storage
	dom   *domain.Domain
	cli   *checkRequestClient
}

func (s *testPointGetSuite) SetUpSuite(c *C) {
	cli := &checkRequestClient{}
	hijackClient := func(c tikv.Client) tikv.Client {
		cli.Client = c
		return cli
	}
	s.cli = cli

	var err error
	s.store, err = mockstore.NewMockTikvStore(
		mockstore.WithHijackClient(hijackClient),
	)
	c.Assert(err, IsNil)
	s.dom, err = session.BootstrapSession(s.store)
	c.Assert(err, IsNil)
	s.dom.SetStatsUpdating(true)
}

func (s *testPointGetSuite) TearDownSuite(c *C) {
	s.dom.Close()
	s.store.Close()
}

func (s *testPointGetSuite) TearDownTest(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	r := tk.MustQuery("show tables")
	for _, tb := range r.Rows() {
		tableName := tb[0]
		tk.MustExec(fmt.Sprintf("drop table %v", tableName))
	}
}

func (s *testPointGetSuite) TestPointGet(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table point (id int primary key, c int, d varchar(10), unique c_d (c, d))")
	tk.MustExec("insert point values (1, 1, 'a')")
	tk.MustExec("insert point values (2, 2, 'b')")
	tk.MustQuery("select * from point where id = 1 and c = 0").Check(testkit.Rows())
	tk.MustQuery("select * from point where id < 0 and c = 1 and d = 'b'").Check(testkit.Rows())
	result, err := tk.Exec("select id as ident from point where id = 1")
	c.Assert(err, IsNil)
	fields := result.Fields()
	c.Assert(fields[0].ColumnAsName.O, Equals, "ident")
	result.Close()

	tk.MustExec("CREATE TABLE tab3(pk INTEGER PRIMARY KEY, col0 INTEGER, col1 FLOAT, col2 TEXT, col3 INTEGER, col4 FLOAT, col5 TEXT);")
	tk.MustExec("CREATE UNIQUE INDEX idx_tab3_0 ON tab3 (col4);")
	tk.MustExec("INSERT INTO tab3 VALUES(0,854,111.96,'mguub',711,966.36,'snwlo');")
	tk.MustQuery("SELECT ALL * FROM tab3 WHERE col4 = 85;").Check(testkit.Rows())

	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a bigint primary key, b bigint, c bigint);`)
	tk.MustExec(`insert into t values(1, NULL, NULL), (2, NULL, 2), (3, 3, NULL), (4, 4, 4), (5, 6, 7);`)
	tk.MustQuery(`select * from t where a = 1;`).Check(testkit.Rows(
		`1 <nil> <nil>`,
	))
	tk.MustQuery(`select * from t where a = 2;`).Check(testkit.Rows(
		`2 <nil> 2`,
	))
	tk.MustQuery(`select * from t where a = 3;`).Check(testkit.Rows(
		`3 3 <nil>`,
	))
	tk.MustQuery(`select * from t where a = 4;`).Check(testkit.Rows(
		`4 4 4`,
	))
	tk.MustQuery(`select a, a, b, a, b, c, b, c, c from t where a = 5;`).Check(testkit.Rows(
		`5 5 6 5 6 7 6 7 7`,
	))
	tk.MustQuery(`select b, b from t where a = 1`).Check(testkit.Rows(
		"<nil> <nil>"))
}

func (s *testPointGetSuite) TestPointGetOverflow(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t0")
	tk.MustExec("CREATE TABLE t0(c1 BOOL UNIQUE)")
	tk.MustExec("INSERT INTO t0(c1) VALUES (-128)")
	tk.MustExec("INSERT INTO t0(c1) VALUES (127)")
	tk.MustQuery("SELECT t0.c1 FROM t0 WHERE t0.c1=-129").Check(testkit.Rows()) // no result
	tk.MustQuery("SELECT t0.c1 FROM t0 WHERE t0.c1=-128").Check(testkit.Rows("-128"))
	tk.MustQuery("SELECT t0.c1 FROM t0 WHERE t0.c1=128").Check(testkit.Rows())
	tk.MustQuery("SELECT t0.c1 FROM t0 WHERE t0.c1=127").Check(testkit.Rows("127"))
}

func (s *testPointGetSuite) TestPointGetCharPK(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(4) primary key, b char(4));`)
	tk.MustExec(`insert into t values("aa", "bb");`)

	// Test CHAR type.
	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustPointGet(`select * from t where a = "aab";`).Check(testkit.Rows())

	tk.MustExec(`truncate table t;`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows(`a b`))
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a  ";`).Check(testkit.Rows())

	// Test CHAR BINARY.
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(2) binary primary key, b char(2));`)
	tk.MustExec(`insert into t values("  ", "  ");`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows(`a b`))
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows())
	tk.MustTableDual(`select * from t where a = "a  ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "";`).Check(testkit.Rows(` `))
	tk.MustPointGet(`select * from t where a = "  ";`).Check(testkit.Rows())
	tk.MustTableDual(`select * from t where a = "   ";`).Check(testkit.Rows())

}

func (s *testPointGetSuite) TestPointGetAliasTableCharPK(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(2) primary key, b char(2));`)
	tk.MustExec(`insert into t values("aa", "bb");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t tmp where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustTableDual(`select * from t tmp where a = "aab";`).Check(testkit.Rows())

	tk.MustExec(`truncate table t;`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t tmp where a = "a";`).Check(testkit.Rows(`a b`))
	tk.MustPointGet(`select * from t tmp where a = "a ";`).Check(testkit.Rows())
	tk.MustTableDual(`select * from t tmp where a = "a  ";`).Check(testkit.Rows())

	// Test CHAR BINARY.
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(2) binary primary key, b char(2));`)
	tk.MustExec(`insert into t values("  ", "  ");`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t tmp where a = "a";`).Check(testkit.Rows(`a b`))
	tk.MustPointGet(`select * from t tmp where a = "a ";`).Check(testkit.Rows())
	tk.MustTableDual(`select * from t tmp where a = "a  ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t tmp where a = "";`).Check(testkit.Rows(` `))
	tk.MustPointGet(`select * from t tmp where a = "  ";`).Check(testkit.Rows())
	tk.MustTableDual(`select * from t tmp where a = "   ";`).Check(testkit.Rows())

	// Test both wildcard and column name exist in select field list
	tk.MustExec(`set @@sql_mode="";`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(2) primary key, b char(2));`)
	tk.MustExec(`insert into t values("aa", "bb");`)
	tk.MustPointGet(`select *, a from t tmp where a = "aa";`).Check(testkit.Rows(`aa bb aa`))

	// Test using table alias in field list
	tk.MustPointGet(`select tmp.* from t tmp where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustPointGet(`select tmp.a, tmp.b from t tmp where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustPointGet(`select tmp.*, tmp.a, tmp.b from t tmp where a = "aa";`).Check(testkit.Rows(`aa bb aa bb`))
	tk.MustTableDual(`select tmp.* from t tmp where a = "aab";`).Check(testkit.Rows())
	tk.MustTableDual(`select tmp.a, tmp.b from t tmp where a = "aab";`).Check(testkit.Rows())
	tk.MustTableDual(`select tmp.*, tmp.a, tmp.b from t tmp where a = "aab";`).Check(testkit.Rows())

	// Test using table alias in where clause
	tk.MustPointGet(`select * from t tmp where tmp.a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustPointGet(`select a, b from t tmp where tmp.a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustPointGet(`select *, a, b from t tmp where tmp.a = "aa";`).Check(testkit.Rows(`aa bb aa bb`))

	// Unknown table name in where clause and field list
	err := tk.ExecToErr(`select a from t where xxxxx.a = "aa"`)
	c.Assert(err, ErrorMatches, ".*Unknown column 'xxxxx.a' in 'where clause'")
	err = tk.ExecToErr(`select xxxxx.a from t where a = "aa"`)
	c.Assert(err, ErrorMatches, ".*Unknown column 'xxxxx.a' in 'field list'")

	// When an alias is provided, it completely hides the actual name of the table.
	err = tk.ExecToErr(`select a from t tmp where t.a = "aa"`)
	c.Assert(err, ErrorMatches, ".*Unknown column 't.a' in 'where clause'")
	err = tk.ExecToErr(`select t.a from t tmp where a = "aa"`)
	c.Assert(err, ErrorMatches, ".*Unknown column 't.a' in 'field list'")
	err = tk.ExecToErr(`select t.* from t tmp where a = "aa"`)
	c.Assert(err, ErrorMatches, ".*Unknown table 't'")
}

func (s *testPointGetSuite) TestIndexLookupChar(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(2), b char(2), index idx_1(a));`)
	tk.MustExec(`insert into t values("aa", "bb");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustIndexLookup(`select * from t where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustIndexLookup(`select * from t where a = "aab";`).Check(testkit.Rows())

	// Test query with table alias
	tk.MustIndexLookup(`select * from t tmp where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustIndexLookup(`select * from t tmp where a = "aab";`).Check(testkit.Rows())

	tk.MustExec(`truncate table t;`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustIndexLookup(`select * from t where a = "a";`).Check(testkit.Rows(`a b`))
	tk.MustIndexLookup(`select * from t where a = "a ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a  ";`).Check(testkit.Rows())

	// Test CHAR BINARY.
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a char(2) binary, b char(2), index idx_1(a));`)
	tk.MustExec(`insert into t values("  ", "  ");`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustIndexLookup(`select * from t where a = "a";`).Check(testkit.Rows(`a b`))
	tk.MustIndexLookup(`select * from t where a = "a ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a  ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "";`).Check(testkit.Rows(` `))
	tk.MustIndexLookup(`select * from t where a = " ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "  ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "   ";`).Check(testkit.Rows())

}

func (s *testPointGetSuite) TestPointGetVarcharPK(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a varchar(2) primary key, b varchar(2));`)
	tk.MustExec(`insert into t values("aa", "bb");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "aa";`).Check(testkit.Rows(`aa bb`))
	tk.MustTableDual(`select * from t where a = "aab";`).Check(testkit.Rows())

	tk.MustExec(`truncate table t;`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustTableDual(`select * from t where a = "a  ";`).Check(testkit.Rows())

	// // Test VARCHAR BINARY.
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a varchar(2) binary primary key, b varchar(2));`)
	tk.MustExec(`insert into t values("  ", "  ");`)
	tk.MustExec(`insert into t values("a ", "b ");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustTableDual(`select * from t where a = "a  ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = " ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "  ";`).Check(testkit.Rows(`     `))
	tk.MustTableDual(`select * from t where a = "   ";`).Check(testkit.Rows())

}

func (s *testPointGetSuite) TestPointGetBinaryPK(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a binary(2) primary key, b binary(2));`)
	tk.MustExec(`insert into t values("a", "b");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a  ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a\0";`).Check(testkit.Rows("a\x00 b\x00"))

	tk.MustExec(`insert into t values("a ", "b ");`)
	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustPointGet(`select * from t where a = "a  ";`).Check(testkit.Rows())

	tk.MustPointGet(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustPointGet(`select * from t where a = "a  ";`).Check(testkit.Rows())
}

func (s *testPointGetSuite) TestPointGetAliasTableBinaryPK(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a binary(2) primary key, b binary(2));`)
	tk.MustExec(`insert into t values("a", "b");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustPointGet(`select * from t tmp where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t tmp where a = "a ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t tmp where a = "a  ";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t tmp where a = "a\0";`).Check(testkit.Rows("a\x00 b\x00"))

	tk.MustExec(`insert into t values("a ", "b ");`)
	tk.MustPointGet(`select * from t tmp where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t tmp where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustPointGet(`select * from t tmp where a = "a  ";`).Check(testkit.Rows())

	tk.MustPointGet(`select * from t tmp where a = "a";`).Check(testkit.Rows())
	tk.MustPointGet(`select * from t tmp where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustPointGet(`select * from t tmp where a = "a  ";`).Check(testkit.Rows())
}

func (s *testPointGetSuite) TestIndexLookupBinary(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t(a binary(2), b binary(2), index idx_1(a));`)
	tk.MustExec(`insert into t values("a", "b");`)

	tk.MustExec(`set @@sql_mode="";`)
	tk.MustIndexLookup(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a  ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a\0";`).Check(testkit.Rows("a\x00 b\x00"))

	// Test query with table alias
	tk.MustExec(`set @@sql_mode="";`)
	tk.MustIndexLookup(`select * from t tmp where a = "a";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t tmp where a = "a ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t tmp where a = "a  ";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t tmp where a = "a\0";`).Check(testkit.Rows("a\x00 b\x00"))

	tk.MustExec(`insert into t values("a ", "b ");`)
	tk.MustIndexLookup(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustIndexLookup(`select * from t where a = "a  ";`).Check(testkit.Rows())

	tk.MustIndexLookup(`select * from t where a = "a";`).Check(testkit.Rows())
	tk.MustIndexLookup(`select * from t where a = "a ";`).Check(testkit.Rows(`a  b `))
	tk.MustIndexLookup(`select * from t where a = "a  ";`).Check(testkit.Rows())

}

func (s *testPointGetSuite) TestOverflowOrTruncated(c *C) {
	tk := testkit.NewTestKitWithInit(c, s.store)
	tk.MustExec("create table t6 (id bigint, a bigint, primary key(id), unique key(a));")
	tk.MustExec("insert into t6 values(9223372036854775807, 9223372036854775807);")
	tk.MustExec("insert into t6 values(1, 1);")
	var nilVal []string
	// for unique key
	tk.MustQuery("select * from t6 where a = 9223372036854775808").Check(testkit.Rows(nilVal...))
	tk.MustQuery("select * from t6 where a = '1.123'").Check(testkit.Rows(nilVal...))
	// for primary key
	tk.MustQuery("select * from t6 where id = 9223372036854775808").Check(testkit.Rows(nilVal...))
	tk.MustQuery("select * from t6 where id = '1.123'").Check(testkit.Rows(nilVal...))
}

func (s *testPointGetSuite) TestIssue10448(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(pk int1 primary key)")
	tk.MustExec("insert into t values(125)")
	tk.MustQuery("desc select * from t where pk = 9223372036854775807").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551616").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775808").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551615").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 128").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(pk int8 primary key)")
	tk.MustExec("insert into t values(9223372036854775807)")
	tk.MustQuery("select * from t where pk = 9223372036854775807").Check(testkit.Rows("9223372036854775807"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775807").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:9223372036854775807"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551616").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775808").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551615").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(pk int1 unsigned primary key)")
	tk.MustExec("insert into t values(255)")
	tk.MustQuery("select * from t where pk = 255").Check(testkit.Rows("255"))
	tk.MustQuery("desc select * from t where pk = 256").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775807").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551616").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775808").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551615").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))

	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(pk int8 unsigned primary key)")
	tk.MustExec("insert into t value(18446744073709551615)")
	tk.MustQuery("desc select * from t where pk = 18446744073709551615").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:18446744073709551615"))
	tk.MustQuery("select * from t where pk = 18446744073709551615").Check(testkit.Rows("18446744073709551615"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775807").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:9223372036854775807"))
	tk.MustQuery("desc select * from t where pk = 18446744073709551616").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("desc select * from t where pk = 9223372036854775808").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:9223372036854775808"))
}

func (s *testPointGetSuite) TestIssue10677(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(pk int1 primary key)")
	tk.MustExec("insert into t values(1)")
	tk.MustQuery("desc select * from t where pk = 1.1").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("select * from t where pk = 1.1").Check(testkit.Rows())
	tk.MustQuery("desc select * from t where pk = '1.1'").Check(testkit.Rows("TableDual_2 0.00 root  rows:0"))
	tk.MustQuery("select * from t where pk = '1.1'").Check(testkit.Rows())
	tk.MustQuery("desc select * from t where pk = 1").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:1"))
	tk.MustQuery("select * from t where pk = 1").Check(testkit.Rows("1"))
	tk.MustQuery("desc select * from t where pk = '1'").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:1"))
	tk.MustQuery("select * from t where pk = '1'").Check(testkit.Rows("1"))
	tk.MustQuery("desc select * from t where pk = '1.0'").Check(testkit.Rows("Point_Get_1 1.00 root table:t handle:1"))
	tk.MustQuery("select * from t where pk = '1.0'").Check(testkit.Rows("1"))
}

func (s *testPointGetSuite) TestForUpdateRetry(c *C) {
	tk := testkit.NewTestKitWithInit(c, s.store)
	tk.Exec("drop table if exists t")
	tk.MustExec("create table t(pk int primary key, c int)")
	tk.MustExec("insert into t values (1, 1), (2, 2)")
	tk.MustExec("set @@tidb_disable_txn_auto_retry = 0")
	tk.MustExec("begin")
	tk.MustQuery("select * from t where pk = 1 for update")
	tk2 := testkit.NewTestKitWithInit(c, s.store)
	tk2.MustExec("update t set c = c + 1 where pk = 1")
	tk.MustExec("update t set c = c + 1 where pk = 2")
	_, err := tk.Exec("commit")
	c.Assert(session.ErrForUpdateCantRetry.Equal(err), IsTrue)
}

func (s *testPointGetSuite) TestPointGetByRowID(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a varchar(20), b int)")
	tk.MustExec("insert into t values(\"aaa\", 12)")
	tk.MustQuery("explain select * from t where t._tidb_rowid = 1").Check(testkit.Rows(
		"Point_Get_1 1.00 root table:t handle:1"))
	tk.MustQuery("select * from t where t._tidb_rowid = 1").Check(testkit.Rows("aaa 12"))
}

func (s *testPointGetSuite) TestSelectCheckVisibility(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a varchar(10) key, b int,index idx(b))")
	tk.MustExec("insert into t values('1',1)")
	tk.MustExec("begin")
	txn, err := tk.Se.Txn(false)
	c.Assert(err, IsNil)
	ts := txn.StartTS()
	store := tk.Se.GetStore().(tikv.Storage)
	// Update gc safe time for check data visibility.
	store.UpdateSPCache(ts+1, time.Now())
	checkSelectResultError := func(sql string, expectErr *terror.Error) {
		re, err := tk.Exec(sql)
		c.Assert(err, IsNil)
		_, err = session.ResultSetToStringSlice(context.Background(), tk.Se, re)
		c.Assert(err, NotNil)
		c.Assert(expectErr.Equal(err), IsTrue)
	}
	// Test point get.
	checkSelectResultError("select * from t where a='1'", tikv.ErrGCTooEarly)
	// Test batch point get.
	checkSelectResultError("select * from t where a in ('1','2')", tikv.ErrGCTooEarly)
	// Test Index look up read.
	checkSelectResultError("select * from t where b > 0 ", tikv.ErrGCTooEarly)
	// Test Index read.
	checkSelectResultError("select b from t where b > 0 ", tikv.ErrGCTooEarly)
	// Test table read.
	checkSelectResultError("select * from t", tikv.ErrGCTooEarly)
}

func (s *testPointGetSuite) TestNullValues(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t ( id bigint(10) primary key, f varchar(191) default null, unique key `idx_f` (`f`))")
	tk.MustExec(`insert into t values (1, "")`)
	rs := tk.MustQuery(`select * from t where f in (null)`).Rows()
	c.Assert(len(rs), Equals, 0)
}

func (s *testPointGetSuite) TestReturnValues(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a varchar(64) primary key, b int)")
	tk.MustExec("insert t values ('a', 1), ('b', 2), ('c', 3)")
	tk.MustExec("begin pessimistic")
	tk.MustQuery("select * from t where a = 'b' for update").Check(testkit.Rows("b 2"))
	tid := tk.GetTableID("t")
	idxVal, err := codec.EncodeKey(tk.Se.GetSessionVars().StmtCtx, nil, types.NewStringDatum("b"))
	c.Assert(err, IsNil)
	pk := tablecodec.EncodeIndexSeekKey(tid, 1, idxVal)
	txnCtx := tk.Se.GetSessionVars().TxnCtx
	val, ok := txnCtx.GetKeyInPessimisticLockCache(pk)
	c.Assert(ok, IsTrue)
	handle, err := tablecodec.DecodeHandle(val)
	c.Assert(err, IsNil)
	rowKey := tablecodec.EncodeRowKeyWithHandle(tid, handle)
	_, ok = txnCtx.GetKeyInPessimisticLockCache(rowKey)
	c.Assert(ok, IsTrue)
	tk.MustExec("rollback")
}

func (s *testPointGetSuite) TestWithTiDBSnapshot(c *C) {
	// Fix issue https://github.com/pingcap/tidb/issues/22436
	// Point get should not use math.MaxUint64 when variable @@tidb_snapshot is set.
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists xx")
	tk.MustExec(`create table xx (id int key)`)
	tk.MustExec(`insert into xx values (1), (7)`)

	// Unrelated code, to make this test pass in the unit test.
	// The `tikv_gc_safe_point` global variable must be there, otherwise the 'set @@tidb_snapshot' operation fails.
	timeSafe := time.Now().Add(-48 * 60 * 60 * time.Second).Format("20060102-15:04:05 -0700 MST")
	safePointSQL := `INSERT HIGH_PRIORITY INTO mysql.tidb VALUES ('tikv_gc_safe_point', '%[1]s', '')
			       ON DUPLICATE KEY
			       UPDATE variable_value = '%[1]s'`
	tk.MustExec(fmt.Sprintf(safePointSQL, timeSafe))

	// Record the current tso.
	tk.MustExec("begin")
	tso := tk.Se.GetSessionVars().TxnCtx.StartTS
	tk.MustExec("rollback")
	c.Assert(tso > 0, IsTrue)

	// Insert data.
	tk.MustExec("insert into xx values (8)")

	// Change the snapshot before the tso, the inserted data should not be seen.
	tk.MustExec(fmt.Sprintf("set @@tidb_snapshot = '%d'", tso))
	tk.MustQuery("select * from xx where id = 8").Check(testkit.Rows())

	tk.MustQuery("select * from xx").Check(testkit.Rows("1", "7"))

	// Check the query inside a transaction.
	tk.MustExec("begin")
	tk.MustQuery("select * from xx where id = 8").Check(testkit.Rows())
	tk.MustExec("rollback")
}

func (s *testPointGetSuite) TestPointGetLockExistKey(c *C) {
	var wg sync.WaitGroup
	errCh := make(chan error)

	testLock := func(rc bool, key string, tableName string) {
		doneCh := make(chan struct{}, 1)
		tk1, tk2 := testkit.NewTestKit(c, s.store), testkit.NewTestKit(c, s.store)

		errCh <- tk1.ExecToErr("use test")
		errCh <- tk2.ExecToErr("use test")

		errCh <- tk1.ExecToErr(fmt.Sprintf("drop table if exists %s", tableName))
		errCh <- tk1.ExecToErr(fmt.Sprintf("create table %s(id int, v int, k int, %s key0(id, v))", tableName, key))
		errCh <- tk1.ExecToErr(fmt.Sprintf("insert into %s values(1, 1, 1)", tableName))

		if rc {
			errCh <- tk1.ExecToErr("set tx_isolation = 'READ-COMMITTED'")
			errCh <- tk2.ExecToErr("set tx_isolation = 'READ-COMMITTED'")
		}

		// select for update
		errCh <- tk1.ExecToErr("begin pessimistic")
		errCh <- tk2.ExecToErr("begin pessimistic")
		// lock exist key
		errCh <- tk1.ExecToErr(fmt.Sprintf("select * from %s where id = 1 and v = 1 for update", tableName))
		// read committed will not lock non-exist key
		if rc {
			errCh <- tk1.ExecToErr(fmt.Sprintf("select * from %s where id = 2 and v = 2 for update", tableName))
		}
		errCh <- tk2.ExecToErr(fmt.Sprintf("insert into %s values(2, 2, 2)", tableName))
		go func() {
			errCh <- tk2.ExecToErr(fmt.Sprintf("insert into %s values(1, 1, 10)", tableName))
			//tk2.MustExec(fmt.Sprintf("insert into %s values(1, 1, 10)", tableName))
			doneCh <- struct{}{}
		}()
		time.Sleep(150 * time.Millisecond)
		errCh <- tk1.ExecToErr(fmt.Sprintf("update %s set v = 2 where id = 1 and v = 1", tableName))
		errCh <- tk1.ExecToErr("commit")
		<-doneCh
		errCh <- tk2.ExecToErr("commit")
		tk1.MustQuery(fmt.Sprintf("select * from %s", tableName)).Check(testkit.Rows(
			"1 2 1",
			"2 2 2",
			"1 1 10",
		))

		// update
		errCh <- tk1.ExecToErr("begin pessimistic")
		errCh <- tk2.ExecToErr("begin pessimistic")
		// lock exist key
		errCh <- tk1.ExecToErr(fmt.Sprintf("update %s set v = 3 where id = 2 and v = 2", tableName))
		// read committed will not lock non-exist key
		if rc {
			errCh <- tk1.ExecToErr(fmt.Sprintf("update %s set v =4 where id = 3 and v = 3", tableName))
		}
		errCh <- tk2.ExecToErr(fmt.Sprintf("insert into %s values(3, 3, 3)", tableName))
		go func() {
			errCh <- tk2.ExecToErr(fmt.Sprintf("insert into %s values(2, 2, 20)", tableName))
			doneCh <- struct{}{}
		}()
		time.Sleep(150 * time.Millisecond)
		errCh <- tk1.ExecToErr("commit")
		<-doneCh
		errCh <- tk2.ExecToErr("commit")
		tk1.MustQuery(fmt.Sprintf("select * from %s", tableName)).Check(testkit.Rows(
			"1 2 1",
			"2 3 2",
			"1 1 10",
			"3 3 3",
			"2 2 20",
		))

		// delete
		errCh <- tk1.ExecToErr("begin pessimistic")
		errCh <- tk2.ExecToErr("begin pessimistic")
		// lock exist key
		errCh <- tk1.ExecToErr(fmt.Sprintf("delete from %s where id = 3 and v = 3", tableName))
		// read committed will not lock non-exist key
		if rc {
			errCh <- tk1.ExecToErr(fmt.Sprintf("delete from %s where id = 4 and v = 4", tableName))
		}
		errCh <- tk2.ExecToErr(fmt.Sprintf("insert into %s values(4, 4, 4)", tableName))
		go func() {
			errCh <- tk2.ExecToErr(fmt.Sprintf("insert into %s values(3, 3, 30)", tableName))
			doneCh <- struct{}{}
		}()
		time.Sleep(50 * time.Millisecond)
		errCh <- tk1.ExecToErr("commit")
		<-doneCh
		errCh <- tk2.ExecToErr("commit")
		tk1.MustQuery(fmt.Sprintf("select * from %s", tableName)).Check(testkit.Rows(
			"1 2 1",
			"2 3 2",
			"1 1 10",
			"2 2 20",
			"4 4 4",
			"3 3 30",
		))
		wg.Done()
	}

	for i, one := range []struct {
		rc  bool
		key string
	}{
		{rc: false, key: "primary key"},
		{rc: false, key: "unique key"},
		{rc: true, key: "primary key"},
		{rc: true, key: "unique key"},
	} {
		wg.Add(1)
		tableName := fmt.Sprintf("t_%d", i)
		go testLock(one.rc, one.key, tableName)
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()
	for err := range errCh {
		c.Assert(err, IsNil)
	}
}
