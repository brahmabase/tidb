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

package statistics

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

// Histogram represents statistics for a column or index.
type Histogram struct {
	ID        int64 // Column ID.
	NDV       int64 // Number of distinct values.
	NullCount int64 // Number of null values.
	// LastUpdateVersion is the version that this histogram updated last time.
	LastUpdateVersion uint64

	Tp *types.FieldType

	// Histogram elements.
	//
	// A bucket bound is the smallest and greatest values stored in the bucket. The lower and upper bound
	// are stored in one column.
	//
	// A bucket count is the number of items stored in all previous buckets and the current bucket.
	// Bucket counts are always in increasing order.
	//
	// A bucket repeat is the number of repeats of the bucket value, it can be used to find popular values.
	Bounds  *chunk.Chunk
	Buckets []Bucket

	// Used for estimating fraction of the interval [lower, upper] that lies within the [lower, value].
	// For some types like `Int`, we do not build it because we can get them directly from `Bounds`.
	scalars []scalar
	// TotColSize is the total column size for the histogram.
	TotColSize int64

	// Correlation is the statistical correlation between physical row ordering and logical ordering of
	// the column values. This ranges from -1 to +1, and it is only valid for Column histogram, not for
	// Index histogram.
	Correlation float64
}

// Bucket store the bucket count and repeat.
type Bucket struct {
	Count  int64
	Repeat int64
}

type scalar struct {
	lower        float64
	upper        float64
	commonPfxLen int // commonPfxLen is the common prefix length of the lower bound and upper bound when the value type is KindString or KindBytes.
}

// NewHistogram creates a new histogram.
func NewHistogram(id, ndv, nullCount int64, version uint64, tp *types.FieldType, bucketSize int, totColSize int64) *Histogram {
	return &Histogram{
		ID:                id,
		NDV:               ndv,
		NullCount:         nullCount,
		LastUpdateVersion: version,
		Tp:                tp,
		Bounds:            chunk.NewChunkWithCapacity([]*types.FieldType{tp}, 2*bucketSize),
		Buckets:           make([]Bucket, 0, bucketSize),
		TotColSize:        totColSize,
	}
}

// GetLower gets the lower bound of bucket `idx`.
func (hg *Histogram) GetLower(idx int) *types.Datum {
	d := hg.Bounds.GetRow(2*idx).GetDatum(0, hg.Tp)
	return &d
}

// GetUpper gets the upper bound of bucket `idx`.
func (hg *Histogram) GetUpper(idx int) *types.Datum {
	d := hg.Bounds.GetRow(2*idx+1).GetDatum(0, hg.Tp)
	return &d
}

// AvgColSize is the average column size of the histogram.
func (c *Column) AvgColSize(count int64) float64 {
	if count == 0 {
		return 0
	}
	switch c.Histogram.Tp.Tp {
	case mysql.TypeFloat:
		return 4
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong,
		mysql.TypeDouble, mysql.TypeYear:
		return 8
	case mysql.TypeDuration, mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		return 16
	case mysql.TypeNewDecimal:
		return types.MyDecimalStructSize
	default:
		// Keep two decimal place.
		return math.Round(float64(c.TotColSize)/float64(count)*100) / 100
	}
}

// AppendBucket appends a bucket into `hg`.
func (hg *Histogram) AppendBucket(lower *types.Datum, upper *types.Datum, count, repeat int64) {
	hg.Buckets = append(hg.Buckets, Bucket{Count: count, Repeat: repeat})
	hg.Bounds.AppendDatum(0, lower)
	hg.Bounds.AppendDatum(0, upper)
}

func (hg *Histogram) updateLastBucket(upper *types.Datum, count, repeat int64) {
	len := hg.Len()
	hg.Bounds.TruncateTo(2*len - 1)
	hg.Bounds.AppendDatum(0, upper)
	hg.Buckets[len-1] = Bucket{Count: count, Repeat: repeat}
}

// DecodeTo decodes the histogram bucket values into `Tp`.
func (hg *Histogram) DecodeTo(tp *types.FieldType, timeZone *time.Location) error {
	oldIter := chunk.NewIterator4Chunk(hg.Bounds)
	hg.Bounds = chunk.NewChunkWithCapacity([]*types.FieldType{tp}, oldIter.Len())
	hg.Tp = tp
	for row := oldIter.Begin(); row != oldIter.End(); row = oldIter.Next() {
		datum, err := tablecodec.DecodeColumnValue(row.GetBytes(0), tp, timeZone)
		if err != nil {
			return errors.Trace(err)
		}
		hg.Bounds.AppendDatum(0, &datum)
	}
	return nil
}

// ConvertTo converts the histogram bucket values into `Tp`.
func (hg *Histogram) ConvertTo(sc *stmtctx.StatementContext, tp *types.FieldType) (*Histogram, error) {
	hist := NewHistogram(hg.ID, hg.NDV, hg.NullCount, hg.LastUpdateVersion, tp, hg.Len(), hg.TotColSize)
	hist.Correlation = hg.Correlation
	iter := chunk.NewIterator4Chunk(hg.Bounds)
	for row := iter.Begin(); row != iter.End(); row = iter.Next() {
		d := row.GetDatum(0, hg.Tp)
		d, err := d.ConvertTo(sc, tp)
		if err != nil {
			return nil, errors.Trace(err)
		}
		hist.Bounds.AppendDatum(0, &d)
	}
	hist.Buckets = hg.Buckets
	return hist, nil
}

