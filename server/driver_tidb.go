// Copyright 2015 PingCAP, Inc.
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

// Copyright 2021 Digital China Group Co.,Ltd
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

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/DigitalChinaOpenSource/DCParser/ast"
	"github.com/DigitalChinaOpenSource/DCParser/auth"
	"github.com/DigitalChinaOpenSource/DCParser/charset"
	"github.com/DigitalChinaOpenSource/DCParser/mysql"
	"github.com/DigitalChinaOpenSource/DCParser/terror"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/sqlexec"
)

// TiDBDriver implements IDriver.
type TiDBDriver struct {
	store kv.Storage
}

// NewTiDBDriver creates a new TiDBDriver.
func NewTiDBDriver(store kv.Storage) *TiDBDriver {
	driver := &TiDBDriver{
		store: store,
	}
	return driver
}

// TiDBContext implements QueryCtx.
type TiDBContext struct {
	session   session.Session
	currentDB string
	stmts     map[int]*TiDBStatement
}

// TiDBStatement implements PreparedStatement.
// PGSQL Modified
type TiDBStatement struct {
	id           uint32
	numParams    int
	boundParams  [][]byte
	paramsType   []byte
	ctx          *TiDBContext
	rs           ResultSet
	sql          string
	columnInfo   []*ColumnInfo
	args         []types.Datum
	resultFormat []int16
}

// ID implements PreparedStatement ID method.
func (ts *TiDBStatement) ID() int {
	return int(ts.id)
}

// Execute implements PreparedStatement Execute method.
func (ts *TiDBStatement) Execute(ctx context.Context, args []types.Datum) (rs ResultSet, err error) {
	tidbRecordset, err := ts.ctx.session.ExecutePreparedStmt(ctx, ts.id, args)
	if err != nil {
		return nil, err
	}
	if tidbRecordset == nil {
		return
	}
	rs = &tidbResultSet{
		recordSet:    tidbRecordset,
		preparedStmt: ts.ctx.GetSessionVars().PreparedStmts[ts.id].(*core.CachedPrepareStmt),
	}
	return
}

// AppendParam implements PreparedStatement AppendParam method.
func (ts *TiDBStatement) AppendParam(paramID int, data []byte) error {
	if paramID >= len(ts.boundParams) {
		return mysql.NewErr(mysql.ErrWrongArguments, "stmt_send_longdata")
	}
	// If len(data) is 0, append an empty byte slice to the end to distinguish no data and no parameter.
	if len(data) == 0 {
		ts.boundParams[paramID] = []byte{}
	} else {
		ts.boundParams[paramID] = append(ts.boundParams[paramID], data...)
	}
	return nil
}

// NumParams implements PreparedStatement NumParams method.
func (ts *TiDBStatement) NumParams() int {
	return ts.numParams
}

// BoundParams implements PreparedStatement BoundParams method.
func (ts *TiDBStatement) BoundParams() [][]byte {
	return ts.boundParams
}

// SetParamsType implements PreparedStatement SetParamsType method.
func (ts *TiDBStatement) SetParamsType(paramsType []byte) {
	ts.paramsType = paramsType
}

// GetParamsType implements PreparedStatement GetParamsType method.
func (ts *TiDBStatement) GetParamsType() []byte {
	return ts.paramsType
}

// StoreResultSet stores ResultSet for stmt fetching
func (ts *TiDBStatement) StoreResultSet(rs ResultSet) {
	// refer to https://dev.mysql.com/doc/refman/5.7/en/cursor-restrictions.html
	// You can have open only a single cursor per prepared statement.
	// closing previous ResultSet before associating a new ResultSet with this statement
	// if it exists
	if ts.rs != nil {
		terror.Call(ts.rs.Close)
	}
	ts.rs = rs
}

// GetResultSet gets ResultSet associated this statement
func (ts *TiDBStatement) GetResultSet() ResultSet {
	return ts.rs
}

// Reset implements PreparedStatement Reset method.
func (ts *TiDBStatement) Reset() {
	for i := range ts.boundParams {
		ts.boundParams[i] = nil
	}

	// closing previous ResultSet if it exists
	if ts.rs != nil {
		terror.Call(ts.rs.Close)
		ts.rs = nil
	}
}

