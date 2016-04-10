package exec

import (
	"database/sql/driver"
	"encoding/gob"
	"fmt"
	"strings"
	"time"

	u "github.com/araddon/gou"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/plan"
	"github.com/araddon/qlbridge/rel"
	"github.com/araddon/qlbridge/value"
	"github.com/araddon/qlbridge/vm"
)

var (
	_ = u.EMPTY

	// Ensure that we implement the Task Runner interface
	_ TaskRunner = (*GroupBy)(nil)
)

func init() {
	gob.Register(AggPartial{})
}

// Group by:   Sql Group By Operator
//   creates a hashable key commposed of key = {each,value,of,column,in,groupby}
//
// A very stupid naive parallel groupby holds values in memory
//
//   task   ->  groupby  -->
//
type GroupBy struct {
	*TaskBase
	closed bool
	p      *plan.GroupBy
}

func NewGroupBy(ctx *plan.Context, p *plan.GroupBy) *GroupBy {
	m := &GroupBy{
		TaskBase: NewTaskBase(ctx),
		p:        p,
	}
	return m
}

// Group by:   Sql Group By Operator finalizer for partials
//
type GroupByFinal struct {
	*TaskBase
	p          *plan.GroupBy
	complete   chan bool
	closed     bool
	isComplete bool
}

func NewGroupByFinal(ctx *plan.Context, p *plan.GroupBy) *GroupByFinal {
	m := &GroupByFinal{
		TaskBase: NewTaskBase(ctx),
		p:        p,
		complete: make(chan bool),
	}
	return m
}

func (m *GroupBy) Run() error {
	defer m.Ctx.Recover()
	defer close(m.msgOutCh)

	outCh := m.MessageOut()
	inCh := m.MessageIn()

	columns := m.p.Stmt.Columns
	colIndex := m.p.Stmt.ColIndexes()

	aggs, err := buildAggs(m.p)
	if err != nil {
		return err
	}

	// are are going to hold entire row in memory while we are calculating
	//  so obviously not scalable.
	gb := make(map[string][]*datasource.SqlDriverMessageMap)

msgReadLoop:
	for {

		select {
		case <-m.SigChan():
			u.Warnf("got signal quit")
			return nil
		case msg, ok := <-inCh:
			if !ok {
				//u.Debugf("NICE, got closed channel shutdown")
				break msgReadLoop
			} else {
				var sdm *datasource.SqlDriverMessageMap

				switch mt := msg.(type) {
				case *datasource.SqlDriverMessageMap:
					sdm = mt
				default:

					msgReader, isContextReader := msg.(expr.ContextReader)
					if !isContextReader {
						err := fmt.Errorf("To use Join must use SqlDriverMessageMap but got %T", msg)
						u.Errorf("unrecognized msg %T", msg)
						close(m.TaskBase.sigCh)
						return err
					}

					sdm = datasource.NewSqlDriverMessageMapCtx(msg.Id(), msgReader, colIndex)
				}
				// We are going to use VM Engine to create a value for each statement in group by
				//  then join each value together to create a unique key.
				keys := make([]string, len(m.p.Stmt.GroupBy))
				for i, col := range m.p.Stmt.GroupBy {
					if col.Expr != nil {
						if key, ok := vm.Eval(sdm, col.Expr); ok {
							//u.Debugf("msgtype:%T  key:%q for-expr:%s", sdm, key, col.Expr)
							keys[i] = key.ToString()
						} else {
							// Is this an error?
							//u.Warnf("no key?  %s for %+v", col.Expr, sdm)
						}
					} else {
						u.Warnf("no col.expr? %#v", col)
					}
				}
				key := strings.Join(keys, ",")
				//u.Infof("found key:%s for %+v", key, sdm)
				gb[key] = append(gb[key], sdm)
			}
		}
	}

	i := uint64(0)
	for key, v := range gb {
		//u.Debugf("got %s:%v msgs", k, len(v))

		for _, mm := range v {
			for i, col := range columns {
				//u.Debugf("col: idx:%v sidx: %v pidx:%v key:%v   %s", col.Index, col.SourceIndex, col.ParentIndex, col.Key(), col.Expr)

				if col.Expr == nil {
					u.Warnf("wat?   nil col expr? %#v", col)
				} else {
					v, ok := vm.Eval(mm, col.Expr)
					//u.Infof("mt: %T  mm %#v", mm, mm)
					if !ok || v == nil {
						//u.Debugf("evaled nil? key=%v  val=%v expr:%s", col.Key(), v, col.Expr.String())
						//u.Infof("mt: %T  mm %#v", mm, mm)
						aggs[i].Do(value.NewNilValue())
					} else {
						//u.Debugf("evaled: key=%v  val=%v", col.Key(), v.Value())
						aggs[i].Do(v)
					}
				}
			}
		}

		row := make([]driver.Value, len(columns))
		for i, agg := range aggs {
			row[i] = driver.Value(agg.Result())
			agg.Reset()
			//u.Debugf("agg result: %#v  %v", row[i], row[i])
		}

		if m.p.Partial {
			// Partial results, append key at end?  shouldn't be able to be fit in message itself?
			row = append(row, key)
			//u.Debugf("GroupBy output row? key:%s %#v", key, row)
		}
		//u.Debugf("row: %v  cols:%v", row, colIndex)
		outCh <- datasource.NewSqlDriverMessageMap(i, row, colIndex)
		i++
	}

	return nil
}

