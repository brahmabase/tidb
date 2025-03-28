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
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tidb/util/stringutil"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

var (
	_ Executor = &TableReaderExecutor{}
	_ Executor = &IndexReaderExecutor{}
	_ Executor = &IndexLookUpExecutor{}
)

// LookupTableTaskChannelSize represents the channel size of the index double read taskChan.
var LookupTableTaskChannelSize int32 = 50

// lookupTableTask is created from a partial result of an index request which
// contains the handles in those index keys.
type lookupTableTask struct {
	handles []int64
	rowIdx  []int // rowIdx represents the handle index for every row. Only used when keep order.
	rows    []chunk.Row
	cursor  int

	doneCh chan error

	// indexOrder map is used to save the original index order for the handles.
	// Without this map, the original index order might be lost.
	// The handles fetched from index is originally ordered by index, but we need handles to be ordered by itself
	// to do table request.
	indexOrder map[int64]int

	// memUsage records the memory usage of this task calculated by table worker.
	// memTracker is used to release memUsage after task is done and unused.
	//
	// The sequence of function calls are:
	//   1. calculate task.memUsage.
	//   2. task.memTracker = tableWorker.memTracker
	//   3. task.memTracker.Consume(task.memUsage)
	//   4. task.memTracker.Consume(-task.memUsage)
	//
	// Step 1~3 are completed in "tableWorker.executeTask".
	// Step 4   is  completed in "IndexLookUpExecutor.Next".
	memUsage   int64
	memTracker *memory.Tracker
}

func (task *lookupTableTask) Len() int {
	return len(task.rows)
}

func (task *lookupTableTask) Less(i, j int) bool {
	return task.rowIdx[i] < task.rowIdx[j]
}

func (task *lookupTableTask) Swap(i, j int) {
	task.rowIdx[i], task.rowIdx[j] = task.rowIdx[j], task.rowIdx[i]
	task.rows[i], task.rows[j] = task.rows[j], task.rows[i]
}

// Closeable is a interface for closeable structures.
type Closeable interface {
	// Close closes the object.
	Close() error
}

