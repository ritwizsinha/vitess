/*
Copyright 2020 The Vitess Authors.

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

package semantics

import (
	"fmt"
	"runtime/debug"
	"strings"

	querypb "vitess.io/vitess/go/vt/proto/query"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
)

// analyzer controls the flow of the analysis.
// It starts the tree walking and controls which part of the analysis sees which parts of the tree
type analyzer struct {
	scoper *scoper
	tables *tableCollector
	binder *binder
	typer  *typer

	err          error
	inProjection int

	projErr error

	hasRewritten bool
}

// newAnalyzer create the semantic analyzer
func newAnalyzer(dbName string, si SchemaInformation) *analyzer {
	s := newScoper()
	a := &analyzer{
		scoper: s,
		tables: newTableCollector(s, si, dbName),
		typer:  newTyper(),
	}

	a.binder = newBinder(s, a, a.tables, a.typer)

	return a
}

type rewriteFunc = func(statement sqlparser.SelectStatement, semTable *SemTable) error

// NoRewrite is a helper implementation for tests
var NoRewrite = func(statement sqlparser.SelectStatement, semTable *SemTable) error {
	return nil
}

// Analyze analyzes the parsed query.
func Analyze(statement sqlparser.SelectStatement, currentDb string, si SchemaInformation, rewrite rewriteFunc) (*SemTable, error) {
	analyzer := newAnalyzer(currentDb, si)

	// Analysis for initial scope
	err := analyzer.analyze(statement)
	if err != nil {
		return nil, err
	}

	// Creation of the semantic table
	semTable := analyzer.newSemTable(statement)

	// Rewriting operation (expand star)
	if err = rewrite(statement, semTable); err != nil {
		return nil, err
	}
	analyzer.hasRewritten = true

	// Analysis post rewriting
	err = analyzer.analyze(statement)
	if err != nil {
		return nil, err
	}

	semTable.ProjectionErr = analyzer.projErr
	return semTable, nil
}

func (a analyzer) newSemTable(statement sqlparser.SelectStatement) *SemTable {
	return &SemTable{
		ExprBaseTableDeps: a.binder.exprRecursiveDeps,
		ExprDeps:          a.binder.exprDeps,
		exprTypes:         a.typer.exprTypes,
		Tables:            a.tables.Tables,
		selectScope:       a.scoper.rScope,
		ProjectionErr:     a.projErr,
		Comments:          statement.GetComments(),
		SubqueryMap:       a.binder.subqueryMap,
		SubqueryRef:       a.binder.subqueryRef,
		ColumnEqualities:  map[columnName][]sqlparser.Expr{},
	}
}

func (a *analyzer) setError(err error) {
	prErr, ok := err.(ProjError)
	if ok {
		a.projErr = prErr.inner
		return
	}

	if a.inProjection > 0 && vterrors.ErrState(err) == vterrors.NonUniqError {
		a.projErr = err
	} else {
		a.err = err
	}
}

func (a *analyzer) analyzeDown(cursor *sqlparser.Cursor) bool {
	// If we have an error we keep on going down the tree without checking for anything else
	// this way we can abort when we come back up.
	if !a.shouldContinue() {
		return true
	}

	if !a.hasRewritten {
		if err := checkForInvalidConstructs(cursor); err != nil {
			a.setError(err)
			return true
		}

		a.scoper.down(cursor)
	} else { // after expand star
		a.scoper.downPost(cursor)

		if err := a.binder.down(cursor); err != nil {
			a.setError(err)
			return true
		}
	}

	a.enterProjection(cursor)
	// this is the visitor going down the tree. Returning false here would just not visit the children
	// to the current node, but that is not what we want if we have encountered an error.
	// In order to abort the whole visitation, we have to return true here and then return false in the `analyzeUp` method
	return true
}

func (a *analyzer) analyzeUp(cursor *sqlparser.Cursor) bool {
	if !a.shouldContinue() {
		return false
	}

	if !a.hasRewritten {
		if err := a.scoper.up(cursor); err != nil {
			a.setError(err)
			return false
		}
		if err := a.tables.up(cursor); err != nil {
			a.setError(err)
			return false
		}
	} else { // after expand star
		if err := a.scoper.upPost(cursor); err != nil {
			a.setError(err)
			return false
		}
		if err := a.typer.up(cursor); err != nil {
			a.setError(err)
			return false
		}
	}

	a.leaveProjection(cursor)
	return a.shouldContinue()
}

/*
	errors that happen when we are evaluating SELECT expressions are saved until we know
	if we can merge everything into a single route or not
*/
func (a *analyzer) enterProjection(cursor *sqlparser.Cursor) {
	_, ok := cursor.Node().(sqlparser.SelectExprs)
	if ok && isParentSelect(cursor) {
		a.inProjection++
	}
}

