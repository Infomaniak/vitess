/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/vtgate/evalengine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/abstract"

	"vitess.io/vitess/go/vt/vtgate/semantics"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"

	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/engine"
)

type horizonPlanning struct {
	sel             *sqlparser.Select
	qp              *abstract.QueryProjection
	needsTruncation bool
	vtgateGrouping  bool
}

func (hp *horizonPlanning) planHorizon(ctx *planningContext, plan logicalPlan) (logicalPlan, error) {
	rb, isRoute := plan.(*routeGen4)
	if !isRoute && ctx.semTable.ShardedError != nil {
		return nil, ctx.semTable.ShardedError
	}

	if isRoute && rb.isSingleShard() {
		err := planSingleShardRoutePlan(hp.sel, rb)
		if err != nil {
			return nil, err
		}
		return plan, nil
	}

	qp, err := abstract.CreateQPFromSelect(hp.sel, ctx.semTable)
	if err != nil {
		return nil, err
	}

	hp.qp = qp

	needAggrOrHaving := hp.qp.NeedsAggregation() || hp.sel.Having != nil
	canShortcut := isRoute && !needAggrOrHaving && len(hp.qp.OrderExprs) == 0

	if needAggrOrHaving {
		plan, err = hp.planAggregations(ctx, plan)
		if err != nil {
			return nil, err
		}
	} else {
		_, isOA := plan.(*orderedAggregate)
		if isOA {
			plan = &simpleProjection{
				logicalPlanCommon: newBuilderCommon(plan),
				eSimpleProj:       &engine.SimpleProjection{},
			}
		}

		if canShortcut {
			err = planSingleShardRoutePlan(hp.sel, rb)
			if err != nil {
				return nil, err
			}
		} else {
			err = pushProjections(ctx, plan, hp.qp.SelectExprs)
			if err != nil {
				return nil, err
			}
		}
	}

	// If we have done the shortcut that means we already planned order by
	// and group by, thus we don't need to do it again.
	if !canShortcut {
		if len(hp.qp.OrderExprs) > 0 {
			plan, err = hp.planOrderBy(ctx, hp.qp.OrderExprs, plan)
			if err != nil {
				return nil, err
			}
		}

		if hp.qp.CanPushDownSorting && hp.vtgateGrouping {
			plan, err = hp.planGroupByUsingOrderBy(ctx, plan)
			if err != nil {
				return nil, err
			}
		}
	}

	plan, err = hp.planDistinct(ctx, plan)
	if err != nil {
		return nil, err
	}

	err = hp.truncateColumnsIfNeeded(plan)
	if err != nil {
		return nil, err
	}

	return plan, nil
}

func pushProjections(ctx *planningContext, plan logicalPlan, selectExprs []abstract.SelectExpr) error {
	for _, e := range selectExprs {
		aliasExpr, err := e.GetAliasedExpr()
		if err != nil {
			return err
		}
		if _, _, err := pushProjection(aliasExpr, plan, ctx.semTable, true, false, false); err != nil {
			return err
		}
	}
	return nil
}

func (hp *horizonPlanning) truncateColumnsIfNeeded(plan logicalPlan) error {
	if !hp.needsTruncation {
		return nil
	}

	switch p := plan.(type) {
	case *routeGen4:
		p.eroute.SetTruncateColumnCount(hp.sel.GetColumnCount())
	case *joinGen4, *semiJoin, *hashJoin:
		// since this is a join, we can safely add extra columns and not need to truncate them
	case *orderedAggregate:
		p.eaggr.SetTruncateColumnCount(hp.sel.GetColumnCount())
	case *memorySort:
		p.truncater.SetTruncateColumnCount(hp.sel.GetColumnCount())
	case *pulloutSubquery:
		return hp.truncateColumnsIfNeeded(p.underlying)
	case *filter:
		return hp.truncateColumnsIfNeeded(p.input)
	default:
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "plan type not known for column truncation: %T", plan)
	}

	return nil
}