// closeAll closes all objects even if an object returns an error.
// If multiple objects returns error, the first error will be returned.
func closeAll(objs ...Closeable) error {
	var err error
	for _, obj := range objs {
		if obj != nil {
			err1 := obj.Close()
			if err == nil && err1 != nil {
				err = err1
			}
		}
	}
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// statementContextToFlags converts StatementContext to tipb.SelectRequest.Flags.
func statementContextToFlags(sc *stmtctx.StatementContext) uint64 {
	var flags uint64
	if sc.InInsertStmt {
		flags |= model.FlagInInsertStmt
	} else if sc.InUpdateStmt || sc.InDeleteStmt {
		flags |= model.FlagInUpdateOrDeleteStmt
	} else if sc.InSelectStmt {
		flags |= model.FlagInSelectStmt
	}
	if sc.IgnoreTruncate {
		flags |= model.FlagIgnoreTruncate
	} else if sc.TruncateAsWarning {
		flags |= model.FlagTruncateAsWarning
	}
	if sc.OverflowAsWarning {
		flags |= model.FlagOverflowAsWarning
	}
	if sc.IgnoreZeroInDate {
		flags |= model.FlagIgnoreZeroInDate
	}
	if sc.DividedByZeroAsWarning {
		flags |= model.FlagDividedByZeroAsWarning
	}
	if sc.PadCharToFullLength {
		flags |= model.FlagPadCharToFullLength
	}
	return flags
}

// handleIsExtra checks whether this column is a extra handle column generated during plan building phase.
func handleIsExtra(col *expression.Column) bool {
	if col != nil && col.ID == model.ExtraHandleID {
		return true
	}
	return false
}

func splitRanges(ranges []*ranger.Range, keepOrder bool, desc bool) ([]*ranger.Range, []*ranger.Range) {
	if len(ranges) == 0 || ranges[0].LowVal[0].Kind() == types.KindInt64 {
		return ranges, nil
	}
	idx := sort.Search(len(ranges), func(i int) bool { return ranges[i].HighVal[0].GetUint64() > math.MaxInt64 })
	if idx == len(ranges) {
		return ranges, nil
	}
	if ranges[idx].LowVal[0].GetUint64() > math.MaxInt64 {
		signedRanges := ranges[0:idx]
		unsignedRanges := ranges[idx:]
		if !keepOrder {
			return append(unsignedRanges, signedRanges...), nil
		}
		if desc {
			return unsignedRanges, signedRanges
		}
		return signedRanges, unsignedRanges
	}
	signedRanges := make([]*ranger.Range, 0, idx+1)
	unsignedRanges := make([]*ranger.Range, 0, len(ranges)-idx)
	signedRanges = append(signedRanges, ranges[0:idx]...)
	if !(ranges[idx].LowVal[0].GetUint64() == math.MaxInt64 && ranges[idx].LowExclude) {
		signedRanges = append(signedRanges, &ranger.Range{
			LowVal:     ranges[idx].LowVal,
			LowExclude: ranges[idx].LowExclude,
			HighVal:    []types.Datum{types.NewUintDatum(math.MaxInt64)},
		})
	}
	if !(ranges[idx].HighVal[0].GetUint64() == math.MaxInt64+1 && ranges[idx].HighExclude) {
		unsignedRanges = append(unsignedRanges, &ranger.Range{
			LowVal:      []types.Datum{types.NewUintDatum(math.MaxInt64 + 1)},
			HighVal:     ranges[idx].HighVal,
			HighExclude: ranges[idx].HighExclude,
		})
	}
	if idx < len(ranges) {
		unsignedRanges = append(unsignedRanges, ranges[idx+1:]...)
	}
	if !keepOrder {
		return append(unsignedRanges, signedRanges...), nil
	}
	if desc {
		return unsignedRanges, signedRanges
	}
	return signedRanges, unsignedRanges
}

// rebuildIndexRanges will be called if there's correlated column in access conditions. We will rebuild the range
// by substitute correlated column with the constant.
func rebuildIndexRanges(ctx sessionctx.Context, is *plannercore.PhysicalIndexScan, idxCols []*expression.Column, colLens []int) (ranges []*ranger.Range, err error) {
	access := make([]expression.Expression, 0, len(is.AccessCondition))
	for _, cond := range is.AccessCondition {
		newCond, err1 := expression.SubstituteCorCol2Constant(cond)
		if err1 != nil {
			return nil, err1
		}
		access = append(access, newCond)
	}
	ranges, _, err = ranger.DetachSimpleCondAndBuildRangeForIndex(ctx, access, idxCols, colLens)
	return ranges, err
}

// IndexReaderExecutor sends dag request and reads index data from kv layer.
type IndexReaderExecutor struct {
	baseExecutor

	// For a partitioned table, the IndexReaderExecutor works on a partition, so
	// the type of this table field is actually `table.PhysicalTable`.
	table           table.Table
	index           *model.IndexInfo
	physicalTableID int64
	keepOrder       bool
	desc            bool
	ranges          []*ranger.Range
	dagPB           *tipb.DAGRequest

	// result returns one or more distsql.PartialResult and each PartialResult is returned by one region.
	result distsql.SelectResult
	// columns are only required by union scan.
	columns   []*model.ColumnInfo
	streaming bool
	feedback  *statistics.QueryFeedback

	corColInFilter bool
	corColInAccess bool
	idxCols        []*expression.Column
	colLens        []int
	plans          []plannercore.PhysicalPlan

	selectResultHook // for testing
}

// Close clears all resources hold by current object.
func (e *IndexReaderExecutor) Close() error {
	err := e.result.Close()
	e.result = nil
	if e.runtimeStats != nil {
		copStats := e.ctx.GetSessionVars().StmtCtx.RuntimeStatsColl.GetRootStats(e.plans[0].ExplainID().String())
		copStats.SetRowNum(e.feedback.Actual())
	}
	e.ctx.StoreQueryFeedback(e.feedback)
	return err
}

// Next implements the Executor Next interface.
func (e *IndexReaderExecutor) Next(ctx context.Context, req *chunk.Chunk) error {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("tableReader.Next", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
	}
	if e.runtimeStats != nil {
		start := time.Now()
		defer func() { e.runtimeStats.Record(time.Since(start), req.NumRows()) }()
	}
	err := e.result.Next(ctx, req)
	if err != nil {
		e.feedback.Invalidate()
	}
	return err
}

// Open implements the Executor Open interface.
func (e *IndexReaderExecutor) Open(ctx context.Context) error {
	var err error
	if e.corColInAccess {
		e.ranges, err = rebuildIndexRanges(e.ctx, e.plans[0].(*plannercore.PhysicalIndexScan), e.idxCols, e.colLens)
		if err != nil {
			return err
		}
	}
	kvRanges, err := distsql.IndexRangesToKVRanges(e.ctx.GetSessionVars().StmtCtx, e.physicalTableID, e.index.ID, e.ranges, e.feedback)
	if err != nil {
		e.feedback.Invalidate()
		return err
	}
	return e.open(ctx, kvRanges)
}

var indexReaderDistSQLTrackerLabel fmt.Stringer = stringutil.StringerStr("IndexReaderDistSQLTracker")

func (e *IndexReaderExecutor) open(ctx context.Context, kvRanges []kv.KeyRange) error {
	var err error
	if e.corColInFilter {
		e.dagPB.Executors, _, err = constructDistExec(e.ctx, e.plans)
		if err != nil {
			return err
		}
	}

	if e.runtimeStats != nil {
		collExec := true
		e.dagPB.CollectExecutionSummaries = &collExec
	}

	var builder distsql.RequestBuilder
	kvReq, err := builder.SetKeyRanges(kvRanges).
		SetDAGRequest(e.dagPB).
		SetDesc(e.desc).
		SetKeepOrder(e.keepOrder).
		SetStreaming(e.streaming).
		SetFromSessionVars(e.ctx.GetSessionVars()).
		SetMemTracker(e.ctx, indexReaderDistSQLTrackerLabel).
		Build()
	if err != nil {
		e.feedback.Invalidate()
		return err
	}
	e.result, err = e.SelectResult(ctx, e.ctx, kvReq, retTypes(e), e.feedback, getPhysicalPlanIDs(e.plans))
	if err != nil {
		e.feedback.Invalidate()
		return err
	}
	e.result.Fetch(ctx)
	return nil
}

// IndexLookUpExecutor implements double read for index scan.
type IndexLookUpExecutor struct {
	baseExecutor

	table     table.Table
	index     *model.IndexInfo
	keepOrder bool
	desc      bool
	ranges    []*ranger.Range
	dagPB     *tipb.DAGRequest
	// handleIdx is the index of handle, which is only used for case of keeping order.
	handleIdx    int
	tableRequest *tipb.DAGRequest
	// columns are only required by union scan.
	columns        []*model.ColumnInfo
	indexStreaming bool
	tableStreaming bool
	*dataReaderBuilder
	// All fields above are immutable.

	idxWorkerWg sync.WaitGroup
	tblWorkerWg sync.WaitGroup
	finished    chan struct{}

	kvRanges      []kv.KeyRange
	workerStarted bool

	resultCh   chan *lookupTableTask
	resultCurr *lookupTableTask
	feedback   *statistics.QueryFeedback

	// memTracker is used to track the memory usage of this executor.
	memTracker *memory.Tracker

	// isCheckOp is used to determine whether we need to check the consistency of the index data.
	isCheckOp bool

	corColInIdxSide bool
	idxPlans        []plannercore.PhysicalPlan
	corColInTblSide bool
	tblPlans        []plannercore.PhysicalPlan
	corColInAccess  bool
	idxCols         []*expression.Column
	colLens         []int
}

// Open implements the Executor Open interface.
func (e *IndexLookUpExecutor) Open(ctx context.Context) error {
	var err error
	if e.corColInAccess {
		e.ranges, err = rebuildIndexRanges(e.ctx, e.idxPlans[0].(*plannercore.PhysicalIndexScan), e.idxCols, e.colLens)
		if err != nil {
			return err
		}
	}
	e.kvRanges, err = distsql.IndexRangesToKVRanges(e.ctx.GetSessionVars().StmtCtx, getPhysicalTableID(e.table), e.index.ID, e.ranges, e.feedback)
	if err != nil {
		e.feedback.Invalidate()
		return err
	}
	err = e.open(ctx)
	if err != nil {
		e.feedback.Invalidate()
	}
	return err
}

func (e *IndexLookUpExecutor) open(ctx context.Context) error {
	// We have to initialize "memTracker" and other execution resources in here
	// instead of in function "Open", because this "IndexLookUpExecutor" may be
	// constructed by a "IndexLookUpJoin" and "Open" will not be called in that
	// situation.
	e.memTracker = memory.NewTracker(e.id, e.ctx.GetSessionVars().MemQuotaIndexLookupReader)
	e.memTracker.AttachTo(e.ctx.GetSessionVars().StmtCtx.MemTracker)

	e.finished = make(chan struct{})
	e.resultCh = make(chan *lookupTableTask, atomic.LoadInt32(&LookupTableTaskChannelSize))

	var err error
	if e.corColInIdxSide {
		e.dagPB.Executors, _, err = constructDistExec(e.ctx, e.idxPlans)
		if err != nil {
			return err
		}
	}

	if e.corColInTblSide {
		e.tableRequest.Executors, _, err = constructDistExec(e.ctx, e.tblPlans)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *IndexLookUpExecutor) startWorkers(ctx context.Context, initBatchSize int) error {
	// indexWorker will write to workCh and tableWorker will read from workCh,
	// so fetching index and getting table data can run concurrently.
	workCh := make(chan *lookupTableTask, 1)
	if err := e.startIndexWorker(ctx, e.kvRanges, workCh, initBatchSize); err != nil {
		return err
	}
	e.startTableWorker(ctx, workCh)
	e.workerStarted = true
	return nil
}

var indexLookupDistSQLTrackerLabel fmt.Stringer = stringutil.StringerStr("IndexLookupDistSQLTracker")

// startIndexWorker launch a background goroutine to fetch handles, send the results to workCh.
func (e *IndexLookUpExecutor) startIndexWorker(ctx context.Context, kvRanges []kv.KeyRange, workCh chan<- *lookupTableTask, initBatchSize int) error {
	if e.runtimeStats != nil {
		collExec := true
		e.dagPB.CollectExecutionSummaries = &collExec
	}

	var builder distsql.RequestBuilder
	kvReq, err := builder.SetKeyRanges(kvRanges).
		SetDAGRequest(e.dagPB).
		SetDesc(e.desc).
		SetKeepOrder(e.keepOrder).
		SetStreaming(e.indexStreaming).
		SetFromSessionVars(e.ctx.GetSessionVars()).
		SetMemTracker(e.ctx, indexLookupDistSQLTrackerLabel).
		Build()
	if err != nil {
		return err
	}
	// Since the first read only need handle information. So its returned col is only 1.
	result, err := distsql.SelectWithRuntimeStats(ctx, e.ctx, kvReq, []*types.FieldType{types.NewFieldType(mysql.TypeLonglong)}, e.feedback, getPhysicalPlanIDs(e.idxPlans))
	if err != nil {
		return err
	}
	result.Fetch(ctx)
	worker := &indexWorker{
		idxLookup:    e,
		workCh:       workCh,
		finished:     e.finished,
		resultCh:     e.resultCh,
		keepOrder:    e.keepOrder,
		batchSize:    initBatchSize,
		maxBatchSize: e.ctx.GetSessionVars().IndexLookupSize,
		maxChunkSize: e.maxChunkSize,
	}
	if worker.batchSize > worker.maxBatchSize {
		worker.batchSize = worker.maxBatchSize
	}
	e.idxWorkerWg.Add(1)
	go func() {
		ctx1, cancel := context.WithCancel(ctx)
		count, err := worker.fetchHandles(ctx1, result)
		if err != nil {
			e.feedback.Invalidate()
		}
		cancel()
		if err := result.Close(); err != nil {
			logutil.Logger(ctx).Error("close Select result failed", zap.Error(err))
		}
		if e.runtimeStats != nil {
			copStats := e.ctx.GetSessionVars().StmtCtx.RuntimeStatsColl.GetRootStats(e.idxPlans[len(e.idxPlans)-1].ExplainID().String())
			copStats.SetRowNum(count)
			copStats = e.ctx.GetSessionVars().StmtCtx.RuntimeStatsColl.GetRootStats(e.tblPlans[0].ExplainID().String())
			copStats.SetRowNum(count)
		}
		e.ctx.StoreQueryFeedback(e.feedback)
		close(workCh)
		close(e.resultCh)
		e.idxWorkerWg.Done()
	}()
	return nil
}

var tableWorkerLabel fmt.Stringer = stringutil.StringerStr("tableWorker")

// startTableWorker launchs some background goroutines which pick tasks from workCh and execute the task.
func (e *IndexLookUpExecutor) startTableWorker(ctx context.Context, workCh <-chan *lookupTableTask) {
	lookupConcurrencyLimit := e.ctx.GetSessionVars().IndexLookupConcurrency
	e.tblWorkerWg.Add(lookupConcurrencyLimit)
	for i := 0; i < lookupConcurrencyLimit; i++ {
		worker := &tableWorker{
			idxLookup:      e,
			workCh:         workCh,
			finished:       e.finished,
			buildTblReader: e.buildTableReader,
			keepOrder:      e.keepOrder,
			handleIdx:      e.handleIdx,
			isCheckOp:      e.isCheckOp,
			memTracker:     memory.NewTracker(tableWorkerLabel, -1),
		}
		worker.memTracker.AttachTo(e.memTracker)
		ctx1, cancel := context.WithCancel(ctx)
		go func() {
			worker.pickAndExecTask(ctx1)
			cancel()
			e.tblWorkerWg.Done()
		}()
	}
}

func (e *IndexLookUpExecutor) buildTableReader(ctx context.Context, handles []int64) (Executor, error) {
	tableReaderExec := &TableReaderExecutor{
		baseExecutor:   newBaseExecutor(e.ctx, e.schema, stringutil.MemoizeStr(func() string { return e.id.String() + "_tableReader" })),
		table:          e.table,
		dagPB:          e.tableRequest,
		streaming:      e.tableStreaming,
		feedback:       statistics.NewQueryFeedback(0, nil, 0, false),
		corColInFilter: e.corColInTblSide,
		plans:          e.tblPlans,
	}
	tableReader, err := e.dataReaderBuilder.buildTableReaderFromHandles(ctx, tableReaderExec, handles)
	if err != nil {
		logutil.Logger(ctx).Error("build table reader from handles failed", zap.Error(err))
		return nil, err
	}
	return tableReader, nil
}

// Close implements Exec Close interface.
func (e *IndexLookUpExecutor) Close() error {
	if !e.workerStarted || e.finished == nil {
		return nil
	}

	close(e.finished)
	// Drain the resultCh and discard the result, in case that Next() doesn't fully
	// consume the data, background worker still writing to resultCh and block forever.
	for range e.resultCh {
	}
	e.idxWorkerWg.Wait()
	e.tblWorkerWg.Wait()
	e.finished = nil
	e.workerStarted = false
	e.memTracker.Detach()
	e.memTracker = nil
	if e.runtimeStats != nil {
		copStats := e.ctx.GetSessionVars().StmtCtx.RuntimeStatsColl.GetRootStats(e.idxPlans[0].ExplainID().String())
		copStats.SetRowNum(e.feedback.Actual())
	}
	return nil
}

// Next implements Exec Next interface.
func (e *IndexLookUpExecutor) Next(ctx context.Context, req *chunk.Chunk) error {
	if e.runtimeStats != nil {
		start := time.Now()
		defer func() { e.runtimeStats.Record(time.Since(start), req.NumRows()) }()
	}
	if !e.workerStarted {
		if err := e.startWorkers(ctx, req.RequiredRows()); err != nil {
			return err
		}
	}
	req.Reset()
	for {
		resultTask, err := e.getResultTask()
		if err != nil {
			return err
		}
		if resultTask == nil {
			return nil
		}
		for resultTask.cursor < len(resultTask.rows) {
			req.AppendRow(resultTask.rows[resultTask.cursor])
			resultTask.cursor++
			if req.IsFull() {
				return nil
			}
		}
	}
}

func (e *IndexLookUpExecutor) getResultTask() (*lookupTableTask, error) {
	if e.resultCurr != nil && e.resultCurr.cursor < len(e.resultCurr.rows) {
		return e.resultCurr, nil
	}
	task, ok := <-e.resultCh
	if !ok {
		return nil, nil
	}
	if err := <-task.doneCh; err != nil {
		return nil, err
	}

	// Release the memory usage of last task before we handle a new task.
	if e.resultCurr != nil {
		e.resultCurr.memTracker.Consume(-e.resultCurr.memUsage)
	}
	e.resultCurr = task
	return e.resultCurr, nil
}

// indexWorker is used by IndexLookUpExecutor to maintain index lookup background goroutines.
type indexWorker struct {
	idxLookup *IndexLookUpExecutor
	workCh    chan<- *lookupTableTask
	finished  <-chan struct{}
	resultCh  chan<- *lookupTableTask
	keepOrder bool

	// batchSize is for lightweight startup. It will be increased exponentially until reaches the max batch size value.
	batchSize    int
	maxBatchSize int
	maxChunkSize int
}

// fetchHandles fetches a batch of handles from index data and builds the index lookup tasks.
// The tasks are sent to workCh to be further processed by tableWorker, and sent to e.resultCh
// at the same time to keep data ordered.
func (w *indexWorker) fetchHandles(ctx context.Context, result distsql.SelectResult) (count int64, err error) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			stackSize := runtime.Stack(buf, false)
			buf = buf[:stackSize]
			logutil.Logger(ctx).Error("indexWorker in IndexLookupExecutor panicked", zap.String("stack", string(buf)))
			err4Panic := errors.Errorf("%v", r)
			doneCh := make(chan error, 1)
			doneCh <- err4Panic
			w.resultCh <- &lookupTableTask{
				doneCh: doneCh,
			}
			if err != nil {
				err = errors.Trace(err4Panic)
			}
		}
	}()
	chk := chunk.NewChunkWithCapacity([]*types.FieldType{types.NewFieldType(mysql.TypeLonglong)}, w.idxLookup.maxChunkSize)
	for {
		handles, err := w.extractTaskHandles(ctx, chk, result)
		if err != nil {
			doneCh := make(chan error, 1)
			doneCh <- err
			w.resultCh <- &lookupTableTask{
				doneCh: doneCh,
			}
			return count, err
		}
		if len(handles) == 0 {
			return count, nil
		}
		count += int64(len(handles))
		task := w.buildTableTask(handles)
		select {
		case <-ctx.Done():
			return count, nil
		case <-w.finished:
			return count, nil
		case w.workCh <- task:
			w.resultCh <- task
		}
	}
}

