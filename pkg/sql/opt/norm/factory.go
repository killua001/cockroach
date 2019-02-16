// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package norm

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
)

// ReplaceFunc is the callback function passed to the Factory.Replace method.
// It is called with each child of the expression passed to Replace. See the
// Replace method for more details.
type ReplaceFunc func(e opt.Expr) opt.Expr

// MatchedRuleFunc defines the callback function for the NotifyOnMatchedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been matched. The name of the
// matched rule is passed as a parameter. If the function returns false, then
// the rule is not applied (i.e. skipped).
type MatchedRuleFunc func(ruleName opt.RuleName) bool

// AppliedRuleFunc defines the callback function for the NotifyOnAppliedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been applied.
//
// The function is called with the name of the rule and the expressions it
// affected. For a normalization rule, the source is always nil, and the target
// is the expression constructed by the replace pattern. For an exploration
// rule, the source is the expression matched by the rule, and the target is
// the first expression constructed by the replace pattern. If no expressions
// were constructed, it is nil. Additional expressions beyond the first can be
// accessed by following the NextExpr links on the target expression.
type AppliedRuleFunc func(ruleName opt.RuleName, source, target opt.Expr)

// placeholderError wraps errors that occur during placeholder assignment, and
// is passed as an argument to panic. The panic is caught and converted back to
// an error by AssignPlaceholders.
type placeholderError struct {
	error
}

// Factory constructs a normalized expression tree within the memo. As each
// kind of expression is constructed by the factory, it transitively runs
// normalization transformations defined for that expression type. This may
// result in the construction of a different type of expression than what was
// requested. If, after normalization, the expression is already part of the
// memo, then construction is a no-op. Otherwise, a new memo group is created,
// with the normalized expression as its first and only expression.
//
// Factory is largely auto-generated by optgen. The generated code can be found
// in factory.og.go. The factory.go file contains helper functions that are
// invoked by normalization patterns. While most patterns are specified in the
// Optgen DSL, the factory always calls the `onConstruct` method as its last
// step, in order to allow any custom manual code to execute.
type Factory struct {
	evalCtx *tree.EvalContext

	// mem is the Memo data structure that the factory builds.
	mem *memo.Memo

	// funcs is the struct used to call all custom match and replace functions
	// used by the normalization rules. It wraps an unnamed xfunc.CustomFuncs,
	// so it provides a clean interface for calling functions from both the norm
	// and xfunc packages using the same prefix.
	funcs CustomFuncs

	// matchedRule is the callback function that is invoked each time a normalize
	// rule has been matched by the factory. It can be set via a call to the
	// NotifyOnMatchedRule method.
	matchedRule MatchedRuleFunc

	// appliedRule is the callback function which is invoked each time a normalize
	// rule has been applied by the factory. It can be set via a call to the
	// NotifyOnAppliedRule method.
	appliedRule AppliedRuleFunc
}

// Init initializes a Factory structure with a new, blank memo structure inside.
// This must be called before the factory can be used (or reused).
func (f *Factory) Init(evalCtx *tree.EvalContext) {
	// Initialize (or reinitialize) the memo.
	if f.mem == nil {
		f.mem = &memo.Memo{}
	}
	f.mem.Init(evalCtx)

	f.evalCtx = evalCtx
	f.funcs.Init(f)
	f.matchedRule = nil
	f.appliedRule = nil
}

// DetachMemo extracts the memo from the optimizer, and then re-initializes the
// factory so that its reuse will not impact the detached memo. This method is
// used to extract a read-only memo during the PREPARE phase.
func (f *Factory) DetachMemo() *memo.Memo {
	detach := f.mem
	f.mem = nil
	f.Init(f.evalCtx)
	return detach
}

// DisableOptimizations disables all transformation rules. The unaltered input
// expression tree becomes the output expression tree (because no transforms
// are applied).
func (f *Factory) DisableOptimizations() {
	f.NotifyOnMatchedRule(func(opt.RuleName) bool { return false })
}

// NotifyOnMatchedRule sets a callback function which is invoked each time a
// normalize rule has been matched by the factory. If matchedRule is nil, then
// no further notifications are sent, and all rules are applied by default. In
// addition, callers can invoke the DisableOptimizations convenience method to
// disable all rules.
func (f *Factory) NotifyOnMatchedRule(matchedRule MatchedRuleFunc) {
	f.matchedRule = matchedRule
}