// pushProjection pushes a projection to the plan.
func pushProjection(expr *sqlparser.AliasedExpr, plan logicalPlan, semTable *semantics.SemTable, inner, reuseCol, hasAggregation bool) (offset int, added bool, err error) {
	switch node := plan.(type) {
	case *routeGen4:
		_, isColName := expr.Expr.(*sqlparser.ColName)
		if !isColName {
			_, err := evalengine.Convert(expr.Expr, semTable)
			if err != nil {
				if vterrors.Code(err) != vtrpcpb.Code_UNIMPLEMENTED {
					return 0, false, err
				} else if !inner {
					return 0, false, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard left join and column expressions")
				}
			}
		}
		if reuseCol {
			if i := checkIfAlreadyExists(expr, node.Select, semTable); i != -1 {
				return i, false, nil
			}
		}
		expr.Expr = sqlparser.RemoveKeyspaceFromColName(expr.Expr)
		sel, isSel := node.Select.(*sqlparser.Select)
		if !isSel {
			return 0, false, vterrors.NewErrorf(vtrpcpb.Code_INVALID_ARGUMENT, vterrors.BadFieldError, "Unknown column '%s' in 'order clause'", sqlparser.String(expr))
		}

		// if we are trying to push a projection that belongs to a DerivedTable
		// we rewrite that expression, so it matches the column name used inside
		// that derived table.
		err = rewriteProjectionOfDerivedTable(expr, semTable)
		if err != nil {
			return 0, false, err
		}

		offset := len(sel.SelectExprs)
		sel.SelectExprs = append(sel.SelectExprs, expr)
		return offset, true, nil
	case *hashJoin:
		lhsSolves := node.Left.ContainsTables()
		rhsSolves := node.Right.ContainsTables()
		deps := semTable.RecursiveDeps(expr.Expr)
		var column int
		var appended bool
		passDownReuseCol := reuseCol
		if !reuseCol {
			passDownReuseCol = expr.As.IsEmpty()
		}
		switch {
		case deps.IsSolvedBy(lhsSolves):
			offset, added, err := pushProjection(expr, node.Left, semTable, inner, passDownReuseCol, hasAggregation)
			if err != nil {
				return 0, false, err
			}
			column = -(offset + 1)
			appended = added
		case deps.IsSolvedBy(rhsSolves):
			offset, added, err := pushProjection(expr, node.Right, semTable, inner && node.Opcode != engine.LeftJoin, passDownReuseCol, hasAggregation)
			if err != nil {
				return 0, false, err
			}
			column = offset + 1
			appended = added
		default:
			// if an expression has aggregation, then it should not be split up and pushed to both sides,
			// for example an expression like count(*) will have dependencies on both sides, but we should not push it
			// instead we should return an error
			if hasAggregation {
				return 0, false, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard query with aggregates")
			}
			return 0, false, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: hash join with projection from both sides of the join")
		}
		if reuseCol && !appended {
			for idx, col := range node.Cols {
				if column == col {
					return idx, false, nil
				}
			}
			// the column was not appended to either child, but we could not find it in out cols list,
			// so we'll still add it
		}
		node.Cols = append(node.Cols, column)
		return len(node.Cols) - 1, true, nil
	case *joinGen4:
		lhsSolves := node.Left.ContainsTables()
		rhsSolves := node.Right.ContainsTables()
		deps := semTable.RecursiveDeps(expr.Expr)
		var column int
		var appended bool
		passDownReuseCol := reuseCol
		if !reuseCol {
			passDownReuseCol = expr.As.IsEmpty()
		}
		switch {
		case deps.IsSolvedBy(lhsSolves):
			offset, added, err := pushProjection(expr, node.Left, semTable, inner, passDownReuseCol, hasAggregation)
			if err != nil {
				return 0, false, err
			}
			column = -(offset + 1)
			appended = added
		case deps.IsSolvedBy(rhsSolves):
			offset, added, err := pushProjection(expr, node.Right, semTable, inner && node.Opcode != engine.LeftJoin, passDownReuseCol, hasAggregation)
			if err != nil {
				return 0, false, err
			}
			column = offset + 1
			appended = added
		default:
			// if an expression has aggregation, then it should not be split up and pushed to both sides,
			// for example an expression like count(*) will have dependencies on both sides, but we should not push it
			// instead we should return an error
			if hasAggregation {
				return 0, false, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard query with aggregates")
			}
			// now we break the expression into left and right side dependencies and rewrite the left ones to bind variables
			bvName, cols, rewrittenExpr, err := breakExpressionInLHSandRHS(expr.Expr, semTable, lhsSolves)
			if err != nil {
				return 0, false, err
			}
			// go over all the columns coming from the left side of the tree and push them down. While at it, also update the bind variable map.
			// It is okay to reuse the columns on the left side since
			// the final expression which will be selected will be pushed into the right side.
			for i, col := range cols {
				colOffset, _, err := pushProjection(&sqlparser.AliasedExpr{Expr: col}, node.Left, semTable, inner, true, false)
				if err != nil {
					return 0, false, err
				}
				node.Vars[bvName[i]] = colOffset
			}
			// push the rewritten expression on the right side of the tree. Here we should take care whether we want to reuse the expression or not.
			expr.Expr = rewrittenExpr
			offset, added, err := pushProjection(expr, node.Right, semTable, inner && node.Opcode != engine.LeftJoin, passDownReuseCol, false)
			if err != nil {
				return 0, false, err
			}
			column = offset + 1
			appended = added
		}
		if reuseCol && !appended {
			for idx, col := range node.Cols {
				if column == col {
					return idx, false, nil
				}
			}
			// the column was not appended to either child, but we could not find it in out cols list,
			// so we'll still add it
		}
		node.Cols = append(node.Cols, column)
		return len(node.Cols) - 1, true, nil
	case *pulloutSubquery:
		// push projection to the outer query
		return pushProjection(expr, node.underlying, semTable, inner, reuseCol, hasAggregation)
	case *simpleProjection:
		offset, _, err := pushProjection(expr, node.input, semTable, inner, true, hasAggregation)
		if err != nil {
			return 0, false, err
		}
		for i, value := range node.eSimpleProj.Cols {
			// we return early if we already have the column in the simple projection's
			// output list so we do not add it again.
			if reuseCol && value == offset {
				return i, false, nil
			}
		}
		node.eSimpleProj.Cols = append(node.eSimpleProj.Cols, offset)
		return len(node.eSimpleProj.Cols) - 1, true, nil
	case *orderedAggregate:
		colName, isColName := expr.Expr.(*sqlparser.ColName)
		for _, aggregate := range node.eaggr.Aggregates {
			if sqlparser.EqualsExpr(aggregate.Expr, expr.Expr) {
				return aggregate.Col, false, nil
			}
			if isColName && colName.Name.EqualString(aggregate.Alias) {
				return aggregate.Col, false, nil
			}
		}
		return 0, false, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "cannot push projections in ordered aggregates")
	case *vindexFunc:
		colsBefore := len(node.eVindexFunc.Cols)
		i, err := node.SupplyProjection(expr, reuseCol)
		if err != nil {
			return 0, false, err
		}
		return i /* col added */, len(node.eVindexFunc.Cols) > colsBefore, nil
	case *limit:
		return pushProjection(expr, node.input, semTable, inner, reuseCol, hasAggregation)
	case *distinct:
		return pushProjection(expr, node.input, semTable, inner, reuseCol, hasAggregation)
	case *semiJoin:
		passDownReuseCol := reuseCol
		if !reuseCol {
			passDownReuseCol = expr.As.IsEmpty()
		}
		offset, added, err := pushProjection(expr, node.lhs, semTable, inner, passDownReuseCol, hasAggregation)
		if err != nil {
			return 0, false, err
		}
		column := -(offset + 1)
		if reuseCol && !added {
			for idx, col := range node.cols {
				if column == col {
					return idx, false, nil
				}
			}
		}
		node.cols = append(node.cols, column)
		return len(node.cols) - 1, true, nil
	case *concatenateGen4:
		if hasAggregation {
			return 0, false, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: aggregation on unions")
		}
		offset, added, err := pushProjection(expr, node.sources[0], semTable, inner, reuseCol, hasAggregation)
		if err != nil {
			return 0, false, err
		}
		if added {
			return 0, false, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "pushing projection %v on concatenate should reference an existing column", sqlparser.String(expr))
		}
		return offset, false, nil
	default:
		return 0, false, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "[BUG] push projection does not yet support: %T", node)
	}
}