func (w *indexWorker) extractTaskHandles(ctx context.Context, chk *chunk.Chunk, idxResult distsql.SelectResult) (handles []int64, err error) {
	handles = make([]int64, 0, w.batchSize)
	for len(handles) < w.batchSize {
		chk.SetRequiredRows(w.batchSize-len(handles), w.maxChunkSize)
		err = idxResult.Next(ctx, chk)
		if err != nil {
			return handles, err
		}
		if chk.NumRows() == 0 {
			return handles, nil
		}
		for i := 0; i < chk.NumRows(); i++ {
			handles = append(handles, chk.GetRow(i).GetInt64(0))
		}
	}
	w.batchSize *= 2
	if w.batchSize > w.maxBatchSize {
		w.batchSize = w.maxBatchSize
	}
	return handles, nil
}

func (w *indexWorker) buildTableTask(handles []int64) *lookupTableTask {
	var indexOrder map[int64]int
	if w.keepOrder {
		// Save the index order.
		indexOrder = make(map[int64]int, len(handles))
		for i, h := range handles {
			indexOrder[h] = i
		}
	}
	task := &lookupTableTask{
		handles:    handles,
		indexOrder: indexOrder,
	}
	task.doneCh = make(chan error, 1)
	return task
}

// tableWorker is used by IndexLookUpExecutor to maintain table lookup background goroutines.
type tableWorker struct {
	idxLookup      *IndexLookUpExecutor
	workCh         <-chan *lookupTableTask
	finished       <-chan struct{}
	buildTblReader func(ctx context.Context, handles []int64) (Executor, error)
	keepOrder      bool
	handleIdx      int

	// memTracker is used to track the memory usage of this executor.
	memTracker *memory.Tracker

	// isCheckOp is used to determine whether we need to check the consistency of the index data.
	isCheckOp bool
}