func (a *analyzer) leaveProjection(cursor *sqlparser.Cursor) {
	_, ok := cursor.Node().(sqlparser.SelectExprs)
	if ok && isParentSelect(cursor) {
		a.inProjection--
	}
}

func isParentSelect(cursor *sqlparser.Cursor) bool {
	_, isSelect := cursor.Parent().(*sqlparser.Select)
	return isSelect
}

type originable interface {
	tableSetFor(t *sqlparser.AliasedTableExpr) TableSet
	depsForExpr(expr sqlparser.Expr) (TableSet, *querypb.Type)
}

func (a *analyzer) depsForExpr(expr sqlparser.Expr) (TableSet, *querypb.Type) {
	ts := a.binder.exprRecursiveDeps.Dependencies(expr)
	qt, isFound := a.typer.exprTypes[expr]
	if !isFound {
		return ts, nil
	}
	return ts, &qt
}

func (v *vTableInfo) checkForDuplicates() error {
	for i, name := range v.columnNames {
		for j, name2 := range v.columnNames {
			if i == j {
				continue
			}
			if name == name2 {
				return vterrors.NewErrorf(vtrpcpb.Code_INVALID_ARGUMENT, vterrors.DupFieldName, "Duplicate column name '%s'", name)
			}
		}
	}
	return nil
}

func (a *analyzer) analyze(statement sqlparser.Statement) error {
	_ = sqlparser.Rewrite(statement, a.analyzeDown, a.analyzeUp)
	return a.err
}

func checkForInvalidConstructs(cursor *sqlparser.Cursor) error {
	switch node := cursor.Node().(type) {
	case *sqlparser.JoinTableExpr:
		if node.Condition != nil && node.Condition.Using != nil {
			return vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: join with USING(column_list) clause for complex queries")
		}
		if node.Join == sqlparser.NaturalJoinType || node.Join == sqlparser.NaturalRightJoinType || node.Join == sqlparser.NaturalLeftJoinType {
			return vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: "+node.Join.ToString())
		}
	case *sqlparser.Subquery:
		sel, ok := node.Select.(*sqlparser.Select)
		if !ok {
			return Gen4NotSupportedF("%T in subquery", node.Select)
		}
		if sel.Into != nil {
			return vterrors.NewErrorf(vtrpcpb.Code_INVALID_ARGUMENT, vterrors.CantUseOptionHere, "Incorrect usage/placement of 'INTO'")
		}
	case *sqlparser.DerivedTable:
		sel, ok := node.Select.(*sqlparser.Select)
		if !ok {
			return nil
		}
		if sel.Into != nil {
			return vterrors.NewErrorf(vtrpcpb.Code_INVALID_ARGUMENT, vterrors.CantUseOptionHere, "Incorrect usage/placement of 'INTO'")
		}
	case *sqlparser.FuncExpr:
		if sqlparser.IsLockingFunc(node) {
			return vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "%v allowed only with dual", sqlparser.String(node))
		}

		if node.Distinct {
			err := vterrors.Errorf(vtrpcpb.Code_INVALID_ARGUMENT, "syntax error: %s", sqlparser.String(node))
			if len(node.Exprs) != 1 {
				return err
			} else if _, ok := node.Exprs[0].(*sqlparser.AliasedExpr); !ok {
				return err
			}
		}
	}

	return nil
}

func createVTableInfoForExpressions(expressions sqlparser.SelectExprs) *vTableInfo {
	vTbl := &vTableInfo{}
	for _, selectExpr := range expressions {
		expr, ok := selectExpr.(*sqlparser.AliasedExpr)
		if !ok {
			continue
		}
		vTbl.cols = append(vTbl.cols, expr.Expr)
		if expr.As.IsEmpty() {
			switch expr := expr.Expr.(type) {
			case *sqlparser.ColName:
				// for projections, we strip out the qualifier and keep only the column name
				vTbl.columnNames = append(vTbl.columnNames, expr.Name.String())
			default:
				vTbl.columnNames = append(vTbl.columnNames, sqlparser.String(expr))
			}
		} else {
			vTbl.columnNames = append(vTbl.columnNames, expr.As.String())
		}
	}
	return vTbl
}

func (a *analyzer) shouldContinue() bool {
	return a.err == nil
}

func (a *analyzer) tableSetFor(t *sqlparser.AliasedTableExpr) TableSet {
	return a.tables.tableSetFor(t)
}

// Gen4NotSupportedF returns a common error for shortcomings in the gen4 planner
func Gen4NotSupportedF(format string, args ...interface{}) error {
	message := fmt.Sprintf("gen4 does not yet support: "+format, args...)

	// add the line that this happens in so it is easy to find it
	stack := string(debug.Stack())
	lines := strings.Split(stack, "\n")
	message += "\n" + lines[6]
	return vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, message)
}

// ProjError is used to mark an error as something that should only be returned
// if the planner fails to merge everything down to a single route
type ProjError struct {
	inner error
}

func (p ProjError) Error() string {
	return p.inner.Error()
}