func (m *GroupByFinal) Run() error {
	defer m.Ctx.Recover()
	defer close(m.msgOutCh)

	outCh := m.MessageOut()
	inCh := m.MessageIn()

	columns := m.p.Stmt.Columns
	colIndex := m.p.Stmt.ColIndexes()

	m.p.Partial = false
	aggs, err := buildAggs(m.p)
	if err != nil {
		return err
	}

	gb := make(map[string][][]driver.Value)

msgReadLoop:
	for {

		select {
		case <-m.SigChan():
			u.Warnf("got signal quit")
			return nil
		case msg, ok := <-inCh:
			if !ok {
				//u.Debugf("GroupByFinal, got closed channel shutdown")
				break msgReadLoop
			} else {
				//u.Infof("got gbfinal message %#v", msg)
				switch mt := msg.(type) {
				case *datasource.SqlDriverMessageMap:
					if len(mt.Vals) != len(columns)+1 {
						u.Warnf("Wrong number of values? %#v", mt)
					}
					key, ok := mt.Vals[len(mt.Vals)-1].(string)
					if !ok {
						u.Warnf("expected key?  %#v", mt.Vals)
					}
					vals := mt.Vals[0 : len(mt.Vals)-1]
					//u.Infof("found key:%s for %#v", key, mt.Vals)
					gb[key] = append(gb[key], vals)
				default:
					err := fmt.Errorf("To use Join must use SqlDriverMessageMap but got %T", msg)
					u.Errorf("unrecognized msg %T", msg)
					close(m.TaskBase.sigCh)
					return err
				}
			}
		}
	}

	i := uint64(0)
	for _, vals := range gb {
		//u.Debugf("got %s:%v msgs", key, vals)

		for _, dv := range vals {
			for i, col := range columns {
				//u.Debugf("col: idx:%v sidx: %v pidx:%v key:%v   %s", col.Index, col.SourceIndex, col.ParentIndex, col.Key(), col.Expr)

				if col.Expr == nil {
					u.Warnf("wat?   nil col expr? %#v", col)
				} else {
					v := dv[i]
					switch vt := v.(type) {
					case *AggPartial:
						//u.Debugf("evaled: key=%v  val=%v", col.Key(), v.Value())
						aggs[i].Merge(vt)
					case AggPartial:
						aggs[i].Merge(&vt)
					default:
						u.Warnf("unhandled type: %#v", v)
					}
				}
			}
		}

		row := make([]driver.Value, len(columns))
		for i, agg := range aggs {
			row[i] = driver.Value(agg.Result())
			agg.Reset()
			//u.Debugf("agg result: %#v  %v", row[i], row[i])
		}
		//u.Debugf("GroupBy output row? %v", row)
		outCh <- datasource.NewSqlDriverMessageMap(i, row, colIndex)
		i++
	}

	m.isComplete = true
	close(m.complete)
	return nil
}