func rewriteProjectionOfDerivedTable(expr *sqlparser.AliasedExpr, semTable *semantics.SemTable) error {
	ti, err := semTable.TableInfoForExpr(expr.Expr)
	if err != nil && err != semantics.ErrMultipleTables {
		return err
	}
	_, isDerivedTable := ti.(*semantics.DerivedTable)
	if isDerivedTable {
		expr.Expr, err = semantics.RewriteDerivedExpression(expr.Expr, ti)
		if err != nil {
			return err
		}
	}
	return nil
}

func checkIfAlreadyExists(expr *sqlparser.AliasedExpr, node sqlparser.SelectStatement, semTable *semantics.SemTable) int {
	exprDep := semTable.RecursiveDeps(expr.Expr)
	// Here to find if the expr already exists in the SelectStatement, we have 3 cases
	// input is a Select -> In this case we want to search in the select
	// input is a Union -> In this case we want to search in the First Select of the Union
	// input is a Parenthesised Select -> In this case we want to search in the select
	// all these three cases are handled by the call to GetFirstSelect.
	sel := sqlparser.GetFirstSelect(node)

	for i, selectExpr := range sel.SelectExprs {
		selectExpr, ok := selectExpr.(*sqlparser.AliasedExpr)
		if !ok {
			continue
		}

		selectExprCol, isSelectExprCol := selectExpr.Expr.(*sqlparser.ColName)
		exprCol, isExprCol := expr.Expr.(*sqlparser.ColName)
		selectExprDep := semTable.RecursiveDeps(selectExpr.Expr)

		// Check that the two expressions have the same dependencies
		if selectExprDep != exprDep {
			continue
		}

		if selectExpr.As.IsEmpty() {
			// we don't have an alias

			if isSelectExprCol && isExprCol && exprCol.Name.Equal(selectExprCol.Name) {
				// the expressions are ColName, we compare their name
				return i
			} else if sqlparser.EqualsExpr(selectExpr.Expr, expr.Expr) {
				// the expressions are not ColName, so we just compare the expressions
				return i
			}
		} else if isExprCol && selectExpr.As.Equal(exprCol.Name) {
			// we have an aliased column, checking if the expression is matching the alias
			return i
		}
	}
	return -1
}

func (hp *horizonPlanning) haveToTruncate(v bool) {
	hp.needsTruncation = hp.needsTruncation || v
}

