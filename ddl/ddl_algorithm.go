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

package ddl

import (
	"fmt"

	"github.com/pingcap/parser/ast"
)

// AlterAlgorithm is used to store supported alter algorithm.
// For now, TiDB only support AlterAlgorithmInplace and AlterAlgorithmInstant.
// The most alter operations are using instant algorithm, and only the add index is using inplace(not really inplace,
// because we never block the DML but costs some time to backfill the index data)
// See https://dev.mysql.com/doc/refman/8.0/en/alter-table.html#alter-table-performance.
type AlterAlgorithm struct {
	supported []ast.AlterAlgorithm
	// If the alter algorithm is not given, the defAlgorithm will be used.
	defAlgorithm ast.AlterAlgorithm
}

var (
	instantAlgorithm = &AlterAlgorithm{
		supported:    []ast.AlterAlgorithm{ast.AlterAlgorithmInstant},
		defAlgorithm: ast.AlterAlgorithmInstant,
	}

	inplaceAlgorithm = &AlterAlgorithm{
		supported:    []ast.AlterAlgorithm{ast.AlterAlgorithmInplace},
		defAlgorithm: ast.AlterAlgorithmInplace,
	}

	defaultAlgorithm = ast.AlterAlgorithmInstant
)

func getProperAlgorithm(specify ast.AlterAlgorithm, algorithm *AlterAlgorithm) (ast.AlterAlgorithm, error) {
	if specify == ast.AlterAlgorithmDefault {
		return algorithm.defAlgorithm, nil
	}

	for _, a := range algorithm.supported {
		if specify == a {
			return specify, nil
		}
	}

	return algorithm.defAlgorithm, ErrAlterOperationNotSupported.GenWithStackByArgs(fmt.Sprintf("ALGORITHM=%s", specify), fmt.Sprintf("Cannot alter table by %s", specify), fmt.Sprintf("ALGORITHM=%s", algorithm.defAlgorithm))
}

// ResolveAlterAlgorithm resolves the algorithm of the alterSpec.
// If specify algorithm is not supported by the alter action, errAlterOperationNotSupported will be returned.
// If specify is the ast.AlterAlgorithmDefault, then the default algorithm of the alter action will be returned.
func ResolveAlterAlgorithm(alterSpec *ast.AlterTableSpec, specify ast.AlterAlgorithm) (ast.AlterAlgorithm, error) {
	switch alterSpec.Tp {
	// For now, TiDB only support inplace algorithm and instant algorithm.
	case ast.AlterTableAddConstraint:
		return getProperAlgorithm(specify, inplaceAlgorithm)
	default:
		return getProperAlgorithm(specify, instantAlgorithm)
	}
}
