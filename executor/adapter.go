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

package executor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/planner"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/plugin"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/sqlexec"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// processinfoSetter is the interface use to set current running process info.
type processinfoSetter interface {
	SetProcessInfo(string, time.Time, byte, uint64)
}

// recordSet wraps an executor, implements sqlexec.RecordSet interface
type recordSet struct {
	fields     []*ast.ResultField
	executor   Executor
	stmt       *ExecStmt
	lastErr    error
	txnStartTS uint64
}

func (a *recordSet) Fields() []*ast.ResultField {
	if len(a.fields) == 0 {
		a.fields = schema2ResultFields(a.executor.Schema(), a.stmt.Ctx.GetSessionVars().CurrentDB)
	}
	return a.fields
}

func schema2ResultFields(schema *expression.Schema, defaultDB string) (rfs []*ast.ResultField) {
	rfs = make([]*ast.ResultField, 0, schema.Len())
	for _, col := range schema.Columns {
		dbName := col.DBName.O
		if dbName == "" && col.TblName.L != "" {
			dbName = defaultDB
		}
		origColName := col.OrigColName
		if origColName.L == "" {
			origColName = col.ColName
		}
		rf := &ast.ResultField{
			ColumnAsName: col.ColName,
			TableAsName:  col.TblName,
			DBName:       model.NewCIStr(dbName),
			Table:        &model.TableInfo{Name: col.OrigTblName},
			Column: &model.ColumnInfo{
				FieldType: *col.RetType,
				Name:      origColName,
			},
		}
		rfs = append(rfs, rf)
	}
	return rfs
}

// Next use uses recordSet's executor to get next available chunk for later usage.
// If chunk does not contain any rows, then we update last query found rows in session variable as current found rows.
// The reason we need update is that chunk with 0 rows indicating we already finished current query, we need prepare for
// next query.
// If stmt is not nil and chunk with some rows inside, we simply update last query found rows by the number of row in chunk.
func (a *recordSet) Next(ctx context.Context, req *chunk.Chunk) error {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("recordSet.Next", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
	}

	err := Next(ctx, a.executor, req)
	if err != nil {
		a.lastErr = err
		return err
	}
	numRows := req.NumRows()
	if numRows == 0 {
		if a.stmt != nil {
			a.stmt.Ctx.GetSessionVars().LastFoundRows = a.stmt.Ctx.GetSessionVars().StmtCtx.FoundRows()
		}
		return nil
	}
	if a.stmt != nil {
		a.stmt.Ctx.GetSessionVars().StmtCtx.AddFoundRows(uint64(numRows))
	}
	return nil
}

// NewChunk create a chunk base on top-level executor's newFirstChunk().
func (a *recordSet) NewChunk() *chunk.Chunk {
	return newFirstChunk(a.executor)
}

func (a *recordSet) Close() error {
	err := a.executor.Close()
	a.stmt.LogSlowQuery(a.txnStartTS, a.lastErr == nil)
	a.stmt.logAudit()
	return err
}

// ExecStmt implements the sqlexec.Statement interface, it builds a planner.Plan to an sqlexec.Statement.
type ExecStmt struct {
	// InfoSchema stores a reference to the schema information.
	InfoSchema infoschema.InfoSchema
	// Plan stores a reference to the final physical plan.
	Plan plannercore.Plan
	// LowerPriority represents whether to lower the execution priority of a query.
	LowerPriority bool
	// Cacheable represents whether the physical plan can be cached.
	Cacheable bool
	// Text represents the origin query text.
	Text string

	StmtNode ast.StmtNode

	Ctx sessionctx.Context
	// StartTime stands for the starting time when executing the statement.
	StartTime         time.Time
	isPreparedStmt    bool
	isSelectForUpdate bool
	retryCount        uint
}

// OriginText returns original statement as a string.
func (a *ExecStmt) OriginText() string {
	return a.Text
}

// IsPrepared returns true if stmt is a prepare statement.
func (a *ExecStmt) IsPrepared() bool {
	return a.isPreparedStmt
}