func (hp *horizonPlanning) planAggregations(ctx *planningContext, plan logicalPlan) (logicalPlan, error) {
	newPlan := plan
	var oa *orderedAggregate
	uniqVindex := hasUniqueVindex(ctx.vschema, ctx.semTable, hp.qp.GroupByExprs)
	joinPlan := isJoin(plan)
	if !uniqVindex || joinPlan {
		if hp.qp.ProjectionError != nil {
			return nil, hp.qp.ProjectionError
		}
		eaggr := &engine.OrderedAggregate{}
		oa = &orderedAggregate{
			resultsBuilder: resultsBuilder{
				logicalPlanCommon: newBuilderCommon(plan),
				weightStrings:     make(map[*resultColumn]int),
				truncater:         eaggr,
			},
			eaggr: eaggr,
		}
		newPlan = oa
		hp.vtgateGrouping = true
	}

	if joinPlan && hp.qp.HasAggr && len(hp.qp.GroupByExprs) > 0 {
		return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard query with aggregates")
	}

	for _, e := range hp.qp.SelectExprs {
		aliasExpr, err := e.GetAliasedExpr()
		if err != nil {
			return nil, err
		}

		// push all expression if they are non-aggregating or the plan is not ordered aggregated plan.
		if !e.Aggr || oa == nil {
			_, _, err := pushProjection(aliasExpr, plan, ctx.semTable, true, false, false)
			if err != nil {
				return nil, err
			}
			continue
		}

		fExpr, isFunc := aliasExpr.Expr.(*sqlparser.FuncExpr)
		if !isFunc {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: in scatter query: complex aggregate expression")
		}
		funcName := fExpr.Name.Lowered()
		opcode, found := engine.SupportedAggregates[funcName]
		if !found {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: in scatter query: aggregation function '%s'", funcName)
		}
		handleDistinct, innerAliased, err := hp.needDistinctHandling(ctx, fExpr, opcode, plan)
		if err != nil {
			return nil, err
		}

		pushExpr, alias, opcode := hp.createPushExprAndAlias(e, handleDistinct, innerAliased, opcode, oa)
		offset, _, err := pushProjection(pushExpr, plan, ctx.semTable, true, false, true)
		if err != nil {
			return nil, err
		}
		oa.eaggr.Aggregates = append(oa.eaggr.Aggregates, &engine.AggregateParams{
			Opcode: opcode,
			Col:    offset,
			Alias:  alias,
			Expr:   fExpr,
		})
	}

	for _, groupExpr := range hp.qp.GroupByExprs {
		added, err := planGroupByGen4(groupExpr, newPlan, ctx.semTable, false)
		if err != nil {
			return nil, err
		}
		hp.haveToTruncate(added)
	}

	newPlan, err := hp.planHaving(ctx, newPlan)
	if err != nil {
		return nil, err
	}

	if !hp.qp.CanPushDownSorting && oa != nil {
		var orderExprs []abstract.OrderBy
		// if we can't at a later stage push down the sorting to our inputs, we have to do ordering here
		for _, groupExpr := range hp.qp.GroupByExprs {
			orderExprs = append(orderExprs, abstract.OrderBy{
				Inner:         &sqlparser.Order{Expr: groupExpr.Inner},
				WeightStrExpr: groupExpr.WeightStrExpr},
			)
		}
		if len(orderExprs) > 0 {
			newInput, err := hp.planOrderBy(ctx, orderExprs, plan)
			if err != nil {
				return nil, err
			}
			oa.input = newInput
			plan = oa
		}
	} else {
		plan = newPlan
	}

	// done with aggregation planning. let's check if we should fail the query
	if _, planIsRoute := plan.(*routeGen4); !planIsRoute {
		// if we had to build up additional operators around the route, we have to fail this query
		for _, expr := range hp.qp.SelectExprs {
			colExpr, err := expr.GetExpr()
			if err != nil {
				return nil, err
			}
			if !sqlparser.IsAggregation(colExpr) && sqlparser.ContainsAggregation(colExpr) {
				return nil, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: in scatter query: complex aggregate expression")
			}
		}
	}

	return plan, nil
}

// createPushExprAndAlias creates the expression that should be pushed down to the leaves,
// and changes the opcode so it is a distinct one if needed
func (hp *horizonPlanning) createPushExprAndAlias(
	expr abstract.SelectExpr,
	handleDistinct bool,
	innerAliased *sqlparser.AliasedExpr,
	opcode engine.AggregateOpcode,
	oa *orderedAggregate,
) (*sqlparser.AliasedExpr, string, engine.AggregateOpcode) {
	aliasExpr, isAlias := expr.Col.(*sqlparser.AliasedExpr)
	if !isAlias {
		return nil, "", 0
	}
	var alias string
	if aliasExpr.As.IsEmpty() {
		alias = sqlparser.String(aliasExpr.Expr)
	} else {
		alias = aliasExpr.As.String()
	}
	if handleDistinct {
		aliasExpr = innerAliased

		switch opcode {
		case engine.AggregateCount:
			opcode = engine.AggregateCountDistinct
		case engine.AggregateSum:
			opcode = engine.AggregateSumDistinct
		}

		oa.eaggr.PreProcess = true
		hp.haveToTruncate(true)
		by := abstract.GroupBy{
			Inner:             innerAliased.Expr,
			WeightStrExpr:     innerAliased.Expr,
			DistinctAggrIndex: len(oa.eaggr.Aggregates) + 1,
		}
		hp.qp.GroupByExprs = append(hp.qp.GroupByExprs, by)
	}
	return aliasExpr, alias, opcode
}

func hasUniqueVindex(vschema ContextVSchema, semTable *semantics.SemTable, groupByExprs []abstract.GroupBy) bool {
	for _, groupByExpr := range groupByExprs {
		if exprHasUniqueVindex(vschema, semTable, groupByExpr.WeightStrExpr) {
			return true
		}
	}
	return false
}

func planGroupByGen4(groupExpr abstract.GroupBy, plan logicalPlan, semTable *semantics.SemTable, wsAdded bool) (bool, error) {
	switch node := plan.(type) {
	case *routeGen4:
		sel := node.Select.(*sqlparser.Select)
		sel.GroupBy = append(sel.GroupBy, groupExpr.Inner)
		// If a weight_string function is added to the select list,
		// then we need to add that to the group by clause otherwise the query will fail on mysql with full_group_by error
		// as the weight_string function might not be functionally dependent on the group by.
		if wsAdded {
			sel.GroupBy = append(sel.GroupBy, weightStringFor(groupExpr.WeightStrExpr))
		}
		return false, nil
	case *joinGen4, *hashJoin:
		_, _, added, err := wrapAndPushExpr(groupExpr.Inner, groupExpr.WeightStrExpr, node, semTable)
		return added, err
	case *orderedAggregate:
		keyCol, wsOffset, colAdded, err := wrapAndPushExpr(groupExpr.Inner, groupExpr.WeightStrExpr, node.input, semTable)
		if err != nil {
			return false, err
		}
		if groupExpr.DistinctAggrIndex == 0 {
			node.eaggr.GroupByKeys = append(node.eaggr.GroupByKeys, &engine.GroupByParams{KeyCol: keyCol, WeightStringCol: wsOffset, Expr: groupExpr.WeightStrExpr, CollationID: semTable.CollationFor(groupExpr.Inner)})
		} else {
			if wsOffset != -1 {
				node.eaggr.Aggregates[groupExpr.DistinctAggrIndex-1].WAssigned = true
				node.eaggr.Aggregates[groupExpr.DistinctAggrIndex-1].WCol = wsOffset
			}
		}
		colAddedRecursively, err := planGroupByGen4(groupExpr, node.input, semTable, wsOffset != -1)
		if err != nil {
			return false, err
		}
		return colAdded || colAddedRecursively, nil
	case *pulloutSubquery:
		return planGroupByGen4(groupExpr, node.underlying, semTable, wsAdded)
	case *semiJoin:
		return false, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: group by in a query having a correlated subquery")
	default:
		return false, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: group by on: %T", plan)
	}
}