// Len is the number of buckets in the histogram.
func (hg *Histogram) Len() int {
	return len(hg.Buckets)
}

// HistogramEqual tests if two histograms are equal.
func HistogramEqual(a, b *Histogram, ignoreID bool) bool {
	if ignoreID {
		old := b.ID
		b.ID = a.ID
		defer func() { b.ID = old }()
	}
	return bytes.Equal([]byte(a.ToString(0)), []byte(b.ToString(0)))
}

// constants for stats version. These const can be used for solving compatibility issue.
const (
	CurStatsVersion = Version1
	Version1        = 1
)

// AnalyzeFlag is set when the statistics comes from analyze and has not been modified by feedback.
const AnalyzeFlag = 1

// IsAnalyzed checks whether this flag contains AnalyzeFlag.
func IsAnalyzed(flag int64) bool {
	return (flag & AnalyzeFlag) > 0
}

// ResetAnalyzeFlag resets the AnalyzeFlag because it has been modified by feedback.
func ResetAnalyzeFlag(flag int64) int64 {
	return flag &^ AnalyzeFlag
}

// ValueToString converts a possible encoded value to a formatted string. If the value is encoded, then
// idxCols equals to number of origin values, else idxCols is 0.
func ValueToString(value *types.Datum, idxCols int) (string, error) {
	if idxCols == 0 {
		return value.ToString()
	}
	decodedVals, err := codec.DecodeRange(value.GetBytes(), idxCols)
	if err != nil {
		return "", errors.Trace(err)
	}
	str, err := types.DatumsToString(decodedVals, true)
	if err != nil {
		return "", errors.Trace(err)
	}
	return str, nil
}

// BucketToString change the given bucket to string format.
func (hg *Histogram) BucketToString(bktID, idxCols int) string {
	upperVal, err := ValueToString(hg.GetUpper(bktID), idxCols)
	terror.Log(errors.Trace(err))
	lowerVal, err := ValueToString(hg.GetLower(bktID), idxCols)
	terror.Log(errors.Trace(err))
	return fmt.Sprintf("num: %d lower_bound: %s upper_bound: %s repeats: %d", hg.bucketCount(bktID), lowerVal, upperVal, hg.Buckets[bktID].Repeat)
}

// ToString gets the string representation for the histogram.
func (hg *Histogram) ToString(idxCols int) string {
	strs := make([]string, 0, hg.Len()+1)
	if idxCols > 0 {
		strs = append(strs, fmt.Sprintf("index:%d ndv:%d", hg.ID, hg.NDV))
	} else {
		strs = append(strs, fmt.Sprintf("column:%d ndv:%d totColSize:%d", hg.ID, hg.NDV, hg.TotColSize))
	}
	for i := 0; i < hg.Len(); i++ {
		strs = append(strs, hg.BucketToString(i, idxCols))
	}
	return strings.Join(strs, "\n")
}

// equalRowCount estimates the row count where the column equals to value.
func (hg *Histogram) equalRowCount(value types.Datum) float64 {
	index, match := hg.Bounds.LowerBound(0, &value)
	// Since we store the lower and upper bound together, if the index is an odd number, then it points to a upper bound.
	if index%2 == 1 {
		if match {
			return float64(hg.Buckets[index/2].Repeat)
		}
		return hg.notNullCount() / float64(hg.NDV)
	}
	if match {
		cmp := chunk.GetCompareFunc(hg.Tp)
		if cmp(hg.Bounds.GetRow(index), 0, hg.Bounds.GetRow(index+1), 0) == 0 {
			return float64(hg.Buckets[index/2].Repeat)
		}
		return hg.notNullCount() / float64(hg.NDV)
	}
	return 0
}

// greaterRowCount estimates the row count where the column greater than value.
func (hg *Histogram) greaterRowCount(value types.Datum) float64 {
	gtCount := hg.notNullCount() - hg.lessRowCount(value) - hg.equalRowCount(value)
	if gtCount < 0 {
		gtCount = 0
	}
	return gtCount
}

// LessRowCountWithBktIdx estimates the row count where the column less than value.
func (hg *Histogram) LessRowCountWithBktIdx(value types.Datum) (float64, int) {
	// All the values are null.
	if hg.Bounds.NumRows() == 0 {
		return 0, 0
	}
	index, match := hg.Bounds.LowerBound(0, &value)
	if index == hg.Bounds.NumRows() {
		return hg.notNullCount(), hg.Len() - 1
	}
	// Since we store the lower and upper bound together, so dividing the index by 2 will get the bucket index.
	bucketIdx := index / 2
	curCount, curRepeat := float64(hg.Buckets[bucketIdx].Count), float64(hg.Buckets[bucketIdx].Repeat)
	preCount := float64(0)
	if bucketIdx > 0 {
		preCount = float64(hg.Buckets[bucketIdx-1].Count)
	}
	if index%2 == 1 {
		if match {
			return curCount - curRepeat, bucketIdx
		}
		return preCount + hg.calcFraction(bucketIdx, &value)*(curCount-curRepeat-preCount), bucketIdx
	}
	return preCount, bucketIdx
}

