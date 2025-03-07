// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package eval

import (
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree/treecmp"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// Expr evaluates a TypedExpr into a Datum.
func Expr(ctx *Context, n tree.TypedExpr) (tree.Datum, error) {
	return n.Eval((*evaluator)(ctx))
}

type evaluator Context

func (e *evaluator) ctx() *Context { return (*Context)(e) }

func (e *evaluator) EvalAllColumnsSelector(selector *tree.AllColumnsSelector) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", selector)
}

func (e *evaluator) EvalAndExpr(expr *tree.AndExpr) (tree.Datum, error) {
	left, err := expr.Left.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if left != tree.DNull {
		if v, err := tree.GetBool(left); err != nil {
			return nil, err
		} else if !v {
			return left, nil
		}
	}
	right, err := expr.Right.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if right == tree.DNull {
		return tree.DNull, nil
	}
	if v, err := tree.GetBool(right); err != nil {
		return nil, err
	} else if !v {
		return right, nil
	}
	return left, nil
}

func (e *evaluator) EvalArray(t *tree.Array) (tree.Datum, error) {
	array, err := arrayOfType(t.ResolvedType())
	if err != nil {
		return nil, err
	}

	for _, ae := range t.Exprs {
		d, err := ae.(tree.TypedExpr).Eval(e)
		if err != nil {
			return nil, err
		}
		if err := array.Append(d); err != nil {
			return nil, err
		}
	}
	return array, nil
}

func (e *evaluator) EvalArrayFlatten(t *tree.ArrayFlatten) (tree.Datum, error) {
	array, err := arrayOfType(t.ResolvedType())
	if err != nil {
		return nil, err
	}

	d, err := t.Subquery.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}

	tuple, ok := d.(*tree.DTuple)
	if !ok {
		return nil, errors.AssertionFailedf("array subquery result (%v) is not DTuple", d)
	}
	array.Array = tuple.D
	return array, nil
}

func (e *evaluator) EvalBinaryExpr(expr *tree.BinaryExpr) (tree.Datum, error) {
	left, err := expr.Left.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if left == tree.DNull && !expr.Op.CalledOnNullInput {
		return tree.DNull, nil
	}
	right, err := expr.Right.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if right == tree.DNull && !expr.Op.CalledOnNullInput {
		return tree.DNull, nil
	}
	res, err := expr.Op.EvalOp.Eval(e, left, right)
	if err != nil {
		return nil, err
	}
	if e.TestingKnobs.AssertBinaryExprReturnTypes {
		if err := ensureExpectedType(expr.Op.ReturnType, res); err != nil {
			return nil, errors.NewAssertionErrorWithWrappedErrf(err,
				"binary op %q", expr)
		}
	}
	return res, err
}

func (e *evaluator) EvalCaseExpr(expr *tree.CaseExpr) (tree.Datum, error) {
	if expr.Expr != nil {
		// CASE <val> WHEN <expr> THEN ...
		//
		// For each "when" expression we compare for equality to <val>.
		val, err := expr.Expr.(tree.TypedExpr).Eval(e)
		if err != nil {
			return nil, err
		}

		for _, when := range expr.Whens {
			arg, err := when.Cond.(tree.TypedExpr).Eval(e)
			if err != nil {
				return nil, err
			}
			d, err := evalComparison(e.ctx(), treecmp.MakeComparisonOperator(treecmp.EQ), val, arg)
			if err != nil {
				return nil, err
			}
			if db, err := tree.GetBool(d); err != nil {
				return nil, err
			} else if db {
				return when.Val.(tree.TypedExpr).Eval(e)
			}
		}
	} else {
		// CASE WHEN <bool-expr> THEN ...
		for _, when := range expr.Whens {
			d, err := when.Cond.(tree.TypedExpr).Eval(e)
			if err != nil {
				return nil, err
			}
			if db, err := tree.GetBool(d); err != nil {
				return nil, err
			} else if db {
				return when.Val.(tree.TypedExpr).Eval(e)
			}
		}
	}

	if expr.Else != nil {
		return expr.Else.(tree.TypedExpr).Eval(e)
	}
	return tree.DNull, nil
}

func (e *evaluator) EvalCastExpr(expr *tree.CastExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}

	// NULL cast to anything is NULL.
	if d == tree.DNull {
		return d, nil
	}
	d = UnwrapDatum(e.ctx(), d)
	return PerformCast(e.ctx(), d, expr.ResolvedType())
}

func (e *evaluator) EvalCoalesceExpr(expr *tree.CoalesceExpr) (tree.Datum, error) {
	for _, ex := range expr.Exprs {
		d, err := ex.(tree.TypedExpr).Eval(e)
		if err != nil {
			return nil, err
		}
		if d != tree.DNull {
			return d, nil
		}
	}
	return tree.DNull, nil
}