func (hp *horizonPlanning) planGroupByUsingOrderBy(ctx *planningContext, plan logicalPlan) (logicalPlan, error) {
	var orderExprs []abstract.OrderBy
	for _, groupExpr := range hp.qp.GroupByExprs {
		addExpr := true
		for _, orderExpr := range hp.qp.OrderExprs {
			if sqlparser.EqualsExpr(groupExpr.Inner, orderExpr.Inner.Expr) {
				addExpr = false
				break
			}
		}
		if addExpr {
			orderExprs = append(orderExprs, abstract.OrderBy{
				Inner:         &sqlparser.Order{Expr: groupExpr.Inner},
				WeightStrExpr: groupExpr.WeightStrExpr},
			)
		}
	}
	if len(orderExprs) > 0 {
		return hp.planOrderBy(ctx, orderExprs, plan)
	}
	return plan, nil
}

func (hp *horizonPlanning) planOrderBy(ctx *planningContext, orderExprs []abstract.OrderBy, plan logicalPlan) (logicalPlan, error) {
	switch plan := plan.(type) {
	case *routeGen4:
		newPlan, truncate, err := planOrderByForRoute(orderExprs, plan, ctx.semTable, hp.qp.HasStar)
		if err != nil {
			return nil, err
		}
		hp.haveToTruncate(truncate)
		return newPlan, nil
	case *joinGen4:
		newPlan, err := hp.planOrderByForJoin(ctx, orderExprs, plan)
		if err != nil {
			return nil, err
		}

		return newPlan, nil
	case *hashJoin:
		newPlan, err := hp.planOrderByForHashJoin(ctx, orderExprs, plan)
		if err != nil {
			return nil, err
		}

		return newPlan, nil
	case *orderedAggregate:
		// remove ORDER BY NULL from the list of order by expressions since we will be doing the ordering on vtgate level so NULL is not useful
		var orderExprsWithoutNils []abstract.OrderBy
		for _, expr := range orderExprs {
			if sqlparser.IsNull(expr.Inner.Expr) {
				continue
			}
			orderExprsWithoutNils = append(orderExprsWithoutNils, expr)
		}
		orderExprs = orderExprsWithoutNils

		for _, order := range orderExprs {
			if sqlparser.ContainsAggregation(order.WeightStrExpr) {
				ms, err := createMemorySortPlanOnAggregation(plan, orderExprs)
				if err != nil {
					return nil, err
				}
				return ms, nil
			}
		}
		newInput, err := hp.planOrderBy(ctx, orderExprs, plan.input)
		if err != nil {
			return nil, err
		}
		plan.input = newInput
		return plan, nil
	case *memorySort:
		return plan, nil
	case *simpleProjection:
		return hp.createMemorySortPlan(ctx, plan, orderExprs, true)
	case *vindexFunc:
		// This is evaluated at VTGate only, so weight_string function cannot be used.
		return hp.createMemorySortPlan(ctx, plan, orderExprs /* useWeightStr */, false)
	case *limit, *semiJoin, *filter, *pulloutSubquery:
		inputs := plan.Inputs()
		if len(inputs) == 0 {
			break
		}
		newFirstInput, err := hp.planOrderBy(ctx, orderExprs, inputs[0])
		if err != nil {
			return nil, err
		}
		inputs[0] = newFirstInput
		err = plan.Rewrite(inputs...)
		if err != nil {
			return nil, err
		}
		return plan, nil
	}
	return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "ordering on complex query %T", plan)
}

func isSpecialOrderBy(o abstract.OrderBy) bool {
	if sqlparser.IsNull(o.Inner.Expr) {
		return true
	}
	f, isFunction := o.Inner.Expr.(*sqlparser.FuncExpr)
	return isFunction && f.Name.Lowered() == "rand"
}

func planOrderByForRoute(orderExprs []abstract.OrderBy, plan *routeGen4, semTable *semantics.SemTable, hasStar bool) (logicalPlan, bool, error) {
	origColCount := plan.Select.GetColumnCount()
	for _, order := range orderExprs {
		err := checkOrderExprCanBePlannedInScatter(plan, order, hasStar)
		if err != nil {
			return nil, false, err
		}
		plan.Select.AddOrder(order.Inner)
		if isSpecialOrderBy(order) {
			continue
		}
		offset, weightStringOffset, _, err := wrapAndPushExpr(order.Inner.Expr, order.WeightStrExpr, plan, semTable)
		if err != nil {
			return nil, false, err
		}
		plan.eroute.OrderBy = append(plan.eroute.OrderBy, engine.OrderByParams{
			Col:             offset,
			WeightStringCol: weightStringOffset,
			Desc:            order.Inner.Direction == sqlparser.DescOrder,
			CollationID:     semTable.CollationFor(order.Inner.Expr),
		})
	}
	return plan, origColCount != plan.Select.GetColumnCount(), nil
}