func (hg *Histogram) lessRowCount(value types.Datum) float64 {
	result, _ := hg.LessRowCountWithBktIdx(value)
	return result
}

// BetweenRowCount estimates the row count where column greater or equal to a and less than b.
func (hg *Histogram) BetweenRowCount(a, b types.Datum) float64 {
	lessCountA := hg.lessRowCount(a)
	lessCountB := hg.lessRowCount(b)
	// If lessCountA is not less than lessCountB, it may be that they fall to the same bucket and we cannot estimate
	// the fraction, so we use `totalCount / NDV` to estimate the row count, but the result should not greater than
	// lessCountB or notNullCount-lessCountA.
	if lessCountA >= lessCountB && hg.NDV > 0 {
		result := math.Min(lessCountB, hg.notNullCount()-lessCountA)
		return math.Min(result, hg.notNullCount()/float64(hg.NDV))
	}
	return lessCountB - lessCountA
}

// TotalRowCount returns the total count of this histogram.
func (hg *Histogram) TotalRowCount() float64 {
	return hg.notNullCount() + float64(hg.NullCount)
}

// notNullCount indicates the count of non-null values in column histogram and single-column index histogram,
// for multi-column index histogram, since we cannot define null for the row, we treat all rows as non-null, that means,
// notNullCount would return same value as TotalRowCount for multi-column index histograms.
func (hg *Histogram) notNullCount() float64 {
	if hg.Len() == 0 {
		return 0
	}
	return float64(hg.Buckets[hg.Len()-1].Count)
}

// mergeBuckets is used to Merge every two neighbor buckets.
func (hg *Histogram) mergeBuckets(bucketIdx int) {
	curBuck := 0
	c := chunk.NewChunkWithCapacity([]*types.FieldType{hg.Tp}, bucketIdx)
	for i := 0; i+1 <= bucketIdx; i += 2 {
		hg.Buckets[curBuck] = hg.Buckets[i+1]
		c.AppendDatum(0, hg.GetLower(i))
		c.AppendDatum(0, hg.GetUpper(i+1))
		curBuck++
	}
	if bucketIdx%2 == 0 {
		hg.Buckets[curBuck] = hg.Buckets[bucketIdx]
		c.AppendDatum(0, hg.GetLower(bucketIdx))
		c.AppendDatum(0, hg.GetUpper(bucketIdx))
		curBuck++
	}
	hg.Bounds = c
	hg.Buckets = hg.Buckets[:curBuck]
	return
}

// GetIncreaseFactor will return a factor of data increasing after the last analysis.
func (hg *Histogram) GetIncreaseFactor(totalCount int64) float64 {
	columnCount := hg.TotalRowCount()
	if columnCount == 0 {
		// avoid dividing by 0
		return 1.0
	}
	return float64(totalCount) / columnCount
}

// validRange checks if the range is Valid, it is used by `SplitRange` to remove the invalid range,
// the possible types of range are index key range and handle key range.
func validRange(sc *stmtctx.StatementContext, ran *ranger.Range, encoded bool) bool {
	var low, high []byte
	if encoded {
		low, high = ran.LowVal[0].GetBytes(), ran.HighVal[0].GetBytes()
	} else {
		var err error
		low, err = codec.EncodeKey(sc, nil, ran.LowVal[0])
		if err != nil {
			return false
		}
		high, err = codec.EncodeKey(sc, nil, ran.HighVal[0])
		if err != nil {
			return false
		}
	}
	if ran.LowExclude {
		low = kv.Key(low).PrefixNext()
	}
	if !ran.HighExclude {
		high = kv.Key(high).PrefixNext()
	}
	return bytes.Compare(low, high) < 0
}

func checkKind(vals []types.Datum, kind byte) bool {
	if kind == types.KindString {
		kind = types.KindBytes
	}
	for _, val := range vals {
		valKind := val.Kind()
		if valKind == types.KindNull || valKind == types.KindMinNotNull || valKind == types.KindMaxValue {
			continue
		}
		if valKind == types.KindString {
			valKind = types.KindBytes
		}
		if valKind != kind {
			return false
		}
		// Only check the first non-null value.
		break
	}
	return true
}

func (hg *Histogram) typeMatch(ranges []*ranger.Range) bool {
	kind := hg.GetLower(0).Kind()
	for _, ran := range ranges {
		if !checkKind(ran.LowVal, kind) || !checkKind(ran.HighVal, kind) {
			return false
		}
	}
	return true
}