// Close implements PreparedStatement Close method.
func (ts *TiDBStatement) Close() error {
	//TODO close at tidb level
	if ts.ctx.GetSessionVars().TxnCtx != nil && ts.ctx.GetSessionVars().TxnCtx.CouldRetry {
		err := ts.ctx.session.DropPreparedStmt(ts.id)
		if err != nil {
			return err
		}
	} else {
		if core.PreparedPlanCacheEnabled() {
			preparedPointer := ts.ctx.GetSessionVars().PreparedStmts[ts.id]
			preparedObj, ok := preparedPointer.(*core.CachedPrepareStmt)
			if !ok {
				return errors.Errorf("invalid CachedPrepareStmt type")
			}
			ts.ctx.session.PreparedPlanCache().Delete(core.NewPSTMTPlanCacheKey(
				ts.ctx.GetSessionVars(), ts.id, preparedObj.PreparedAst.SchemaVersion))
		}
		ts.ctx.GetSessionVars().RemovePreparedStmt(ts.id)
	}
	delete(ts.ctx.stmts, int(ts.id))

	// close ResultSet associated with this statement
	if ts.rs != nil {
		terror.Call(ts.rs.Close)
	}
	return nil
}

// SetColumnInfo 设置 TiDBStatement 中返回行数据信息
// 如果有返回数据的语句，则存储返回数据的结构，如果没有则为空
// PGSQL Modified
func (ts *TiDBStatement) SetColumnInfo(columns []*ColumnInfo) {
	ts.columnInfo = columns
}

// GetColumnInfo 获取 TiDBStatement 中返回行数据信息
// PGSQL Modified
func (ts *TiDBStatement) GetColumnInfo() []*ColumnInfo {
	return ts.columnInfo
}

// GetArgs 获取绑定后的参数值
func (ts *TiDBStatement) GetArgs() []types.Datum {
	return ts.args
}

// SetArgs 进行参数绑定值
func (ts *TiDBStatement) SetArgs(args []types.Datum) {
	ts.args = args
}

// GetResultFormat 获取结果返回的格式 0 为 Text, 1 为 Binary
func (ts *TiDBStatement) GetResultFormat() []int16 {
	return ts.resultFormat
}

// SetResultFormat 设置结果返回的格式 0 为 Text, 1 为 Binary
func (ts *TiDBStatement) SetResultFormat(rf []int16) {
	ts.resultFormat = rf
}

// OpenCtx implements IDriver.
func (qd *TiDBDriver) OpenCtx(connID uint64, capability uint32, collation uint8, dbname string, tlsState *tls.ConnectionState) (QueryCtx, error) {
	se, err := session.CreateSession(qd.store)
	if err != nil {
		return nil, err
	}
	se.SetTLSState(tlsState)
	err = se.SetCollation(int(collation))
	if err != nil {
		return nil, err
	}
	se.SetClientCapability(capability)
	se.SetConnectionID(connID)
	tc := &TiDBContext{
		session:   se,
		currentDB: dbname,
		stmts:     make(map[int]*TiDBStatement),
	}
	return tc, nil
}

// Status implements QueryCtx Status method.
func (tc *TiDBContext) Status() uint16 {
	return tc.session.Status()
}

// LastInsertID implements QueryCtx LastInsertID method.
func (tc *TiDBContext) LastInsertID() uint64 {
	return tc.session.LastInsertID()
}

// Value implements QueryCtx Value method.
func (tc *TiDBContext) Value(key fmt.Stringer) interface{} {
	return tc.session.Value(key)
}

// SetValue implements QueryCtx SetValue method.
func (tc *TiDBContext) SetValue(key fmt.Stringer, value interface{}) {
	tc.session.SetValue(key, value)
}

// CommitTxn implements QueryCtx CommitTxn method.
func (tc *TiDBContext) CommitTxn(ctx context.Context) error {
	return tc.session.CommitTxn(ctx)
}

// SetProcessInfo implements QueryCtx SetProcessInfo method.
func (tc *TiDBContext) SetProcessInfo(sql string, t time.Time, command byte, maxExecutionTime uint64) {
	tc.session.SetProcessInfo(sql, t, command, maxExecutionTime)
}

// RollbackTxn implements QueryCtx RollbackTxn method.
func (tc *TiDBContext) RollbackTxn() {
	tc.session.RollbackTxn(context.TODO())
}

// AffectedRows implements QueryCtx AffectedRows method.
func (tc *TiDBContext) AffectedRows() uint64 {
	return tc.session.AffectedRows()
}

// LastMessage implements QueryCtx LastMessage method.
func (tc *TiDBContext) LastMessage() string {
	return tc.session.LastMessage()
}

// CurrentDB implements QueryCtx CurrentDB method.
func (tc *TiDBContext) CurrentDB() string {
	return tc.currentDB
}