// checkOrderExprCanBePlannedInScatter verifies that the given order by expression can be planned.
// It checks if the expression exists in the plan's select list when the query is a scatter.
func checkOrderExprCanBePlannedInScatter(plan *routeGen4, order abstract.OrderBy, hasStar bool) error {
	if !hasStar {
		return nil
	}
	sel := sqlparser.GetFirstSelect(plan.Select)
	found := false
	for _, expr := range sel.SelectExprs {
		aliasedExpr, isAliasedExpr := expr.(*sqlparser.AliasedExpr)
		if isAliasedExpr && sqlparser.EqualsExpr(aliasedExpr.Expr, order.Inner.Expr) {
			found = true
			break
		}
	}
	if !found {
		return vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: in scatter query: order by must reference a column in the select list: "+sqlparser.String(order.Inner))
	}
	return nil
}

// wrapAndPushExpr pushes the expression and weighted_string function to the plan using semantics.SemTable
// It returns (expr offset, weight_string offset, new_column added, error)
func wrapAndPushExpr(expr sqlparser.Expr, weightStrExpr sqlparser.Expr, plan logicalPlan, semTable *semantics.SemTable) (int, int, bool, error) {
	offset, added, err := pushProjection(&sqlparser.AliasedExpr{Expr: expr}, plan, semTable, true, true, false)
	if err != nil {
		return 0, 0, false, err
	}
	if weightStrExpr == nil {
		return offset, -1, added, nil
	}
	if !sqlparser.IsColName(expr) {
		unary, ok := expr.(*sqlparser.UnaryExpr)
		if ok && sqlparser.IsColName(unary.Expr) {
			expr = unary.Expr
		} else {
			return 0, 0, false, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: in scatter query: complex order by expression: %s", sqlparser.String(expr))
		}
	}
	qt := semTable.TypeFor(expr)
	wsNeeded := true
	if qt != nil && sqltypes.IsNumber(*qt) {
		wsNeeded = false
	}

	weightStringOffset := -1
	var wAdded bool
	if wsNeeded {
		weightStringOffset, wAdded, err = pushProjection(&sqlparser.AliasedExpr{Expr: weightStringFor(weightStrExpr)}, plan, semTable, true, true, false)
		if err != nil {
			return 0, 0, false, err
		}
	}
	return offset, weightStringOffset, added || wAdded, nil
}

func weightStringFor(expr sqlparser.Expr) sqlparser.Expr {
	return &sqlparser.FuncExpr{
		Name: sqlparser.NewColIdent("weight_string"),
		Exprs: []sqlparser.SelectExpr{
			&sqlparser.AliasedExpr{
				Expr: expr,
			},
		},
	}

}

func (hp *horizonPlanning) planOrderByForHashJoin(ctx *planningContext, orderExprs []abstract.OrderBy, plan *hashJoin) (logicalPlan, error) {
	if len(orderExprs) == 1 && isSpecialOrderBy(orderExprs[0]) {
		rhs, err := hp.planOrderBy(ctx, orderExprs, plan.Right)
		if err != nil {
			return nil, err
		}
		plan.Right = rhs
		return plan, nil
	}
	if orderExprsDependsOnTableSet(orderExprs, ctx.semTable, plan.Right.ContainsTables()) {
		newRight, err := hp.planOrderBy(ctx, orderExprs, plan.Right)
		if err != nil {
			return nil, err
		}
		plan.Right = newRight
		return plan, nil
	}
	sortPlan, err := hp.createMemorySortPlan(ctx, plan, orderExprs, true)
	if err != nil {
		return nil, err
	}
	return sortPlan, nil
}

func (hp *horizonPlanning) planOrderByForJoin(ctx *planningContext, orderExprs []abstract.OrderBy, plan *joinGen4) (logicalPlan, error) {
	if len(orderExprs) == 1 && isSpecialOrderBy(orderExprs[0]) {
		lhs, err := hp.planOrderBy(ctx, orderExprs, plan.Left)
		if err != nil {
			return nil, err
		}
		rhs, err := hp.planOrderBy(ctx, orderExprs, plan.Right)
		if err != nil {
			return nil, err
		}
		plan.Left = lhs
		plan.Right = rhs
		return plan, nil
	}
	if orderExprsDependsOnTableSet(orderExprs, ctx.semTable, plan.Left.ContainsTables()) {
		newLeft, err := hp.planOrderBy(ctx, orderExprs, plan.Left)
		if err != nil {
			return nil, err
		}
		plan.Left = newLeft
		return plan, nil
	}
	sortPlan, err := hp.createMemorySortPlan(ctx, plan, orderExprs, true)
	if err != nil {
		return nil, err
	}
	return sortPlan, nil
}