// SplitRange splits the range according to the histogram upper bound. Note that we treat last bucket's upper bound
// as inf, so all the split Ranges will totally fall in one of the (-inf, u(0)], (u(0), u(1)],...(u(n-3), u(n-2)],
// (u(n-2), +inf), where n is the number of buckets, u(i) is the i-th bucket's upper bound.
func (hg *Histogram) SplitRange(sc *stmtctx.StatementContext, oldRanges []*ranger.Range, encoded bool) ([]*ranger.Range, bool) {
	if !hg.typeMatch(oldRanges) {
		return oldRanges, false
	}
	ranges := make([]*ranger.Range, 0, len(oldRanges))
	for _, ran := range oldRanges {
		ranges = append(ranges, ran.Clone())
	}
	split := make([]*ranger.Range, 0, len(ranges))
	for len(ranges) > 0 {
		// Find the last bound that greater or equal to the LowVal.
		idx := hg.Bounds.UpperBound(0, &ranges[0].LowVal[0])
		if !ranges[0].LowExclude && idx > 0 {
			cmp := chunk.Compare(hg.Bounds.GetRow(idx-1), 0, &ranges[0].LowVal[0])
			if cmp == 0 {
				idx--
			}
		}
		// Treat last bucket's upper bound as inf, so we do not need split any more.
		if idx >= hg.Bounds.NumRows()-2 {
			split = append(split, ranges...)
			break
		}
		// Get the corresponding upper bound.
		if idx%2 == 0 {
			idx++
		}
		upperBound := hg.Bounds.GetRow(idx)
		var i int
		// Find the first range that need to be split by the upper bound.
		for ; i < len(ranges); i++ {
			if chunk.Compare(upperBound, 0, &ranges[i].HighVal[0]) < 0 {
				break
			}
		}
		split = append(split, ranges[:i]...)
		ranges = ranges[i:]
		if len(ranges) == 0 {
			break
		}
		// Split according to the upper bound.
		cmp := chunk.Compare(upperBound, 0, &ranges[0].LowVal[0])
		if cmp > 0 || (cmp == 0 && !ranges[0].LowExclude) {
			upper := upperBound.GetDatum(0, hg.Tp)
			split = append(split, &ranger.Range{
				LowExclude:  ranges[0].LowExclude,
				LowVal:      []types.Datum{ranges[0].LowVal[0]},
				HighVal:     []types.Datum{upper},
				HighExclude: false})
			ranges[0].LowVal[0] = upper
			ranges[0].LowExclude = true
			if !validRange(sc, ranges[0], encoded) {
				ranges = ranges[1:]
			}
		}
	}
	return split, true
}

func (hg *Histogram) bucketCount(idx int) int64 {
	if idx == 0 {
		return hg.Buckets[0].Count
	}
	return hg.Buckets[idx].Count - hg.Buckets[idx-1].Count
}

// HistogramToProto converts Histogram to its protobuf representation.
// Note that when this is used, the lower/upper bound in the bucket must be BytesDatum.
func HistogramToProto(hg *Histogram) *tipb.Histogram {
	protoHg := &tipb.Histogram{
		Ndv: hg.NDV,
	}
	for i := 0; i < hg.Len(); i++ {
		bkt := &tipb.Bucket{
			Count:      hg.Buckets[i].Count,
			LowerBound: hg.GetLower(i).GetBytes(),
			UpperBound: hg.GetUpper(i).GetBytes(),
			Repeats:    hg.Buckets[i].Repeat,
		}
		protoHg.Buckets = append(protoHg.Buckets, bkt)
	}
	return protoHg
}

// HistogramFromProto converts Histogram from its protobuf representation.
// Note that we will set BytesDatum for the lower/upper bound in the bucket, the decode will
// be after all histograms merged.
func HistogramFromProto(protoHg *tipb.Histogram) *Histogram {
	tp := types.NewFieldType(mysql.TypeBlob)
	hg := NewHistogram(0, protoHg.Ndv, 0, 0, tp, len(protoHg.Buckets), 0)
	for _, bucket := range protoHg.Buckets {
		lower, upper := types.NewBytesDatum(bucket.LowerBound), types.NewBytesDatum(bucket.UpperBound)
		hg.AppendBucket(&lower, &upper, bucket.Count, bucket.Repeats)
	}
	return hg
}

func (hg *Histogram) popFirstBucket() {
	hg.Buckets = hg.Buckets[1:]
	c := chunk.NewChunkWithCapacity([]*types.FieldType{hg.Tp, hg.Tp}, hg.Bounds.NumRows()-2)
	c.Append(hg.Bounds, 2, hg.Bounds.NumRows())
	hg.Bounds = c
}

// IsIndexHist checks whether current histogram is one for index.
func (hg *Histogram) IsIndexHist() bool {
	return hg.Tp.Tp == mysql.TypeBlob
}