func (m *GroupBy) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	return m.TaskBase.Close()
}
func (m *GroupByFinal) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	//u.Infof("%p group by final Close() waiting for complete", m)
	select {
	case <-ticker.C:
		u.Warnf("timeout???? ")
	case <-m.complete:
		//u.Warnf("%p got groupbyfinal complete", m)
	}

	return m.TaskBase.Close()
}

type AggPartial struct {
	Ct int64
	N  float64
}

type AggFunc func(v value.Value)
type resultFunc func() interface{}
type Aggregator interface {
	Do(v value.Value)
	Result() interface{}
	Reset()
	Merge(*AggPartial)
}
type agg struct {
	do     AggFunc
	result resultFunc
}
type groupByFunc struct {
	last interface{}
}

func (m *groupByFunc) Do(v value.Value)    { m.last = v.Value() }
func (m *groupByFunc) Result() interface{} { return m.last }
func (m *groupByFunc) Reset()              { m.last = nil }
func (m *groupByFunc) Merge(a *AggPartial) {}
func NewGroupByValue(col *rel.Column) Aggregator {
	return &groupByFunc{}
}

type sum struct {
	partial bool
	ct      int64
	n       float64
}

func (m *sum) Do(v value.Value) {
	m.ct++
	switch vt := v.(type) {
	case value.IntValue:
		m.n += vt.Float()
	case value.NumberValue:
		m.n += vt.Val()
	}
}
func (m *sum) Result() interface{} {
	if !m.partial {
		return m.n
	}
	return &AggPartial{
		m.ct,
		m.n,
	}
}
func (m *sum) Reset() { m.n = 0 }
func (m *sum) Merge(a *AggPartial) {
	m.ct += a.Ct
	m.n += a.N
}
func NewSum(col *rel.Column, partial bool) Aggregator {
	return &sum{partial: partial}
}

type avg struct {
	partial bool
	ct      int64
	n       float64
}

func (m *avg) Do(v value.Value) {
	m.ct++
	switch vt := v.(type) {
	case value.IntValue:
		m.n += vt.Float()
	case value.NumberValue:
		m.n += vt.Val()
	}
}
func (m *avg) Result() interface{} {
	if !m.partial {
		return m.n / float64(m.ct)
	}
	return &AggPartial{
		m.ct,
		m.n,
	}
}
func (m *avg) Reset() { m.n = 0; m.ct = 0 }
func (m *avg) Merge(a *AggPartial) {
	m.ct += a.Ct
	m.n += a.N
}
func NewAvg(col *rel.Column, partial bool) Aggregator {
	return &avg{partial: partial}
}

type count struct {
	n int64
}

func (m *count) Do(v value.Value)    { m.n++ }
func (m *count) Result() interface{} { return m.n }
func (m *count) Reset()              { m.n = 0 }
func (m *count) Merge(a *AggPartial) {
	m.n += a.Ct
}
func NewCount(col *rel.Column) Aggregator {
	return &count{}
}

func buildAggs(p *plan.GroupBy) ([]Aggregator, error) {
	//u.Debugf("build aggs: partial:%v  sql:%s", p.Partial, p.Stmt)
	aggs := make([]Aggregator, len(p.Stmt.Columns))
colLoop:
	for colIdx, col := range p.Stmt.Columns {
		for _, gb := range p.Stmt.GroupBy {
			if gb.As == col.As {
				// simple Non Aggregate Value
				aggs[colIdx] = NewGroupByValue(col)
				continue colLoop
			}
		}
		// Since we made it here, ann aggregate func
		//  move to a registry of some kind to allow extension
		switch n := col.Expr.(type) {
		case *expr.FuncNode:

			// TODO:  extract to a UDF Registry Similar to builtins
			switch strings.ToLower(n.Name) {
			case "avg":
				aggs[colIdx] = NewAvg(col, p.Partial)
			case "count":
				aggs[colIdx] = NewCount(col)
			case "sum":
				aggs[colIdx] = NewSum(col, p.Partial)
			default:
				return nil, fmt.Errorf("Not impelemneted groupby for column: %s", col.Expr)
			}
		case *expr.BinaryNode:
			// binary logic?
			return nil, fmt.Errorf("Not impelemneted groupby for column: %s", col.Expr)
		}
	}
	return aggs, nil
}