// IsReadOnly returns true if a statement is read only.
// If current StmtNode is an ExecuteStmt, we can get its prepared stmt,
// then using ast.IsReadOnly function to determine a statement is read only or not.
func (a *ExecStmt) IsReadOnly(vars *variable.SessionVars) bool {
	if execStmt, ok := a.StmtNode.(*ast.ExecuteStmt); ok {
		s, err := getPreparedStmt(execStmt, vars)
		if err != nil {
			logutil.Logger(context.Background()).Error("getPreparedStmt failed", zap.Error(err))
			return false
		}
		return ast.IsReadOnly(s)
	}
	return ast.IsReadOnly(a.StmtNode)
}

// RebuildPlan rebuilds current execute statement plan.
// It returns the current information schema version that 'a' is using.
func (a *ExecStmt) RebuildPlan() (int64, error) {
	is := GetInfoSchema(a.Ctx)
	a.InfoSchema = is
	if err := plannercore.Preprocess(a.Ctx, a.StmtNode, is, plannercore.InTxnRetry); err != nil {
		return 0, err
	}
	p, err := planner.Optimize(a.Ctx, a.StmtNode, is)
	if err != nil {
		return 0, err
	}
	a.Plan = p
	return is.SchemaMetaVersion(), nil
}

// Exec builds an Executor from a plan. If the Executor doesn't return result,
// like the INSERT, UPDATE statements, it executes in this function, if the Executor returns
// result, execution is done after this function returns, in the returned sqlexec.RecordSet Next method.
func (a *ExecStmt) Exec(ctx context.Context) (_ sqlexec.RecordSet, err error) {
	a.StartTime = time.Now()
	sctx := a.Ctx
	if _, ok := a.Plan.(*plannercore.Analyze); ok && sctx.GetSessionVars().InRestrictedSQL {
		oriStats, _ := sctx.GetSessionVars().GetSystemVar(variable.TiDBBuildStatsConcurrency)
		oriScan := sctx.GetSessionVars().DistSQLScanConcurrency
		oriIndex := sctx.GetSessionVars().IndexSerialScanConcurrency
		oriIso, _ := sctx.GetSessionVars().GetSystemVar(variable.TxnIsolation)
		terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TiDBBuildStatsConcurrency, "1"))
		sctx.GetSessionVars().DistSQLScanConcurrency = 1
		sctx.GetSessionVars().IndexSerialScanConcurrency = 1
		terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TxnIsolation, ast.ReadCommitted))
		defer func() {
			terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TiDBBuildStatsConcurrency, oriStats))
			sctx.GetSessionVars().DistSQLScanConcurrency = oriScan
			sctx.GetSessionVars().IndexSerialScanConcurrency = oriIndex
			terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TxnIsolation, oriIso))
		}()
	}

	e, err := a.buildExecutor()
	if err != nil {
		return nil, err
	}

	if err = e.Open(ctx); err != nil {
		terror.Call(e.Close)
		return nil, err
	}

	cmd32 := atomic.LoadUint32(&sctx.GetSessionVars().CommandValue)
	cmd := byte(cmd32)
	var pi processinfoSetter
	if raw, ok := sctx.(processinfoSetter); ok {
		pi = raw
		sql := a.OriginText()
		if simple, ok := a.Plan.(*plannercore.Simple); ok && simple.Statement != nil {
			if ss, ok := simple.Statement.(ast.SensitiveStmtNode); ok {
				// Use SecureText to avoid leak password information.
				sql = ss.SecureText()
			}
		}
		maxExecutionTime := getMaxExecutionTime(sctx, a.StmtNode)
		// Update processinfo, ShowProcess() will use it.
		pi.SetProcessInfo(sql, time.Now(), cmd, maxExecutionTime)
		a.Ctx.GetSessionVars().StmtCtx.StmtType = GetStmtLabel(a.StmtNode)
	}

	isPessimistic := sctx.GetSessionVars().TxnCtx.IsPessimistic

	// Special handle for "select for update statement" in pessimistic transaction.
	if isPessimistic && a.isSelectForUpdate {
		return a.handlePessimisticSelectForUpdate(ctx, e)
	}

	// If the executor doesn't return any result to the client, we execute it without delay.
	if e.Schema().Len() == 0 {
		if isPessimistic {
			return nil, a.handlePessimisticDML(ctx, e)
		}
		return a.handleNoDelayExecutor(ctx, e)
	} else if proj, ok := e.(*ProjectionExec); ok && proj.calculateNoDelay {
		// Currently this is only for the "DO" statement. Take "DO 1, @a=2;" as an example:
		// the Projection has two expressions and two columns in the schema, but we should
		// not return the result of the two expressions.
		return a.handleNoDelayExecutor(ctx, e)
	}

	var txnStartTS uint64
	txn, err := sctx.Txn(false)
	if err != nil {
		return nil, err
	}
	if txn.Valid() {
		txnStartTS = txn.StartTS()
	}
	return &recordSet{
		executor:   e,
		stmt:       a,
		txnStartTS: txnStartTS,
	}, nil
}