// MergeHistograms merges two histograms.
func MergeHistograms(sc *stmtctx.StatementContext, lh *Histogram, rh *Histogram, bucketSize int) (*Histogram, error) {
	if lh.Len() == 0 {
		return rh, nil
	}
	if rh.Len() == 0 {
		return lh, nil
	}
	lh.NDV += rh.NDV
	lLen := lh.Len()
	cmp, err := lh.GetUpper(lLen-1).CompareDatum(sc, rh.GetLower(0))
	if err != nil {
		return nil, errors.Trace(err)
	}
	offset := int64(0)
	if cmp == 0 {
		lh.NDV--
		lh.updateLastBucket(rh.GetUpper(0), lh.Buckets[lLen-1].Count+rh.Buckets[0].Count, rh.Buckets[0].Repeat)
		offset = rh.Buckets[0].Count
		rh.popFirstBucket()
	}
	for lh.Len() > bucketSize {
		lh.mergeBuckets(lh.Len() - 1)
	}
	if rh.Len() == 0 {
		return lh, nil
	}
	for rh.Len() > bucketSize {
		rh.mergeBuckets(rh.Len() - 1)
	}
	lCount := lh.Buckets[lh.Len()-1].Count
	rCount := rh.Buckets[rh.Len()-1].Count - offset
	lAvg := float64(lCount) / float64(lh.Len())
	rAvg := float64(rCount) / float64(rh.Len())
	for lh.Len() > 1 && lAvg*2 <= rAvg {
		lh.mergeBuckets(lh.Len() - 1)
		lAvg *= 2
	}
	for rh.Len() > 1 && rAvg*2 <= lAvg {
		rh.mergeBuckets(rh.Len() - 1)
		rAvg *= 2
	}
	for i := 0; i < rh.Len(); i++ {
		lh.AppendBucket(rh.GetLower(i), rh.GetUpper(i), rh.Buckets[i].Count+lCount-offset, rh.Buckets[i].Repeat)
	}
	for lh.Len() > bucketSize {
		lh.mergeBuckets(lh.Len() - 1)
	}
	return lh, nil
}

// AvgCountPerNotNullValue gets the average row count per value by the data of histogram.
func (hg *Histogram) AvgCountPerNotNullValue(totalCount int64) float64 {
	factor := hg.GetIncreaseFactor(totalCount)
	totalNotNull := hg.notNullCount() * factor
	curNDV := float64(hg.NDV) * factor
	if curNDV == 0 {
		curNDV = 1
	}
	return totalNotNull / curNDV
}

func (hg *Histogram) outOfRange(val types.Datum) bool {
	if hg.Len() == 0 {
		return true
	}
	return chunk.Compare(hg.Bounds.GetRow(0), 0, &val) > 0 ||
		chunk.Compare(hg.Bounds.GetRow(hg.Bounds.NumRows()-1), 0, &val) < 0
}

// Copy deep copies the histogram.
func (hg *Histogram) Copy() *Histogram {
	newHist := *hg
	newHist.Bounds = hg.Bounds.CopyConstruct()
	newHist.Buckets = make([]Bucket, 0, len(hg.Buckets))
	for _, bkt := range hg.Buckets {
		newHist.Buckets = append(newHist.Buckets, bkt)
	}
	return &newHist
}

// RemoveUpperBound removes the upper bound from histogram.
// It is used when merge stats for incremental analyze.
func (hg *Histogram) RemoveUpperBound() *Histogram {
	hg.Buckets[hg.Len()-1].Count -= hg.Buckets[hg.Len()-1].Repeat
	hg.Buckets[hg.Len()-1].Repeat = 0
	return hg
}

// TruncateHistogram truncates the histogram to `numBkt` buckets.
func (hg *Histogram) TruncateHistogram(numBkt int) *Histogram {
	hist := hg.Copy()
	hist.Buckets = hist.Buckets[:numBkt]
	hist.Bounds.TruncateTo(numBkt * 2)
	return hist
}

// ErrorRate is the error rate of estimate row count by bucket and cm sketch.
type ErrorRate struct {
	ErrorTotal float64
	QueryTotal int64
}

// MaxErrorRate is the max error rate of estimate row count of a not pseudo column.
// If the table is pseudo, but the average error rate is less than MaxErrorRate,
// then the column is not pseudo.
const MaxErrorRate = 0.25

// NotAccurate is true when the total of query is zero or the average error
// rate is greater than MaxErrorRate.
func (e *ErrorRate) NotAccurate() bool {
	if e.QueryTotal == 0 {
		return true
	}
	return e.ErrorTotal/float64(e.QueryTotal) > MaxErrorRate
}

// Update updates the ErrorRate.
func (e *ErrorRate) Update(rate float64) {
	e.QueryTotal++
	e.ErrorTotal += rate
}

// Merge range merges two ErrorRate.
func (e *ErrorRate) Merge(rate *ErrorRate) {
	e.QueryTotal += rate.QueryTotal
	e.ErrorTotal += rate.ErrorTotal
}

// Column represents a column histogram.
type Column struct {
	Histogram
	*CMSketch
	PhysicalID int64
	Count      int64
	Info       *model.ColumnInfo
	IsHandle   bool
	ErrorRate
	Flag           int64
	LastAnalyzePos types.Datum
}

func (c *Column) String() string {
	return c.Histogram.ToString(0)
}

// HistogramNeededColumns stores the columns whose Histograms need to be loaded from physical kv layer.
// Currently, we only load index/pk's Histogram from kv automatically. Columns' are loaded by needs.
var HistogramNeededColumns = neededColumnMap{cols: map[tableColumnID]struct{}{}}

// IsInvalid checks if this column is invalid. If this column has histogram but not loaded yet, then we mark it
// as need histogram.
func (c *Column) IsInvalid(sc *stmtctx.StatementContext, collPseudo bool) bool {
	if collPseudo && c.NotAccurate() {
		return true
	}
	if c.NDV > 0 && c.Len() == 0 && sc != nil {
		sc.SetHistogramsNotLoad()
		HistogramNeededColumns.insert(tableColumnID{TableID: c.PhysicalID, ColumnID: c.Info.ID})
	}
	return c.TotalRowCount() == 0 || (c.NDV > 0 && c.Len() == 0)
}