// pickAndExecTask picks tasks from workCh, and execute them.
func (w *tableWorker) pickAndExecTask(ctx context.Context) {
	var task *lookupTableTask
	var ok bool
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			stackSize := runtime.Stack(buf, false)
			buf = buf[:stackSize]
			logutil.Logger(ctx).Error("tableWorker in IndexLookUpExecutor panicked", zap.String("stack", string(buf)))
			task.doneCh <- errors.Errorf("%v", r)
		}
	}()
	for {
		// Don't check ctx.Done() on purpose. If background worker get the signal and all
		// exit immediately, session's goroutine doesn't know this and still calling Next(),
		// it may block reading task.doneCh forever.
		select {
		case task, ok = <-w.workCh:
			if !ok {
				return
			}
		case <-w.finished:
			return
		}
		err := w.executeTask(ctx, task)
		task.doneCh <- err
	}
}

// executeTask executes the table look up tasks. We will construct a table reader and send request by handles.
// Then we hold the returning rows and finish this task.
func (w *tableWorker) executeTask(ctx context.Context, task *lookupTableTask) error {
	tableReader, err := w.buildTblReader(ctx, task.handles)
	if err != nil {
		logutil.Logger(ctx).Error("build table reader failed", zap.Error(err))
		return err
	}
	defer terror.Call(tableReader.Close)

	task.memTracker = w.memTracker
	memUsage := int64(cap(task.handles) * 8)
	task.memUsage = memUsage
	task.memTracker.Consume(memUsage)
	handleCnt := len(task.handles)
	task.rows = make([]chunk.Row, 0, handleCnt)
	for {
		chk := newFirstChunk(tableReader)
		err = tableReader.Next(ctx, chk)
		if err != nil {
			logutil.Logger(ctx).Error("table reader fetch next chunk failed", zap.Error(err))
			return err
		}
		if chk.NumRows() == 0 {
			break
		}
		memUsage = chk.MemoryUsage()
		task.memUsage += memUsage
		task.memTracker.Consume(memUsage)
		iter := chunk.NewIterator4Chunk(chk)
		for row := iter.Begin(); row != iter.End(); row = iter.Next() {
			task.rows = append(task.rows, row)
		}
	}

	memUsage = int64(cap(task.rows)) * int64(unsafe.Sizeof(chunk.Row{}))
	task.memUsage += memUsage
	task.memTracker.Consume(memUsage)
	if w.keepOrder {
		task.rowIdx = make([]int, 0, len(task.rows))
		for i := range task.rows {
			handle := task.rows[i].GetInt64(w.handleIdx)
			task.rowIdx = append(task.rowIdx, task.indexOrder[handle])
		}
		memUsage = int64(cap(task.rowIdx) * 4)
		task.memUsage += memUsage
		task.memTracker.Consume(memUsage)
		sort.Sort(task)
	}

	if handleCnt != len(task.rows) {
		if w.isCheckOp {
			obtainedHandlesMap := make(map[int64]struct{}, len(task.rows))
			for _, row := range task.rows {
				handle := row.GetInt64(w.handleIdx)
				obtainedHandlesMap[handle] = struct{}{}
			}
			return errors.Errorf("inconsistent index %s handle count %d isn't equal to value count %d, missing handles %v in a batch",
				w.idxLookup.index.Name.O, handleCnt, len(task.rows), GetLackHandles(task.handles, obtainedHandlesMap))
		}

		if len(w.idxLookup.tblPlans) == 1 {
			obtainedHandlesMap := make(map[int64]struct{}, len(task.rows))
			for _, row := range task.rows {
				handle := row.GetInt64(w.handleIdx)
				obtainedHandlesMap[handle] = struct{}{}
			}

			logutil.Logger(ctx).Error("inconsistent index handles", zap.String("index", w.idxLookup.index.Name.O),
				zap.Int("index_cnt", handleCnt), zap.Int("table_cnt", len(task.rows)),
				zap.Int64s("missing_handles", GetLackHandles(task.handles, obtainedHandlesMap)),
				zap.Int64s("total_handles", task.handles))

			// table scan in double read can never has conditions according to convertToIndexScan.
			// if this table scan has no condition, the number of rows it returns must equal to the length of handles.
			return errors.Errorf("inconsistent index %s handle count %d isn't equal to value count %d",
				w.idxLookup.index.Name.O, handleCnt, len(task.rows))
		}
	}

	return nil
}

// GetLackHandles gets the handles in expectedHandles but not in obtainedHandlesMap.
func GetLackHandles(expectedHandles []int64, obtainedHandlesMap map[int64]struct{}) []int64 {
	diffCnt := len(expectedHandles) - len(obtainedHandlesMap)
	diffHandles := make([]int64, 0, diffCnt)
	var cnt int
	for _, handle := range expectedHandles {
		isExist := false
		if _, ok := obtainedHandlesMap[handle]; ok {
			delete(obtainedHandlesMap, handle)
			isExist = true
		}
		if !isExist {
			diffHandles = append(diffHandles, handle)
			cnt++
			if cnt == diffCnt {
				break
			}
		}
	}

	return diffHandles
}

func getPhysicalPlanIDs(plans []plannercore.PhysicalPlan) []fmt.Stringer {
	planIDs := make([]fmt.Stringer, 0, len(plans))
	for _, p := range plans {
		planIDs = append(planIDs, p.ExplainID())
	}
	return planIDs
}