func (e *evaluator) EvalCollateExpr(expr *tree.CollateExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	unwrapped := UnwrapDatum(e.ctx(), d)
	if unwrapped == tree.DNull {
		return tree.DNull, nil
	}
	switch d := unwrapped.(type) {
	case *tree.DString:
		return tree.NewDCollatedString(string(*d), expr.Locale, &e.CollationEnv)
	case *tree.DCollatedString:
		return tree.NewDCollatedString(d.Contents, expr.Locale, &e.CollationEnv)
	default:
		return nil, pgerror.Newf(pgcode.DatatypeMismatch, "incompatible type for COLLATE: %s", d)
	}
}

func (e *evaluator) EvalColumnAccessExpr(expr *tree.ColumnAccessExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if d == tree.DNull {
		return d, nil
	}
	return d.(*tree.DTuple).D[expr.ColIndex], nil
}

func (e *evaluator) EvalColumnItem(expr *tree.ColumnItem) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", expr)
}

func (e *evaluator) EvalComparisonExpr(expr *tree.ComparisonExpr) (tree.Datum, error) {
	left, err := expr.Left.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	right, err := expr.Right.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}

	op := expr.Operator
	if op.Symbol.HasSubOperator() {
		return ComparisonExprWithSubOperator(e.ctx(), expr, left, right)
	}

	_, newLeft, newRight, _, not := tree.FoldComparisonExpr(op, left, right)
	if !expr.Op.CalledOnNullInput && (newLeft == tree.DNull || newRight == tree.DNull) {
		return tree.DNull, nil
	}
	d, err := expr.Op.EvalOp.Eval(e, newLeft.(tree.Datum), newRight.(tree.Datum))
	if d == tree.DNull || err != nil {
		return d, err
	}
	b, ok := d.(*tree.DBool)
	if !ok {
		return nil, errors.AssertionFailedf("%v is %T and not *DBool", d, d)
	}
	return tree.MakeDBool(*b != tree.DBool(not)), nil
}

func (e *evaluator) EvalIndexedVar(iv *tree.IndexedVar) (tree.Datum, error) {
	if e.IVarContainer == nil {
		return nil, errors.AssertionFailedf(
			"indexed var must be bound to a container before evaluation")
	}
	eivc, ok := e.IVarContainer.(IndexedVarContainer)
	if !ok {
		return nil, errors.AssertionFailedf(
			"indexed var container of type %T may not be evaluated", e.IVarContainer)
	}
	return eivc.IndexedVarEval(iv.Idx, e)
}

func (e *evaluator) EvalIndirectionExpr(expr *tree.IndirectionExpr) (tree.Datum, error) {
	var subscriptIdx int

	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if d == tree.DNull {
		return d, nil
	}

	switch d.ResolvedType().Family() {
	case types.ArrayFamily:
		for i, t := range expr.Indirection {
			if t.Slice || i > 0 {
				return nil, errors.AssertionFailedf("unsupported feature should have been rejected during planning")
			}

			beginDatum, err := t.Begin.(tree.TypedExpr).Eval(e)
			if err != nil {
				return nil, err
			}
			if beginDatum == tree.DNull {
				return tree.DNull, nil
			}
			subscriptIdx = int(tree.MustBeDInt(beginDatum))
		}

		// Index into the DArray, using 1-indexing.
		arr := tree.MustBeDArray(d)

		// VECTOR types use 0-indexing.
		if arr.FirstIndex() == 0 {
			subscriptIdx++
		}
		if subscriptIdx < 1 || subscriptIdx > arr.Len() {
			return tree.DNull, nil
		}
		return arr.Array[subscriptIdx-1], nil
	case types.JsonFamily:
		j := tree.MustBeDJSON(d)
		curr := j.JSON
		for _, t := range expr.Indirection {
			if t.Slice {
				return nil, errors.AssertionFailedf("unsupported feature should have been rejected during planning")
			}

			field, err := t.Begin.(tree.TypedExpr).Eval(e)
			if err != nil {
				return nil, err
			}
			if field == tree.DNull {
				return tree.DNull, nil
			}
			switch field.ResolvedType().Family() {
			case types.StringFamily:
				if curr, err = curr.FetchValKeyOrIdx(string(tree.MustBeDString(field))); err != nil {
					return nil, err
				}
			case types.IntFamily:
				if curr, err = curr.FetchValIdx(int(tree.MustBeDInt(field))); err != nil {
					return nil, err
				}
			default:
				return nil, errors.AssertionFailedf("unsupported feature should have been rejected during planning")
			}
			if curr == nil {
				return tree.DNull, nil
			}
		}
		return tree.NewDJSON(curr), nil
	}
	return nil, errors.AssertionFailedf("unsupported feature should have been rejected during planning")
}