func (c *Column) equalRowCount(sc *stmtctx.StatementContext, val types.Datum, modifyCount int64) (float64, error) {
	if val.IsNull() {
		return float64(c.NullCount), nil
	}
	// All the values are null.
	if c.Histogram.Bounds.NumRows() == 0 {
		return 0.0, nil
	}
	if c.NDV > 0 && c.outOfRange(val) {
		return float64(modifyCount) / float64(c.NDV), nil
	}
	if c.CMSketch != nil {
		count, err := c.CMSketch.queryValue(sc, val)
		return float64(count), errors.Trace(err)
	}
	return c.Histogram.equalRowCount(val), nil
}

// GetColumnRowCount estimates the row count by a slice of Range.
func (c *Column) GetColumnRowCount(sc *stmtctx.StatementContext, ranges []*ranger.Range, modifyCount int64) (float64, error) {
	var rowCount float64
	for _, rg := range ranges {
		cmp, err := rg.LowVal[0].CompareDatum(sc, &rg.HighVal[0])
		if err != nil {
			return 0, errors.Trace(err)
		}
		if cmp == 0 {
			// the point case.
			if !rg.LowExclude && !rg.HighExclude {
				var cnt float64
				cnt, err = c.equalRowCount(sc, rg.LowVal[0], modifyCount)
				if err != nil {
					return 0, errors.Trace(err)
				}
				rowCount += cnt
			}
			continue
		}
		// The interval case.
		cnt := c.BetweenRowCount(rg.LowVal[0], rg.HighVal[0])
		if (c.outOfRange(rg.LowVal[0]) && !rg.LowVal[0].IsNull()) || c.outOfRange(rg.HighVal[0]) {
			cnt += float64(modifyCount) / outOfRangeBetweenRate
		}
		// `betweenRowCount` returns count for [l, h) range, we adjust cnt for boudaries here.
		// Note that, `cnt` does not include null values, we need specially handle cases
		// where null is the lower bound.
		if rg.LowExclude && !rg.LowVal[0].IsNull() {
			lowCnt, err := c.equalRowCount(sc, rg.LowVal[0], modifyCount)
			if err != nil {
				return 0, errors.Trace(err)
			}
			cnt -= lowCnt
		}
		if !rg.LowExclude && rg.LowVal[0].IsNull() {
			cnt += float64(c.NullCount)
		}
		if !rg.HighExclude {
			highCnt, err := c.equalRowCount(sc, rg.HighVal[0], modifyCount)
			if err != nil {
				return 0, errors.Trace(err)
			}
			cnt += highCnt
		}
		rowCount += cnt
	}
	if rowCount > c.TotalRowCount() {
		rowCount = c.TotalRowCount()
	} else if rowCount < 0 {
		rowCount = 0
	}
	return rowCount, nil
}

// Index represents an index histogram.
type Index struct {
	Histogram
	*CMSketch
	ErrorRate
	StatsVer       int64 // StatsVer is the version of the current stats, used to maintain compatibility
	Info           *model.IndexInfo
	Flag           int64
	LastAnalyzePos types.Datum
}

func (idx *Index) String() string {
	return idx.Histogram.ToString(len(idx.Info.Columns))
}

// IsInvalid checks if this index is invalid.
func (idx *Index) IsInvalid(collPseudo bool) bool {
	return (collPseudo && idx.NotAccurate()) || idx.TotalRowCount() == 0
}

var nullKeyBytes, _ = codec.EncodeKey(nil, nil, types.NewDatum(nil))

func (idx *Index) equalRowCount(sc *stmtctx.StatementContext, b []byte, modifyCount int64) (float64, error) {
	if len(idx.Info.Columns) == 1 {
		if bytes.Equal(b, nullKeyBytes) {
			return float64(idx.NullCount), nil
		}
	}
	val := types.NewBytesDatum(b)
	if idx.NDV > 0 && idx.outOfRange(val) {
		return float64(modifyCount) / (float64(idx.NDV)), nil
	}
	if idx.CMSketch != nil {
		return float64(idx.CMSketch.QueryBytes(b)), nil
	}
	return idx.Histogram.equalRowCount(val), nil
}

// GetRowCount returns the row count of the given ranges.
// It uses the modifyCount to adjust the influence of modifications on the table.
func (idx *Index) GetRowCount(sc *stmtctx.StatementContext, indexRanges []*ranger.Range, modifyCount int64) (float64, error) {
	totalCount := float64(0)
	isSingleCol := len(idx.Info.Columns) == 1
	for _, indexRange := range indexRanges {
		lb, err := codec.EncodeKey(sc, nil, indexRange.LowVal...)
		if err != nil {
			return 0, err
		}
		rb, err := codec.EncodeKey(sc, nil, indexRange.HighVal...)
		if err != nil {
			return 0, err
		}
		fullLen := len(indexRange.LowVal) == len(indexRange.HighVal) && len(indexRange.LowVal) == len(idx.Info.Columns)
		if bytes.Equal(lb, rb) {
			if indexRange.LowExclude || indexRange.HighExclude {
				continue
			}
			if fullLen {
				count, err := idx.equalRowCount(sc, lb, modifyCount)
				if err != nil {
					return 0, err
				}
				totalCount += count
				continue
			}
		}
		if indexRange.LowExclude {
			lb = kv.Key(lb).PrefixNext()
		}
		if !indexRange.HighExclude {
			rb = kv.Key(rb).PrefixNext()
		}
		l := types.NewBytesDatum(lb)
		r := types.NewBytesDatum(rb)
		totalCount += idx.BetweenRowCount(l, r)
		lowIsNull := bytes.Equal(lb, nullKeyBytes)
		if (idx.outOfRange(l) && !(isSingleCol && lowIsNull)) || idx.outOfRange(r) {
			totalCount += float64(modifyCount) / outOfRangeBetweenRate
		}
		if isSingleCol && lowIsNull {
			totalCount += float64(idx.NullCount)
		}
	}
	if totalCount > idx.TotalRowCount() {
		totalCount = idx.TotalRowCount()
	}
	return totalCount, nil
}

