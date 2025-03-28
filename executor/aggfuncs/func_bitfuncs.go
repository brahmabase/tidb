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

package aggfuncs

import (
	"math"

	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/chunk"
)

type baseBitAggFunc struct {
	baseAggFunc
}

type partialResult4BitFunc = uint64

func (e *baseBitAggFunc) AllocPartialResult() PartialResult {
	return PartialResult(new(partialResult4BitFunc))
}

func (e *baseBitAggFunc) ResetPartialResult(pr PartialResult) {
	p := (*partialResult4BitFunc)(pr)
	*p = 0
}

func (e *baseBitAggFunc) AppendFinalResult2Chunk(sctx sessionctx.Context, pr PartialResult, chk *chunk.Chunk) error {
	p := (*partialResult4BitFunc)(pr)
	chk.AppendUint64(e.ordinal, *p)
	return nil
}

type bitOrUint64 struct {
	baseBitAggFunc
}

func (e *bitOrUint64) UpdatePartialResult(sctx sessionctx.Context, rowsInGroup []chunk.Row, pr PartialResult) error {
	p := (*partialResult4BitFunc)(pr)
	for _, row := range rowsInGroup {
		inputValue, isNull, err := e.args[0].EvalInt(sctx, row)
		if err != nil {
			return err
		}
		if isNull {
			continue
		}
		*p |= uint64(inputValue)
	}
	return nil
}

func (*bitOrUint64) MergePartialResult(sctx sessionctx.Context, src, dst PartialResult) error {
	p1, p2 := (*partialResult4BitFunc)(src), (*partialResult4BitFunc)(dst)
	*p2 |= uint64(*p1)
	return nil
}

type bitXorUint64 struct {
	baseBitAggFunc
}

func (e *bitXorUint64) UpdatePartialResult(sctx sessionctx.Context, rowsInGroup []chunk.Row, pr PartialResult) error {
	p := (*partialResult4BitFunc)(pr)
	for _, row := range rowsInGroup {
		inputValue, isNull, err := e.args[0].EvalInt(sctx, row)
		if err != nil {
			return err
		}
		if isNull {
			continue
		}
		*p ^= uint64(inputValue)
	}
	return nil
}

func (*bitXorUint64) MergePartialResult(sctx sessionctx.Context, src, dst PartialResult) error {
	p1, p2 := (*partialResult4BitFunc)(src), (*partialResult4BitFunc)(dst)
	*p2 ^= uint64(*p1)
	return nil
}

type bitAndUint64 struct {
	baseBitAggFunc
}

func (e *bitAndUint64) AllocPartialResult() PartialResult {
	p := new(partialResult4BitFunc)
	*p = math.MaxUint64
	return PartialResult(p)
}

func (e *bitAndUint64) ResetPartialResult(pr PartialResult) {
	p := (*partialResult4BitFunc)(pr)
	*p = math.MaxUint64
}

func (e *bitAndUint64) UpdatePartialResult(sctx sessionctx.Context, rowsInGroup []chunk.Row, pr PartialResult) error {
	p := (*partialResult4BitFunc)(pr)
	for _, row := range rowsInGroup {
		inputValue, isNull, err := e.args[0].EvalInt(sctx, row)
		if err != nil {
			return err
		}
		if isNull {
			continue
		}
		*p &= uint64(inputValue)
	}
	return nil
}

func (*bitAndUint64) MergePartialResult(sctx sessionctx.Context, src, dst PartialResult) error {
	p1, p2 := (*partialResult4BitFunc)(src), (*partialResult4BitFunc)(dst)
	*p2 &= uint64(*p1)
	return nil
}