func (e *evaluator) EvalDefaultVal(expr *tree.DefaultVal) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", expr)
}

func (e *evaluator) EvalIsNotNullExpr(expr *tree.IsNotNullExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if d == tree.DNull {
		return tree.MakeDBool(false), nil
	}
	if t, ok := d.(*tree.DTuple); ok {
		// A tuple IS NOT NULL if all elements are not NULL.
		for _, tupleDatum := range t.D {
			if tupleDatum == tree.DNull {
				return tree.MakeDBool(false), nil
			}
		}
		return tree.MakeDBool(true), nil
	}
	return tree.MakeDBool(true), nil
}

func (e *evaluator) EvalIsNullExpr(expr *tree.IsNullExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if d == tree.DNull {
		return tree.MakeDBool(true), nil
	}
	if t, ok := d.(*tree.DTuple); ok {
		// A tuple IS NULL if all elements are NULL.
		for _, tupleDatum := range t.D {
			if tupleDatum != tree.DNull {
				return tree.MakeDBool(false), nil
			}
		}
		return tree.MakeDBool(true), nil
	}
	return tree.MakeDBool(false), nil
}

func (e *evaluator) EvalIsOfTypeExpr(expr *tree.IsOfTypeExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	datumTyp := d.ResolvedType()

	for _, t := range expr.ResolvedTypes() {
		if datumTyp.Equivalent(t) {
			return tree.MakeDBool(tree.DBool(!expr.Not)), nil
		}
	}
	return tree.MakeDBool(tree.DBool(expr.Not)), nil
}

func (e *evaluator) EvalNotExpr(expr *tree.NotExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if d == tree.DNull {
		return tree.DNull, nil
	}
	got, err := tree.GetBool(d)
	if err != nil {
		return nil, err
	}
	return tree.MakeDBool(!got), nil
}

func (e *evaluator) EvalNullIfExpr(expr *tree.NullIfExpr) (tree.Datum, error) {
	expr1, err := expr.Expr1.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	expr2, err := expr.Expr2.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	cond, err := evalComparison(e.ctx(), treecmp.MakeComparisonOperator(treecmp.EQ), expr1, expr2)
	if err != nil {
		return nil, err
	}
	if cond == tree.DBoolTrue {
		return tree.DNull, nil
	}
	return expr1, nil
}

func (e *evaluator) EvalFuncExpr(expr *tree.FuncExpr) (tree.Datum, error) {
	fn := expr.ResolvedOverload()
	if fn.FnWithExprs != nil {
		return fn.FnWithExprs.(FnWithExprsOverload)(e.ctx(), expr.Exprs)
	}

	nullResult, args, err := e.evalFuncArgs(expr)
	if err != nil {
		return nil, err
	}
	if nullResult {
		return tree.DNull, err
	}

	res, err := fn.Fn.(FnOverload)(e.ctx(), args)
	if err != nil {
		return nil, expr.MaybeWrapError(err)
	}
	if e.TestingKnobs.AssertFuncExprReturnTypes {
		if err := ensureExpectedType(fn.FixedReturnType(), res); err != nil {
			return nil, errors.NewAssertionErrorWithWrappedErrf(err, "function %q", expr)
		}
	}
	return res, nil
}

func (e *evaluator) evalFuncArgs(
	expr *tree.FuncExpr,
) (propagateNulls bool, args tree.Datums, _ error) {
	args = make(tree.Datums, len(expr.Exprs))
	for i, argExpr := range expr.Exprs {
		arg, err := argExpr.(tree.TypedExpr).Eval(e)
		if err != nil {
			return false, nil, err
		}
		if arg == tree.DNull && !expr.ResolvedOverload().CalledOnNullInput {
			return true, nil, nil
		}
		args[i] = arg
	}
	return false, args, nil
}

func (e *evaluator) EvalIfErrExpr(expr *tree.IfErrExpr) (tree.Datum, error) {
	cond, evalErr := expr.Cond.(tree.TypedExpr).Eval(e)
	if evalErr == nil {
		if expr.Else == nil {
			return tree.DBoolFalse, nil
		}
		return cond, nil
	}
	if expr.ErrCode != nil {
		errpat, err := expr.ErrCode.(tree.TypedExpr).Eval(e)
		if err != nil {
			return nil, err
		}
		if errpat == tree.DNull {
			return nil, evalErr
		}
		errpatStr := string(tree.MustBeDString(errpat))
		if code := pgerror.GetPGCode(evalErr); code != pgcode.MakeCode(errpatStr) {
			return nil, evalErr
		}
	}
	if expr.Else == nil {
		return tree.DBoolTrue, nil
	}
	return expr.Else.(tree.TypedExpr).Eval(e)
}