type countByRangeFunc = func(*stmtctx.StatementContext, int64, []*ranger.Range) (float64, error)

// newHistogramBySelectivity fulfills the content of new histogram by the given selectivity result.
// TODO: Datum is not efficient, try to avoid using it here.
//  Also, there're redundant calculation with Selectivity(). We need to reduce it too.
func newHistogramBySelectivity(sc *stmtctx.StatementContext, histID int64, oldHist, newHist *Histogram, ranges []*ranger.Range, cntByRangeFunc countByRangeFunc) error {
	cntPerVal := int64(oldHist.AvgCountPerNotNullValue(int64(oldHist.TotalRowCount())))
	var totCnt int64
	for boundIdx, ranIdx, highRangeIdx := 0, 0, 0; boundIdx < oldHist.Bounds.NumRows() && ranIdx < len(ranges); boundIdx, ranIdx = boundIdx+2, highRangeIdx {
		for highRangeIdx < len(ranges) && chunk.Compare(oldHist.Bounds.GetRow(boundIdx+1), 0, &ranges[highRangeIdx].HighVal[0]) >= 0 {
			highRangeIdx++
		}
		if boundIdx+2 >= oldHist.Bounds.NumRows() && highRangeIdx < len(ranges) && ranges[highRangeIdx].HighVal[0].Kind() == types.KindMaxValue {
			highRangeIdx++
		}
		if ranIdx == highRangeIdx {
			continue
		}
		cnt, err := cntByRangeFunc(sc, histID, ranges[ranIdx:highRangeIdx])
		// This should not happen.
		if err != nil {
			return err
		}
		if cnt == 0 {
			continue
		}
		if int64(cnt) > oldHist.bucketCount(boundIdx/2) {
			cnt = float64(oldHist.bucketCount(boundIdx / 2))
		}
		newHist.Bounds.AppendRow(oldHist.Bounds.GetRow(boundIdx))
		newHist.Bounds.AppendRow(oldHist.Bounds.GetRow(boundIdx + 1))
		totCnt += int64(cnt)
		bkt := Bucket{Count: totCnt}
		if chunk.Compare(oldHist.Bounds.GetRow(boundIdx+1), 0, &ranges[highRangeIdx-1].HighVal[0]) == 0 && !ranges[highRangeIdx-1].HighExclude {
			bkt.Repeat = cntPerVal
		}
		newHist.Buckets = append(newHist.Buckets, bkt)
		switch newHist.Tp.EvalType() {
		case types.ETString, types.ETDecimal, types.ETDatetime, types.ETTimestamp:
			newHist.scalars = append(newHist.scalars, oldHist.scalars[boundIdx/2])
		}
	}
	return nil
}

func (idx *Index) newIndexBySelectivity(sc *stmtctx.StatementContext, statsNode *StatsNode) (*Index, error) {
	var (
		ranLowEncode, ranHighEncode []byte
		err                         error
	)
	newIndexHist := &Index{Info: idx.Info, StatsVer: idx.StatsVer, CMSketch: idx.CMSketch}
	newIndexHist.Histogram = *NewHistogram(idx.ID, int64(float64(idx.NDV)*statsNode.Selectivity), 0, 0, types.NewFieldType(mysql.TypeBlob), chunk.InitialCapacity, 0)

	lowBucketIdx, highBucketIdx := 0, 0
	var totCnt int64

	// Bucket bound of index is encoded one, so we need to decode it if we want to calculate the fraction accurately.
	// TODO: enhance its calculation.
	// Now just remove the bucket that no range fell in.
	for _, ran := range statsNode.Ranges {
		lowBucketIdx = highBucketIdx
		ranLowEncode, ranHighEncode, err = ran.Encode(sc, ranLowEncode, ranHighEncode)
		if err != nil {
			return nil, err
		}
		for ; highBucketIdx < idx.Len(); highBucketIdx++ {
			// Encoded value can only go to its next quickly. So ranHighEncode is actually range.HighVal's PrefixNext value.
			// So the Bound should also go to its PrefixNext.
			bucketLowerEncoded := idx.Bounds.GetRow(highBucketIdx * 2).GetBytes(0)
			if bytes.Compare(ranHighEncode, kv.Key(bucketLowerEncoded).PrefixNext()) < 0 {
				break
			}
		}
		for ; lowBucketIdx < highBucketIdx; lowBucketIdx++ {
			bucketUpperEncoded := idx.Bounds.GetRow(lowBucketIdx*2 + 1).GetBytes(0)
			if bytes.Compare(ranLowEncode, bucketUpperEncoded) <= 0 {
				break
			}
		}
		if lowBucketIdx >= idx.Len() {
			break
		}
		for i := lowBucketIdx; i < highBucketIdx; i++ {
			newIndexHist.Bounds.AppendRow(idx.Bounds.GetRow(i * 2))
			newIndexHist.Bounds.AppendRow(idx.Bounds.GetRow(i*2 + 1))
			totCnt += idx.bucketCount(i)
			newIndexHist.Buckets = append(newIndexHist.Buckets, Bucket{Repeat: idx.Buckets[i].Repeat, Count: totCnt})
			newIndexHist.scalars = append(newIndexHist.scalars, idx.scalars[i])
		}
	}
	return newIndexHist, nil
}

