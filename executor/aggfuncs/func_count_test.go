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

package aggfuncs_test

import (
	. "github.com/pingcap/check"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/mysql"
)

func (s *testSuite) TestMergePartialResult4Count(c *C) {
	tester := buildAggTester(ast.AggFuncCount, mysql.TypeLonglong, 5, 5, 3, 8)
	s.testMergePartialResult(c, tester)
}

func (s *testSuite) TestCount(c *C) {
	tests := []aggTest{
		buildAggTester(ast.AggFuncCount, mysql.TypeLonglong, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeFloat, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeDouble, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeNewDecimal, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeString, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeDate, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeDuration, 5, 0, 5),
		buildAggTester(ast.AggFuncCount, mysql.TypeJSON, 5, 0, 5),
	}
	for _, test := range tests {
		s.testAggFunc(c, test)
	}
}