func (e *evaluator) EvalIfExpr(expr *tree.IfExpr) (tree.Datum, error) {
	cond, err := expr.Cond.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if cond == tree.DBoolTrue {
		return expr.True.(tree.TypedExpr).Eval(e)
	}
	return expr.Else.(tree.TypedExpr).Eval(e)
}

func (e *evaluator) EvalOrExpr(expr *tree.OrExpr) (tree.Datum, error) {
	left, err := expr.Left.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if left != tree.DNull {
		if got, err := tree.GetBool(left); err != nil {
			return nil, err
		} else if got {
			return left, nil
		}
	}
	right, err := expr.Right.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if right == tree.DNull {
		return tree.DNull, nil
	}
	if got, err := tree.GetBool(right); err != nil {
		return nil, err
	} else if got {
		return right, nil
	}
	if left == tree.DNull {
		return tree.DNull, nil
	}
	return tree.DBoolFalse, nil
}

func (e *evaluator) EvalParenExpr(expr *tree.ParenExpr) (tree.Datum, error) {
	return expr.Expr.(tree.TypedExpr).Eval(e)
}

func (e *evaluator) EvalPlaceholder(t *tree.Placeholder) (tree.Datum, error) {
	if !e.ctx().HasPlaceholders() {
		// While preparing a query, there will be no available placeholders. A
		// placeholder evaluates to itself at this point.
		return t, nil
	}
	ex, ok := e.Placeholders.Value(t.Idx)
	if !ok {
		return nil, tree.NewNoValueProvidedForPlaceholderErr(t.Idx)
	}
	// Placeholder expressions cannot contain other placeholders, so we do
	// not need to recurse.
	typ := e.Placeholders.Types[t.Idx]
	if typ == nil {
		// All placeholders should be typed at this point.
		return nil, errors.AssertionFailedf("missing type for placeholder %s", t)
	}
	if !ex.ResolvedType().Equivalent(typ) {
		// This happens when we overrode the placeholder's type during type
		// checking, since the placeholder's type hint didn't match the desired
		// type for the placeholder. In this case, we cast the expression to
		// the desired type.
		// TODO(jordan,mgartner): Introduce a restriction on what casts are
		// allowed here. Most likely, only implicit casts should be allowed.
		cast := tree.NewTypedCastExpr(ex, typ)
		return cast.Eval(e)
	}
	return ex.Eval(e)
}

func (e *evaluator) EvalRangeCond(cond *tree.RangeCond) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", cond)
}

func (e *evaluator) EvalSubquery(subquery *tree.Subquery) (tree.Datum, error) {
	return e.Planner.EvalSubquery(subquery)
}

func (e *evaluator) EvalRoutineExpr(routine *tree.RoutineExpr) (tree.Datum, error) {
	var err error
	var input tree.Datums
	if len(routine.Input) > 0 {
		// Evaluate each input expression.
		// TODO(mgartner): Use a scratch tree.Datums to avoid allocation on
		// every invocation.
		input = make(tree.Datums, len(routine.Input))
		for i := range routine.Input {
			input[i], err = routine.Input[i].Eval(e)
			if err != nil {
				return nil, err
			}
		}
	}
	return e.Planner.EvalRoutineExpr(e.Context, routine, input)
}

func (e *evaluator) EvalTuple(t *tree.Tuple) (tree.Datum, error) {
	tuple := tree.NewDTupleWithLen(t.ResolvedType(), len(t.Exprs))
	for i, expr := range t.Exprs {
		d, err := expr.(tree.TypedExpr).Eval(e)
		if err != nil {
			return nil, err
		}
		tuple.D[i] = d
	}
	return tuple, nil
}

func (e *evaluator) EvalTupleStar(star *tree.TupleStar) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", star)
}

func (e *evaluator) EvalTypedDummy(*tree.TypedDummy) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("should not eval typed dummy")
}

func (e *evaluator) EvalUnaryExpr(expr *tree.UnaryExpr) (tree.Datum, error) {
	d, err := expr.Expr.(tree.TypedExpr).Eval(e)
	if err != nil {
		return nil, err
	}
	if d == tree.DNull {
		return d, nil
	}
	op := expr.GetOp()
	res, err := op.EvalOp.Eval(e, d)
	if err != nil {
		return nil, err
	}
	if e.TestingKnobs.AssertUnaryExprReturnTypes {
		if err := ensureExpectedType(op.ReturnType, res); err != nil {
			return nil, errors.NewAssertionErrorWithWrappedErrf(err, "unary op %q", expr)
		}
	}
	return res, err
}

func (e *evaluator) EvalUnresolvedName(name *tree.UnresolvedName) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", name)
}

func (e *evaluator) EvalUnqualifiedStar(star tree.UnqualifiedStar) (tree.Datum, error) {
	return nil, errors.AssertionFailedf("unhandled type %T", star)
}

var _ tree.ExprEvaluator = (*evaluator)(nil)