// NotifyOnAppliedRule sets a callback function which is invoked each time a
// normalize rule has been applied by the factory. If appliedRule is nil, then
// no further notifications are sent.
func (f *Factory) NotifyOnAppliedRule(appliedRule AppliedRuleFunc) {
	f.appliedRule = appliedRule
}

// Memo returns the memo structure that the factory is operating upon.
func (f *Factory) Memo() *memo.Memo {
	return f.mem
}

// Metadata returns the query-specific metadata, which includes information
// about the columns and tables used in this particular query.
func (f *Factory) Metadata() *opt.Metadata {
	return f.mem.Metadata()
}

// CustomFuncs returns the set of custom functions used by normalization rules.
func (f *Factory) CustomFuncs() *CustomFuncs {
	return &f.funcs
}

// CopyAndReplace builds this factory's memo by constructing a copy of a subtree
// that is part of another memo. That memo's metadata is copied to this
// factory's memo so that tables and columns referenced by the copied memo can
// keep the same ids. The copied subtree becomes the root of the destination
// memo, having the given physical properties.
//
// The "replace" callback function allows the caller to override the default
// traversal and cloning behavior with custom logic. It is called for each node
// in the "from" subtree, and has the choice of constructing an arbitrary
// replacement node, or delegating to the default behavior by calling
// CopyAndReplaceDefault, which constructs a copy of the source operator using
// children returned by recursive calls to the replace callback. Note that if a
// non-leaf replacement node is constructed, its inputs must be copied using
// CopyAndReplaceDefault.
//
// Sample usage:
//
//   var replaceFn ReplaceFunc
//   replaceFn = func(e opt.Expr) opt.Expr {
//     if e.Op() == opt.PlaceholderOp {
//       return f.ConstructConst(evalPlaceholder(e))
//     }
//
//     // Copy e, calling replaceFn on its inputs recursively.
//     return f.CopyAndReplaceDefault(e, replaceFn)
//   }
//
//   f.CopyAndReplace(from, fromProps, replaceFn)
//
// NOTE: Callers must take care to always create brand new copies of non-
// singleton source nodes rather than referencing existing nodes. The source
// memo should always be treated as immutable, and the destination memo must be
// completely independent of it once CopyAndReplace has completed.
func (f *Factory) CopyAndReplace(
	from memo.RelExpr, fromProps *physical.Required, replace ReplaceFunc,
) {
	if !f.mem.IsEmpty() {
		panic("destination memo must be empty")
	}

	// Copy all metadata to the target memo so that referenced tables and columns
	// can keep the same ids they had in the "from" memo.
	f.mem.Metadata().CopyFrom(from.Memo().Metadata())

	// Perform copy and replacement, and store result as the root of this
	// factory's memo.
	to := f.invokeReplace(from, replace).(memo.RelExpr)
	f.Memo().SetRoot(to, fromProps)
}

// AssignPlaceholders is used just before execution of a prepared Memo. It makes
// a copy of the given memo, but with any placeholder values replaced by their
// assigned values. This can trigger additional normalization rules that can
// substantially rewrite the tree. Once all placeholders are assigned, the
// exploration phase can begin.
func (f *Factory) AssignPlaceholders(from *memo.Memo) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// This code allows us to propagate errors without adding lots of checks
			// for `if err != nil` throughout the construction code. This is only
			// possible because the code does not update shared state and does not
			// manipulate locks.
			if bldErr, ok := r.(placeholderError); ok {
				err = bldErr.error
			} else {
				panic(r)
			}
		}
	}()

	// Copy the "from" memo to this memo, replacing any Placeholder operators as
	// the copy proceeds.
	var replaceFn ReplaceFunc
	replaceFn = func(e opt.Expr) opt.Expr {
		if placeholder, ok := e.(*memo.PlaceholderExpr); ok {
			d, err := e.(*memo.PlaceholderExpr).Value.Eval(f.evalCtx)
			if err != nil {
				panic(placeholderError{err})
			}
			return f.ConstructConstVal(d, placeholder.DataType())
		}
		return f.CopyAndReplaceDefault(e, replaceFn)
	}
	f.CopyAndReplace(from.RootExpr().(memo.RelExpr), from.RootProps(), replaceFn)

	return nil
}