func createMemorySortPlanOnAggregation(plan *orderedAggregate, orderExprs []abstract.OrderBy) (logicalPlan, error) {
	primitive := &engine.MemorySort{}
	ms := &memorySort{
		resultsBuilder: resultsBuilder{
			logicalPlanCommon: newBuilderCommon(plan),
			weightStrings:     make(map[*resultColumn]int),
			truncater:         primitive,
		},
		eMemorySort: primitive,
	}

	for _, order := range orderExprs {
		offset, woffset, idx, found := findExprInOrderedAggr(plan, order)
		if !found {
			return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "expected to find the order by expression (%s) in orderedAggregate", sqlparser.String(order.Inner))
		}

		collationID := collations.Unknown
		if woffset != -1 {
			collationID = plan.eaggr.GroupByKeys[idx].CollationID
		}
		ms.eMemorySort.OrderBy = append(ms.eMemorySort.OrderBy, engine.OrderByParams{
			Col:               offset,
			WeightStringCol:   woffset,
			Desc:              order.Inner.Direction == sqlparser.DescOrder,
			StarColFixedIndex: offset,
			CollationID:       collationID,
		})
	}
	return ms, nil
}

func findExprInOrderedAggr(plan *orderedAggregate, order abstract.OrderBy) (keyCol int, weightStringCol int, index int, found bool) {
	for idx, key := range plan.eaggr.GroupByKeys {
		if sqlparser.EqualsExpr(order.WeightStrExpr, key.Expr) {
			return key.KeyCol, key.WeightStringCol, idx, true
		}
	}
	for idx, aggregate := range plan.eaggr.Aggregates {
		if sqlparser.EqualsExpr(order.WeightStrExpr, aggregate.Expr) {
			return aggregate.Col, -1, idx, true
		}
	}
	return 0, 0, 0, false
}

func (hp *horizonPlanning) createMemorySortPlan(ctx *planningContext, plan logicalPlan, orderExprs []abstract.OrderBy, useWeightStr bool) (logicalPlan, error) {
	primitive := &engine.MemorySort{}
	ms := &memorySort{
		resultsBuilder: resultsBuilder{
			logicalPlanCommon: newBuilderCommon(plan),
			weightStrings:     make(map[*resultColumn]int),
			truncater:         primitive,
		},
		eMemorySort: primitive,
	}

	for _, order := range orderExprs {
		wsExpr := order.WeightStrExpr
		if !useWeightStr {
			wsExpr = nil
		}
		offset, weightStringOffset, added, err := wrapAndPushExpr(order.Inner.Expr, wsExpr, plan, ctx.semTable)
		if err != nil {
			return nil, err
		}
		hp.haveToTruncate(added)
		ms.eMemorySort.OrderBy = append(ms.eMemorySort.OrderBy, engine.OrderByParams{
			Col:               offset,
			WeightStringCol:   weightStringOffset,
			Desc:              order.Inner.Direction == sqlparser.DescOrder,
			StarColFixedIndex: offset,
			CollationID:       ctx.semTable.CollationFor(order.Inner.Expr),
		})
	}
	return ms, nil
}

func orderExprsDependsOnTableSet(orderExprs []abstract.OrderBy, semTable *semantics.SemTable, ts semantics.TableSet) bool {
	for _, expr := range orderExprs {
		exprDependencies := semTable.RecursiveDeps(expr.Inner.Expr)
		if !exprDependencies.IsSolvedBy(ts) {
			return false
		}
	}
	return true
}

func (hp *horizonPlanning) planDistinct(ctx *planningContext, plan logicalPlan) (logicalPlan, error) {
	if !hp.qp.NeedsDistinct() {
		return plan, nil
	}
	switch p := plan.(type) {
	case *routeGen4:
		// we always make the underlying query distinct,
		// and then we might also add a distinct operator on top if it is needed
		p.Select.MakeDistinct()
		if p.isSingleShard() || selectHasUniqueVindex(ctx.vschema, ctx.semTable, hp.qp.SelectExprs) {
			return plan, nil
		}

		return hp.addDistinct(ctx, plan)
	case *joinGen4, *pulloutSubquery:
		return hp.addDistinct(ctx, plan)
	case *orderedAggregate:
		return hp.planDistinctOA(ctx.semTable, p)
	default:
		return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "unknown plan type for DISTINCT %T", plan)
	}
}

func (hp *horizonPlanning) planDistinctOA(semTable *semantics.SemTable, currPlan *orderedAggregate) (logicalPlan, error) {
	eaggr := &engine.OrderedAggregate{}
	oa := &orderedAggregate{
		resultsBuilder: resultsBuilder{
			logicalPlanCommon: newBuilderCommon(currPlan),
			weightStrings:     make(map[*resultColumn]int),
			truncater:         eaggr,
		},
		eaggr: eaggr,
	}
	for _, sExpr := range hp.qp.SelectExprs {
		expr, err := sExpr.GetExpr()
		if err != nil {
			return nil, err
		}
		found := false
		for _, grpParam := range currPlan.eaggr.GroupByKeys {
			if sqlparser.EqualsExpr(expr, grpParam.Expr) {
				found = true
				eaggr.GroupByKeys = append(eaggr.GroupByKeys, grpParam)
				break
			}
		}
		if found {
			continue
		}
		for _, aggrParam := range currPlan.eaggr.Aggregates {
			if sqlparser.EqualsExpr(expr, aggrParam.Expr) {
				found = true
				eaggr.GroupByKeys = append(eaggr.GroupByKeys, &engine.GroupByParams{KeyCol: aggrParam.Col, WeightStringCol: -1, CollationID: semTable.CollationFor(expr)})
				break
			}
		}
		if !found {
			return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] unable to plan distinct query as the column is not projected: %s", sqlparser.String(sExpr.Col))
		}
	}
	return oa, nil
}