// getMaxExecutionTime get the max execution timeout value.
func getMaxExecutionTime(sctx sessionctx.Context, stmtNode ast.StmtNode) uint64 {
	ret := sctx.GetSessionVars().MaxExecutionTime
	if sel, ok := stmtNode.(*ast.SelectStmt); ok {
		for _, hint := range sel.TableHints {
			if hint.HintName.L == variable.MaxExecutionTime {
				ret = hint.MaxExecutionTime
				break
			}
		}
	}
	return ret
}

type chunkRowRecordSet struct {
	rows   []chunk.Row
	idx    int
	fields []*ast.ResultField
	e      Executor
}

func (c *chunkRowRecordSet) Fields() []*ast.ResultField {
	return c.fields
}

func (c *chunkRowRecordSet) Next(ctx context.Context, chk *chunk.Chunk) error {
	chk.Reset()
	for !chk.IsFull() && c.idx < len(c.rows) {
		chk.AppendRow(c.rows[c.idx])
		c.idx++
	}
	return nil
}

func (c *chunkRowRecordSet) NewChunk() *chunk.Chunk {
	return newFirstChunk(c.e)
}

func (c *chunkRowRecordSet) Close() error {
	return nil
}

func (a *ExecStmt) handlePessimisticSelectForUpdate(ctx context.Context, e Executor) (sqlexec.RecordSet, error) {
	for {
		rs, err := a.runPessimisticSelectForUpdate(ctx, e)
		e, err = a.handlePessimisticLockError(ctx, err)
		if err != nil {
			return nil, err
		}
		if e == nil {
			return rs, nil
		}
	}
}

func (a *ExecStmt) runPessimisticSelectForUpdate(ctx context.Context, e Executor) (sqlexec.RecordSet, error) {
	rs := &recordSet{
		executor: e,
		stmt:     a,
	}
	defer func() {
		terror.Log(rs.Close())
	}()

	var rows []chunk.Row
	var err error
	fields := rs.Fields()
	req := rs.NewChunk()
	for {
		err = rs.Next(ctx, req)
		if err != nil {
			// Handle 'write conflict' error.
			break
		}
		if req.NumRows() == 0 {
			return &chunkRowRecordSet{rows: rows, fields: fields, e: e}, nil
		}
		iter := chunk.NewIterator4Chunk(req)
		for r := iter.Begin(); r != iter.End(); r = iter.Next() {
			rows = append(rows, r)
		}
		req = chunk.Renew(req, a.Ctx.GetSessionVars().MaxChunkSize)
	}
	return nil, err
}

func (a *ExecStmt) handleNoDelayExecutor(ctx context.Context, e Executor) (sqlexec.RecordSet, error) {
	sctx := a.Ctx
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("executor.handleNoDelayExecutor", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
	}

	// Check if "tidb_snapshot" is set for the write executors.
	// In history read mode, we can not do write operations.
	switch e.(type) {
	case *DeleteExec, *InsertExec, *UpdateExec, *ReplaceExec, *LoadDataExec, *DDLExec:
		snapshotTS := sctx.GetSessionVars().SnapshotTS
		if snapshotTS != 0 {
			return nil, errors.New("can not execute write statement when 'tidb_snapshot' is set")
		}
		lowResolutionTSO := sctx.GetSessionVars().LowResolutionTSO
		if lowResolutionTSO {
			return nil, errors.New("can not execute write statement when 'tidb_low_resolution_tso' is set")
		}
	}

	var err error
	defer func() {
		terror.Log(e.Close())
		a.logAudit()
	}()

	err = Next(ctx, e, newFirstChunk(e))
	if err != nil {
		return nil, err
	}
	return nil, err
}