// NewHistCollBySelectivity creates new HistColl by the given statsNodes.
func (coll *HistColl) NewHistCollBySelectivity(sc *stmtctx.StatementContext, statsNodes []*StatsNode) *HistColl {
	newColl := &HistColl{
		Columns:       make(map[int64]*Column),
		Indices:       make(map[int64]*Index),
		Idx2ColumnIDs: coll.Idx2ColumnIDs,
		ColID2IdxID:   coll.ColID2IdxID,
		Count:         coll.Count,
	}
	for _, node := range statsNodes {
		if node.Tp == IndexType {
			idxHist, ok := coll.Indices[node.ID]
			if !ok {
				continue
			}
			newIdxHist, err := idxHist.newIndexBySelectivity(sc, node)
			if err != nil {
				logutil.Logger(context.Background()).Warn("[Histogram-in-plan]: error happened when calculating row count, failed to build histogram for index %v of table %v",
					zap.String("index", idxHist.Info.Name.O), zap.String("table", idxHist.Info.Table.O), zap.Error(err))
				continue
			}
			newColl.Indices[node.ID] = newIdxHist
			continue
		}
		oldCol, ok := coll.Columns[node.ID]
		if !ok {
			continue
		}
		newCol := &Column{
			PhysicalID: oldCol.PhysicalID,
			Info:       oldCol.Info,
			IsHandle:   oldCol.IsHandle,
			CMSketch:   oldCol.CMSketch,
		}
		newCol.Histogram = *NewHistogram(oldCol.ID, int64(float64(oldCol.NDV)*node.Selectivity), 0, 0, oldCol.Tp, chunk.InitialCapacity, 0)
		var err error
		splitRanges, ok := oldCol.Histogram.SplitRange(sc, node.Ranges, false)
		if !ok {
			logutil.Logger(context.Background()).Warn("[Histogram-in-plan]: the type of histogram and ranges mismatch")
			continue
		}
		// Deal with some corner case.
		if len(splitRanges) > 0 {
			// Deal with NULL values.
			if splitRanges[0].LowVal[0].IsNull() {
				newCol.NullCount = oldCol.NullCount
				if splitRanges[0].HighVal[0].IsNull() {
					splitRanges = splitRanges[1:]
				} else {
					splitRanges[0].LowVal[0].SetMinNotNull()
				}
			}
		}
		if oldCol.IsHandle {
			err = newHistogramBySelectivity(sc, node.ID, &oldCol.Histogram, &newCol.Histogram, splitRanges, coll.GetRowCountByIntColumnRanges)
		} else {
			err = newHistogramBySelectivity(sc, node.ID, &oldCol.Histogram, &newCol.Histogram, splitRanges, coll.GetRowCountByColumnRanges)
		}
		if err != nil {
			logutil.Logger(context.Background()).Warn("[Histogram-in-plan]: error happened when calculating row count", zap.Error(err))
			continue
		}
		newColl.Columns[node.ID] = newCol
	}
	for id, idx := range coll.Indices {
		_, ok := newColl.Indices[id]
		if !ok {
			newColl.Indices[id] = idx
		}
	}
	for id, col := range coll.Columns {
		_, ok := newColl.Columns[id]
		if !ok {
			newColl.Columns[id] = col
		}
	}
	return newColl
}

func (idx *Index) outOfRange(val types.Datum) bool {
	if idx.Histogram.Len() == 0 {
		return true
	}
	withInLowBoundOrPrefixMatch := chunk.Compare(idx.Bounds.GetRow(0), 0, &val) <= 0 ||
		matchPrefix(idx.Bounds.GetRow(0), 0, &val)
	withInHighBound := chunk.Compare(idx.Bounds.GetRow(idx.Bounds.NumRows()-1), 0, &val) >= 0
	return !withInLowBoundOrPrefixMatch || !withInHighBound
}

// matchPrefix checks whether ad is the prefix of value
func matchPrefix(row chunk.Row, colIdx int, ad *types.Datum) bool {
	switch ad.Kind() {
	case types.KindString, types.KindBytes, types.KindBinaryLiteral, types.KindMysqlBit:
		return strings.HasPrefix(row.GetString(colIdx), ad.GetString())
	}
	return false
}