func (hp *horizonPlanning) addDistinct(ctx *planningContext, plan logicalPlan) (logicalPlan, error) {
	eaggr := &engine.OrderedAggregate{}
	var orderExprs []abstract.OrderBy
	for index, sExpr := range hp.qp.SelectExprs {
		aliasExpr, err := sExpr.GetAliasedExpr()
		if err != nil {
			return nil, err
		}
		if isAmbiguousOrderBy(index, aliasExpr.As, hp.qp.SelectExprs) {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "generating order by clause: ambiguous symbol reference: %s", sqlparser.String(aliasExpr.As))
		}
		var inner sqlparser.Expr
		if aliasExpr.As.IsEmpty() {
			inner = aliasExpr.Expr
		} else {
			// If we have an alias, we need to use the alias and not the original expression
			// to make sure dependencies work correctly,
			// we simply copy the dependencies of the original expression here
			inner = sqlparser.NewColName(aliasExpr.As.String())
			ctx.semTable.CopyDependencies(aliasExpr.Expr, inner)
		}
		grpParam := &engine.GroupByParams{KeyCol: index, WeightStringCol: -1, CollationID: ctx.semTable.CollationFor(inner)}
		_, wOffset, added, err := wrapAndPushExpr(aliasExpr.Expr, aliasExpr.Expr, plan, ctx.semTable)
		if err != nil {
			return nil, err
		}
		hp.needsTruncation = hp.needsTruncation || added
		grpParam.WeightStringCol = wOffset
		eaggr.GroupByKeys = append(eaggr.GroupByKeys, grpParam)

		orderExprs = append(orderExprs, abstract.OrderBy{
			Inner:         &sqlparser.Order{Expr: inner},
			WeightStrExpr: aliasExpr.Expr},
		)
	}
	innerPlan, err := hp.planOrderBy(ctx, orderExprs, plan)
	if err != nil {
		return nil, err
	}
	oa := &orderedAggregate{
		resultsBuilder: resultsBuilder{
			logicalPlanCommon: newBuilderCommon(innerPlan),
			weightStrings:     make(map[*resultColumn]int),
			truncater:         eaggr,
		},
		eaggr: eaggr,
	}
	return oa, nil
}

func isAmbiguousOrderBy(index int, col sqlparser.ColIdent, exprs []abstract.SelectExpr) bool {
	if col.String() == "" {
		return false
	}
	for i, expr := range exprs {
		if i == index {
			continue
		}
		aliasExpr, isAlias := expr.Col.(*sqlparser.AliasedExpr)
		if !isAlias {
			// TODO: handle star expression error
			return true
		}
		alias := aliasExpr.As
		if alias.IsEmpty() {
			if col, ok := aliasExpr.Expr.(*sqlparser.ColName); ok {
				alias = col.Name
			}
		}
		if col.Equal(alias) {
			return true
		}
	}
	return false
}

func selectHasUniqueVindex(vschema ContextVSchema, semTable *semantics.SemTable, sel []abstract.SelectExpr) bool {
	for _, expr := range sel {
		exp, err := expr.GetExpr()
		if err != nil {
			// TODO: handle star expression error
			return false
		}
		if exprHasUniqueVindex(vschema, semTable, exp) {
			return true
		}
	}
	return false
}

// needDistinctHandling returns true if oa needs to handle the distinct clause.
// If true, it will also return the aliased expression that needs to be pushed
// down into the underlying route.
func (hp *horizonPlanning) needDistinctHandling(ctx *planningContext, funcExpr *sqlparser.FuncExpr, opcode engine.AggregateOpcode, input logicalPlan) (bool, *sqlparser.AliasedExpr, error) {
	if !funcExpr.Distinct {
		return false, nil, nil
	}
	if opcode != engine.AggregateCount && opcode != engine.AggregateSum {
		return false, nil, nil
	}
	innerAliased, ok := funcExpr.Exprs[0].(*sqlparser.AliasedExpr)
	if !ok {
		return false, nil, vterrors.Errorf(vtrpcpb.Code_INVALID_ARGUMENT, "syntax error: %s", sqlparser.String(funcExpr))
	}
	_, ok = input.(*routeGen4)
	if !ok {
		// Unreachable
		return true, innerAliased, nil
	}
	if exprHasUniqueVindex(ctx.vschema, ctx.semTable, innerAliased.Expr) {
		// if we can see a unique vindex on this table/column,
		// we know the results will be unique, and we don't need to DISTINCTify them
		return false, nil, nil
	}
	return true, innerAliased, nil
}

func (hp *horizonPlanning) planHaving(ctx *planningContext, plan logicalPlan) (logicalPlan, error) {
	if hp.sel.Having == nil {
		return plan, nil
	}
	return pushHaving(hp.sel.Having.Expr, plan, ctx.semTable)
}

func pushHaving(expr sqlparser.Expr, plan logicalPlan, semTable *semantics.SemTable) (logicalPlan, error) {
	switch node := plan.(type) {
	case *routeGen4:
		sel := sqlparser.GetFirstSelect(node.Select)
		sel.AddHaving(expr)
		return plan, nil
	case *pulloutSubquery:
		return pushHaving(expr, node.underlying, semTable)
	case *simpleProjection:
		return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: filtering on results of cross-shard derived table")
	case *orderedAggregate:
		return newFilter(semTable, plan, expr)
	}
	return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] unreachable %T.filtering", plan)
}

func isJoin(plan logicalPlan) bool {
	switch plan.(type) {
	case *joinGen4, *hashJoin:
		return true
	default:
		return false
	}
}