// WarningCount implements QueryCtx WarningCount method.
func (tc *TiDBContext) WarningCount() uint16 {
	return tc.session.GetSessionVars().StmtCtx.WarningCount()
}

// ExecuteStmt implements QueryCtx interface.
func (tc *TiDBContext) ExecuteStmt(ctx context.Context, stmt ast.StmtNode) (ResultSet, error) {
	rs, err := tc.session.ExecuteStmt(ctx, stmt)
	if err != nil {
		return nil, err
	}
	if rs == nil {
		return nil, nil
	}
	return &tidbResultSet{
		recordSet: rs,
	}, nil
}

// Parse implements QueryCtx interface.
func (tc *TiDBContext) Parse(ctx context.Context, sql string) ([]ast.StmtNode, error) {
	return tc.session.Parse(ctx, sql)
}

// SetSessionManager implements the QueryCtx interface.
func (tc *TiDBContext) SetSessionManager(sm util.SessionManager) {
	tc.session.SetSessionManager(sm)
}

// SetClientCapability implements QueryCtx SetClientCapability method.
func (tc *TiDBContext) SetClientCapability(flags uint32) {
	tc.session.SetClientCapability(flags)
}

// Close implements QueryCtx Close method.
func (tc *TiDBContext) Close() error {
	// close PreparedStatement associated with this connection
	for _, v := range tc.stmts {
		terror.Call(v.Close)
	}

	tc.session.Close()
	return nil
}

// Auth implements QueryCtx Auth method.
func (tc *TiDBContext) Auth(user *auth.UserIdentity, auth []byte, salt []byte) bool {
	return tc.session.Auth(user, auth, salt)
}

// NeedPassword implements QueryCtx NeedPassword method
func (tc *TiDBContext) NeedPassword(user *auth.UserIdentity) bool {
	return tc.session.NeedPassword(user)
}

// FieldList implements QueryCtx FieldList method.
func (tc *TiDBContext) FieldList(table string) (columns []*ColumnInfo, err error) {
	fields, err := tc.session.FieldList(table)
	if err != nil {
		return nil, err
	}
	columns = make([]*ColumnInfo, 0, len(fields))
	for _, f := range fields {
		columns = append(columns, convertColumnInfo(f))
	}
	return columns, nil
}

// GetStatement implements QueryCtx GetStatement method.
func (tc *TiDBContext) GetStatement(stmtID int) PreparedStatement {
	tcStmt := tc.stmts[stmtID]
	if tcStmt != nil {
		return tcStmt
	}
	return nil
}

// Prepare implements QueryCtx Prepare method.
// PgSQL Modified
func (tc *TiDBContext) Prepare(sql string, name string) (statement PreparedStatement, columns, params []*ColumnInfo, err error) {
	stmtID, paramCount, fields, err := tc.session.PrepareStmt(sql, name)
	if err != nil {
		return
	}

	columns = make([]*ColumnInfo, len(fields))
	for i := range fields {
		columns[i] = convertColumnInfo(fields[i])
	}
	params = make([]*ColumnInfo, paramCount)
	for i := range params {
		params[i] = &ColumnInfo{
			Type: mysql.TypeBlob,
		}
	}
	stmt := &TiDBStatement{
		sql:         sql,
		id:          stmtID,
		numParams:   paramCount,
		boundParams: make([][]byte, paramCount),
		ctx:         tc,
		columnInfo:  columns,
	}

	statement = stmt
	tc.stmts[int(stmtID)] = stmt
	return
}

// ShowProcess implements QueryCtx ShowProcess method.
func (tc *TiDBContext) ShowProcess() *util.ProcessInfo {
	return tc.session.ShowProcess()
}

// SetCommandValue implements QueryCtx SetCommandValue method.
func (tc *TiDBContext) SetCommandValue(command byte) {
	tc.session.SetCommandValue(command)
}

// GetSessionVars return SessionVars.
func (tc *TiDBContext) GetSessionVars() *variable.SessionVars {
	return tc.session.GetSessionVars()
}

type tidbResultSet struct {
	recordSet    sqlexec.RecordSet
	columns      []*ColumnInfo
	rows         []chunk.Row
	closed       int32
	preparedStmt *core.CachedPrepareStmt
}

// IsPrepareStmt 判断是否为预处理查询
func (trs *tidbResultSet) IsPrepareStmt() bool {
	if trs.preparedStmt != nil {
		return true
	}
	return false
}

func (trs *tidbResultSet) NewChunk() *chunk.Chunk {
	return trs.recordSet.NewChunk()
}

