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

package domain

import (
	. "github.com/pingcap/check"
	"github.com/pingcap/tidb/util/mock"
	"github.com/pingcap/tidb/util/testleak"
)

var _ = Suite(&testDomainCtxSuite{})

type testDomainCtxSuite struct {
}

func (s *testDomainCtxSuite) TestDomain(c *C) {
	defer testleak.AfterTest(c)()
	ctx := mock.NewContext()

	c.Assert(domainKey.String(), Not(Equals), "")

	BindDomain(ctx, nil)
	v := GetDomain(ctx)
	c.Assert(v, IsNil)

	ctx.ClearValue(domainKey)
	v = GetDomain(ctx)
	c.Assert(v, IsNil)
}