// onConstructRelational is called as a final step by each factory method that
// constructs a relational expression, so that any custom manual pattern
// matching/replacement code can be run.
func (f *Factory) onConstructRelational(rel memo.RelExpr) memo.RelExpr {
	// [SimplifyZeroCardinalityGroup]
	// SimplifyZeroCardinalityGroup replaces a group with [0 - 0] cardinality
	// with an empty values expression. It is placed here because it depends on
	// the logical properties of the group in question.
	if rel.Op() != opt.ValuesOp {
		relational := rel.Relational()
		if relational.Cardinality.IsZero() && !relational.CanHaveSideEffects {
			if f.matchedRule == nil || f.matchedRule(opt.SimplifyZeroCardinalityGroup) {
				values := f.funcs.ConstructEmptyValues(relational.OutputCols)
				if f.appliedRule != nil {
					f.appliedRule(opt.SimplifyZeroCardinalityGroup, nil, values)
				}
				return values
			}
		}
	}

	return rel
}

// onConstructScalar is called as a final step by each factory method that
// constructs a scalar expression, so that any custom manual pattern matching/
// replacement code can be run.
func (f *Factory) onConstructScalar(scalar opt.ScalarExpr) opt.ScalarExpr {
	return scalar
}

// ----------------------------------------------------------------------
//
// Convenience construction methods.
//
// ----------------------------------------------------------------------

// ConstructZeroValues constructs a Values operator with zero rows and zero
// columns. It is used to create a dummy input for operators like CreateTable.
func (f *Factory) ConstructZeroValues() memo.RelExpr {
	return f.ConstructValues(memo.EmptyScalarListExpr, opt.ColList{})
}

// ConstructJoin constructs the join operator that corresponds to the given join
// operator type.
func (f *Factory) ConstructJoin(
	joinOp opt.Operator, left, right memo.RelExpr, on memo.FiltersExpr, private *memo.JoinPrivate,
) memo.RelExpr {
	switch joinOp {
	case opt.InnerJoinOp:
		return f.ConstructInnerJoin(left, right, on, private)
	case opt.InnerJoinApplyOp:
		return f.ConstructInnerJoinApply(left, right, on, private)
	case opt.LeftJoinOp:
		return f.ConstructLeftJoin(left, right, on, private)
	case opt.LeftJoinApplyOp:
		return f.ConstructLeftJoinApply(left, right, on, private)
	case opt.RightJoinOp:
		return f.ConstructRightJoin(left, right, on, private)
	case opt.RightJoinApplyOp:
		return f.ConstructRightJoinApply(left, right, on, private)
	case opt.FullJoinOp:
		return f.ConstructFullJoin(left, right, on, private)
	case opt.FullJoinApplyOp:
		return f.ConstructFullJoinApply(left, right, on, private)
	case opt.SemiJoinOp:
		return f.ConstructSemiJoin(left, right, on, private)
	case opt.SemiJoinApplyOp:
		return f.ConstructSemiJoinApply(left, right, on, private)
	case opt.AntiJoinOp:
		return f.ConstructAntiJoin(left, right, on, private)
	case opt.AntiJoinApplyOp:
		return f.ConstructAntiJoinApply(left, right, on, private)
	}
	panic(fmt.Sprintf("unexpected join operator: %v", joinOp))
}

// ConstructConstVal constructs one of the constant value operators from the
// given datum value. While most constants are represented with Const, there are
// special-case operators for True, False, and Null, to make matching easier.
// Null operators require the static type to be specified, so that rewrites do
// not change it.
func (f *Factory) ConstructConstVal(d tree.Datum, t types.T) opt.ScalarExpr {
	if d == tree.DNull {
		return f.ConstructNull(t)
	}
	if boolVal, ok := d.(*tree.DBool); ok {
		// Map True/False datums to True/False operator.
		if *boolVal {
			return memo.TrueSingleton
		}
		return memo.FalseSingleton
	}
	return f.ConstructConst(d)
}