func (trs *tidbResultSet) Next(ctx context.Context, req *chunk.Chunk) error {
	return trs.recordSet.Next(ctx, req)
}

func (trs *tidbResultSet) StoreFetchedRows(rows []chunk.Row) {
	trs.rows = rows
}

func (trs *tidbResultSet) GetFetchedRows() []chunk.Row {
	if trs.rows == nil {
		trs.rows = make([]chunk.Row, 0, 1024)
	}
	return trs.rows
}

func (trs *tidbResultSet) Close() error {
	if !atomic.CompareAndSwapInt32(&trs.closed, 0, 1) {
		return nil
	}
	err := trs.recordSet.Close()
	trs.recordSet = nil
	return err
}

// OnFetchReturned implements fetchNotifier#OnFetchReturned
func (trs *tidbResultSet) OnFetchReturned() {
	if cl, ok := trs.recordSet.(fetchNotifier); ok {
		cl.OnFetchReturned()
	}
}

func (trs *tidbResultSet) Columns() []*ColumnInfo {
	if trs.columns != nil {
		return trs.columns
	}
	// for prepare statement, try to get cached columnInfo array
	if trs.preparedStmt != nil {
		ps := trs.preparedStmt
		if colInfos, ok := ps.ColumnInfos.([]*ColumnInfo); ok {
			trs.columns = colInfos
		}
	}
	if trs.columns == nil {
		fields := trs.recordSet.Fields()
		for _, v := range fields {
			trs.columns = append(trs.columns, convertColumnInfo(v))
		}
		if trs.preparedStmt != nil {
			// if ColumnInfo struct has allocated object,
			// here maybe we need deep copy ColumnInfo to do caching
			trs.preparedStmt.ColumnInfos = trs.columns
		}
	}
	return trs.columns
}

func convertColumnInfo(fld *ast.ResultField) (ci *ColumnInfo) {
	ci = &ColumnInfo{
		Name:    fld.ColumnAsName.O,
		OrgName: fld.Column.Name.O,
		Table:   fld.TableAsName.O,
		Schema:  fld.DBName.O,
		Flag:    uint16(fld.Column.Flag),
		Charset: uint16(mysql.CharsetNameToID(fld.Column.Charset)),
		Type:    fld.Column.Tp,
	}

	if fld.Table != nil {
		ci.OrgTable = fld.Table.Name.O
	}
	if fld.Column.Flen == types.UnspecifiedLength {
		ci.ColumnLength = 0
	} else {
		ci.ColumnLength = uint32(fld.Column.Flen)
	}
	if fld.Column.Tp == mysql.TypeNewDecimal {
		// Consider the negative sign.
		ci.ColumnLength++
		if fld.Column.Decimal > int(types.DefaultFsp) {
			// Consider the decimal point.
			ci.ColumnLength++
		}
	} else if types.IsString(fld.Column.Tp) ||
		fld.Column.Tp == mysql.TypeEnum || fld.Column.Tp == mysql.TypeSet { // issue #18870
		// Fix issue #4540.
		// The flen is a hint, not a precise value, so most client will not use the value.
		// But we found in rare MySQL client, like Navicat for MySQL(version before 12) will truncate
		// the `show create table` result. To fix this case, we must use a large enough flen to prevent
		// the truncation, in MySQL, it will multiply bytes length by a multiple based on character set.
		// For examples:
		// * latin, the multiple is 1
		// * gb2312, the multiple is 2
		// * Utf-8, the multiple is 3
		// * utf8mb4, the multiple is 4
		// We used to check non-string types to avoid the truncation problem in some MySQL
		// client such as Navicat. Now we only allow string type enter this branch.
		charsetDesc, err := charset.GetCharsetDesc(fld.Column.Charset)
		if err != nil {
			ci.ColumnLength = ci.ColumnLength * 4
		} else {
			ci.ColumnLength = ci.ColumnLength * uint32(charsetDesc.Maxlen)
		}
	}

	if fld.Column.Decimal == types.UnspecifiedLength {
		if fld.Column.Tp == mysql.TypeDuration {
			ci.Decimal = uint8(types.DefaultFsp)
		} else {
			ci.Decimal = mysql.NotFixedDec
		}
	} else {
		ci.Decimal = uint8(fld.Column.Decimal)
	}

	// Keep things compatible for old clients.
	// Refer to mysql-server/sql/protocol.cc send_result_set_metadata()
	if ci.Type == mysql.TypeVarchar {
		ci.Type = mysql.TypeVarString
	}
	return
}
