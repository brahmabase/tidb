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
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
)

var (
	// ErrBodyMissing response body is missing error
	ErrBodyMissing = errors.New("response body is missing")
	// When TiDB is closing and send request to tikv fail, do not retry, return this error.
	errTiDBShuttingDown = errors.New("tidb server shutting down")
)

// mismatchClusterID represents the message that the cluster ID of the PD client does not match the PD.
const mismatchClusterID = "mismatch cluster id"

// MySQL error instances.
var (
	ErrTiKVServerTimeout  = terror.ClassTiKV.New(mysql.ErrTiKVServerTimeout, mysql.MySQLErrName[mysql.ErrTiKVServerTimeout])
	ErrResolveLockTimeout = terror.ClassTiKV.New(mysql.ErrResolveLockTimeout, mysql.MySQLErrName[mysql.ErrResolveLockTimeout])
	ErrPDServerTimeout    = terror.ClassTiKV.New(mysql.ErrPDServerTimeout, mysql.MySQLErrName[mysql.ErrPDServerTimeout])
	ErrRegionUnavailable  = terror.ClassTiKV.New(mysql.ErrRegionUnavailable, mysql.MySQLErrName[mysql.ErrRegionUnavailable])
	ErrTiKVServerBusy     = terror.ClassTiKV.New(mysql.ErrTiKVServerBusy, mysql.MySQLErrName[mysql.ErrTiKVServerBusy])
	ErrGCTooEarly         = terror.ClassTiKV.New(mysql.ErrGCTooEarly, mysql.MySQLErrName[mysql.ErrGCTooEarly])
)

// ErrDeadlock wraps *kvrpcpb.Deadlock to implement the error interface.
// It also marks if the deadlock is retryable.
type ErrDeadlock struct {
	*kvrpcpb.Deadlock
	IsRetryable bool
}

func (d *ErrDeadlock) Error() string {
	return d.Deadlock.String()
}

func init() {
	tikvMySQLErrCodes := map[terror.ErrCode]uint16{
		mysql.ErrTiKVServerTimeout:   mysql.ErrTiKVServerTimeout,
		mysql.ErrResolveLockTimeout:  mysql.ErrResolveLockTimeout,
		mysql.ErrPDServerTimeout:     mysql.ErrPDServerTimeout,
		mysql.ErrRegionUnavailable:   mysql.ErrRegionUnavailable,
		mysql.ErrTiKVServerBusy:      mysql.ErrTiKVServerBusy,
		mysql.ErrGCTooEarly:          mysql.ErrGCTooEarly,
		mysql.ErrTruncatedWrongValue: mysql.ErrTruncatedWrongValue,
	}
	terror.ErrClassToMySQLCodes[terror.ClassTiKV] = tikvMySQLErrCodes
}
