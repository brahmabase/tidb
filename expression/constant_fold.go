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

package expression

import (
	"context"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

// specialFoldHandler stores functions for special UDF to constant fold
var specialFoldHandler = map[string]func(*ScalarFunction) (Expression, bool){}

func init() {
	specialFoldHandler = map[string]func(*ScalarFunction) (Expression, bool){
		ast.If:     ifFoldHandler,
		ast.Ifnull: ifNullFoldHandler,
	}
}

// FoldConstant does constant folding optimization on an expression excluding deferred ones.
func FoldConstant(expr Expression) Expression {
	e, _ := foldConstant(expr)
	return e
}

func ifFoldHandler(expr *ScalarFunction) (Expression, bool) {
	args := expr.GetArgs()
	foldedArg0, _ := foldConstant(args[0])
	if constArg, isConst := foldedArg0.(*Constant); isConst {
		arg0, isNull0, err := constArg.EvalInt(expr.Function.getCtx(), chunk.Row{})
		if err != nil {
			// Failed to fold this expr to a constant, print the DEBUG log and
			// return the original expression to let the error to be evaluated
			// again, in that time, the error is returned to the client.
			logutil.Logger(context.Background()).Debug("fold expression to constant", zap.String("expression", expr.ExplainInfo()), zap.Error(err))
			return expr, false
		}
		if !isNull0 && arg0 != 0 {
			return foldConstant(args[1])
		}
		return foldConstant(args[2])
	}
	var isDeferred, isDeferredConst bool
	expr.GetArgs()[1], isDeferred = foldConstant(args[1])
	isDeferredConst = isDeferredConst || isDeferred
	expr.GetArgs()[2], isDeferred = foldConstant(args[2])
	isDeferredConst = isDeferredConst || isDeferred
	return expr, isDeferredConst
}

func ifNullFoldHandler(expr *ScalarFunction) (Expression, bool) {
	args := expr.GetArgs()
	foldedArg0, isDeferred := foldConstant(args[0])
	if constArg, isConst := foldedArg0.(*Constant); isConst {
		// Only check constArg.Value here. Because deferred expression is
		// evaluated to constArg.Value after foldConstant(args[0]), it's not
		// needed to be checked.
		if constArg.Value.IsNull() {
			return foldConstant(args[1])
		}
		return constArg, isDeferred
	}
	var isDeferredConst bool
	expr.GetArgs()[1], isDeferredConst = foldConstant(args[1])
	return expr, isDeferredConst
}

func foldConstant(expr Expression) (Expression, bool) {
	switch x := expr.(type) {
	case *ScalarFunction:
		if _, ok := unFoldableFunctions[x.FuncName.L]; ok {
			return expr, false
		}
		if function := specialFoldHandler[x.FuncName.L]; function != nil {
			return function(x)
		}

		args := x.GetArgs()
		sc := x.GetCtx().GetSessionVars().StmtCtx
		argIsConst := make([]bool, len(args))
		hasNullArg := false
		allConstArg := true
		isDeferredConst := false
		for i := 0; i < len(args); i++ {
			foldedArg, isDeferred := foldConstant(args[i])
			x.GetArgs()[i] = foldedArg
			con, conOK := foldedArg.(*Constant)
			argIsConst[i] = conOK
			allConstArg = allConstArg && conOK
			hasNullArg = hasNullArg || (conOK && con.Value.IsNull())
			isDeferredConst = isDeferredConst || isDeferred
		}
		if !allConstArg {
			if !hasNullArg || !sc.InNullRejectCheck || x.FuncName.L == ast.NullEQ {
				return expr, isDeferredConst
			}
			constArgs := make([]Expression, len(args))
			for i, arg := range args {
				if argIsConst[i] {
					constArgs[i] = arg
				} else {
					constArgs[i] = One
				}
			}
			dummyScalarFunc, err := NewFunctionBase(x.GetCtx(), x.FuncName.L, x.GetType(), constArgs...)
			if err != nil {
				return expr, isDeferredConst
			}
			value, err := dummyScalarFunc.Eval(chunk.Row{})
			if err != nil {
				return expr, isDeferredConst
			}
			if value.IsNull() {
				if isDeferredConst {
					return &Constant{Value: value, RetType: x.RetType, DeferredExpr: x}, true
				}
				return &Constant{Value: value, RetType: x.RetType}, false
			}
			if isTrue, err := value.ToBool(sc); err == nil && isTrue == 0 {
				if isDeferredConst {
					return &Constant{Value: value, RetType: x.RetType, DeferredExpr: x}, true
				}
				return &Constant{Value: value, RetType: x.RetType}, false
			}
			return expr, isDeferredConst
		}
		value, err := x.Eval(chunk.Row{})
		if err != nil {
			logutil.Logger(context.Background()).Debug("fold expression to constant", zap.String("expression", x.ExplainInfo()), zap.Error(err))
			return expr, isDeferredConst
		}
		if isDeferredConst {
			return &Constant{Value: value, RetType: x.RetType, DeferredExpr: x}, true
		}
		return &Constant{Value: value, RetType: x.RetType}, false
	case *Constant:
		if x.DeferredExpr != nil {
			value, err := x.DeferredExpr.Eval(chunk.Row{})
			if err != nil {
				logutil.Logger(context.Background()).Debug("fold expression to constant", zap.String("expression", x.ExplainInfo()), zap.Error(err))
				return expr, true
			}
			return &Constant{Value: value, RetType: x.RetType, DeferredExpr: x.DeferredExpr}, true
		}
	}
	return expr, false
}