func (a *ExecStmt) handlePessimisticDML(ctx context.Context, e Executor) error {
	sctx := a.Ctx
	txn, err := sctx.Txn(true)
	if err != nil {
		return err
	}
	txnCtx := sctx.GetSessionVars().TxnCtx
	for {
		_, err = a.handleNoDelayExecutor(ctx, e)
		if err != nil {
			// It is possible the DML has point get plan that locks the key.
			e, err = a.handlePessimisticLockError(ctx, err)
			if err != nil {
				return err
			}
			continue
		}
		keys, err1 := txn.(pessimisticTxn).KeysNeedToLock()
		if err1 != nil {
			return err1
		}
		if len(keys) == 0 {
			return nil
		}
		forUpdateTS := txnCtx.GetForUpdateTS()
		err = txn.LockKeys(ctx, forUpdateTS, keys...)
		if err == nil {
			return nil
		}
		e, err = a.handlePessimisticLockError(ctx, err)
		if err != nil {
			return err
		}
	}
}

// handlePessimisticLockError updates TS and rebuild executor if the err is write conflict.
func (a *ExecStmt) handlePessimisticLockError(ctx context.Context, err error) (Executor, error) {
	txnCtx := a.Ctx.GetSessionVars().TxnCtx
	var newForUpdateTS uint64
	if deadlock, ok := errors.Cause(err).(*tikv.ErrDeadlock); ok {
		if !deadlock.IsRetryable {
			return nil, ErrDeadlock
		}
		logutil.Logger(ctx).Info("single statement deadlock, retry statement",
			zap.Uint64("txn", txnCtx.StartTS),
			zap.Uint64("lockTS", deadlock.LockTs),
			zap.Binary("lockKey", deadlock.LockKey),
			zap.Uint64("deadlockKeyHash", deadlock.DeadlockKeyHash))
	} else if terror.ErrorEqual(kv.ErrWriteConflict, err) {
		conflictCommitTS := extractConflictCommitTS(err.Error())
		if conflictCommitTS == 0 {
			logutil.Logger(ctx).Warn("failed to extract conflictCommitTS from a conflict error")
		}
		forUpdateTS := txnCtx.GetForUpdateTS()
		logutil.Logger(ctx).Info("pessimistic write conflict, retry statement",
			zap.Uint64("txn", txnCtx.StartTS),
			zap.Uint64("forUpdateTS", forUpdateTS),
			zap.Uint64("conflictCommitTS", conflictCommitTS))
		if conflictCommitTS > forUpdateTS {
			newForUpdateTS = conflictCommitTS
		}
	} else {
		return nil, err
	}
	if a.retryCount >= config.GetGlobalConfig().PessimisticTxn.MaxRetryCount {
		return nil, errors.New("pessimistic lock retry limit reached")
	}
	a.retryCount++
	if newForUpdateTS == 0 {
		newForUpdateTS, err = a.Ctx.GetStore().GetOracle().GetTimestamp(ctx)
		if err != nil {
			return nil, err
		}
	}
	txnCtx.SetForUpdateTS(newForUpdateTS)
	txn, err := a.Ctx.Txn(true)
	if err != nil {
		return nil, err
	}
	txn.SetOption(kv.SnapshotTS, newForUpdateTS)
	e, err := a.buildExecutor()
	if err != nil {
		return nil, err
	}
	// Rollback the statement change before retry it.
	a.Ctx.StmtRollback()
	a.Ctx.GetSessionVars().StmtCtx.ResetForRetry()

	if err = e.Open(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func extractConflictCommitTS(errStr string) uint64 {
	strs := strings.Split(errStr, "conflictCommitTS=")
	if len(strs) != 2 {
		return 0
	}
	tsPart := strs[1]
	length := strings.IndexByte(tsPart, ',')
	if length < 0 {
		return 0
	}
	tsStr := tsPart[:length]
	ts, err := strconv.ParseUint(tsStr, 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

type pessimisticTxn interface {
	kv.Transaction
	// KeysNeedToLock returns the keys need to be locked.
	KeysNeedToLock() ([]kv.Key, error)
}

// buildExecutor build a executor from plan, prepared statement may need additional procedure.
func (a *ExecStmt) buildExecutor() (Executor, error) {
	ctx := a.Ctx
	if _, ok := a.Plan.(*plannercore.Execute); !ok {
		// Do not sync transaction for Execute statement, because the real optimization work is done in
		// "ExecuteExec.Build".
		useMaxTS, err := IsPointGetWithPKOrUniqueKeyByAutoCommit(ctx, a.Plan)
		if err != nil {
			return nil, err
		}
		if useMaxTS {
			logutil.Logger(context.Background()).Debug("init txnStartTS with MaxUint64", zap.Uint64("conn", ctx.GetSessionVars().ConnectionID), zap.String("text", a.Text))
			err = ctx.InitTxnWithStartTS(math.MaxUint64)
		} else if ctx.GetSessionVars().SnapshotTS != 0 {
			if _, ok := a.Plan.(*plannercore.CheckTable); ok {
				err = ctx.InitTxnWithStartTS(ctx.GetSessionVars().SnapshotTS)
			}
		}
		if err != nil {
			return nil, err
		}

		stmtCtx := ctx.GetSessionVars().StmtCtx
		if stmtPri := stmtCtx.Priority; stmtPri == mysql.NoPriority {
			switch {
			case useMaxTS:
				stmtCtx.Priority = kv.PriorityHigh
			case a.LowerPriority:
				stmtCtx.Priority = kv.PriorityLow
			}
		}
	}
	if _, ok := a.Plan.(*plannercore.Analyze); ok && ctx.GetSessionVars().InRestrictedSQL {
		ctx.GetSessionVars().StmtCtx.Priority = kv.PriorityLow
	}

	b := newExecutorBuilder(ctx, a.InfoSchema)
	e := b.build(a.Plan)
	if b.err != nil {
		return nil, errors.Trace(b.err)
	}

	// ExecuteExec is not a real Executor, we only use it to build another Executor from a prepared statement.
	if executorExec, ok := e.(*ExecuteExec); ok {
		err := executorExec.Build(b)
		if err != nil {
			return nil, err
		}
		a.isPreparedStmt = true
		a.Plan = executorExec.plan
		if executorExec.lowerPriority {
			ctx.GetSessionVars().StmtCtx.Priority = kv.PriorityLow
		}
		e = executorExec.stmtExec
	}
	a.isSelectForUpdate = b.isSelectForUpdate
	return e, nil
}

// QueryReplacer replaces new line and tab for grep result including query string.
var QueryReplacer = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")

func (a *ExecStmt) logAudit() {
	sessVars := a.Ctx.GetSessionVars()
	if sessVars.InRestrictedSQL {
		return
	}
	err := plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
		audit := plugin.DeclareAuditManifest(p.Manifest)
		if audit.OnGeneralEvent != nil {
			cmd := mysql.Command2Str[byte(atomic.LoadUint32(&a.Ctx.GetSessionVars().CommandValue))]
			ctx := context.WithValue(context.Background(), plugin.ExecStartTimeCtxKey, a.StartTime)
			audit.OnGeneralEvent(ctx, sessVars, plugin.Log, cmd)
		}
		return nil
	})
	if err != nil {
		log.Error("log audit log failure", zap.Error(err))
	}
}

// LogSlowQuery is used to print the slow query in the log files.
func (a *ExecStmt) LogSlowQuery(txnTS uint64, succ bool) {
	sessVars := a.Ctx.GetSessionVars()
	level := log.GetLevel()
	if level > zapcore.WarnLevel {
		return
	}
	cfg := config.GetGlobalConfig()
	costTime := time.Since(a.StartTime)
	threshold := time.Duration(atomic.LoadUint64(&cfg.Log.SlowThreshold)) * time.Millisecond
	if costTime < threshold && level > zapcore.DebugLevel {
		return
	}
	sql := a.Text
	if maxQueryLen := atomic.LoadUint64(&cfg.Log.QueryLogMaxLen); uint64(len(sql)) > maxQueryLen {
		sql = fmt.Sprintf("%.*q(len:%d)", maxQueryLen, sql, len(a.Text))
	}
	sql = QueryReplacer.Replace(sql) + sessVars.GetExecuteArgumentsInfo()

	var tableIDs, indexIDs string
	if len(sessVars.StmtCtx.TableIDs) > 0 {
		tableIDs = strings.Replace(fmt.Sprintf("%v", a.Ctx.GetSessionVars().StmtCtx.TableIDs), " ", ",", -1)
	}
	if len(sessVars.StmtCtx.IndexIDs) > 0 {
		indexIDs = strings.Replace(fmt.Sprintf("%v", a.Ctx.GetSessionVars().StmtCtx.IndexIDs), " ", ",", -1)
	}
	execDetail := sessVars.StmtCtx.GetExecDetails()
	copTaskInfo := sessVars.StmtCtx.CopTasksDetails()
	statsInfos := plannercore.GetStatsInfo(a.Plan)
	memMax := sessVars.StmtCtx.MemTracker.MaxConsumed()
	if costTime < threshold {
		_, digest := sessVars.StmtCtx.SQLDigest()
		logutil.SlowQueryLogger.Debug(sessVars.SlowLogFormat(txnTS, costTime, execDetail, indexIDs, digest, statsInfos, copTaskInfo, memMax, sql))
	} else {
		_, digest := sessVars.StmtCtx.SQLDigest()
		logutil.SlowQueryLogger.Warn(sessVars.SlowLogFormat(txnTS, costTime, execDetail, indexIDs, digest, statsInfos, copTaskInfo, memMax, sql))
		metrics.TotalQueryProcHistogram.Observe(costTime.Seconds())
		metrics.TotalCopProcHistogram.Observe(execDetail.ProcessTime.Seconds())
		metrics.TotalCopWaitHistogram.Observe(execDetail.WaitTime.Seconds())
		var userString string
		if sessVars.User != nil {
			userString = sessVars.User.String()
		}
		domain.GetDomain(a.Ctx).LogSlowQuery(&domain.SlowQueryInfo{
			SQL:      sql,
			Digest:   digest,
			Start:    a.StartTime,
			Duration: costTime,
			Detail:   sessVars.StmtCtx.GetExecDetails(),
			Succ:     succ,
			ConnID:   sessVars.ConnectionID,
			TxnTS:    txnTS,
			User:     userString,
			DB:       sessVars.CurrentDB,
			TableIDs: tableIDs,
			IndexIDs: indexIDs,
			Internal: sessVars.InRestrictedSQL,
		})
	}
}

// IsPointGetWithPKOrUniqueKeyByAutoCommit returns true when meets following conditions:
//  1. ctx is auto commit tagged
//  2. txn is not valid
//  2. plan is point get by pk, or point get by unique index (no double read)
func IsPointGetWithPKOrUniqueKeyByAutoCommit(ctx sessionctx.Context, p plannercore.Plan) (bool, error) {
	// check auto commit
	if !ctx.GetSessionVars().IsAutocommit() {
		return false, nil
	}

	// check txn
	txn, err := ctx.Txn(false)
	if err != nil {
		return false, err
	}
	if txn.Valid() {
		return false, nil
	}

	// check plan
	if proj, ok := p.(*plannercore.PhysicalProjection); ok {
		if len(proj.Children()) != 1 {
			return false, nil
		}
		p = proj.Children()[0]
	}

	switch v := p.(type) {
	case *plannercore.PhysicalIndexReader:
		indexScan := v.IndexPlans[0].(*plannercore.PhysicalIndexScan)
		return indexScan.IsPointGetByUniqueKey(ctx.GetSessionVars().StmtCtx), nil
	case *plannercore.PhysicalTableReader:
		tableScan := v.TablePlans[0].(*plannercore.PhysicalTableScan)
		return len(tableScan.Ranges) == 1 && tableScan.Ranges[0].IsPoint(ctx.GetSessionVars().StmtCtx), nil
	case *plannercore.PointGetPlan:
		// If the PointGetPlan needs to read data using unique index (double read), we
		// can't use max uint64, because using math.MaxUint64 can't guarantee repeatable-read
		// and the data and index would be inconsistent!
		return v.IndexInfo == nil, nil
	default:
		return false, nil
	}
}
